// Command auth is the account microservice: sign-up and sign-in with email +
// password, Sign in with Apple and passkeys (Face ID / Touch ID). It issues
// short-lived EdDSA access tokens (verified by other services and hubs via
// /.well-known/jwks.json) and rotating refresh tokens. Data lives in the
// "auth" schema of PostgreSQL.
package main

import (
	"context"
	"crypto/ed25519"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgxpool"

	"hearth/internal/authtoken"
	"hearth/internal/config"
	"hearth/internal/httpx"
	"hearth/internal/pg"
)

//go:embed migrations/*.sql
var migrations embed.FS

type server struct {
	db       *pgxpool.Pool
	signer   authtoken.Signer
	verifier *authtoken.Verifier
	apple    *appleVerifier
	wa       *webauthn.WebAuthn
	teamApp  string // "<team id>.<bundle id>" for apple-app-site-association
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := pg.Connect(ctx, config.Must("DATABASE_URL"))
	if err != nil {
		slog.Error("database", "err", err)
		os.Exit(1)
	}
	sub, _ := fs.Sub(migrations, "migrations")
	if err := pg.Migrate(ctx, db, "auth", sub); err != nil {
		slog.Error("migrate", "err", err)
		os.Exit(1)
	}
	seed, err := base64.StdEncoding.DecodeString(config.Must("AUTH_SIGNING_KEY"))
	if err != nil || len(seed) != ed25519.SeedSize {
		slog.Error("AUTH_SIGNING_KEY must be a base64 32-byte Ed25519 seed")
		os.Exit(2)
	}
	rpID := config.Must("WEBAUTHN_RP_ID")
	origins := strings.Split(config.Get("WEBAUTHN_ORIGINS", "https://"+rpID), ",")
	s, err := newServer(db, ed25519.NewKeyFromSeed(seed), config.Get("AUTH_KEY_ID", "k1"),
		config.Get("APPLE_CLIENT_ID", "com.example.hearth"), config.Get("APPLE_TEAM_ID", ""), rpID, origins)
	if err != nil {
		slog.Error("setup", "err", err)
		os.Exit(1)
	}
	go func() {
		for range time.Tick(time.Hour) {
			s.cleanup(ctx)
		}
	}()

	srv := &http.Server{Addr: config.Get("LISTEN_ADDR", ":8080"), Handler: httpx.Logging(s.routes()),
		ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	slog.Info("auth listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}

func newServer(db *pgxpool.Pool, key ed25519.PrivateKey, kid, appleClient, teamID, rpID string, origins []string) (*server, error) {
	wa, err := webauthn.New(&webauthn.Config{RPID: rpID, RPDisplayName: "Hearth", RPOrigins: origins})
	if err != nil {
		return nil, err
	}
	s := &server{
		db:      db,
		signer:  authtoken.Signer{KeyID: kid, Key: key, TTL: 15 * time.Minute},
		apple:   newAppleVerifier(appleClient),
		wa:      wa,
		teamApp: teamID + "." + appleClient,
	}
	s.verifier = authtoken.NewVerifier("")
	s.verifier.SetKeys(map[string]ed25519.PublicKey{kid: key.Public().(ed25519.PublicKey)})
	return s, nil
}

func (s *server) routes() http.Handler {
	strict := &httpx.Limiter{N: 10, Window: time.Minute} // password guessing
	normal := &httpx.Limiter{N: 60, Window: time.Minute}
	auth := func(h http.HandlerFunc) http.HandlerFunc { return httpx.RequireUser(s.verifier, h) }

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		httpx.JSON(w, 200, s.signer.JWKS())
	})
	mux.HandleFunc("GET /.well-known/apple-app-site-association", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, 200, map[string]any{"webcredentials": map[string]any{"apps": []string{s.teamApp}}})
	})

	mux.HandleFunc("POST /v1/auth/signup", strict.Wrap(s.signup))
	mux.HandleFunc("POST /v1/auth/login", strict.Wrap(s.login))
	mux.HandleFunc("POST /v1/auth/apple", normal.Wrap(s.appleLogin))
	mux.HandleFunc("POST /v1/auth/refresh", normal.Wrap(s.refresh))
	mux.HandleFunc("POST /v1/auth/logout", s.logout)
	mux.HandleFunc("POST /v1/auth/passkey/begin", normal.Wrap(s.passkeyLoginBegin))
	mux.HandleFunc("POST /v1/auth/passkey/finish", normal.Wrap(s.passkeyLoginFinish))

	mux.HandleFunc("GET /v1/me", auth(s.me))
	mux.HandleFunc("POST /v1/me/passkeys/begin", auth(s.passkeyRegisterBegin))
	mux.HandleFunc("POST /v1/me/passkeys/finish", auth(s.passkeyRegisterFinish))
	return mux
}

