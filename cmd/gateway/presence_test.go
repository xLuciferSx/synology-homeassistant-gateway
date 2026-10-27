package main

import (
	"testing"
	"time"

	"hearth/internal/model"
)

func TestPhoneReportOverridesTracker(t *testing.T) {
	p := loadPresence(t.TempDir())
	d := model.Device{Kind: model.KindPresence, State: map[string]any{"home": []any{"Sam"}, "away": []any{"Alex"}}}
	p.report("Alex Morgan", true, time.Now())
	got := p.apply(d)
	if h := got.State["home"].([]string); len(h) != 2 || got.State["everyone_home"] != true {
		t.Fatalf("got %v", got.State)
	}
}
