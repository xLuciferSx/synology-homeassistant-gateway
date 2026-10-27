package main

import (
	"testing"
	"time"

	"hearth/internal/model"
)

func TestCarRoutineLearnsWhenTheCarLeavesAndComesBack(t *testing.T) {
	monday := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	now := monday
	h := &habits{auto: map[string]time.Time{}, now: func() time.Time { return now }}
	charger := func(status string) model.Device {
		return model.Device{ID: "ohme", Kind: model.KindEVCharger, Online: true, State: map[string]any{"status": status}}
	}
	for i, leave := range []int{735, 742, 728, 750} { // Mon–Thu
		day := monday.AddDate(0, 0, i)
		now = time.Date(day.Year(), day.Month(), day.Day(), leave/100, leave%100, 0, 0, time.Local)
		prev := charger("plugged_in")
		h.deviceChanged(&prev, charger("unplugged"))
		now = time.Date(day.Year(), day.Month(), day.Day(), 18, 20+i*5, 0, 0, time.Local)
		prev = charger("unplugged")
		h.deviceChanged(&prev, charger("charging"))
	}
	got := h.carRoutine("ohme")
	week := got["weekdays"]
	if week.Leaves != "07:40" || week.PlugsIn != "18:30" || week.Seen != 4 {
		t.Fatalf("weekday routine: %+v", got)
	}
	if _, ok := got["weekends"]; ok {
		t.Fatalf("no weekend data, got %+v", got["weekends"])
	}
	if s := h.suggest([]model.Device{charger("charging")}, nil); len(s) != 0 {
		t.Fatalf("car plugging should not become light suggestions: %+v", s)
	}
	// Charging starting and stopping while plugged in is not a plug event.
	before := len(h.events)
	prev := charger("plugged_in")
	h.deviceChanged(&prev, charger("charging"))
	if len(h.events) != before {
		t.Fatal("recorded a charge start as a plug-in")
	}
}
