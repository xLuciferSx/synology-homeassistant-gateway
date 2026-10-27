package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"hearth/internal/model"
)

// z2mDevice is one entry of the retained zigbee2mqtt/bridge/devices list.
type z2mDevice struct {
	IEEE               string         `json:"ieee_address"`
	FriendlyName       string         `json:"friendly_name"`
	Type               string         `json:"type"` // Coordinator, Router, EndDevice
	Supported          bool           `json:"supported"`
	Disabled           bool           `json:"disabled"`
	InterviewCompleted bool           `json:"interview_completed"`
	Manufacturer       string         `json:"manufacturer"`
	ModelID            string         `json:"model_id"`
	Definition         *z2mDefinition `json:"definition"`
}

type z2mDefinition struct {
	Model       string   `json:"model"`
	Vendor      string   `json:"vendor"`
	Description string   `json:"description"`
	Exposes     []expose `json:"exposes"`
}

// expose describes one capability (see Zigbee2MQTT "exposes" docs).
type expose struct {
	Type      string   `json:"type"` // light, switch, climate, lock, cover, fan, numeric, binary, enum, text, composite
	Name      string   `json:"name"`
	Property  string   `json:"property"`
	Endpoint  string   `json:"endpoint"`
	Access    int      `json:"access"`
	ValueOn   any      `json:"value_on"`
	ValueOff  any      `json:"value_off"`
	ValueMin  *float64 `json:"value_min"`
	ValueMax  *float64 `json:"value_max"`
	ValueStep *float64 `json:"value_step"`
	Values    []string `json:"values"`
	Features  []expose `json:"features"`
}

const accessSet = 2

// product is a Zigbee device translated into the shared product model.
type product struct {
	dev      model.Device
	name     string // Zigbee2MQTT friendly_name (topic)
	ieee     string
	nameRoom string // room from the "Room/Device" name, if any

	stateProp     string // on/off, lock or cover state property
	onVal, offVal any
	brightnessMax float64
	setpointProp  string
	presetProp    string
	modes         []string
	presets       []string
	presses       []string
	min, max, stp *float64

	raw       map[string]any
	available *bool
}

// all returns every expose including nested features, flattened.
func all(ex []expose) []expose {
	var out []expose
	for _, e := range ex {
		out = append(out, e)
		out = append(out, all(e.Features)...)
	}
	return out
}

func find(ex []expose, props ...string) *expose {
	for _, e := range all(ex) {
		if slices.Contains(props, e.Property) {
			return &e
		}
	}
	return nil
}

// splitName maps the "Room/Device" friendly-name convention to a room.
func splitName(friendly string) (room, name string) {
	if i := strings.Index(friendly, "/"); i > 0 {
		return strings.TrimSpace(friendly[:i]), strings.TrimSpace(friendly[i+1:])
	}
	return "", friendly
}

// slug makes a room id compatible with Home Assistant area ids ("Main Bedroom" → "main_bedroom"),
// so rooms from both bridges merge into one.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '-':
			b.WriteRune('_')
		}
	}
	return b.String()
}

func include(d z2mDevice) bool {
	return d.Type != "Coordinator" && !d.Disabled && d.InterviewCompleted && d.Definition != nil
}

