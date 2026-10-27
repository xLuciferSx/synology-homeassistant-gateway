package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"hearth/internal/model"
)

// habits remembers when people switch things on and off by hand (app, wall
// switch, voice) and spots the ones that happen at about the same time on most
// days, so the app can offer to automate them.

const (
	habitKeep     = 28 * 24 * time.Hour
	habitWindow   = 40 // minutes a routine may wander
	habitMinDays  = 4  // days it must have happened on
	habitAutoSkip = 90 * time.Second
)

var habitKinds = map[model.Kind]bool{model.KindLight: true, model.KindSwitch: true, model.KindFan: true, model.KindAirPurifier: true}

type habitEvent struct {
	DeviceID string    `json:"d"`
	On       bool      `json:"on"` // for an EV charger: the car was plugged in
	At       time.Time `json:"t"`
	EV       bool      `json:"ev,omitempty"`
}

type habits struct {
	mu     sync.Mutex
	path   string
	events []habitEvent
	auto   map[string]time.Time // device → when an automation last commanded it
	dirty  bool
	now    func() time.Time
}

type Suggestion struct {
	ID         string     `json:"id"`
	DeviceID   string     `json:"device_id"`
	Name       string     `json:"name"`
	On         bool       `json:"on"`
	At         string     `json:"at"`
	Days       []int      `json:"days,omitempty"`
	Seen       int        `json:"seen"`
	Automation Automation `json:"automation"`
	// Set for suggestions that aren't a simple on/off at a time; the app shows them as they are.
	Title  string `json:"title,omitempty"`
	Detail string `json:"detail,omitempty"`
	Symbol string `json:"symbol,omitempty"`
}

func loadHabits(dir string) *habits {
	h := &habits{path: filepath.Join(dir, "habits.json"), auto: map[string]time.Time{}, now: time.Now}
	if b, err := os.ReadFile(h.path); err == nil {
		_ = json.Unmarshal(b, &h.events)
	}
	go func() {
		for range time.Tick(5 * time.Minute) {
			h.flush()
		}
	}()
	return h
}

func (h *habits) flush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.dirty {
		return
	}
	cut := h.now().Add(-habitKeep)
	i := sort.Search(len(h.events), func(i int) bool { return h.events[i].At.After(cut) })
	h.events = h.events[i:]
	if b, err := json.Marshal(h.events); err == nil && os.WriteFile(h.path, b, 0o600) == nil {
		h.dirty = false
	}
}

func (h *habits) automated(deviceID string) {
	h.mu.Lock()
	h.auto[deviceID] = h.now()
	h.mu.Unlock()
}

