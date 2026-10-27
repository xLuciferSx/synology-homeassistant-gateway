package main

import (
	"hearth/internal/model"
	"strings"
	"testing"
)

func TestKeyScript(t *testing.T) {
	s := keyScript([]string{"DPAD_UP", "DPAD_UP"})
	if strings.Count(s, "sendevent $d 1 103 1") != 2 || !strings.Contains(s, "input keyevent KEYCODE_DPAD_UP KEYCODE_DPAD_UP") {
		t.Fatalf("script: %s", s)
	}
	if s := keyScript([]string{"DPAD_UP", "SEARCH"}); s != "input keyevent KEYCODE_DPAD_UP KEYCODE_SEARCH" {
		t.Fatalf("fallback: %s", s)
	}
}

func TestStepVolume(t *testing.T) {
	p := &product{dev: model.Device{ID: "tv-step", Kind: model.KindTV, State: map[string]any{"volume": 0.1}}}
	for _, want := range []float64{0.11, 0.12} {
		if got, ok := stepVolume(p, true); !ok || got != want {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if got, _ := stepVolume(p, false); got != 0.11 {
		t.Fatalf("down: %v", got)
	}
	speaker := &product{dev: model.Device{ID: "sp", Kind: model.KindSpeaker, State: map[string]any{"volume": 0.1}}}
	if _, ok := stepVolume(speaker, true); ok {
		t.Fatal("speakers keep their own step")
	}
}