// newProduct classifies a device from its exposes.
func newProduct(d z2mDevice) *product {
	roomName, name := splitName(d.FriendlyName)
	def := d.Definition
	p := &product{
		name:     d.FriendlyName,
		ieee:     d.IEEE,
		nameRoom: slug(roomName),
		raw:      map[string]any{},
		dev: model.Device{
			ID:           "zb-" + d.IEEE,
			Name:         name,
			RoomID:       slug(roomName),
			Manufacturer: def.Vendor,
			Model:        strings.TrimSpace(def.Description + " (" + def.Model + ")"),
			Source:       source,
			Online:       true,
			Kind:         model.KindSensor,
		},
	}
	top := func(t string) *expose {
		for _, e := range def.Exposes {
			if e.Type == t {
				return &e
			}
		}
		return nil
	}
	useState := func(e *expose) {
		if s := find(e.Features, "state", "state_"+e.Endpoint); s != nil {
			p.stateProp, p.onVal, p.offVal = s.Property, s.ValueOn, s.ValueOff
		}
	}

	switch {
	case top("climate") != nil:
		c := top("climate")
		p.dev.Kind = model.KindThermostat
		if sp := find(c.Features, "occupied_heating_setpoint", "current_heating_setpoint"); sp != nil {
			p.setpointProp, p.min, p.max, p.stp = sp.Property, sp.ValueMin, sp.ValueMax, sp.ValueStep
		}
		if m := find(c.Features, "system_mode"); m != nil {
			p.modes = m.Values
		}
	case top("lock") != nil:
		p.dev.Kind = model.KindLock
		useState(top("lock"))
	case top("cover") != nil:
		p.dev.Kind = model.KindCover
		useState(top("cover"))
	case top("light") != nil:
		l := top("light")
		p.dev.Kind = model.KindLight
		useState(l)
		p.brightnessMax = 254
		if b := find(l.Features, "brightness", "brightness_"+l.Endpoint); b != nil && b.ValueMax != nil {
			p.brightnessMax = *b.ValueMax
		}
	case top("fan") != nil || find(def.Exposes, "fan_state") != nil:
		p.dev.Kind = model.KindFan
		if find(def.Exposes, "pm25") != nil {
			p.dev.Kind = model.KindAirPurifier
		}
		if s := find(def.Exposes, "fan_state", "state"); s != nil {
			p.stateProp, p.onVal, p.offVal = s.Property, s.ValueOn, s.ValueOff
		}
		if m := find(def.Exposes, "fan_mode"); m != nil {
			p.presetProp, p.presets = m.Property, m.Values
		}
	case top("switch") != nil:
		p.dev.Kind = model.KindSwitch
		// A relay wired to lights ("Upstairs Bathroom Lights") is a light to people.
		if strings.Contains(strings.ToLower(name), "light") || strings.Contains(strings.ToLower(name), "lamp") {
			p.dev.Kind = model.KindLight
		}
		useState(top("switch"))
	case find(def.Exposes, "action") != nil:
		p.dev.Kind = model.KindButton
		p.presses = find(def.Exposes, "action").Values
	}
	if p.onVal == nil {
		p.onVal, p.offVal = "ON", "OFF"
	}
	p.dev.Capabilities = p.capabilities()
	p.refresh()
	return p
}

func (p *product) capabilities() []string {
	switch p.dev.Kind {
	case model.KindLight:
		return []string{model.ActTurnOn, model.ActTurnOff, model.ActToggle, model.ActSetBrightness}
	case model.KindSwitch:
		return []string{model.ActTurnOn, model.ActTurnOff, model.ActToggle}
	case model.KindThermostat:
		return []string{model.ActSetTemperature, model.ActSetMode, model.ActTurnOn, model.ActTurnOff}
	case model.KindFan, model.KindAirPurifier:
		c := []string{model.ActTurnOn, model.ActTurnOff, model.ActToggle}
		if p.presetProp != "" {
			c = append(c, model.ActSetPreset)
		}
		return c
	case model.KindLock:
		return []string{model.ActLock, model.ActUnlock}
	case model.KindCover:
		return []string{model.ActOpen, model.ActClose, model.ActStop}
	}
	return []string{}
}

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// refresh recomputes the normalized state (same keys as the other bridges).
func (p *product) refresh() {
	r := p.raw
	st := map[string]any{}
	isOn := func() bool { return p.stateProp != "" && fmt.Sprint(r[p.stateProp]) == fmt.Sprint(p.onVal) }

	switch p.dev.Kind {
	case model.KindLight, model.KindSwitch:
		st["on"] = isOn()
		if b, ok := num(r["brightness"]); ok && p.brightnessMax > 0 {
			st["brightness"] = int(b / p.brightnessMax * 100)
		}
	case model.KindThermostat:
		st["mode"] = r["system_mode"]
		st["modes"] = p.modes
		st["current"] = r["local_temperature"]
		st["target"] = r[p.setpointProp]
		switch r["running_state"] {
		case "heat":
			st["action"] = "heating"
		case "idle":
			st["action"] = "idle"
		}
		if p.min != nil {
			st["min"] = *p.min
		}
		if p.max != nil {
			st["max"] = *p.max
		}
		if p.stp != nil {
			st["step"] = *p.stp
		}
		for _, k := range []string{"pi_heating_demand", "position", "valve_position"} {
			if v, ok := num(r[k]); ok {
				st["valve_position"] = v
				break
			}
		}
	case model.KindFan, model.KindAirPurifier:
		st["on"] = isOn()
		if p.presetProp != "" {
			st["preset"] = r[p.presetProp]
			st["presets"] = p.presets
		}
		st["pm25"] = r["pm25"]
	case model.KindLock:
		st["locked"] = isOn()
		st["status"] = r["lock_state"]
	case model.KindCover:
		st["status"] = r["state"]
		st["position"] = r["position"]
	case model.KindButton:
		st["presses"] = p.presses
		if a, _ := r["action"].(string); a != "" {
			st["press"] = a
		}
		if t, _ := r["last_pressed"].(string); t != "" {
			st["last_pressed"] = t
			st["last_press"] = r["last_press"]
		}
	}
	// sensors and shared readings
	if v, ok := r["occupancy"].(bool); ok {
		st["motion"] = v
	} else if v, ok := r["presence"].(bool); ok {
		st["motion"] = v
	}
	if v, ok := r["contact"].(bool); ok {
		st["open"] = !v // Zigbee2MQTT: contact=true means closed
	}
	if v, ok := r["water_leak"].(bool); ok {
		st["leak"] = v
	}
	if v, ok := r["smoke"].(bool); ok {
		st["smoke"] = v
	}
	copyNum := func(from, to string) {
		if v, ok := num(r[from]); ok {
			st[to] = v
		}
	}
	copyNum("battery", "battery")
	copyNum("temperature", "temperature")
	copyNum("humidity", "humidity")
	copyNum("linkquality", "signal")
	copyNum("illuminance", "illuminance")
	copyNum("power", "power")
	if p.dev.Kind == model.KindThermostat {
		if _, ok := st["temperature"]; !ok {
			copyNum("local_temperature", "temperature")
		}
	}
	for k, v := range st {
		if v == nil {
			delete(st, k)
		}
	}
	if p.available != nil {
		p.dev.Online = *p.available
	}
	p.dev.State = st
	p.dev.UpdatedAt = time.Now().UTC()
}

