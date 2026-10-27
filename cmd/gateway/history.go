package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"hearth/internal/model"
)

// historyKeys are the readings worth charting.
var historyKeys = []string{"temperature", "current", "target", "humidity", "pm25", "power_w", "power_kw", "car_battery", "battery", "elec_rate_p"}

const (
	historyStep   = 5 * time.Minute
	historyWindow = 7 * 24 * time.Hour
)

type point struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// history keeps a week of readings per device, one point per five minutes, in memory and on disk.
type history struct {
	mu     sync.Mutex
	path   string
	series map[string]map[string][]point
	dirty  bool
	now    func() time.Time
}

func loadHistory(dir string) *history {
	h := &history{path: filepath.Join(dir, "history.json"), series: map[string]map[string][]point{}, now: time.Now}
	if b, err := os.ReadFile(h.path); err == nil {
		_ = json.Unmarshal(b, &h.series)
	}
	return h
}

func (h *history) record(d model.Device) {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range historyKeys {
		v, ok := d.State[k].(float64)
		if !ok {
			if i, isInt := d.State[k].(int); isInt {
				v, ok = float64(i), true
			}
		}
		if !ok {
			continue
		}
		if h.series[d.ID] == nil {
			h.series[d.ID] = map[string][]point{}
		}
		list := h.series[d.ID][k]
		p := point{T: now.Unix(), V: v}
		step := int64(historyStep / time.Second)
		if n := len(list); n > 0 && list[n-1].T/step == p.T/step {
			list[n-1] = p
		} else {
			list = append(list, p)
		}
		cut := now.Add(-historyWindow).Unix()
		i := 0
		for i < len(list) && list[i].T < cut {
			i++
		}
		h.series[d.ID][k] = list[i:]
		h.dirty = true
	}
}

func (h *history) get(id, key string, since time.Duration) []point {
	h.mu.Lock()
	defer h.mu.Unlock()
	cut := h.now().Add(-since).Unix()
	out := []point{}
	for _, p := range h.series[id][key] {
		if p.T >= cut {
			out = append(out, p)
		}
	}
	return out
}

func (h *history) flush() {
	h.mu.Lock()
	if !h.dirty {
		h.mu.Unlock()
		return
	}
	b, err := json.Marshal(h.series)
	h.dirty = false
	h.mu.Unlock()
	if err != nil {
		return
	}
	tmp := h.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, h.path)
	}
}
