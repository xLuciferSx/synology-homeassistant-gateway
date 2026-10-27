package main

import (
	"testing"
	"time"

	"hearth/internal/model"
)

func TestBriefUnavailableKeepsDeviceOnline(t *testing.T) {
	m := &mapper{states: map[string]haState{"sensor.p_status": {EntityID: "sensor.p_status", State: "idle"}}, products: map[string]*product{}}
	p := &product{dev: model.Device{ID: "p", Kind: model.KindPrinter3D}, refs: map[string]string{"primary": "sensor.p_status", "print_status": "sensor.p_status"}, all: []string{"sensor.p_status"}}
	m.products["p"] = p
	m.refresh(p)
	if !p.dev.Online || p.dev.State["status"] != "idle" {
		t.Fatalf("not online: %+v", p.dev)
	}
	m.states["sensor.p_status"] = haState{EntityID: "sensor.p_status", State: "unavailable"}
	m.refresh(p)
	if !p.dev.Online || p.dev.State["status"] != "idle" || len(m.recheckLost()) != 0 {
		t.Fatalf("blip took it offline: %+v", p.dev)
	}
	p.lostAt = time.Now().Add(-offlineGrace)
	if len(m.recheckLost()) != 1 || p.dev.Online {
		t.Fatalf("still online after the grace: %+v", p.dev)
	}
	m.states["sensor.p_status"] = haState{EntityID: "sensor.p_status", State: "idle"}
	m.refresh(p)
	if !p.dev.Online || !p.lostAt.IsZero() {
		t.Fatalf("did not come back: %+v", p.dev)
	}
}