// --- responses ---

type userInfo struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	HasPassword bool   `json:"has_password"`
	HasApple    bool   `json:"has_apple"`
	Passkeys    int    `json:"passkeys"`
}

func infoOf(u *User) userInfo {
	return userInfo{ID: u.ID, Email: u.Email, Name: u.Name, HasPassword: u.PasswordHash != nil,
		HasApple: u.AppleSub != nil, Passkeys: len(u.Credentials)}
}

type sessionResponse struct {
	AccessToken  string    `json:"access_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	RefreshToken string    `json:"refresh_token"`
	User         userInfo  `json:"user"`
}

func (s *server) startSession(w http.ResponseWriter, r *http.Request, u *User, status int) {
	access, exp, err := s.signer.Issue(u.ID, u.Email, u.Name, nil)
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	refresh, err := s.issueRefresh(r.Context(), u.ID)
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	httpx.JSON(w, status, sessionResponse{AccessToken: access, ExpiresAt: exp, RefreshToken: refresh, User: infoOf(u)})
}

// --- email + password ---

func (s *server) signup(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email, Password, Name string }
	if !httpx.Decode(w, r, &req) {
		return
	}
	email, err := normalizeEmail(req.Email)
	if err != nil {
		httpx.Error(w, 400, err.Error())
		return
	}
	if err := validatePassword(req.Password); err != nil {
		httpx.Error(w, 400, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if len(name) > 100 {
		name = name[:100]
	}
	h := hashPassword(req.Password)
	u, err := s.createUser(r.Context(), email, name, &h, nil)
	if errors.Is(err, ErrConflict) {
		httpx.Error(w, 409, "An account with this email already exists. Sign in instead.")
		return
	}
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	slog.Info("signup", "user", u.ID)
	s.startSession(w, r, u, 201)
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email, Password string }
	if !httpx.Decode(w, r, &req) {
		return
	}
	email, _ := normalizeEmail(req.Email)
	u, err := s.userByEmail(r.Context(), email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		httpx.Internal(w, err)
		return
	}
	hash := dummyHash
	if u != nil && u.PasswordHash != nil {
		hash = *u.PasswordHash
	}
	if ok := checkPassword(hash, req.Password); !ok || u == nil || u.PasswordHash == nil {
		httpx.Error(w, 401, "Wrong email or password.")
		return
	}
	s.startSession(w, r, u, 200)
}

// --- Sign in with Apple ---

func (s *server) appleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IdentityToken string `json:"identity_token"`
		Name          string `json:"name"` // Apple only sends the name on first sign-in
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	id, err := s.apple.Verify(r.Context(), req.IdentityToken)
	if err != nil {
		slog.Warn("apple token rejected", "err", err)
		httpx.Error(w, 401, "Sign in with Apple failed. Please try again.")
		return
	}
	ctx := r.Context()
	u, err := s.userByApple(ctx, id.Sub)
	if errors.Is(err, ErrNotFound) && id.Email != "" && id.EmailVerified {
		// Same verified email as an existing account: link Apple to it.
		if u, err = s.userByEmail(ctx, strings.ToLower(id.Email)); err == nil {
			_, err = s.db.Exec(ctx, `update auth.users set apple_sub=$1 where id=$2::uuid`, id.Sub, u.ID)
			u.AppleSub = &id.Sub
		}
	}
	status := 200
	if errors.Is(err, ErrNotFound) {
		if id.Email == "" {
			httpx.Error(w, 400, "Apple didn't share an email address. Please allow email sharing and try again.")
			return
		}
		u, err = s.createUser(ctx, strings.ToLower(id.Email), strings.TrimSpace(req.Name), nil, &id.Sub)
		status = 201
	}
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	s.startSession(w, r, u, status)
}

// --- tokens ---

func (s *server) refresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	uid, err := s.useRefresh(r.Context(), req.RefreshToken)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, 401, "Please sign in again.")
		return
	}
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	u, err := s.userByID(r.Context(), uid)
	if err != nil {
		httpx.Error(w, 401, "Please sign in again.")
		return
	}
	s.startSession(w, r, u, 200)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if httpx.Decode(w, r, &req) {
		_ = s.revokeRefresh(r.Context(), req.RefreshToken)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	u, err := s.userByID(r.Context(), httpx.Claims(r).Subject)
	if err != nil {
		httpx.Error(w, 401, "Please sign in again.")
		return
	}
	httpx.JSON(w, 200, infoOf(u))
}

// --- passkeys ---

type beginResponse struct {
	SessionID string `json:"session_id"`
	Options   any    `json:"options"`
}

func (s *server) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	u, err := s.userByID(r.Context(), httpx.Claims(r).Subject)
	if err != nil {
		httpx.Error(w, 401, "Please sign in again.")
		return
	}
	exclude := make([]protocol.CredentialDescriptor, 0, len(u.Credentials))
	for _, c := range u.Credentials {
		exclude = append(exclude, c.Descriptor())
	}
	opts, sd, err := s.wa.BeginRegistration(waUser{u},
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(exclude))
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	sid, err := s.saveSession(r.Context(), &u.ID, sd)
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	httpx.JSON(w, 200, beginResponse{SessionID: sid, Options: opts})
}

func (s *server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sd, uid, err := s.takeSession(ctx, r.URL.Query().Get("session"))
	if err != nil || uid == nil || *uid != httpx.Claims(r).Subject {
		httpx.Error(w, 400, "That took too long. Please try again.")
		return
	}
	u, err := s.userByID(ctx, *uid)
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	cred, err := s.wa.FinishRegistration(waUser{u}, *sd, r)
	if err != nil {
		slog.Warn("passkey registration rejected", "err", err)
		httpx.Error(w, 400, "Couldn't save the passkey. Please try again.")
		return
	}
	raw, _ := json.Marshal(cred)
	if _, err := s.db.Exec(ctx, `insert into auth.passkeys (id, user_id, credential) values ($1, $2::uuid, $3)`, cred.ID, u.ID, raw); err != nil {
		httpx.Internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	opts, sd, err := s.wa.BeginDiscoverableLogin()
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	sid, err := s.saveSession(r.Context(), nil, sd)
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	httpx.JSON(w, 200, beginResponse{SessionID: sid, Options: opts})
}

func (s *server) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sd, _, err := s.takeSession(ctx, r.URL.Query().Get("session"))
	if err != nil {
		httpx.Error(w, 400, "That took too long. Please try again.")
		return
	}
	var found *User
	_, cred, err := s.wa.FinishPasskeyLogin(func(_, handle []byte) (webauthn.User, error) {
		u, err := s.userByHandle(ctx, handle)
		if err != nil {
			return nil, err
		}
		found = u
		return waUser{u}, nil
	}, *sd, r)
	if err != nil || found == nil {
		slog.Warn("passkey login rejected", "err", err)
		httpx.Error(w, 401, "That passkey wasn't recognised.")
		return
	}
	raw, _ := json.Marshal(cred) // keep the sign counter current
	_, _ = s.db.Exec(ctx, `update auth.passkeys set credential=$1, last_used_at=now() where id=$2`, raw, cred.ID)
	s.startSession(w, r, found, 200)
}
