package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"hearth/internal/model"
)

// away is the house's "nobody's home" mode: lights off, doors locked, heating
// turned down. It remembers each radiator's setting and puts it back as soon as
// someone comes home.
type away struct {
	mu      sync.Mutex
	path    string
	temp    float64
	send    func(model.Command) error
	devices func() []model.Device
	changed func()
	state   awayState
}

type awayState struct {
	On      bool               `json:"on"`
	Since   time.Time          `json:"since,omitempty"`
	Targets map[string]float64 `json:"targets,omitempty"` // thermostat → target before away
	Heaters []string           `json:"heaters,omitempty"` // heaters that were on
}

func loadAway(dir string, temp float64) *away {
	a := &away{path: filepath.Join(dir, "away.json"), temp: temp, changed: func() {}}
	if b, err := os.ReadFile(a.path); err == nil {
		_ = json.Unmarshal(b, &a.state)
	}
	return a
}

func (a *away) on() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state.On
}

func (a *away) save() {
	if b, err := json.Marshal(a.state); err == nil {
		_ = os.WriteFile(a.path, b, 0o600)
	}
}

func (a *away) cmd(id, action string, params map[string]any) {
	if err := a.send(model.Command{DeviceID: id, Action: action, Params: params}); err != nil {
		slog.Warn("away mode command failed", "device", id, "action", action, "err", err)
	}
}

func can(d model.Device, action string) bool {
	for _, c := range d.Capabilities {
		if c == action {
			return true
		}
	}
	return false
}

// start turns the house down and returns how many things it changed.
func (a *away) start(arm bool) int {
	a.mu.Lock()
	if a.state.On {
		a.mu.Unlock()
		return 0
	}
	a.state = awayState{On: true, Since: time.Now(), Targets: map[string]float64{}}
	n := 0
	for _, d := range a.devices() {
		if !d.Online || d.Hidden {
			continue
		}
		switch d.Kind {
		case model.KindLight:
			if on, _ := d.State["on"].(bool); on {
				a.cmd(d.ID, model.ActTurnOff, nil)
				n++
			}
		case model.KindThermostat:
			target, ok := numOf(d.State["target"])
			if mode, _ := d.State["mode"].(string); ok && mode != "off" && target > a.temp && can(d, model.ActSetTemperature) {
				a.state.Targets[d.ID] = target
				a.cmd(d.ID, model.ActSetTemperature, map[string]any{"value": a.temp})
				n++
			}
		case model.KindHeater:
			if mode, _ := d.State["mode"].(string); mode != "" && mode != "off" {
				a.state.Heaters = append(a.state.Heaters, d.ID)
				a.cmd(d.ID, model.ActTurnOff, nil)
				n++
			}
		case model.KindLock:
			if locked, ok := d.State["locked"].(bool); ok && !locked && can(d, model.ActLock) {
				a.cmd(d.ID, model.ActLock, nil)
				n++
			}
		case model.KindAlarm:
			if status, _ := d.State["status"].(string); arm && status == "disarmed" && can(d, model.ActArmAway) {
				a.cmd(d.ID, model.ActArmAway, nil)
				n++
			}
		}
	}
	a.save()
	a.mu.Unlock()
	a.changed()
	return n
}

// stop puts the heating back the way it was.
func (a *away) stop() int {
	a.mu.Lock()
	if !a.state.On {
		a.mu.Unlock()
		return 0
	}
	n := 0
	for id, target := range a.state.Targets {
		a.cmd(id, model.ActSetTemperature, map[string]any{"value": target})
		n++
	}
	for _, id := range a.state.Heaters {
		a.cmd(id, model.ActTurnOn, nil)
		n++
	}
	a.state = awayState{}
	a.save()
	a.mu.Unlock()
	a.changed()
	return n
}

// presenceChanged ends away mode when the People device says someone is home.
func (a *away) presenceChanged(d model.Device) bool {
	if d.Kind != model.KindPresence {
		return false
	}
	if home, ok := d.State["anyone_home"].(bool); ok && home && a.on() {
		a.stop()
		return true
	}
	return false
}

// routes: GET /api/away tells whether away mode is on; POST /api/away {"on": bool, "arm": bool} switches it.
func (a *away) routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, log func(Entry), who func(*http.Request) string) {
	mux.HandleFunc("GET /api/away", auth(func(w http.ResponseWriter, _ *http.Request) {
		a.mu.Lock()
		s := a.state
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"on": s.On, "since": s.Since, "temperature": a.temp})
	}))
	mux.HandleFunc("POST /api/away", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			On  bool `json:"on"`
			Arm bool `json:"arm"`
		}
		if !decode(w, r, &body) {
			return
		}
		var n int
		if body.On {
			n = a.start(body.Arm)
			log(Entry{Kind: "command", Icon: "figure.walk", Who: who(r), Text: "turned on Away mode"})
		} else {
			n = a.stop()
			log(Entry{Kind: "command", Icon: "house.fill", Who: who(r), Text: "turned off Away mode"})
		}
		writeJSON(w, http.StatusOK, map[string]any{"on": body.On, "changed": n})
	}))
}
