// Package authtoken defines the access tokens the account service issues and
// hubs verify. Tokens are EdDSA (Ed25519) JWTs; hubs fetch the public keys
// from the account service's JWKS endpoint, so no secret is shared with hubs.
package authtoken

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const Issuer = "homeapp-account"

// Claims of an access token. Hubs maps hub id → role ("owner" or "member").
type Claims struct {
	Email string            `json:"email,omitempty"`
	Name  string            `json:"name,omitempty"`
	Hubs  map[string]string `json:"hubs,omitempty"`
	jwt.RegisteredClaims
}

// Signer issues access tokens.
type Signer struct {
	KeyID string
	Key   ed25519.PrivateKey
	TTL   time.Duration
}

func (s Signer) Issue(userID, email, name string, hubs map[string]string) (string, time.Time, error) {
	exp := time.Now().Add(s.TTL)
	c := Claims{
		Email: email, Name: name, Hubs: hubs,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	t.Header["kid"] = s.KeyID
	signed, err := t.SignedString(s.Key)
	return signed, exp, err
}

// JWK is one Ed25519 public key in JWKS form (RFC 8037).
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
}

func (s Signer) JWKS() map[string][]JWK {
	pub := s.Key.Public().(ed25519.PublicKey)
	return map[string][]JWK{"keys": {{
		Kty: "OKP", Crv: "Ed25519", Kid: s.KeyID, Use: "sig", Alg: "EdDSA",
		X: base64.RawURLEncoding.EncodeToString(pub),
	}}}
}

// Verifier checks access tokens against keys fetched from a JWKS URL,
// refreshing when it sees an unknown key id (key rotation).
type Verifier struct {
	URL    string
	Client *http.Client

	mu      sync.Mutex
	keys    map[string]ed25519.PublicKey
	fetched time.Time
}

func NewVerifier(jwksURL string) *Verifier {
	return &Verifier{URL: jwksURL, Client: &http.Client{Timeout: 10 * time.Second}, keys: map[string]ed25519.PublicKey{}}
}

// SetKeys pre-loads keys (tests, or a hub that is offline from the cloud).
func (v *Verifier) SetKeys(keys map[string]ed25519.PublicKey) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.keys, v.fetched = keys, time.Now()
}

func (v *Verifier) key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	if time.Since(v.fetched) < 30*time.Second || v.URL == "" { // don't hammer on unknown kids
		return nil, fmt.Errorf("unknown key %q", kid)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, v.URL, nil)
	resp, err := v.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	var set struct{ Keys []JWK }
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}
	keys := map[string]ed25519.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			continue
		}
		if b, err := base64.RawURLEncoding.DecodeString(k.X); err == nil && len(b) == ed25519.PublicKeySize {
			keys[k.Kid] = b
		}
	}
	v.keys, v.fetched = keys, time.Now()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown key %q", kid)
}

var ErrInvalid = errors.New("invalid token")

func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.key(ctx, kid)
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer(Issuer), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return &c, nil
}
