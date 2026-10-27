package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"hearth/internal/pg"
)

// Tests run against a real PostgreSQL (brew services start postgresql@17;
// createdb homeapp_test). Set TEST_DATABASE_URL to use another server.
func testServer(t *testing.T) (*server, http.Handler) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://localhost/homeapp_test?sslmode=disable"
	}
	ctx := context.Background()
	db, err := pg.Connect(ctx, url)
	if err != nil {
		t.Skipf("no test database: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(ctx, "drop schema if exists auth cascade"); err != nil {
		t.Fatal(err)
	}
	sub, _ := fs.Sub(migrations, "migrations")
	if err := pg.Migrate(ctx, db, "auth", sub); err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	s, err := newServer(db, key, "test", "com.example.hearth", "TEAM", "auth.example.com", []string{"https://auth.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return s, s.routes()
}

func call(t *testing.T, h http.Handler, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.RemoteAddr = "10.0.0.1:1234"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]any{}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestPasswordFlow(t *testing.T) {
	s, h := testServer(t)

	code, _ := call(t, h, "POST", "/v1/auth/signup", "", map[string]string{"email": "bad", "password": "longenough123"})
	if code != 400 {
		t.Fatalf("invalid email: %d", code)
	}
	code, _ = call(t, h, "POST", "/v1/auth/signup", "", map[string]string{"email": "a@b.co", "password": "short"})
	if code != 400 {
		t.Fatalf("short password: %d", code)
	}
	code, out := call(t, h, "POST", "/v1/auth/signup", "", map[string]string{"email": " Alex@Example.com ", "password": "correct horse 1", "name": "Alex"})
	if code != 201 || out["access_token"] == nil || out["refresh_token"] == nil {
		t.Fatalf("signup: %d %v", code, out)
	}
	if u := out["user"].(map[string]any); u["email"] != "alex@example.com" || u["has_password"] != true {
		t.Errorf("user %v", u)
	}
	code, _ = call(t, h, "POST", "/v1/auth/signup", "", map[string]string{"email": "alex@example.com", "password": "another pass 2"})
	if code != 409 {
		t.Errorf("duplicate signup: %d", code)
	}

	code, _ = call(t, h, "POST", "/v1/auth/login", "", map[string]string{"email": "alex@example.com", "password": "wrong password"})
	if code != 401 {
		t.Errorf("wrong password: %d", code)
	}
	code, out = call(t, h, "POST", "/v1/auth/login", "", map[string]string{"email": "ALEX@example.com", "password": "correct horse 1"})
	if code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}
	access, refresh := out["access_token"].(string), out["refresh_token"].(string)

	claims, err := s.verifier.Verify(context.Background(), access)
	if err != nil || claims.Email != "alex@example.com" {
		t.Fatalf("token: %v %v", claims, err)
	}
	code, out = call(t, h, "GET", "/v1/me", access, nil)
	if code != 200 || out["name"] != "Alex" {
		t.Errorf("me: %d %v", code, out)
	}
	if code, _ = call(t, h, "GET", "/v1/me", "garbage", nil); code != 401 {
		t.Errorf("me with bad token: %d", code)
	}

	// refresh rotates: the old token keeps working only for a short grace period
	code, out = call(t, h, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": refresh})
	if code != 200 {
		t.Fatalf("refresh: %d", code)
	}
	if code, _ = call(t, h, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": refresh}); code != 200 {
		t.Errorf("retry within grace: %d", code)
	}
	s.db.Exec(context.Background(), `update auth.refresh_tokens set expires_at = now() - interval '1 second' where token_hash=$1`, hashToken(refresh))
	if code, _ = call(t, h, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": refresh}); code != 401 {
		t.Errorf("reused refresh token after grace: %d", code)
	}
	newRefresh := out["refresh_token"].(string)
	if code, _ = call(t, h, "POST", "/v1/auth/logout", "", map[string]string{"refresh_token": newRefresh}); code != 204 {
		t.Errorf("logout: %d", code)
	}
	if code, _ = call(t, h, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": newRefresh}); code != 401 {
		t.Errorf("refresh after logout: %d", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	_, h := testServer(t)
	var code int
	for i := 0; i < 11; i++ {
		code, _ = call(t, h, "POST", "/v1/auth/login", "", map[string]string{"email": "x@y.co", "password": "nope nope nope"})
	}
	if code != 429 {
		t.Errorf("11th login attempt = %d, want 429", code)
	}
}

func TestAppleSignIn(t *testing.T) {
	s, h := testServer(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "apple1", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer keys.Close()
	s.apple.keysURL = keys.URL

	token := func(sub, email, aud string) string {
		tk := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": "https://appleid.apple.com", "aud": aud, "sub": sub, "email": email,
			"email_verified": "true", "exp": time.Now().Add(time.Minute).Unix(),
		})
		tk.Header["kid"] = "apple1"
		signed, _ := tk.SignedString(key)
		return signed
	}

	code, out := call(t, h, "POST", "/v1/auth/apple", "", map[string]string{"identity_token": token("apple-sub-1", "me@icloud.com", "com.example.hearth"), "name": "Alex"})
	if code != 201 || out["user"].(map[string]any)["has_apple"] != true {
		t.Fatalf("apple signup: %d %v", code, out)
	}
	code, _ = call(t, h, "POST", "/v1/auth/apple", "", map[string]string{"identity_token": token("apple-sub-1", "me@icloud.com", "com.example.hearth")})
	if code != 200 {
		t.Errorf("apple sign-in again: %d", code)
	}
	code, _ = call(t, h, "POST", "/v1/auth/apple", "", map[string]string{"identity_token": token("apple-sub-1", "me@icloud.com", "com.someone.else")})
	if code != 401 {
		t.Errorf("token for another app accepted: %d", code)
	}

	// linking: an email+password account gains Apple sign-in for the same verified email
	call(t, h, "POST", "/v1/auth/signup", "", map[string]string{"email": "both@example.com", "password": "correct horse 1"})
	code, out = call(t, h, "POST", "/v1/auth/apple", "", map[string]string{"identity_token": token("apple-sub-2", "both@example.com", "com.example.hearth")})
	if u := out["user"].(map[string]any); code != 200 || u["has_password"] != true || u["has_apple"] != true {
		t.Errorf("link: %d %v", code, out)
	}
}

func TestPasskeyBegin(t *testing.T) {
	_, h := testServer(t)
	_, out := call(t, h, "POST", "/v1/auth/signup", "", map[string]string{"email": "pk@example.com", "password": "correct horse 1"})
	access := out["access_token"].(string)

	code, out := call(t, h, "POST", "/v1/me/passkeys/begin", access, nil)
	opts, _ := out["options"].(map[string]any)
	if code != 200 || out["session_id"] == "" || opts["publicKey"] == nil {
		t.Fatalf("register begin: %d %v", code, out)
	}
	pk := opts["publicKey"].(map[string]any)
	if pk["rp"].(map[string]any)["id"] != "auth.example.com" {
		t.Errorf("rp %v", pk["rp"])
	}
	code, out = call(t, h, "POST", "/v1/auth/passkey/begin", "", nil)
	if code != 200 || out["session_id"] == "" {
		t.Fatalf("login begin: %d %v", code, out)
	}
	code, _ = call(t, h, "POST", "/v1/auth/passkey/finish?session=nope", "", map[string]string{})
	if code != 400 {
		t.Errorf("unknown session: %d", code)
	}
}
