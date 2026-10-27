package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"hearth/internal/model"
)

// Automations: "when this happens, if that holds, do these things". They run on the hub,
// inside the gateway, which already sees every device change and can send every command,
// so they keep working with every phone switched off. They are kept in DATA_DIR/automations.json.

// Trigger starts an automation. Any one trigger is enough.
type Trigger struct {
	// "time": At ("07:30", hub local time) on Days (0 Sunday … 6 Saturday; empty = every day).
	// "device": DeviceID's state Key compared with Op and Value turns true; Op "changes" fires
	// on any change of Key.
	// "sun": Event "sunrise" or "sunset", OffsetMinutes before (−) or after (+), on Days.
	Type          string `json:"type"`
	At            string `json:"at,omitempty"`
	Days          []int  `json:"days,omitempty"`
	Event         string `json:"event,omitempty"`
	OffsetMinutes int    `json:"offset_minutes,omitempty"`
	DeviceID      string `json:"device_id,omitempty"`
	Key           string `json:"key,omitempty"`
	Op            string `json:"op,omitempty"` // eq, ne, gt, lt, changes
	Value         any    `json:"value,omitempty"`
}

// Condition must hold when a trigger fires. Every condition must hold.
type Condition struct {
	// "time": between After and Before ("22:00"–"06:30" wraps midnight).
	// "device": DeviceID's Key compared with Op and Value.
	Type     string `json:"type"`
	After    string `json:"after,omitempty"`
	Before   string `json:"before,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
	Key      string `json:"key,omitempty"`
	Op       string `json:"op,omitempty"`
	Value    any    `json:"value,omitempty"`
}

// Step is one thing an automation does: a device command, after an optional delay.
type Step struct {
	DeviceID     string         `json:"device_id"`
	Action       string         `json:"action"`
	Params       map[string]any `json:"params,omitempty"`
	DelaySeconds int            `json:"delay_seconds,omitempty"`
}

type Automation struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Icon       string      `json:"icon,omitempty"`
	Enabled    bool        `json:"enabled"`
	Scene      bool        `json:"scene,omitempty"`
	Triggers   []Trigger   `json:"triggers"`
	Conditions []Condition `json:"conditions,omitempty"`
	Steps      []Step      `json:"steps"`
	LastRun    *time.Time  `json:"last_run,omitempty"`
	LastError  string      `json:"last_error,omitempty"`
}

type automations struct {
	path  string
	mu    sync.Mutex
	list  []Automation
	send  func(model.Command) error
	state func(id string) (model.Device, bool)
	now   func() time.Time
	// The minute each time trigger last fired, so a minute is never run twice.
	fired map[string]string
	// Home location for sunrise/sunset (HOME_LAT/HOME_LON).
	lat, lon float64
	hasLoc   bool
	// A run in progress per automation. A new trigger restarts it, so
	// "motion → light on, off after 5 minutes" keeps the light on while people move.
	running map[string]*runHandle
	// notify tells the phones what happened (automation ran / failed).
	notify func(name string, data any)
}

type runHandle struct{ cancel context.CancelFunc }

func loadAutomations(dir string, send func(model.Command) error, state func(string) (model.Device, bool)) *automations {
	a := &automations{path: filepath.Join(dir, "automations.json"), send: send, state: state, now: time.Now,
		fired: map[string]string{}, running: map[string]*runHandle{}, notify: func(string, any) {}}
	if b, err := os.ReadFile(a.path); err == nil {
		_ = json.Unmarshal(b, &a.list)
	}
	return a
}

func (a *automations) save() error {
	b, err := json.MarshalIndent(a.list, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// validate says, in words for the app, what is wrong with an automation.
func validate(x Automation) error {
	if strings.TrimSpace(x.Name) == "" {
		return fmt.Errorf("Give the automation a name.")
	}
	if len(x.Triggers) == 0 && !x.Scene {
		return fmt.Errorf("Choose when it should run.")
	}
	if len(x.Steps) == 0 {
		return fmt.Errorf("Add at least one thing for it to do.")
	}
	for _, t := range x.Triggers {
		switch t.Type {
		case "time":
			if _, err := time.Parse("15:04", t.At); err != nil {
				return fmt.Errorf("The time %q isn't valid.", t.At)
			}
		case "device":
			if t.DeviceID == "" || t.Key == "" {
				return fmt.Errorf("Pick the device and what to watch.")
			}
		case "sun":
			if t.Event != "sunrise" && t.Event != "sunset" {
				return fmt.Errorf("Choose sunrise or sunset.")
			}
			if t.OffsetMinutes < -240 || t.OffsetMinutes > 240 {
				return fmt.Errorf("Sunrise/sunset offsets can be up to 4 hours.")
			}
		default:
			return fmt.Errorf("Unknown trigger %q.", t.Type)
		}
	}
	for _, s := range x.Steps {
		if s.DeviceID == "" || s.Action == "" {
			return fmt.Errorf("Every step needs a device and an action.")
		}
		if s.DelaySeconds < 0 || s.DelaySeconds > 24*3600 {
			return fmt.Errorf("Delays can be up to a day.")
		}
	}
	return nil
}

// deviceChanged runs the device triggers that just became true.
func (a *automations) deviceChanged(prev *model.Device, cur model.Device) {
	a.mu.Lock()
	var due []Automation
	for _, x := range a.list {
		if !x.Enabled {
			continue
		}
		for _, t := range x.Triggers {
			if t.Type != "device" || t.DeviceID != cur.ID {
				continue
			}
			was := prev != nil && compare(prev.State[t.Key], t.Op, t.Value, prev.State[t.Key])
			if t.Op == "changes" {
				if prev != nil && fmt.Sprint(prev.State[t.Key]) != fmt.Sprint(cur.State[t.Key]) {
					due = append(due, x)
				}
				continue
			}
			// On the change into true only, so a temperature above 25° fires once, not on
			// every reading while it stays above.
			if prev != nil && !was && compare(cur.State[t.Key], t.Op, t.Value, nil) {
				due = append(due, x)
			}
		}
	}
	a.mu.Unlock()
	for _, x := range due {
		a.runIfAllowed(x, "device")
	}
}

// tick runs the time triggers due this minute; called every few seconds.
func (a *automations) tick() {
	now := a.now()
	minute := now.Format("2006-01-02 15:04")
	a.mu.Lock()
	var due []Automation
	for _, x := range a.list {
		if !x.Enabled {
			continue
		}
		for i, t := range x.Triggers {
			switch t.Type {
			case "time":
				if t.At != now.Format("15:04") {
					continue
				}
			case "sun":
				at, ok := a.sunAt(now, t)
				if !ok || at.Format("15:04") != now.Format("15:04") {
					continue
				}
			default:
				continue
			}
			if len(t.Days) > 0 && !containsInt(t.Days, int(now.Weekday())) {
				continue
			}
			key := fmt.Sprintf("%s#%d", x.ID, i)
			if a.fired[key] == minute {
				continue
			}
			a.fired[key] = minute
			due = append(due, x)
		}
	}
	a.mu.Unlock()
	for _, x := range due {
		a.runIfAllowed(x, "time")
	}
}

func (a *automations) runIfAllowed(x Automation, why string) {
	for _, c := range x.Conditions {
		if !a.holds(c) {
			slog.Info("automation skipped", "name", x.Name, "trigger", why)
			return
		}
	}
	a.run(x)
}

func (a *automations) holds(c Condition) bool {
	switch c.Type {
	case "time":
		return inWindow(a.now(), c.After, c.Before)
	case "device":
		d, ok := a.state(c.DeviceID)
		return ok && compare(d.State[c.Key], c.Op, c.Value, nil)
	}
	return true
}

// sunAt is today's sunrise/sunset (plus offset) in hub local time.
func (a *automations) sunAt(now time.Time, t Trigger) (time.Time, bool) {
	if !a.hasLoc {
		return time.Time{}, false
	}
	rise, set, ok := sunTimes(now, a.lat, a.lon)
	if !ok {
		return time.Time{}, false
	}
	at := rise
	if t.Event == "sunset" {
		at = set
	}
	return at.Add(time.Duration(t.OffsetMinutes) * time.Minute), true
}

// stop cancels a run in progress (on delete, disable or a fresh trigger).
func (a *automations) stopLocked(id string) {
	if h, ok := a.running[id]; ok {
		h.cancel()
		delete(a.running, id)
	}
}

// run sends the steps in order; a step with a delay waits without holding
// anything up. Triggering again while it waits restarts it.
func (a *automations) run(x Automation) {
	slog.Info("automation run", "name", x.Name)
	ctx, cancel := context.WithCancel(context.Background())
	h := &runHandle{cancel: cancel}
	a.mu.Lock()
	a.stopLocked(x.ID)
	a.running[x.ID] = h
	a.mu.Unlock()
	go func() {
		defer cancel()
		var failure string
		for _, s := range x.Steps {
			if s.DelaySeconds > 0 {
				select {
				case <-ctx.Done():
					return // restarted, disabled or deleted
				case <-time.After(time.Duration(s.DelaySeconds) * time.Second):
				}
			}
			if ctx.Err() != nil {
				return
			}
			if err := a.send(model.Command{DeviceID: s.DeviceID, Action: s.Action, Params: s.Params}); err != nil {
				failure = err.Error()
				slog.Error("automation step", "name", x.Name, "device", s.DeviceID, "err", err)
			}
		}
		a.mu.Lock()
		if a.running[x.ID] == h {
			delete(a.running, x.ID)
		}
		for i := range a.list {
			if a.list[i].ID == x.ID {
				now := a.now()
				a.list[i].LastRun = &now
				a.list[i].LastError = failure
			}
		}
		_ = a.save()
		a.mu.Unlock()
		a.notify("automation_ran", map[string]any{"id": x.ID, "name": x.Name, "error": failure})
	}()
}

func (a *automations) routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	// Today's sunrise and sunset, so the app can show "Sunset (17:42)".
	mux.HandleFunc("GET /api/sun", auth(func(w http.ResponseWriter, _ *http.Request) {
		if !a.hasLoc {
			writeJSON(w, http.StatusOK, map[string]any{"available": false})
			return
		}
		rise, set, ok := sunTimes(a.now(), a.lat, a.lon)
		writeJSON(w, http.StatusOK, map[string]any{"available": ok, "sunrise": rise.Format("15:04"), "sunset": set.Format("15:04")})
	}))
	mux.HandleFunc("GET /api/automations", auth(func(w http.ResponseWriter, _ *http.Request) {
		a.mu.Lock()
		list := append([]Automation{}, a.list...)
		a.mu.Unlock()
		sort.SliceStable(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
		writeJSON(w, http.StatusOK, list)
	}))
	// PUT creates or replaces; an empty id makes a new one.
	mux.HandleFunc("PUT /api/automations", auth(func(w http.ResponseWriter, r *http.Request) {
		var x Automation
		if !decode(w, r, &x) {
			return
		}
		if err := validate(x); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		a.mu.Lock()
		if x.ID == "" {
			x.ID = newID()
			a.list = append(a.list, x)
		} else {
			found := false
			for i := range a.list {
				if a.list[i].ID == x.ID {
					x.LastRun, x.LastError = a.list[i].LastRun, a.list[i].LastError
					a.list[i] = x
					found = true
					if !x.Enabled {
						a.stopLocked(x.ID)
					}
				}
			}
			if !found {
				a.list = append(a.list, x)
			}
		}
		err := a.save()
		a.mu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Couldn't save the automation."})
			return
		}
		writeJSON(w, http.StatusOK, x)
	}))
	mux.HandleFunc("DELETE /api/automations/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		a.mu.Lock()
		kept := a.list[:0]
		for _, x := range a.list {
			if x.ID != id {
				kept = append(kept, x)
			}
		}
		a.list = kept
		a.stopLocked(id)
		err := a.save()
		a.mu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Couldn't delete the automation."})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/automations/{id}/run", auth(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		a.mu.Lock()
		var x *Automation
		for i := range a.list {
			if a.list[i].ID == id {
				c := a.list[i]
				x = &c
			}
		}
		a.mu.Unlock()
		if x == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown automation"})
			return
		}
		a.run(*x) // "Run now" skips the conditions: the person asked for it.
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "running"})
	}))
}

// compare reads a device value against an expected one: numbers as numbers, the rest as text.
func compare(have any, op string, want any, _ any) bool {
	hn, hok := toFloat(have)
	wn, wok := toFloat(want)
	switch op {
	case "gt":
		return hok && wok && hn > wn
	case "lt":
		return hok && wok && hn < wn
	case "ne":
		if hok && wok {
			return hn != wn
		}
		return fmt.Sprint(have) != fmt.Sprint(want)
	default: // eq
		if hok && wok {
			return hn == wn
		}
		return strings.EqualFold(fmt.Sprint(have), fmt.Sprint(want))
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func inWindow(now time.Time, after, before string) bool {
	a, err1 := time.Parse("15:04", after)
	b, err2 := time.Parse("15:04", before)
	if err1 != nil || err2 != nil {
		return true
	}
	m := now.Hour()*60 + now.Minute()
	am, bm := a.Hour()*60+a.Minute(), b.Hour()*60+b.Minute()
	if am <= bm {
		return m >= am && m < bm
	}
	return m >= am || m < bm // across midnight
}

func containsInt(l []int, v int) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
