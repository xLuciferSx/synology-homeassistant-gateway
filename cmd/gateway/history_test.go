package main

import (
	"testing"
	"time"

	"hearth/internal/model"
)

func TestHistoryKeepsOnePointPerStepAndAWeek(t *testing.T) {
	h := loadHistory(t.TempDir())
	clock := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return clock }
	for i := 0; i < 10; i++ {
		h.record(model.Device{ID: "s", State: map[string]any{"temperature": 20 + float64(i)}})
		clock = clock.Add(time.Minute)
	}
	got := h.get("s", "temperature", time.Hour)
	if len(got) != 2 || got[len(got)-1].V != 29 {
		t.Fatalf("got %v", got)
	}
	clock = clock.Add(8 * 24 * time.Hour)
	h.record(model.Device{ID: "s", State: map[string]any{"temperature": 18.0}})
	if all := h.get("s", "temperature", 30*24*time.Hour); len(all) != 1 {
		t.Fatalf("old points kept: %v", all)
	}
}
