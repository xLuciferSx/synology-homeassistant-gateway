package main

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// appleVerifier checks Sign in with Apple identity tokens against Apple's keys.
type appleVerifier struct {
	clientID string // the app's bundle id
	keysURL  string
	client   *http.Client

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

type appleIdentity struct {
	Sub           string
	Email         string
	EmailVerified bool
}

func newAppleVerifier(clientID string) *appleVerifier {
	return &appleVerifier{clientID: clientID, keysURL: "https://appleid.apple.com/auth/keys",
		client: &http.Client{Timeout: 10 * time.Second}, keys: map[string]*rsa.PublicKey{}}
}

func (a *appleVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if k, ok := a.keys[kid]; ok && time.Since(a.fetched) < 24*time.Hour {
		return k, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.keysURL, nil)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var set struct {
		Keys []struct{ Kid, Kty, N, E string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if k.Kty != "RSA" || err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	a.keys, a.fetched = keys, time.Now()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown apple key %q", kid)
}

func (a *appleVerifier) Verify(ctx context.Context, token string) (*appleIdentity, error) {
	if a.clientID == "" {
		return nil, errors.New("sign in with apple is not configured")
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return a.key(ctx, kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer("https://appleid.apple.com"),
		jwt.WithAudience(a.clientID), jwt.WithExpirationRequired())
	if err != nil {
		return nil, err
	}
	id := &appleIdentity{}
	id.Sub, _ = claims["sub"].(string)
	id.Email, _ = claims["email"].(string)
	switch v := claims["email_verified"].(type) { // Apple sends bool or "true"
	case bool:
		id.EmailVerified = v
	case string:
		id.EmailVerified = v == "true"
	}
	if id.Sub == "" {
		return nil, errors.New("apple token has no subject")
	}
	return id, nil
}
