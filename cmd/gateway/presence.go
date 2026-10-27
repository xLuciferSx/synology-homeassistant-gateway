package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"hearth/internal/model"
)

// presence is where people's own phones say they are. It wins over the provider's
// tracker (Home Assistant often lags or loses a phone) for the people it knows about.
type presence struct {
	mu     sync.Mutex
	path   string
	people map[string]presenceReport
	awayOn func() bool // the house's away mode, shown on the People device
}

type presenceReport struct {
	Home bool      `json:"home"`
	At   time.Time `json:"at"`
}

func loadPresence(dir string) *presence {
	p := &presence{path: filepath.Join(dir, "presence.json"), people: map[string]presenceReport{}}
	if b, err := os.ReadFile(p.path); err == nil {
		_ = json.Unmarshal(b, &p.people)
	}
	return p
}

func (p *presence) report(name string, home bool, at time.Time) bool {
	key := strings.ToLower(firstName(name))
	if key == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	prev, had := p.people[key]
	p.people[key] = presenceReport{Home: home, At: at}
	if b, err := json.Marshal(p.people); err == nil {
		_ = os.WriteFile(p.path, b, 0o600)
	}
	return !had || prev.Home != home
}

func firstName(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// apply rewrites a People device's home/away lists with the phones' reports.
func (p *presence) apply(d model.Device) model.Device {
	if d.Kind != model.KindPresence {
		return d
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.people) == 0 {
		return p.withAway(d)
	}
	names := func(v any) []string {
		out := []string{}
		switch l := v.(type) {
		case []string:
			out = append(out, l...)
		case []any:
			for _, x := range l {
				if s, ok := x.(string); ok {
					out = append(out, s)
				}
			}
		}
		return out
	}
	home, away := names(d.State["home"]), names(d.State["away"])
	all := append(append([]string{}, home...), away...)
	home, away = []string{}, []string{}
	seen := map[string]bool{}
	for _, n := range all {
		key := strings.ToLower(firstName(n))
		seen[key] = true
		at := false
		if r, ok := p.people[key]; ok {
			at = r.Home
		} else {
			at = contains(names(d.State["home"]), n)
		}
		if at {
			home = append(home, n)
		} else {
			away = append(away, n)
		}
	}
	for key, r := range p.people {
		if seen[key] {
			continue
		}
		n := strings.ToUpper(key[:1]) + key[1:]
		if r.Home {
			home = append(home, n)
		} else {
			away = append(away, n)
		}
	}
	st := map[string]any{}
	for k, v := range d.State {
		st[k] = v
	}
	st["home"], st["away"] = home, away
	st["anyone_home"] = len(home) > 0
	st["everyone_home"] = len(away) == 0 && len(home) > 0
	st["count_home"] = float64(len(home))
	d.State = st
	return p.withAway(d)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func (p *presence) withAway(d model.Device) model.Device {
	if p.awayOn == nil {
		return d
	}
	st := map[string]any{}
	for k, v := range d.State {
		st[k] = v
	}
	st["away_mode"] = p.awayOn()
	d.State = st
	return d
}
