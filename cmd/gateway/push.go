package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// push sends activity alerts to phones through Apple Push Notifications. Each phone
// registers its token with the categories its owner wants; the hub talks to Apple
// directly, so notifications need no cloud service of ours.

const (
	catSafety  = "safety"  // alarm, leak, smoke, door left open, unlocked at night
	catJobs    = "jobs"    // washer, dishwasher, print, car, vacuum
	catPeople  = "people"  // arrivals and departures
	catDevices = "devices" // offline, low battery, room too hot or cold
)

type pushDevice struct {
	Token      string          `json:"token"`
	User       string          `json:"user"` // account subject ("admin" for the hub token)
	Name       string          `json:"name"`
	Sandbox    bool            `json:"sandbox"`
	Categories map[string]bool `json:"categories"`
	Updated    time.Time       `json:"updated"`
}

type push struct {
	mu      sync.Mutex
	path    string
	devices map[string]pushDevice

	key     *ecdsa.PrivateKey
	keyID   string
	teamID  string
	topic   string
	client  *http.Client
	jwt     string
	jwtAt   time.Time
	allowed func(user string) bool // still a member of this hub
	// critical sends alarm alerts as Critical Alerts, which sound even on silent. Needs Apple's
	// com.apple.developer.usernotifications.critical-alerts entitlement on the app (APNS_CRITICAL=1).
	critical bool
}

func loadPush(dir, keyFile, keyID, teamID, topic string) *push {
	p := &push{path: filepath.Join(dir, "push.json"), devices: map[string]pushDevice{}, keyID: keyID, teamID: teamID, topic: topic,
		client: &http.Client{Timeout: 15 * time.Second}, allowed: func(string) bool { return true }}
	if b, err := os.ReadFile(p.path); err == nil {
		_ = json.Unmarshal(b, &p.devices)
	}
	if keyFile == "" || keyID == "" {
		slog.Info("push notifications off: APNS_KEY_FILE and APNS_KEY_ID not set")
		return p
	}
	key, err := readP8(keyFile)
	if err != nil {
		slog.Warn("push notifications off", "err", err)
		return p
	}
	p.key = key
	slog.Info("push notifications on", "phones", len(p.devices))
	return p
}

func readP8(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("APNs key isn't a PEM file")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("APNs key isn't an EC key")
	}
	return ec, nil
}

func (p *push) configured() bool { return p.key != nil }

func (p *push) save() {
	if b, err := json.Marshal(p.devices); err == nil {
		_ = os.WriteFile(p.path, b, 0o600)
	}
}

func (p *push) register(d pushDevice) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d.Updated = time.Now()
	p.devices[d.Token] = d
	p.save()
}

func (p *push) forget(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.devices[token]; ok {
		delete(p.devices, token)
		p.save()
	}
}

// categoryOf sorts an activity entry into what people can switch on or off;
// "" means the entry isn't worth a notification.
func categoryOf(e Entry) string {
	switch e.Icon {
	case "shield.lefthalf.filled":
		if e.Level == "alert" {
			return catSafety
		}
	case "lock.open.fill", "door.left.hand.open":
		if e.Level == "alert" {
			return catSafety
		}
	case "exclamationmark.triangle.fill":
		if strings.HasSuffix(e.Text, "needs help") {
			return catJobs
		}
		return catSafety
	case "washer.fill", "dishwasher.fill", "printer.fill", "bolt.car.fill":
		if e.Level == "alert" {
			return catJobs
		}
	case "house.fill", "figure.walk":
		return catPeople
	case "wifi.slash", "battery.25percent", "thermometer.medium":
		return catDevices
	}
	return ""
}

// notify sends an activity entry to every phone that wants its category.
func (p *push) notify(e Entry) {
	cat := categoryOf(e)
	if cat == "" || !p.configured() {
		return
	}
	p.mu.Lock()
	var targets []pushDevice
	for _, d := range p.devices {
		if d.Categories[cat] && p.allowed(d.User) {
			targets = append(targets, d)
		}
	}
	p.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	title := e.Room
	if title == "" {
		title = "Hearth"
	}
	aps := map[string]any{
		"alert":     map[string]string{"title": title, "body": e.Text},
		"sound":     "default",
		"thread-id": cat,
	}
	if cat == catSafety {
		aps["interruption-level"] = "time-sensitive"
		if p.critical && e.Level == "alert" {
			aps["interruption-level"] = "critical"
			aps["sound"] = map[string]any{"critical": 1, "name": "default", "volume": 1.0}
		}
	}
	body, _ := json.Marshal(map[string]any{"aps": aps, "device_id": e.DeviceID, "category": cat})
	go func() {
		for _, d := range targets {
			if err := p.send(d, body); err != nil {
				slog.Warn("push failed", "phone", d.Name, "err", err)
			}
		}
	}()
}

func (p *push) token() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.jwt != "" && time.Since(p.jwtAt) < 45*time.Minute {
		return p.jwt, nil
	}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": p.teamID, "iat": time.Now().Unix()})
	t.Header["kid"] = p.keyID
	s, err := t.SignedString(p.key)
	if err != nil {
		return "", err
	}
	p.jwt, p.jwtAt = s, time.Now()
	return s, nil
}

func (p *push) send(d pushDevice, body []byte) error {
	tok, err := p.token()
	if err != nil {
		return err
	}
	host := "https://api.push.apple.com"
	if d.Sandbox {
		host = "https://api.sandbox.push.apple.com"
	}
	req, _ := http.NewRequest(http.MethodPost, host+"/3/device/"+d.Token, bytes.NewReader(body))
	req.Header.Set("authorization", "bearer "+tok)
	req.Header.Set("apns-topic", p.topic)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var reason struct {
		Reason string `json:"reason"`
	}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &reason)
	if resp.StatusCode == http.StatusGone || reason.Reason == "BadDeviceToken" || reason.Reason == "Unregistered" {
		p.forget(d.Token)
	}
	return fmt.Errorf("apns %d %s", resp.StatusCode, reason.Reason)
}

// routes: POST /api/push registers this phone, DELETE /api/push/{token} forgets it,
// POST /api/push/test sends this phone a test notification.
func (p *push) routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	type registration struct {
		Token      string          `json:"token"`
		Name       string          `json:"name"`
		Sandbox    bool            `json:"sandbox"`
		Categories map[string]bool `json:"categories"`
	}
	mux.HandleFunc("POST /api/push", auth(func(w http.ResponseWriter, r *http.Request) {
		var reg registration
		if err := json.NewDecoder(r.Body).Decode(&reg); err != nil || len(reg.Token) < 32 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "That isn't a push token."})
			return
		}
		p.register(pushDevice{Token: reg.Token, User: userFrom(r), Name: reg.Name, Sandbox: reg.Sandbox, Categories: reg.Categories})
		writeJSON(w, http.StatusOK, map[string]any{"configured": p.configured()})
	}))
	mux.HandleFunc("DELETE /api/push/{token}", auth(func(w http.ResponseWriter, r *http.Request) {
		p.forget(r.PathValue("token"))
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/push/test", auth(func(w http.ResponseWriter, r *http.Request) {
		var reg registration
		_ = json.NewDecoder(r.Body).Decode(&reg)
		p.mu.Lock()
		d, ok := p.devices[reg.Token]
		p.mu.Unlock()
		if !ok || !p.configured() {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Notifications aren't set up on the hub yet."})
			return
		}
		body, _ := json.Marshal(map[string]any{"aps": map[string]any{
			"alert": map[string]string{"title": "Hearth", "body": "Notifications are working."}, "sound": "default"}})
		if err := p.send(d, body); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
