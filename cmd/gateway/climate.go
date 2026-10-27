package main

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"hearth/internal/model"
)

// Room climate: the room's temperature is the average of every thermometer in it (separate sensors count
// fully, a heater's own reading counts half because it sits next to the element), and the hub steers the
// heaters' setpoints so that average reaches the target the person chose, not the heater's idea of it.

const (
	climateEvery     = 2 * time.Minute
	climateGain      = 1.0  // °C of extra setpoint per °C the room is off
	climateLearn     = 0.15 // how fast the steady offset adapts, per tick, per °C
	climateMinOffset = -2.0
	climateMaxOffset = 6.0
	climateOverheat  = 0.6 // °C above target where heaters are turned right down
)

type ClimateZone struct {
	RoomID  string  `json:"room_id"`
	Target  float64 `json:"target"`
	Enabled bool    `json:"enabled"`
	Offset  float64 `json:"offset"` // learned: how far above target the heater must be set for the room to settle
}

type ClimateStatus struct {
	ClimateZone
	Average  *float64           `json:"average,omitempty"`
	Sensors  int                `json:"sensors"`
	Setpoint map[string]float64 `json:"setpoints,omitempty"` // heater → what the hub set it to
	Reason   string             `json:"reason,omitempty"`    // why, in a sentence: gas cheaper, window open, away…
}

type roomClimate struct {
	mu        sync.Mutex
	path      string
	zones     map[string]*ClimateZone
	setpoints map[string]float64
	reasons   map[string]string // room → why the heaters are set as they are
	send      func(model.Command) error
	devices   func() []model.Device
}

func loadClimate(dir string, send func(model.Command) error, devices func() []model.Device) *roomClimate {
	c := &roomClimate{path: filepath.Join(dir, "climate.json"), zones: map[string]*ClimateZone{}, setpoints: map[string]float64{}, reasons: map[string]string{}, send: send, devices: devices}
	if b, err := os.ReadFile(c.path); err == nil {
		_ = json.Unmarshal(b, &c.zones)
	}
	return c
}

func (c *roomClimate) saveLocked() {
	if b, err := json.Marshal(c.zones); err == nil {
		_ = os.WriteFile(c.path, b, 0o600)
	}
}

// roomAverage weighs every temperature reading in the room.
func roomAverage(devices []model.Device, roomID string) (avg float64, sensors int, ok bool) {
	var sum, weight float64
	for _, d := range devices {
		if d.RoomID != roomID || !d.Online {
			continue
		}
		switch d.Kind {
		case model.KindSensor:
			if t, has := numOf(d.State["temperature"]); has {
				sum, weight, sensors = sum+t, weight+1, sensors+1
			}
		case model.KindHeater, model.KindThermostat:
			if t, has := numOf(d.State["current"]); has {
				sum, weight = sum+t*0.5, weight+0.5
			}
		}
	}
	if weight == 0 {
		return 0, 0, false
	}
	return sum / weight, sensors, true
}

// Gas boilers turn about 90% of the gas into heat; an electric heater turns all of its electricity into heat.
const boilerEfficiency = 0.9

// energyNow is what the home's tariff and people are doing right now.
type energyNow struct {
	elecP, gasP   float64 // pence per kWh, 0 when unknown
	saving        bool    // Octoplus saving session
	away          bool    // nobody's home
	knownPresence bool
}

func readEnergy(devices []model.Device) energyNow {
	var e energyNow
	for _, d := range devices {
		switch d.Kind {
		case model.KindEnergy:
			e.elecP, _ = numOf(d.State["elec_rate_p"])
			e.gasP, _ = numOf(d.State["gas_rate_p"])
			e.saving, _ = d.State["saving_session"].(bool)
		case model.KindPresence:
			if anyone, ok := d.State["anyone_home"].(bool); ok {
				e.away, e.knownPresence = !anyone, true
			}
		}
	}
	return e
}

// electricIsCheaper: a unit of heat from the electric heater costs no more than one from the gas boiler.
func (e energyNow) electricIsCheaper() bool {
	return e.elecP > 0 && e.gasP > 0 && e.elecP <= e.gasP/boilerEfficiency
}

// windowOpen: a door or window in the room has been open for three minutes or more.
func windowOpen(devices []model.Device, roomID string, now time.Time) bool {
	for _, d := range devices {
		if d.RoomID != roomID || d.Kind != model.KindSensor {
			continue
		}
		if open, _ := d.State["open"].(bool); open {
			since, err := time.Parse(time.RFC3339, strOf(d.State["open_at"]))
			if err != nil || now.Sub(since) >= 3*time.Minute {
				return true
			}
		}
	}
	return false
}

func isElectric(d model.Device) bool { return d.Kind == model.KindHeater }

type heaterPlan struct {
	device   model.Device
	setpoint float64
}

