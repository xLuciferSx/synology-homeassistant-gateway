package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"hearth/internal/authtoken"
	"hearth/internal/hubproto"
)

// cloud links this hub to the hubs service: it registers the hub, shows a
// pairing code until someone claims it, and keeps the list of people allowed
// to use it. App users sign in with their account; the hub checks their
// access token (from the auth service) against that list.
type cloud struct {
	hubsURL   string
	name      string
	lanURL    string
	publicURL string
	dataDir   string
	key       ed25519.PrivateKey
	id        string
	verifier  *authtoken.Verifier
	client    *http.Client

	mu        sync.Mutex
	owned     bool
	members   map[string]hubproto.Member
	pairCode  string
	codeSince time.Time
	lastSync  time.Time
	poke      chan struct{}
}

func newCloud(hubsURL, jwksURL, name, lanURL, publicURL, dataDir string) (*cloud, error) {
	key, err := loadOrCreateKey(filepath.Join(dataDir, "hub-key"))
	if err != nil {
		return nil, err
	}
	c := &cloud{
		hubsURL: hubsURL, name: name, lanURL: lanURL, publicURL: publicURL, dataDir: dataDir,
		key: key, id: hubproto.HubID(key.Public().(ed25519.PublicKey)),
		verifier: authtoken.NewVerifier(jwksURL),
		client:   &http.Client{Timeout: 45 * time.Second}, // the cloud may be waking from scale-to-zero
		members:  map[string]hubproto.Member{},
		poke:     make(chan struct{}, 1),
	}
	c.loadMembers() // work offline with the last known list
	return c, nil
}

func loadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		seed, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(b)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s is corrupt", path)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key.Seed())), 0o600); err != nil {
		return nil, fmt.Errorf("save hub key: %w", err)
	}
	return key, nil
}

func (c *cloud) membersFile() string { return filepath.Join(c.dataDir, "members.json") }

func (c *cloud) loadMembers() {
	b, err := os.ReadFile(c.membersFile())
	if err != nil {
		return
	}
	var list []hubproto.Member
	if json.Unmarshal(b, &list) == nil {
		c.setMembers(list)
	}
}

func (c *cloud) setMembers(list []hubproto.Member) {
	m := map[string]hubproto.Member{}
	owned := false
	for _, x := range list {
		m[x.UserID] = x
		owned = owned || x.Role == "owner"
	}
	c.members, c.owned = m, owned
	if owned {
		c.pairCode = ""
	}
}

// run announces now, then every 5 minutes (or when poked); after a failure
// it retries sooner, backing off from 10 seconds to 5 minutes.
func (c *cloud) run(ctx context.Context) {
	retry := 10 * time.Second
	for {
		wait := 5 * time.Minute
		if err := c.announce(ctx); err != nil {
			slog.Warn("hub announce failed", "err", err, "retry_in", retry)
			wait, retry = retry, min(retry*2, 5*time.Minute)
		} else {
			retry = 10 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-c.poke:
		}
	}
}

func (c *cloud) announce(ctx context.Context) error {
	c.mu.Lock()
	if !c.owned && (c.pairCode == "" || time.Since(c.codeSince) > 25*time.Minute) {
		c.pairCode, c.codeSince = hubproto.NewCode(), time.Now()
		slog.Info("hub not set up yet: in the app choose Add hub and enter this code",
			"code", hubproto.FormatCode(c.pairCode), "hub", c.id)
	}
	a := hubproto.Announce{
		HubID: c.id, PublicKey: base64.StdEncoding.EncodeToString(c.key.Public().(ed25519.PublicKey)),
		Name: c.name, LANURL: c.lanURL, PublicURL: c.publicURL, Version: version,
		PairCode: c.pairCode, Timestamp: time.Now().Unix(),
	}
	c.mu.Unlock()

	body, _ := json.Marshal(a)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.hubsURL+"/v1/hubs/announce", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hubproto.SignatureHeader, hubproto.Sign(c.key, body))
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hubs service: %s", resp.Status)
	}
	var out hubproto.AnnounceResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	c.mu.Lock()
	wasOwned := c.owned
	c.setMembers(out.Members)
	c.lastSync = time.Now()
	c.mu.Unlock()
	if out.Owned && !wasOwned {
		slog.Info("hub claimed", "members", len(out.Members))
	}
	if b, err := json.Marshal(out.Members); err == nil {
		_ = os.WriteFile(c.membersFile(), b, 0o600)
	}
	return nil
}

var errNoAccess = errors.New("no access to this hub")

// authorize checks an access token and that its user may use this hub. An
// unknown user triggers one immediate re-sync (they may have just joined).
func (c *cloud) authorize(ctx context.Context, token string) (*authtoken.Claims, error) {
	claims, err := c.verifier.Verify(ctx, token)
	if err != nil {
		return nil, err
	}
	if c.isMember(claims.Subject) {
		return claims, nil
	}
	c.mu.Lock()
	recent := time.Since(c.lastSync) < 10*time.Second
	c.mu.Unlock()
	if !recent {
		sctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		err := c.announce(sctx)
		cancel()
		if err != nil {
			slog.Warn("member re-sync failed", "err", err)
		}
		if c.isMember(claims.Subject) {
			return claims, nil
		}
	}
	return nil, errNoAccess
}

func (c *cloud) isMember(userID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.members[userID]
	return ok
}

func (c *cloud) status() (owned bool, code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owned, c.pairCode
}
