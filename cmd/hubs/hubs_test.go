package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"hearth/internal/authtoken"
	"hearth/internal/hubproto"
	"hearth/internal/pg"
)

type env struct {
	h      http.Handler
	signer authtoken.Signer
}

func setup(t *testing.T) *env {
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
	if _, err := db.Exec(ctx, "drop schema if exists hubs cascade"); err != nil {
		t.Fatal(err)
	}
	sub, _ := fs.Sub(migrations, "migrations")
	if err := pg.Migrate(ctx, db, "hubs", sub); err != nil {
		t.Fatal(err)
	}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	v := authtoken.NewVerifier("")
	v.SetKeys(map[string]ed25519.PublicKey{"k": pub})
	s := &server{db: db, verifier: v, now: time.Now}
	return &env{h: s.routes(), signer: authtoken.Signer{KeyID: "k", Key: key, TTL: time.Minute}}
}

func (e *env) token(t *testing.T, uid, email string) string {
	tok, _, err := e.signer.Issue(uid, email, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e *env) do(t *testing.T, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(body)
	req := httptest.NewRequest(method, path, &buf)
	req.RemoteAddr = "10.0.0.2:1"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	out := map[string]any{}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

type fakeHub struct {
	key ed25519.PrivateKey
	id  string
}

func newHub() fakeHub {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	return fakeHub{key: key, id: hubproto.HubID(pub)}
}

func (e *env) announce(t *testing.T, hub fakeHub, code string, sign ed25519.PrivateKey) (int, hubproto.AnnounceResponse) {
	t.Helper()
	body, _ := json.Marshal(hubproto.Announce{
		HubID: hub.id, PublicKey: base64.StdEncoding.EncodeToString(hub.key.Public().(ed25519.PublicKey)),
		Name: "Hearth", LANURL: "http://192.168.1.20:8780", PairCode: code, Timestamp: time.Now().Unix(),
	})
	req := httptest.NewRequest("POST", "/v1/hubs/announce", bytes.NewReader(body))
	req.RemoteAddr = "10.0.0.3:1"
	req.Header.Set(hubproto.SignatureHeader, hubproto.Sign(sign, body))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var out hubproto.AnnounceResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestPairInviteFlow(t *testing.T) {
	e := setup(t)
	hub := newHub()
	owner := e.token(t, "11111111-1111-1111-1111-111111111111", "alex@example.com")
	family := e.token(t, "22222222-2222-2222-2222-222222222222", "sam@example.com")

	// a stranger's key can't announce for this hub id
	other := newHub()
	if code, _ := e.announce(t, hub, "ABCD2345", other.key); code != 401 {
		t.Errorf("forged announce: %d", code)
	}

	code, resp := e.announce(t, hub, "ABCD2345", hub.key)
	if code != 200 || resp.Owned || len(resp.Members) != 0 {
		t.Fatalf("first announce: %d %+v", code, resp)
	}

	if code, _ := e.do(t, "POST", "/v1/hubs/claim", owner, map[string]string{"code": "WRONG000"}); code != 404 {
		t.Errorf("wrong code: %d", code)
	}
	code, out := e.do(t, "POST", "/v1/hubs/claim", owner, map[string]string{"code": "abcd-2345"})
	if code != 201 || out["role"] != "owner" || out["lan_url"] != "http://192.168.1.20:8780" || out["online"] != true {
		t.Fatalf("claim: %d %v", code, out)
	}
	// the pairing code is single use
	if code, _ := e.do(t, "POST", "/v1/hubs/claim", family, map[string]string{"code": "ABCD2345"}); code != 404 {
		t.Errorf("reused pairing code: %d", code)
	}

	// the hub learns its owner on the next announce and stops offering codes
	_, resp = e.announce(t, hub, "ZZZZ9999", hub.key)
	if !resp.Owned || len(resp.Members) != 1 || resp.Members[0].Email != "alex@example.com" {
		t.Fatalf("announce after claim: %+v", resp)
	}
	if code, _ := e.do(t, "POST", "/v1/hubs/claim", family, map[string]string{"code": "ZZZZ9999"}); code != 404 {
		t.Errorf("owned hub accepted a new pairing code: %d", code)
	}

	// family member: only the owner can invite
	if code, _ := e.do(t, "POST", "/v1/hubs/"+hub.id+"/invites", family, nil); code != 404 {
		t.Errorf("non-member invite: %d", code)
	}
	code, out = e.do(t, "POST", "/v1/hubs/"+hub.id+"/invites", owner, nil)
	if code != 201 {
		t.Fatalf("invite: %d %v", code, out)
	}
	code, out = e.do(t, "POST", "/v1/hubs/claim", family, map[string]string{"code": out["code"].(string)})
	if code != 201 || out["role"] != "member" || out["members"] != float64(2) {
		t.Fatalf("join: %d %v", code, out)
	}
	if code, _ := e.do(t, "POST", "/v1/hubs/"+hub.id+"/invites", family, nil); code != 403 {
		t.Errorf("member invite: %d", code)
	}
	_, resp = e.announce(t, hub, "", hub.key)
	if len(resp.Members) != 2 {
		t.Errorf("hub members: %+v", resp.Members)
	}

	// listing and leaving
	code, out = e.do(t, "GET", "/v1/hubs", family, nil)
	if hubs := out["hubs"].([]any); code != 200 || len(hubs) != 1 {
		t.Fatalf("list: %d %v", code, out)
	}
	if code, _ := e.do(t, "DELETE", "/v1/hubs/"+hub.id+"/members/11111111-1111-1111-1111-111111111111", owner, nil); code != 400 {
		t.Errorf("owner leaving: %d", code)
	}
	if code, _ := e.do(t, "DELETE", "/v1/hubs/"+hub.id+"/members/22222222-2222-2222-2222-222222222222", family, nil); code != 204 {
		t.Errorf("member leaving: %d", code)
	}
	_, resp = e.announce(t, hub, "", hub.key)
	if len(resp.Members) != 1 {
		t.Errorf("after leave: %+v", resp.Members)
	}
	if code, _ := e.do(t, "GET", "/v1/hubs", "", nil); code != 401 {
		t.Errorf("unauthenticated list: %d", code)
	}
}
