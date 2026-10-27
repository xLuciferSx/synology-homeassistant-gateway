package main

import (
	"encoding/json"
	"testing"

	"hearth/internal/model"
)

// Trimmed from real zigbee2mqtt/bridge/devices payloads.
const devicesJSON = `[
 {"ieee_address":"0x00124b0001","type":"Coordinator","interview_completed":true,"supported":true,"friendly_name":"Coordinator"},
 {"ieee_address":"0x0017880102","type":"Router","interview_completed":true,"supported":true,"friendly_name":"Office/Desk lamp",
  "definition":{"model":"9290012573A","vendor":"Philips","description":"Hue white and color ambiance E27",
   "exposes":[{"type":"light","features":[
     {"type":"binary","name":"state","property":"state","access":7,"value_on":"ON","value_off":"OFF"},
     {"type":"numeric","name":"brightness","property":"brightness","access":7,"value_min":0,"value_max":254}]},
    {"type":"numeric","name":"linkquality","property":"linkquality","access":1}]}},
 {"ieee_address":"0x54ef441003","type":"EndDevice","interview_completed":true,"supported":true,"friendly_name":"Main Bedroom/Radiator",
  "definition":{"model":"SRTS-A01","vendor":"Aqara","description":"Smart radiator thermostat E1",
   "exposes":[{"type":"climate","features":[
     {"type":"numeric","name":"occupied_heating_setpoint","property":"occupied_heating_setpoint","access":7,"value_min":5,"value_max":30,"value_step":0.5},
     {"type":"numeric","name":"local_temperature","property":"local_temperature","access":5},
     {"type":"enum","name":"system_mode","property":"system_mode","access":7,"values":["off","heat"]},
     {"type":"enum","name":"running_state","property":"running_state","access":5,"values":["idle","heat"]}]},
    {"type":"numeric","name":"battery","property":"battery","access":1}]}},
 {"ieee_address":"0x00158d0004","type":"EndDevice","interview_completed":true,"supported":true,"friendly_name":"Hall motion",
  "definition":{"model":"RTCGQ11LM","vendor":"Aqara","description":"Motion sensor",
   "exposes":[{"type":"binary","name":"occupancy","property":"occupancy","access":1,"value_on":true,"value_off":false},
    {"type":"numeric","name":"battery","property":"battery","access":1}]}},
 {"ieee_address":"0x0c4314fffe5","type":"Router","interview_completed":true,"supported":true,"friendly_name":"Living Room/Purifier",
  "definition":{"model":"E2007","vendor":"IKEA","description":"STARKVIND air purifier",
   "exposes":[{"type":"binary","name":"fan_state","property":"fan_state","access":7,"value_on":"ON","value_off":"OFF"},
    {"type":"enum","name":"fan_mode","property":"fan_mode","access":7,"values":["off","auto","1","5","9"]},
    {"type":"numeric","name":"pm25","property":"pm25","access":5}]}},
 {"ieee_address":"0xdead","type":"EndDevice","interview_completed":false,"friendly_name":"Half paired"}
]`

func load(t *testing.T) map[string]*product {
	t.Helper()
	var devs []z2mDevice
	if err := json.Unmarshal([]byte(devicesJSON), &devs); err != nil {
		t.Fatal(err)
	}
	out := map[string]*product{}
	for _, d := range devs {
		if include(d) {
			p := newProduct(d)
			out[p.dev.ID] = p
		}
	}
	return out
}

func TestClassify(t *testing.T) {
	ps := load(t)
	if len(ps) != 4 {
		t.Fatalf("want 4 products (no coordinator, no half-paired), got %d", len(ps))
	}
	cases := map[string]struct {
		kind model.Kind
		room string
		name string
	}{
		"zb-0x0017880102":  {model.KindLight, "office", "Desk lamp"},
		"zb-0x54ef441003":  {model.KindThermostat, "main_bedroom", "Radiator"},
		"zb-0x00158d0004":  {model.KindSensor, "", "Hall motion"},
		"zb-0x0c4314fffe5": {model.KindAirPurifier, "living_room", "Purifier"},
	}
	for id, want := range cases {
		p := ps[id]
		if p == nil {
			t.Fatalf("missing %s", id)
		}
		if p.dev.Kind != want.kind || p.dev.RoomID != want.room || p.dev.Name != want.name {
			t.Errorf("%s = %s/%q/%q, want %s/%q/%q", id, p.dev.Kind, p.dev.RoomID, p.dev.Name, want.kind, want.room, want.name)
		}
	}
}

func TestState(t *testing.T) {
	ps := load(t)

	lamp := ps["zb-0x0017880102"]
	lamp.raw = map[string]any{"state": "ON", "brightness": 127.0, "linkquality": 90.0}
	lamp.refresh()
	if lamp.dev.State["on"] != true || lamp.dev.State["brightness"] != 50 || lamp.dev.State["signal"] != 90.0 {
		t.Errorf("lamp state %v", lamp.dev.State)
	}

	rad := ps["zb-0x54ef441003"]
	rad.raw = map[string]any{"occupied_heating_setpoint": 21.0, "local_temperature": 19.5, "system_mode": "heat", "running_state": "heat", "battery": 15.0}
	rad.refresh()
	s := rad.dev.State
	if s["target"] != 21.0 || s["current"] != 19.5 || s["action"] != "heating" || s["step"] != 0.5 || s["battery"] != 15.0 {
		t.Errorf("radiator state %v", s)
	}

	pir := ps["zb-0x00158d0004"]
	pir.raw = map[string]any{"occupancy": true}
	off := false
	pir.available = &off
	pir.refresh()
	if pir.dev.State["motion"] != true || pir.dev.Online {
		t.Errorf("motion state %v online=%v", pir.dev.State, pir.dev.Online)
	}
}

func TestCommands(t *testing.T) {
	ps := load(t)
	check := func(p *product, cmd model.Command, key string, want any) {
		t.Helper()
		got, err := p.setPayload(cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd.Action, err)
		}
		if got[key] != want {
			t.Errorf("%s: payload %v, want %s=%v", cmd.Action, got, key, want)
		}
	}
	lamp := ps["zb-0x0017880102"]
	check(lamp, model.Command{Action: model.ActToggle}, "state", "TOGGLE")
	check(lamp, model.Command{Action: model.ActSetBrightness, Params: map[string]any{"value": 100.0}}, "brightness", 254)

	rad := ps["zb-0x54ef441003"]
	check(rad, model.Command{Action: model.ActSetTemperature, Params: map[string]any{"value": 22.5}}, "occupied_heating_setpoint", 22.5)
	check(rad, model.Command{Action: model.ActTurnOff}, "system_mode", "off")

	pur := ps["zb-0x0c4314fffe5"]
	check(pur, model.Command{Action: model.ActSetPreset, Params: map[string]any{"preset": "auto"}}, "fan_mode", "auto")
	check(pur, model.Command{Action: model.ActTurnOn}, "fan_state", "ON")

	if _, err := ps["zb-0x00158d0004"].setPayload(model.Command{Action: model.ActTurnOn}); err == nil {
		t.Error("sensor should reject turn_on")
	}
}