// plan works out every heater's setpoint for one room, and a sentence saying why.
func plan(zone ClimateZone, avg float64, heaters []model.Device, e energyNow, open bool) (offset float64, plans []heaterPlan, reason string) {
	target := zone.Target
	switch {
	case e.saving:
		target, reason = target-1, "Saving session: 1° lower"
	case e.away:
		target, reason = math.Max(16, target-3), "Nobody's home: aiming lower"
	}
	err := target - avg
	offset = math.Max(climateMinOffset, math.Min(climateMaxOffset, zone.Offset+climateLearn*err))
	hasCentral := false
	for _, h := range heaters {
		if !isElectric(h) {
			hasCentral = true
		}
	}
	for _, h := range heaters {
		lo, hi, step := 5.0, 35.0, 0.5
		if v, ok := numOf(h.State["min"]); ok {
			lo = v
		}
		if v, ok := numOf(h.State["max"]); ok {
			hi = v
		}
		if v, ok := numOf(h.State["step"]); ok && v > 0 {
			step = v
		}
		set := target + offset + climateGain*err
		switch {
		case open:
			set, reason = lo, "Window open: heating paused"
		case avg >= target+climateOverheat:
			set = lo // warm enough everywhere; let it idle
		case isElectric(h) && hasCentral && e.saving:
			set = lo
		case isElectric(h) && hasCentral && !e.electricIsCheaper() && err < 1:
			set = lo // the radiator is cheaper and can manage
			if reason == "" {
				reason = "Using the radiator: gas is cheaper now"
			}
		case isElectric(h) && hasCentral && e.electricIsCheaper():
			if reason == "" {
				reason = "Using the electric heater: electricity is cheaper now"
			}
		case isElectric(h) && hasCentral:
			if reason == "" {
				reason = "Boosting with the electric heater: the room is well below target"
			}
		}
		set = math.Round(math.Max(lo, math.Min(hi, set))/step) * step
		plans = append(plans, heaterPlan{device: h, setpoint: set})
	}
	if open {
		offset = zone.Offset // don't learn from a room with the window open
	}
	return offset, plans, reason
}

// decide returns the new learned offset and the setpoint for one heater, with no tariff or presence.
func decide(zone ClimateZone, avg float64, heater model.Device) (offset, setpoint float64) {
	offset, plans, _ := plan(zone, avg, []model.Device{heater}, energyNow{}, false)
	return offset, plans[0].setpoint
}

func (c *roomClimate) tick() {
	devices := c.devices()
	e := readEnergy(devices)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	for _, zone := range c.zones {
		if !zone.Enabled {
			continue
		}
		avg, sensors, ok := roomAverage(devices, zone.RoomID)
		if !ok || sensors == 0 {
			continue // no separate thermometer: the heater's own control is as good as it gets
		}
		var heaters []model.Device
		for _, d := range devices {
			if d.RoomID == zone.RoomID && d.Online && (d.Kind == model.KindHeater || d.Kind == model.KindThermostat) && strOf(d.State["mode"]) != "off" {
				heaters = append(heaters, d)
			}
		}
		if len(heaters) == 0 {
			continue
		}
		offset, plans, reason := plan(*zone, avg, heaters, e, windowOpen(devices, zone.RoomID, now))
		zone.Offset, changed = offset, true
		c.reasons[zone.RoomID] = reason
		for _, p := range plans {
			current, _ := numOf(p.device.State["target"])
			if math.Abs(current-p.setpoint) < 0.01 {
				continue
			}
			if err := c.send(model.Command{DeviceID: p.device.ID, Action: model.ActSetTemperature, Params: map[string]any{"value": p.setpoint}}); err != nil {
				slog.Warn("room climate", "room", zone.RoomID, "heater", p.device.ID, "err", err)
				continue
			}
			c.setpoints[p.device.ID] = p.setpoint
			slog.Info("room climate", "room", zone.RoomID, "average", math.Round(avg*10)/10, "target", zone.Target, "heater", p.device.ID, "set", p.setpoint, "why", reason)
		}
	}
	if changed {
		c.saveLocked()
	}
}

func (c *roomClimate) status(roomID string) ClimateStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	devices := c.devices()
	st := ClimateStatus{Reason: c.reasons[roomID], ClimateZone: ClimateZone{RoomID: roomID, Target: 20}}
	if z := c.zones[roomID]; z != nil {
		st.ClimateZone = *z
	} else {
		for _, d := range devices { // not set up yet: start from what the room's heater is aiming for
			if t, ok := numOf(d.State["target"]); ok && d.RoomID == roomID && (d.Kind == model.KindHeater || d.Kind == model.KindThermostat) && t >= 5 {
				st.Target = math.Round(t*2) / 2
				break
			}
		}
	}
	if avg, n, ok := roomAverage(devices, roomID); ok {
		a := math.Round(avg*10) / 10
		st.Average, st.Sensors = &a, n
	}
	for _, d := range devices {
		if v, ok := c.setpoints[d.ID]; ok && d.RoomID == roomID {
			if st.Setpoint == nil {
				st.Setpoint = map[string]float64{}
			}
			st.Setpoint[d.ID] = v
		}
	}
	return st
}

// routes: GET /api/rooms/{id}/climate and PUT it with {"target": 21, "enabled": true}.
func (c *roomClimate) routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/rooms/{id}/climate", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, c.status(r.PathValue("id")))
	}))
	mux.HandleFunc("PUT /api/rooms/{id}/climate", auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Target  *float64 `json:"target"`
			Enabled *bool    `json:"enabled"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Send a target and whether it's on."})
			return
		}
		id := r.PathValue("id")
		c.mu.Lock()
		z := c.zones[id]
		if z == nil {
			z = &ClimateZone{RoomID: id, Target: 20}
			c.zones[id] = z
		}
		if in.Target != nil {
			if *in.Target < 5 || *in.Target > 30 {
				c.mu.Unlock()
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Choose between 5° and 30°."})
				return
			}
			z.Target = math.Round(*in.Target*2) / 2
		}
		switchedOn := in.Enabled != nil && *in.Enabled && !z.Enabled
		if in.Enabled != nil {
			z.Enabled = *in.Enabled
		}
		c.saveLocked()
		c.mu.Unlock()
		if switchedOn { // turning the room on wakes its heaters once; after that, a heater someone turns off stays off
			for _, d := range c.devices() {
				if d.RoomID == id && d.Online && (d.Kind == model.KindHeater || d.Kind == model.KindThermostat) && strOf(d.State["mode"]) == "off" {
					_ = c.send(model.Command{DeviceID: d.ID, Action: model.ActTurnOn})
				}
			}
		}
		go c.tick()
		writeJSON(w, http.StatusOK, c.status(id))
	}))
}
