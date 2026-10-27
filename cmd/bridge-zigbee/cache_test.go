package main

import (
	"path/filepath"
	"testing"
)

func TestLastReadingsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zigbee-state.json")
	before := &bridge{byTop: map[string]*product{}, pendingState: map[string]map[string]any{}, cachePath: path, cacheDirty: true}
	before.byTop["Main Bedroom Temperature Sensor"] = &product{name: "Main Bedroom Temperature Sensor",
		raw: map[string]any{"temperature": 20.4, "humidity": 52.0, "action": "single"}}
	before.saveCache()

	after := &bridge{pendingState: map[string]map[string]any{}, cachePath: path}
	after.loadCache()
	st := after.pendingState["Main Bedroom Temperature Sensor"]
	if st["temperature"] != 20.4 || st["humidity"] != 52.0 {
		t.Fatalf("restored %v", st)
	}
	if _, ok := st["action"]; ok {
		t.Fatal("a button press shouldn't be restored")
	}
}
