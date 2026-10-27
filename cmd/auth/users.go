package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/argon2"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

type User struct {
	ID           string
	Email        string
	Name         string
	PasswordHash *string
	AppleSub     *string
	Handle       []byte // WebAuthn user handle
	Credentials  []webauthn.Credential
}

// waUser adapts User to the WebAuthn library.
type waUser struct{ *User }

func (u waUser) WebAuthnID() []byte                         { return u.Handle }
func (u waUser) WebAuthnName() string                       { return u.Email }
func (u waUser) WebAuthnDisplayName() string                { return firstNonEmpty(u.Name, u.Email) }
func (u waUser) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func normalizeEmail(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 254 {
		return "", errors.New("Enter a valid email address.")
	}
	return e, nil
}

// --- passwords: Argon2id (OWASP parameters) ---

const (
	argonMem  = 19 * 1024 // KiB
	argonTime = 2
	argonPar  = 1
)

func hashPassword(pw string) string {
	salt := randomBytes(16)
	h := argon2.IDKey([]byte(pw), salt, argonTime, argonMem, argonPar, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMem, argonTime, argonPar,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(h))
}

func checkPassword(encoded, pw string) bool {
	var m, t uint32
	var p uint8
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash keeps login timing the same for unknown emails.
var dummyHash = hashPassword("not-a-real-password")

func validatePassword(pw string) error {
	if len(pw) < 10 {
		return errors.New("Use at least 10 characters for your password.")
	}
	if len(pw) > 256 {
		return errors.New("That password is too long.")
	}
	return nil
}

// --- users ---

const userCols = "id::text, email, name, password_hash, apple_sub, webauthn_handle"

func (s *server) scanUser(ctx context.Context, where string, arg any) (*User, error) {
	var u User
	err := s.db.QueryRow(ctx, "select "+userCols+" from auth.users where "+where, arg).
		Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.AppleSub, &u.Handle)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, "select credential from auth.passkeys where user_id=$1", u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var c webauthn.Credential
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if json.Unmarshal(raw, &c) == nil {
			u.Credentials = append(u.Credentials, c)
		}
	}
	return &u, rows.Err()
}

func (s *server) userByID(ctx context.Context, id string) (*User, error) {
	return s.scanUser(ctx, "id=$1::uuid", id)
}
func (s *server) userByEmail(ctx context.Context, email string) (*User, error) {
	return s.scanUser(ctx, "email=$1", email)
}
func (s *server) userByApple(ctx context.Context, sub string) (*User, error) {
	return s.scanUser(ctx, "apple_sub=$1", sub)
}
func (s *server) userByHandle(ctx context.Context, handle []byte) (*User, error) {
	return s.scanUser(ctx, "webauthn_handle=$1", handle)
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func (s *server) createUser(ctx context.Context, email, name string, pwHash, appleSub *string) (*User, error) {
	var id string
	handle := randomBytes(32)
	err := s.db.QueryRow(ctx,
		`insert into auth.users (email, name, password_hash, apple_sub, webauthn_handle)
		 values ($1,$2,$3,$4,$5) returning id::text`, email, name, pwHash, appleSub, handle).Scan(&id)
	if isUniqueViolation(err) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Email: email, Name: name, PasswordHash: pwHash, AppleSub: appleSub, Handle: handle}, nil
}

// --- refresh tokens: random, stored hashed, rotated on use ---

const refreshTTL = 60 * 24 * time.Hour

func hashToken(tok string) []byte {
	sum := sha256.Sum256([]byte(tok))
	return sum[:]
}

func (s *server) issueRefresh(ctx context.Context, userID string) (string, error) {
	ctx = context.WithoutCancel(ctx)
	tok := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	_, err := s.db.Exec(ctx, `insert into auth.refresh_tokens (token_hash, user_id, expires_at) values ($1, $2::uuid, $3)`,
		hashToken(tok), userID, time.Now().Add(refreshTTL))
	return tok, err
}

// useRefreshGrace is how long a rotated refresh token keeps working, so a
// phone whose refresh reply was lost (bad signal, app suspended) can retry.
const useRefreshGrace = 2 * time.Minute

// useRefresh rotates a refresh token: it stays valid only for a short grace
// period after use, then expires. It runs even if the client disconnects.
func (s *server) useRefresh(ctx context.Context, tok string) (string, error) {
	ctx = context.WithoutCancel(ctx)
	var uid string
	err := s.db.QueryRow(ctx, `update auth.refresh_tokens set expires_at = least(expires_at, now() + $2::interval)
		where token_hash=$1 and expires_at > now() returning user_id::text`,
		hashToken(tok), useRefreshGrace.String()).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return uid, err
}

func (s *server) revokeRefresh(ctx context.Context, tok string) error {
	_, err := s.db.Exec(ctx, `delete from auth.refresh_tokens where token_hash=$1`, hashToken(tok))
	return err
}

// --- WebAuthn sessions (challenge state between begin and finish) ---

func (s *server) saveSession(ctx context.Context, userID *string, data *webauthn.SessionData) (string, error) {
	id := base64.RawURLEncoding.EncodeToString(randomBytes(24))
	raw, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(ctx, `insert into auth.webauthn_sessions (id, user_id, data, expires_at) values ($1, $2::uuid, $3, $4)`,
		id, userID, raw, time.Now().Add(5*time.Minute))
	return id, err
}

func (s *server) takeSession(ctx context.Context, id string) (*webauthn.SessionData, *string, error) {
	var raw []byte
	var uid *string
	err := s.db.QueryRow(ctx, `delete from auth.webauthn_sessions where id=$1 and expires_at > now() returning data, user_id::text`, id).
		Scan(&raw, &uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	var sd webauthn.SessionData
	return &sd, uid, json.Unmarshal(raw, &sd)
}

// cleanup removes expired rows; run periodically.
func (s *server) cleanup(ctx context.Context) {
	_, _ = s.db.Exec(ctx, `delete from auth.refresh_tokens where expires_at < now()`)
	_, _ = s.db.Exec(ctx, `delete from auth.webauthn_sessions where expires_at < now()`)
}