// setPayload translates a product command into a Zigbee2MQTT /set payload.
func (p *product) setPayload(cmd model.Command) (map[string]any, error) {
	param := cmd.Params
	switch cmd.Action {
	case model.ActTurnOn, model.ActTurnOff, model.ActToggle:
		if p.dev.Kind == model.KindThermostat {
			mode := "heat"
			if cmd.Action == model.ActTurnOff {
				mode = "off"
			}
			return map[string]any{"system_mode": mode}, nil
		}
		if p.stateProp == "" {
			break
		}
		v := p.onVal
		switch {
		case cmd.Action == model.ActTurnOff:
			v = p.offVal
		case cmd.Action == model.ActToggle && (p.dev.Kind == model.KindLight || p.dev.Kind == model.KindSwitch):
			v = "TOGGLE"
		case cmd.Action == model.ActToggle && p.dev.State["on"] == true:
			v = p.offVal
		}
		return map[string]any{p.stateProp: v}, nil
	case model.ActSetBrightness:
		pct, ok := num(param["value"])
		if !ok || p.dev.Kind != model.KindLight {
			break
		}
		return map[string]any{"brightness": int(pct / 100 * p.brightnessMax), p.stateProp: p.onVal}, nil
	case model.ActSetTemperature:
		if v, ok := num(param["value"]); ok && p.setpointProp != "" {
			return map[string]any{p.setpointProp: v}, nil
		}
	case model.ActSetMode:
		if m, ok := param["mode"].(string); ok {
			return map[string]any{"system_mode": m}, nil
		}
	case model.ActSetPreset:
		if m, ok := param["preset"].(string); ok && p.presetProp != "" {
			return map[string]any{p.presetProp: m}, nil
		}
	case model.ActLock, model.ActUnlock:
		v := p.onVal
		if cmd.Action == model.ActUnlock {
			v = p.offVal
		}
		return map[string]any{p.stateProp: v}, nil
	case model.ActOpen:
		return map[string]any{"state": "OPEN"}, nil
	case model.ActClose:
		return map[string]any{"state": "CLOSE"}, nil
	case model.ActStop:
		return map[string]any{"state": "STOP"}, nil
	}
	return nil, fmt.Errorf("%s not supported by %s", cmd.Action, p.dev.Name)
}
