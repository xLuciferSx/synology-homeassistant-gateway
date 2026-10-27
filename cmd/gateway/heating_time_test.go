package main

import (
	"testing"
	"time"

	"hearth/internal/model"
)

func TestHeatingTimeSplitsAcrossMidnight(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 26, 23, 0, 0, 0, time.Local)
	h := loadHeatingTime(dir)
	h.now = func() time.Time { return now }
	rad := func(action string) model.Device {
		return model.Device{ID: "bath", Kind: model.KindThermostat, Online: true, State: map[string]any{"action": action}}
	}
	h.record(rad("heating"))
	now = now.Add(90 * time.Minute)
	h.record(rad("idle"))
	h.record(model.Device{ID: "lamp", Kind: model.KindLight, State: map[string]any{"action": "heating"}})

	m := loadHeatingTime(dir)
	m.now = func() time.Time { return now }
	got := m.minutes()
	if got["2026-10-26"]["bath"] != 60 || got["2026-10-27"]["bath"] != 30 || len(got["2026-10-27"]) != 1 {
		t.Fatalf("got %v", got)
	}

	m.record(rad("heating"))
	now = now.Add(15 * time.Minute)
	if got := m.minutes()["2026-10-27"]["bath"]; got != 45 {
		t.Fatalf("running spell not counted: %v", got)
	}
}