func (h *habits) deviceChanged(prev *model.Device, cur model.Device) {
	if prev != nil && cur.Kind == model.KindEVCharger {
		h.carPlugChanged(*prev, cur)
		return
	}
	if prev == nil || !habitKinds[cur.Kind] || !cur.Online || !prev.Online {
		return
	}
	was, ok1 := prev.State["on"].(bool)
	is, ok2 := cur.State["on"].(bool)
	if !ok1 || !ok2 || was == is {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if t, ok := h.auto[cur.ID]; ok && now.Sub(t) < habitAutoSkip {
		return
	}
	h.events = append(h.events, habitEvent{DeviceID: cur.ID, On: is, At: now})
	h.dirty = true
}

// suggest finds routines: the same device switched the same way within a
// 40-minute window on at least four separate days of the last four weeks.
func (h *habits) suggest(devices []model.Device, existing []Automation) []Suggestion {
	h.mu.Lock()
	events := append([]habitEvent(nil), h.events...)
	h.mu.Unlock()

	byID := map[string]model.Device{}
	for _, d := range devices {
		byID[d.ID] = d
	}
	type key struct {
		id string
		on bool
	}
	groups := map[key][]time.Time{}
	for _, e := range events {
		if e.EV {
			continue
		}
		groups[key{e.DeviceID, e.On}] = append(groups[key{e.DeviceID, e.On}], e.At.In(time.Local))
	}

	out := []Suggestion{}
	for k, times := range groups {
		d, ok := byID[k.id]
		if !ok || d.Hidden {
			continue
		}
		minute, days := bestWindow(times)
		if len(days) < habitMinDays {
			continue
		}
		at := fmt.Sprintf("%02d:%02d", minute/60, minute%60)
		if covered(existing, k.id, k.on, minute) {
			continue
		}
		var weekdays []int
		weekend := false
		for day := range days {
			wd := int(day.Weekday())
			if wd == 0 || wd == 6 {
				weekend = true
			}
		}
		if !weekend && len(days) >= habitMinDays {
			weekdays = []int{1, 2, 3, 4, 5}
		}
		action, verb := model.ActTurnOff, "off"
		if k.on {
			action, verb = model.ActTurnOn, "on"
		}
		name := fmt.Sprintf("%s %s at %s", d.Name, verb, at)
		out = append(out, Suggestion{
			ID: fmt.Sprintf("habit-%s-%s-%s", d.ID, verb, at), DeviceID: d.ID, Name: d.Name, On: k.on, At: at, Days: weekdays, Seen: len(days),
			Automation: Automation{Name: name, Icon: map[bool]string{true: "lightbulb.fill", false: "lightbulb.slash"}[k.on], Enabled: true,
				Triggers: []Trigger{{Type: "time", At: at, Days: weekdays}},
				Steps:    []Step{{DeviceID: d.ID, Action: action}}},
		})
	}
	out = append(out, h.smartSuggestions(devices, existing)...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seen != out[j].Seen {
			return out[i].Seen > out[j].Seen
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

// bestWindow returns the typical minute of day (rounded to 5) for the window
// that covers the most separate days, and those days.
func bestWindow(times []time.Time) (int, map[time.Time]bool) {
	type point struct {
		minute int
		day    time.Time
	}
	pts := make([]point, 0, len(times))
	for _, t := range times {
		pts = append(pts, point{t.Hour()*60 + t.Minute(), time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].minute < pts[j].minute })
	var bestDays map[time.Time]bool
	bestMinute := 0
	for i := range pts {
		days := map[time.Time]bool{}
		var minutes []int
		for j := i; j < len(pts) && pts[j].minute-pts[i].minute <= habitWindow; j++ {
			if !days[pts[j].day] {
				days[pts[j].day] = true
				minutes = append(minutes, pts[j].minute)
			}
		}
		if len(days) > len(bestDays) {
			bestDays = days
			sort.Ints(minutes)
			bestMinute = (minutes[len(minutes)/2] + 2) / 5 * 5 % (24 * 60)
		}
	}
	return bestMinute, bestDays
}

// covered: an enabled automation already does this within an hour.
func covered(list []Automation, deviceID string, on bool, minute int) bool {
	want := map[bool]string{true: model.ActTurnOn, false: model.ActTurnOff}[on]
	for _, a := range list {
		if !a.Enabled {
			continue
		}
		does := false
		for _, s := range a.Steps {
			if s.DeviceID == deviceID && (s.Action == want || s.Action == model.ActToggle) {
				does = true
			}
		}
		if !does {
			continue
		}
		for _, t := range a.Triggers {
			if t.Type == "sun" {
				return true // a sunset rule for the same light already handles it
			}
			if t.Type != "time" {
				continue // a motion or door rule doesn't cover a routine at a time of day
			}
			if at, err := time.Parse("15:04", t.At); err == nil {
				diff := at.Hour()*60 + at.Minute() - minute
				if diff < 0 {
					diff = -diff
				}
				if diff <= 60 || diff >= 24*60-60 {
					return true
				}
			}
		}
	}
	return false
}

// routes: GET /api/suggestions lists routines worth automating.
func (h *habits) routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, st *store, autos *automations) {
	mux.HandleFunc("GET /api/suggestions", auth(func(w http.ResponseWriter, _ *http.Request) {
		autos.mu.Lock()
		list := append([]Automation(nil), autos.list...)
		autos.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"suggestions": h.suggest(st.snapshot().Devices, list)})
	}))
}
