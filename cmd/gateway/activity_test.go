package main

import (
	"strings"
	"testing"
	"time"

	"hearth/internal/model"
)

func TestActivityRecordsAndAlerts(t *testing.T) {
	a := loadActivity(t.TempDir())
	var emitted []Entry
	a.emit = func(e Entry) { emitted = append(emitted, e) }
	now := time.Date(2026, 10, 26, 23, 30, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	a.roomName = func(id string) string { return map[string]string{"hall": "Hallway", "utility": "Utility Room"}[id] }

	lock := func(l bool) model.Device {
		return model.Device{ID: "lock", Name: "Front Door", Kind: model.KindLock, RoomID: "hall", Online: true, State: map[string]any{"locked": l}}
	}
	locked, unlocked := lock(true), lock(false)
	a.deviceChanged(&locked, unlocked)

	washer := func(st string) model.Device {
		return model.Device{ID: "w", Name: "Washing Machine", Kind: model.KindWasher, RoomID: "utility", Online: true, State: map[string]any{"status": st}}
	}
	run, idle := washer("run"), washer("idle")
	a.deviceChanged(&run, idle)
	a.deviceChanged(&run, idle) // twice: only one alert

	door := func(o bool) model.Device {
		return model.Device{ID: "d", Name: "Patio door", Kind: model.KindSensor, RoomID: "hall", Online: true, State: map[string]any{"open": o}}
	}
	closed, open := door(false), door(true)
	a.deviceChanged(&closed, open)

	people := func(home ...any) model.Device {
		return model.Device{ID: "p", Kind: model.KindPresence, Online: true, State: map[string]any{"home": home}}
	}
	p1, p2 := people("Sam"), people("Sam", "Alex")
	a.deviceChanged(&p1, p2)

	// later: door still open, lock still unlocked at night, low battery
	now = now.Add(15 * time.Minute)
	bat := model.Device{ID: "s", Name: "Hall sensor", Kind: model.KindSensor, Online: true, State: map[string]any{"battery": 9.0}}
	a.devices = func() []model.Device { return []model.Device{unlocked, open, bat} }
	a.check()
	a.check() // no repeats

	var texts []string
	alerts := 0
	for _, e := range emitted {
		texts = append(texts, e.Text)
		if e.Level == "alert" {
			alerts++
		}
	}
	all := strings.Join(texts, " | ")
	for _, want := range []string{"Front Door unlocked", "Washing Machine has finished", "Patio door opened",
		"Alex arrived home", "Patio door has been open for 15 minutes", "Front Door is unlocked at night", "Hall sensor battery is low (9%)"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in %s", want, all)
		}
	}
	if alerts != 4 {
		t.Errorf("alerts = %d, want 4 (washer, open door, unlocked at night, battery): %s", alerts, all)
	}
}

func TestOfflineBlipsAreQuiet(t *testing.T) {
	now := time.Now()
	a := loadActivity(t.TempDir())
	a.now = func() time.Time { return now }
	on := model.Device{ID: "tv", Name: "TV", Kind: model.KindTV, Online: true, State: map[string]any{}}
	off := on
	off.Online = false
	a.devices = func() []model.Device { return []model.Device{off} }
	a.deviceChanged(&on, off)
	a.check()
	a.deviceChanged(&off, on)
	if len(a.list) != 0 {
		t.Fatalf("logged a short blip: %+v", a.list)
	}
	a.deviceChanged(&on, off)
	now = now.Add(offlineGrace + time.Second)
	a.check()
	a.deviceChanged(&off, on)
	if len(a.list) != 2 || a.list[0].Text != "TV went offline" || a.list[1].Text != "TV is back online" {
		t.Fatalf("entries: %+v", a.list)
	}
}

func TestADoorOpeningWhileArmedIsAnAlert(t *testing.T) {
	a := loadActivity(t.TempDir())
	var emitted []Entry
	a.emit = func(e Entry) { emitted = append(emitted, e) }
	a.roomName = func(string) string { return "Hallway" }
	alarm := model.Device{ID: "ss", Kind: model.KindAlarm, Online: true, State: map[string]any{"status": "armed_away"}}
	a.devices = func() []model.Device { return []model.Device{alarm} }
	door := func(o bool) model.Device {
		return model.Device{ID: "d", Name: "Front Door", Kind: model.KindSensor, RoomID: "hall", Online: true, State: map[string]any{"open": o}}
	}
	closed, open := door(false), door(true)
	a.deviceChanged(&closed, open)
	if len(emitted) == 0 || emitted[len(emitted)-1].Level != "alert" || categoryOf(emitted[len(emitted)-1]) != catSafety {
		t.Fatalf("armed door opening should be a safety alert: %+v", emitted)
	}

	pending := alarm
	pending.State = map[string]any{"status": "pending"}
	a.deviceChanged(&alarm, pending)
	if last := emitted[len(emitted)-1]; last.Level != "alert" || !strings.Contains(last.Text, "disarm now") {
		t.Fatalf("entry delay should alert: %+v", last)
	}

	alarm.State = map[string]any{"status": "disarmed"}
	before := len(emitted)
	a.deviceChanged(&open, closed)
	a.deviceChanged(&closed, open)
	for _, e := range emitted[before:] {
		if e.Level == "alert" {
			t.Fatalf("a disarmed home shouldn't alert on a door: %+v", e)
		}
	}
}
