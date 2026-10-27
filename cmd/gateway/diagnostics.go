package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// diagnostics keeps crash and hang reports the app sends after it restarts, so
// problems on a phone can be read on the hub without anyone exporting crash logs.

const maxDiagnostics = 200

type diagnostics struct{ dir string }

type diagnosticReport struct {
	Kind    string          `json:"kind"`
	Build   string          `json:"build"`
	OS      string          `json:"os"`
	Device  string          `json:"device"`
	User    string          `json:"user,omitempty"`
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload"`
}

func loadDiagnostics(dataDir string) *diagnostics {
	d := &diagnostics{dir: filepath.Join(dataDir, "diagnostics")}
	_ = os.MkdirAll(d.dir, 0o700)
	return d
}

func (d *diagnostics) save(rep diagnosticReport) error {
	b, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s-%s.json", time.Now().UTC().Format("20060102T150405.000000000"), safeName(rep.Kind), safeName(rep.Build))
	if err := os.WriteFile(filepath.Join(d.dir, name), b, 0o600); err != nil {
		return err
	}
	d.prune()
	return nil
}

func (d *diagnostics) prune() {
	files, _ := filepath.Glob(filepath.Join(d.dir, "*.json"))
	if len(files) <= maxDiagnostics {
		return
	}
	sort.Strings(files)
	for _, f := range files[:len(files)-maxDiagnostics] {
		_ = os.Remove(f)
	}
}

func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, s)
	if s == "" {
		return "x"
	}
	return s
}

// routes: POST /api/diagnostics stores one report (up to 2 MB).
func (d *diagnostics) routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /api/diagnostics", auth(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		var rep diagnosticReport
		if err != nil || json.Unmarshal(body, &rep) != nil || rep.Kind == "" || len(rep.Payload) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "That isn't a diagnostic report."})
			return
		}
		rep.User = userFrom(r)
		if rep.At.IsZero() {
			rep.At = time.Now()
		}
		if err := d.save(rep); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
