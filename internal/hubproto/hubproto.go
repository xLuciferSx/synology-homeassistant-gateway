// Package hubproto is the contract between a home hub (gateway) and the cloud
// hubs service: how hubs prove who they are and what they exchange.
package hubproto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// SignatureHeader carries base64(Ed25519 signature of the raw request body).
const SignatureHeader = "X-Hub-Signature"

// HubID derives a hub's id from its public key, binding the id to the key.
func HubID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "hub_" + hex.EncodeToString(sum[:10])
}

// Announce is sent by a hub every few minutes (and after start-up).
type Announce struct {
	HubID     string `json:"hub_id"`
	PublicKey string `json:"public_key"` // base64 std
	Name      string `json:"name"`
	LANURL    string `json:"lan_url,omitempty"`
	PublicURL string `json:"public_url,omitempty"`
	Version   string `json:"version,omitempty"`
	PairCode  string `json:"pair_code,omitempty"` // only while the hub has no owner
	Timestamp int64  `json:"ts"`                  // unix seconds; replay protection
}

type Member struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
	Role   string `json:"role"` // owner | member
}

// AnnounceResponse tells the hub whether it is claimed and who may use it.
type AnnounceResponse struct {
	Owned   bool     `json:"owned"`
	Members []Member `json:"members"`
}

func Sign(key ed25519.PrivateKey, body []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, body))
}

func Verify(pub ed25519.PublicKey, body []byte, sig string) bool {
	b, err := base64.StdEncoding.DecodeString(sig)
	return err == nil && ed25519.Verify(pub, body, b)
}

// Codes use an alphabet without look-alikes (no 0/O, 1/I/L), 8 characters,
// shown as XXXX-XXXX.
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func NewCode() string {
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b)
}

// NormalizeCode accepts "abcd-efgh", "ABCD EFGH" etc.
func NormalizeCode(s string) string {
	s = strings.ToUpper(s)
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(codeAlphabet, r) {
			return r
		}
		return -1
	}, s)
}

func FormatCode(c string) string {
	if len(c) == 8 {
		return c[:4] + "-" + c[4:]
	}
	return c
}
