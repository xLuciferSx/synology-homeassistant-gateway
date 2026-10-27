package main

import (
	"testing"
	"time"
)

func TestStuckAppliances(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 16, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }
	states := map[string]haState{
		"sensor.dw_operation_state":           {State: "delayedstart"},
		"sensor.dw_start_in":                  {State: "2.5", LastUpdated: at(3 * time.Hour)},
		"sensor.wm_operation_state":           {State: "run"},
		"sensor.wm_remaining_program_time":    {State: "1.0", LastUpdated: at(time.Minute)},
		"sensor.dryer_operation_state":        {State: "ready"},
		"sensor.dryer_remaining_program_time": {State: "0", LastUpdated: at(5 * time.Hour)},
	}
	got := stuckAppliances(states, now)
	if len(got) != 1 || got[0] != "sensor.dw_operation_state" {
		t.Fatalf("stuck = %v", got)
	}
	if end := countdownEnd(haState{LastUpdated: "2026-10-26T20:59:30.062222+00:00"}, 2.5); end != "2026-10-26T23:29:30Z" {
		t.Fatalf("countdownEnd = %s", end)
	}
}
