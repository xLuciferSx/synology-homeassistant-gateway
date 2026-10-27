package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"hearth/internal/model"
)

const heatKeep = 14

// heatingTime adds up how long each radiator and heater spends actually heating, per local day,
// so the gas bill can be traced to rooms.
type heatingTime struct {
	mu    sync.Mutex
	path  string
	now   func() time.Time
	Days  map[string]map[string]float64 `json:"days"`
	Since map[string]time.Time          `json:"since"`
}

func loadHeatingTime(dir string) *heatingTime {
	h := &heatingTime{path: filepath.Join(dir, "heating_time.json"), now: time.Now,
		Days: map[string]map[string]float64{}, Since: map[string]time.Time{}}
	if b, err := os.ReadFile(h.path); err == nil {
		_ = json.Unmarshal(b, h)
	}
	if h.Days == nil {
		h.Days = map[string]map[string]float64{}
	}
	if h.Since == nil {
		h.Since = map[string]time.Time{}
	}
	return h
}

func (h *heatingTime) record(d model.Device) {
	if d.Kind != model.KindThermostat && d.Kind != model.KindHeater {
		return
	}
	action, _ := d.State["action"].(string)
	heating := d.Online && action == "heating"
	h.mu.Lock()
	defer h.mu.Unlock()
	start, was := h.Since[d.ID]
	switch {
	case heating && !was:
		h.Since[d.ID] = h.now()
	case !heating && was:
		h.addLocked(d.ID, start, h.now())
		delete(h.Since, d.ID)
	default:
		return
	}
	h.saveLocked()
}

// addLocked splits a heating spell across the days it covers, in minutes.
func (h *heatingTime) addLocked(id string, from, to time.Time) {
	from, to = from.In(time.Local), to.In(time.Local)
	for from.Before(to) {
		y, m, d := from.Date()
		next := time.Date(y, m, d+1, 0, 0, 0, 0, from.Location())
		end := to
		if next.Before(end) {
			end = next
		}
		day := from.Format("2006-01-02")
		if h.Days[day] == nil {
			h.Days[day] = map[string]float64{}
		}
		h.Days[day][id] += end.Sub(from).Minutes()
		from = end
	}
	days := make([]string, 0, len(h.Days))
	for k := range h.Days {
		days = append(days, k)
	}
	sort.Strings(days)
	for len(days) > heatKeep {
		delete(h.Days, days[0])
		days = days[1:]
	}
}

func (h *heatingTime) saveLocked() {
	if b, err := json.Marshal(h); err == nil {
		_ = os.WriteFile(h.path, b, 0o600)
	}
}

// minutes returns whole minutes per device per day, including spells still running.
func (h *heatingTime) minutes() map[string]map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	snap := &heatingTime{Days: map[string]map[string]float64{}}
	for day, m := range h.Days {
		snap.Days[day] = map[string]float64{}
		for id, v := range m {
			snap.Days[day][id] = v
		}
	}
	now := h.now()
	for id, start := range h.Since {
		snap.addLocked(id, start, now)
	}
	out := map[string]map[string]int{}
	for day, m := range snap.Days {
		out[day] = map[string]int{}
		for id, v := range m {
			if v >= 1 {
				out[day][id] = int(v)
			}
		}
	}
	return out
}
