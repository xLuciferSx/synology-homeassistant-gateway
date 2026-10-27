package main

import (
	"testing"
	"time"

	"hearth/internal/model"
)

func TestHabitSuggestions(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.Local)
	h := &habits{auto: map[string]time.Time{}, now: func() time.Time { return now }}
	lamp := model.Device{ID: "lamp", Name: "Hall lamp", Kind: model.KindLight, Online: true}
	for i, m := range []int{2301, 2310, 2255, 2318, 2305} {
		day := now.AddDate(0, 0, -i-1)
		h.events = append(h.events, habitEvent{DeviceID: "lamp", On: false,
			At: time.Date(day.Year(), day.Month(), day.Day(), m/100, m%100, 0, 0, time.Local)})
	}
	h.events = append(h.events, habitEvent{DeviceID: "lamp", On: true, At: now.Add(-time.Hour)})

	got := h.suggest([]model.Device{lamp}, nil)
	if len(got) != 1 || got[0].At != "23:05" || got[0].On || got[0].Seen != 5 {
		t.Fatalf("suggestions: %+v", got)
	}
	if err := validate(got[0].Automation); err != nil {
		t.Fatalf("automation invalid: %v", err)
	}

	existing := []Automation{{Enabled: true, Triggers: []Trigger{{Type: "time", At: "23:30"}}, Steps: []Step{{DeviceID: "lamp", Action: model.ActTurnOff}}}}
	if got := h.suggest([]model.Device{lamp}, existing); len(got) != 0 {
		t.Fatalf("already automated: %+v", got)
	}
}

func TestHabitSkipsAutomationChanges(t *testing.T) {
	now := time.Now()
	h := &habits{auto: map[string]time.Time{}, now: func() time.Time { return now }}
	off := model.Device{ID: "lamp", Kind: model.KindLight, Online: true, State: map[string]any{"on": false}}
	on := model.Device{ID: "lamp", Kind: model.KindLight, Online: true, State: map[string]any{"on": true}}
	h.automated("lamp")
	h.deviceChanged(&off, on)
	if len(h.events) != 0 {
		t.Fatal("recorded a change an automation made")
	}
	now = now.Add(5 * time.Minute)
	h.deviceChanged(&on, off)
	if len(h.events) != 1 {
		t.Fatal("missed a manual change")
	}
}
