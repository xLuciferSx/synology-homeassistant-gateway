package main

import "testing"

func TestOctopusDay(t *testing.T) {
	s := haState{Attributes: map[string]any{"charges": []any{
		map[string]any{"start": "2026-10-24T00:00:00+01:00", "rate": 0.066, "consumption": 1.5},
		map[string]any{"start": "2026-10-24T08:00:00+01:00", "rate": 0.358518, "consumption": 0.5},
		map[string]any{"start": "2026-10-24T09:00:00+01:00", "rate": 0.358518, "consumption": 0.25},
	}}}
	day, peak, off := octopusDay(s)
	if day != "2026-10-24" || peak != 0.75 || off != 1.5 {
		t.Fatalf("got %s %v %v", day, peak, off)
	}
}
