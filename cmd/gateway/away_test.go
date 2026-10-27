package main

import (
	"testing"

	"hearth/internal/model"
)

func TestAwayModeTurnsDownAndRestores(t *testing.T) {
	var sent []model.Command
	a := loadAway(t.TempDir(), 16)
	a.send = func(c model.Command) error { sent = append(sent, c); return nil }
	a.devices = func() []model.Device {
		return []model.Device{
			{ID: "lamp", Kind: model.KindLight, Online: true, State: map[string]any{"on": true}},
			{ID: "trv", Kind: model.KindThermostat, Online: true, Capabilities: []string{"set_temperature"}, State: map[string]any{"mode": "heat", "target": 21.0}},
			{ID: "cool", Kind: model.KindThermostat, Online: true, Capabilities: []string{"set_temperature"}, State: map[string]any{"mode": "heat", "target": 15.0}},
			{ID: "dreo", Kind: model.KindHeater, Online: true, State: map[string]any{"mode": "heat"}},
			{ID: "door", Kind: model.KindLock, Online: true, Capabilities: []string{"lock"}, State: map[string]any{"locked": false}},
		}
	}
	if n := a.start(false); n != 4 || !a.on() {
		t.Fatalf("changed %d, on %v: %+v", n, a.on(), sent)
	}
	sent = nil
	people := model.Device{Kind: model.KindPresence, State: map[string]any{"anyone_home": true}}
	if !a.presenceChanged(people) || a.on() {
		t.Fatal("arriving home should end away mode")
	}
	if len(sent) != 2 || sent[0].Params["value"] != 21.0 && sent[1].Params["value"] != 21.0 {
		t.Fatalf("restore: %+v", sent)
	}
	reloaded := loadAway(a.path[:len(a.path)-len("/away.json")], 16)
	if reloaded.on() {
		t.Fatal("away state should be saved as off")
	}
}
