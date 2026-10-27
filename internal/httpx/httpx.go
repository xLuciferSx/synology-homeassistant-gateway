// Package httpx holds the small HTTP helpers shared by the cloud services.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"hearth/internal/authtoken"
)

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// Error writes {"error": msg}. Messages are safe to show in the app.
func Error(w http.ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]string{"error": msg})
}

// Internal logs the real error and returns a generic message.
func Internal(w http.ResponseWriter, err error) {
	slog.Error("internal error", "err", err)
	Error(w, http.StatusInternalServerError, "Something went wrong. Please try again.")
}

// Decode reads a JSON body (max 64 KiB) into v.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		Error(w, http.StatusBadRequest, "invalid request")
		return false
	}
	return true
}

func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

type ctxKey struct{}

// RequireUser verifies the access token and stores its claims in the context.
func RequireUser(v *authtoken.Verifier, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := v.Verify(r.Context(), BearerToken(r))
		if err != nil {
			Error(w, http.StatusUnauthorized, "Please sign in again.")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c)))
	}
}

func Claims(r *http.Request) *authtoken.Claims {
	c, _ := r.Context().Value(ctxKey{}).(*authtoken.Claims)
	return c
}

// ClientIP prefers the proxy header set by Azure Container Apps' ingress.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// Limiter allows n requests per window per key (e.g. client IP).
type Limiter struct {
	N      int
	Window time.Duration

	mu   sync.Mutex
	hits map[string][]time.Time
}

var ErrRateLimited = errors.New("too many attempts")

func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	now := time.Now()
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.Window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.N {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	if len(l.hits) > 10000 { // bound memory
		l.hits = map[string][]time.Time{}
	}
	return true
}

func (l *Limiter) Wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(ClientIP(r)) {
			Error(w, http.StatusTooManyRequests, "Too many attempts. Please wait a minute and try again.")
			return
		}
		next(w, r)
	}
}

// Logging logs each request's method, path, status and duration.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path != "/healthz" {
			slog.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
