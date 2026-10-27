package main

import (
	"testing"
	"time"

	"hearth/internal/model"
)

func TestSmartSuggestions(t *testing.T) {
	monday := time.Date(2026, 10, 19, 7, 40, 0, 0, time.Local)
	h := &habits{auto: map[string]time.Time{}, now: func() time.Time { return monday }}
	for i := 0; i < 4; i++ {
		h.events = append(h.events, habitEvent{DeviceID: "ohme", On: false, EV: true, At: monday.AddDate(0, 0, i)})
	}
	car := model.Device{ID: "ohme", Name: "Ohme", Kind: model.KindEVCharger, Online: true,
		Capabilities: []string{model.ActSetChargeMode, model.ActSetChargeTo, model.ActSetReadyBy}, State: map[string]any{"plugged": false}}
	people := model.Device{ID: "people", Kind: model.KindPresence, Online: true, State: map[string]any{"anyone_home": true}}
	lamp := model.Device{ID: "lamp", Kind: model.KindLight, Online: true, Capabilities: []string{model.ActTurnOff}}
	door := model.Device{ID: "door", Kind: model.KindLock, Online: true, Capabilities: []string{model.ActLock}}

	got := h.smartSuggestions([]model.Device{car, people, lamp, door}, nil)
	if len(got) != 2 {
		t.Fatalf("want car and away suggestions, got %+v", got)
	}
	carS := got[0]
	if carS.Title != "Have the car ready by 07:25" || len(carS.Automation.Steps) != 3 || carS.Automation.Triggers[0].Key != "plugged" {
		t.Fatalf("car suggestion: %+v", carS)
	}
	if err := validate(carS.Automation); err != nil {
		t.Fatalf("car automation invalid: %v", err)
	}
	away := got[1]
	if away.Automation.Triggers[0].Value != false || len(away.Automation.Steps) != 2 {
		t.Fatalf("away suggestion: %+v", away)
	}

	// Already automated: no suggestion.
	existing := []Automation{{Enabled: true, Steps: []Step{{DeviceID: "ohme", Action: model.ActSetChargeMode}}}}
	if got := h.smartSuggestions([]model.Device{car}, existing); len(got) != 0 {
		t.Fatalf("car already automated: %+v", got)
	}
}

func TestAMotionRuleDoesNotHideATimeRoutine(t *testing.T) {
	motion := []Automation{{Enabled: true, Triggers: []Trigger{{Type: "device", DeviceID: "pir", Key: "motion"}},
		Steps: []Step{{DeviceID: "lamp", Action: model.ActTurnOff}}}}
	if covered(motion, "lamp", false, 23*60) {
		t.Fatal("a motion rule shouldn't count as the 23:00 routine")
	}
	sunset := []Automation{{Enabled: true, Triggers: []Trigger{{Type: "sun", Event: "sunset"}}, Steps: []Step{{DeviceID: "lamp", Action: model.ActTurnOn}}}}
	if !covered(sunset, "lamp", true, 17*60) {
		t.Fatal("a sunset rule should cover an evening on routine")
	}
}

func TestSavingSessionsPauseTheCar(t *testing.T) {
	h := &habits{auto: map[string]time.Time{}, now: time.Now}
	energy := model.Device{ID: "ha-energy", Kind: model.KindEnergy, Online: true, State: map[string]any{"saving_session": false}}
	car := model.Device{ID: "ohme", Kind: model.KindEVCharger, Online: true, Capabilities: []string{model.ActSetChargeMode}}
	got := h.smartSuggestions([]model.Device{energy, car}, nil)
	if len(got) != 1 || got[0].ID != "smart-saving-ohme" {
		t.Fatalf("want the saving session suggestion: %+v", got)
	}
	steps := got[0].Automation.Steps
	if len(steps) != 2 || steps[0].Params["mode"] != "paused" || steps[1].DelaySeconds != 3600 || steps[1].Params["mode"] != "smart_charge" {
		t.Fatalf("steps: %+v", steps)
	}
	if err := validate(got[0].Automation); err != nil {
		t.Fatal(err)
	}
}
