package main

import (
	"fmt"
	"net/http"
	"time"

	"hearth/internal/model"
)

// The car's routine: when it usually gets unplugged (the household leaves) and
// plugged back in, learned per weekday and weekend from the charger's status.
// The app uses it to plan charging around when the car is actually needed.

const evMinDays = 3 // days a plug-in or unplug time must repeat on

type CarRoutine struct {
	Leaves  string `json:"leaves,omitempty"`   // usual unplug time, "07:40"
	PlugsIn string `json:"plugs_in,omitempty"` // usual plug-in time, "18:30"
	Seen    int    `json:"seen"`               // days the leave time was seen on
}

func carPlugged(d model.Device) (plugged, known bool) {
	s, _ := d.State["status"].(string)
	if s == "" || !d.Online {
		return false, false
	}
	return s != "unplugged", true
}

func (h *habits) carPlugChanged(prev, cur model.Device) {
	was, ok1 := carPlugged(prev)
	is, ok2 := carPlugged(cur)
	if !ok1 || !ok2 || was == is {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, habitEvent{DeviceID: cur.ID, On: is, At: h.now(), EV: true})
	h.dirty = true
}

// carRoutine works out the weekday and weekend routine for one charger.
func (h *habits) carRoutine(deviceID string) map[string]CarRoutine {
	h.mu.Lock()
	events := append([]habitEvent(nil), h.events...)
	h.mu.Unlock()

	type key struct {
		weekend, plugged bool
	}
	groups := map[key][]time.Time{}
	for _, e := range events {
		if !e.EV || e.DeviceID != deviceID {
			continue
		}
		t := e.At.In(time.Local)
		wd := t.Weekday()
		k := key{wd == time.Saturday || wd == time.Sunday, e.On}
		groups[k] = append(groups[k], t)
	}
	usual := func(times []time.Time) (string, int) {
		if len(times) == 0 {
			return "", 0
		}
		minute, days := bestWindow(times)
		if len(days) < evMinDays {
			return "", len(days)
		}
		return fmt.Sprintf("%02d:%02d", minute/60, minute%60), len(days)
	}
	out := map[string]CarRoutine{}
	for name, weekend := range map[string]bool{"weekdays": false, "weekends": true} {
		leaves, seen := usual(groups[key{weekend, false}])
		plugs, _ := usual(groups[key{weekend, true}])
		if leaves != "" || plugs != "" {
			out[name] = CarRoutine{Leaves: leaves, PlugsIn: plugs, Seen: seen}
		}
	}
	return out
}

// routes: GET /api/devices/{id}/routine returns the learned car routine.
func (h *habits) carRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/devices/{id}/routine", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"routine": h.carRoutine(r.PathValue("id"))})
	}))
}
