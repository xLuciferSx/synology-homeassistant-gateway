// Command hubs is the hub microservice: home hubs register themselves here
// (signed with their own key), owners claim a new hub with the pairing code it
// shows, and invite family members with invite codes. Hubs fetch their member
// list on every announce, so membership changes reach them within minutes.
// Data lives in the "hubs" schema of PostgreSQL.
package main

import (
	"context"
	"crypto/ed25519"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"hearth/internal/authtoken"
	"hearth/internal/config"
	"hearth/internal/httpx"
	"hearth/internal/hubproto"
	"hearth/internal/pg"
)

//go:embed migrations/*.sql
var migrations embed.FS

const (
	pairCodeTTL   = 30 * time.Minute
	inviteCodeTTL = 48 * time.Hour
	onlineWindow  = 12 * time.Minute // hubs announce every 5 minutes
)

type server struct {
	db       *pgxpool.Pool
	verifier *authtoken.Verifier
	now      func() time.Time
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
	if err := pg.Migrate(ctx, db, "hubs", sub); err != nil {
		slog.Error("migrate", "err", err)
		os.Exit(1)
	}
	s := &server{db: db, verifier: authtoken.NewVerifier(config.Must("AUTH_JWKS_URL")), now: time.Now}
	go func() {
		for range time.Tick(time.Hour) {
			_, _ = db.Exec(ctx, `delete from hubs.codes where expires_at < now()`)
		}
	}()

	srv := &http.Server{Addr: config.Get("LISTEN_ADDR", ":8080"), Handler: httpx.Logging(s.routes()), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	slog.Info("hubs listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}

func (s *server) routes() http.Handler {
	claimLimit := &httpx.Limiter{N: 10, Window: time.Minute} // code guessing
	hubLimit := &httpx.Limiter{N: 30, Window: time.Minute}
	auth := func(h http.HandlerFunc) http.HandlerFunc { return httpx.RequireUser(s.verifier, h) }

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /v1/hubs/announce", hubLimit.Wrap(s.announce))
	mux.HandleFunc("GET /v1/hubs", auth(s.list))
	mux.HandleFunc("POST /v1/hubs/claim", claimLimit.Wrap(auth(s.claim)))
	mux.HandleFunc("PATCH /v1/hubs/{id}", auth(s.rename))
	mux.HandleFunc("GET /v1/hubs/{id}/members", auth(s.members))
	mux.HandleFunc("POST /v1/hubs/{id}/invites", auth(s.invite))
	mux.HandleFunc("DELETE /v1/hubs/{id}/members/{user}", auth(s.removeMember))
	return mux
}

// --- hub side ---

func (s *server) announce(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		httpx.Error(w, 400, "invalid request")
		return
	}
	var a hubproto.Announce
	if json.Unmarshal(body, &a) != nil {
		httpx.Error(w, 400, "invalid request")
		return
	}
	pub, err := base64.StdEncoding.DecodeString(a.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize || hubproto.HubID(pub) != a.HubID {
		httpx.Error(w, 400, "hub id does not match its key")
		return
	}
	if !hubproto.Verify(pub, body, r.Header.Get(hubproto.SignatureHeader)) {
		httpx.Error(w, 401, "bad signature")
		return
	}
	if d := s.now().Unix() - a.Timestamp; d > 300 || d < -300 {
		httpx.Error(w, 401, "clock skew too large")
		return
	}
	name := strings.TrimSpace(a.Name)
	if name == "" || len(name) > 60 {
		name = "Home"
	}
	ctx := r.Context()
	// Name is set on first registration; afterwards the owner renames from the app.
	_, err = s.db.Exec(ctx, `insert into hubs.hubs (id, name, public_key, lan_url, public_url, version, last_seen)
		values ($1,$2,$3,$4,$5,$6,now())
		on conflict (id) do update set lan_url=$4, public_url=$5, version=$6, last_seen=now()`,
		a.HubID, name, pub, nullIfEmpty(a.LANURL), nullIfEmpty(a.PublicURL), nullIfEmpty(a.Version))
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	members, err := s.memberList(ctx, a.HubID)
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	owned := false
	for _, m := range members {
		owned = owned || m.Role == "owner"
	}
	if code := hubproto.NormalizeCode(a.PairCode); !owned && len(code) == 8 {
		// One live pairing code per unclaimed hub.
		_, err = s.db.Exec(ctx, `with del as (delete from hubs.codes where hub_id=$2 and role='owner' and code<>$1)
			insert into hubs.codes (code, hub_id, role, expires_at) values ($1,$2,'owner',$3)
			on conflict (code) do update set expires_at=$3 where hubs.codes.hub_id=$2`,
			code, a.HubID, s.now().Add(pairCodeTTL))
		if err != nil {
			httpx.Internal(w, err)
			return
		}
	}
	httpx.JSON(w, 200, hubproto.AnnounceResponse{Owned: owned, Members: members})
}

func (s *server) memberList(ctx context.Context, hubID string) ([]hubproto.Member, error) {
	rows, err := s.db.Query(ctx, `select user_id::text, email, name, role from hubs.members where hub_id=$1 order by role desc, added_at`, hubID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []hubproto.Member{}
	for rows.Next() {
		var m hubproto.Member
		if err := rows.Scan(&m.UserID, &m.Email, &m.Name, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// --- app side ---

type hubView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	LANURL    *string   `json:"lan_url"`
	PublicURL *string   `json:"public_url"`
	Online    bool      `json:"online"`
	LastSeen  time.Time `json:"last_seen"`
	Members   int       `json:"members"`
}

const hubViewSQL = `select h.id, h.name, m.role, h.lan_url, h.public_url, coalesce(h.last_seen, h.created_at),
	(select count(*) from hubs.members x where x.hub_id=h.id)
	from hubs.hubs h join hubs.members m on m.hub_id=h.id and m.user_id=$1::uuid`

func (s *server) scanHub(row pgx.Row) (*hubView, error) {
	var h hubView
	if err := row.Scan(&h.ID, &h.Name, &h.Role, &h.LANURL, &h.PublicURL, &h.LastSeen, &h.Members); err != nil {
		return nil, err
	}
	h.Online = s.now().Sub(h.LastSeen) < onlineWindow
	return &h, nil
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), hubViewSQL+` order by h.name`, httpx.Claims(r).Subject)
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	defer rows.Close()
	out := []*hubView{}
	for rows.Next() {
		h, err := s.scanHub(rows)
		if err != nil {
			httpx.Internal(w, err)
			return
		}
		out = append(out, h)
	}
	httpx.JSON(w, 200, map[string]any{"hubs": out})
}

func (s *server) hubFor(ctx context.Context, userID, hubID string) (*hubView, error) {
	h, err := s.scanHub(s.db.QueryRow(ctx, hubViewSQL+` where h.id=$2`, userID, hubID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotMember
	}
	return h, err
}

var errNotMember = errors.New("not a member")

func (s *server) claim(w http.ResponseWriter, r *http.Request) {
	var req struct{ Code string }
	if !httpx.Decode(w, r, &req) {
		return
	}
	c := httpx.Claims(r)
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var hubID, role string
	err = tx.QueryRow(ctx, `delete from hubs.codes where code=$1 and expires_at > now() returning hub_id, role`,
		hubproto.NormalizeCode(req.Code)).Scan(&hubID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, 404, "That code isn't valid or has expired. Check the code shown by your hub.")
		return
	}
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	_, err = tx.Exec(ctx, `insert into hubs.members (hub_id, user_id, email, name, role) values ($1,$2::uuid,$3,$4,$5)
		on conflict (hub_id, user_id) do update set email=$3, name=$4`, hubID, c.Subject, c.Email, c.Name, role)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		httpx.Error(w, 409, "This hub already has an owner. Ask them for an invite code.")
		return
	}
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		httpx.Internal(w, err)
		return
	}
	slog.Info("hub claimed", "hub", hubID, "user", c.Subject, "role", role)
	h, err := s.hubFor(ctx, c.Subject, hubID)
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	httpx.JSON(w, 201, h)
}

func (s *server) requireRole(w http.ResponseWriter, r *http.Request, owner bool) (*hubView, bool) {
	h, err := s.hubFor(r.Context(), httpx.Claims(r).Subject, r.PathValue("id"))
	if errors.Is(err, errNotMember) {
		httpx.Error(w, 404, "Hub not found.")
		return nil, false
	}
	if err != nil {
		httpx.Internal(w, err)
		return nil, false
	}
	if owner && h.Role != "owner" {
		httpx.Error(w, 403, "Only the hub's owner can do that.")
		return nil, false
	}
	return h, true
}

func (s *server) rename(w http.ResponseWriter, r *http.Request) {
	h, ok := s.requireRole(w, r, true)
	if !ok {
		return
	}
	var req struct{ Name string }
	if !httpx.Decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 60 {
		httpx.Error(w, 400, "Choose a name up to 60 characters.")
		return
	}
	if _, err := s.db.Exec(r.Context(), `update hubs.hubs set name=$1 where id=$2`, name, h.ID); err != nil {
		httpx.Internal(w, err)
		return
	}
	h.Name = name
	httpx.JSON(w, 200, h)
}

func (s *server) members(w http.ResponseWriter, r *http.Request) {
	h, ok := s.requireRole(w, r, false)
	if !ok {
		return
	}
	m, err := s.memberList(r.Context(), h.ID)
	if err != nil {
		httpx.Internal(w, err)
		return
	}
	httpx.JSON(w, 200, map[string]any{"members": m})
}

func (s *server) invite(w http.ResponseWriter, r *http.Request) {
	h, ok := s.requireRole(w, r, true)
	if !ok {
		return
	}
	code, exp := hubproto.NewCode(), s.now().Add(inviteCodeTTL)
	if _, err := s.db.Exec(r.Context(), `insert into hubs.codes (code, hub_id, role, created_by, expires_at) values ($1,$2,'member',$3::uuid,$4)`,
		code, h.ID, httpx.Claims(r).Subject, exp); err != nil {
		httpx.Internal(w, err)
		return
	}
	httpx.JSON(w, 201, map[string]any{"code": hubproto.FormatCode(code), "expires_at": exp})
}

func (s *server) removeMember(w http.ResponseWriter, r *http.Request) {
	me := httpx.Claims(r).Subject
	target := r.PathValue("user")
	h, ok := s.requireRole(w, r, target != me) // members may remove themselves (leave)
	if !ok {
		return
	}
	if target == me && h.Role == "owner" {
		httpx.Error(w, 400, "The owner can't leave their own hub.")
		return
	}
	if _, err := s.db.Exec(r.Context(), `delete from hubs.members where hub_id=$1 and user_id=$2::uuid and role<>'owner'`, h.ID, target); err != nil {
		httpx.Internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
