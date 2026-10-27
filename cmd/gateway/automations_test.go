package main

import (
	"sync"
	"testing"
	"time"

	"hearth/internal/model"
)

func testAutomations(t *testing.T, list ...Automation) (*automations, func() []model.Command) {
	var mu sync.Mutex
	var sent []model.Command
	a := loadAutomations(t.TempDir(), func(c model.Command) error {
		mu.Lock()
		sent = append(sent, c)
		mu.Unlock()
		return nil
	}, func(string) (model.Device, bool) { return model.Device{}, false })
	a.list = list
	return a, func() []model.Command {
		time.Sleep(50 * time.Millisecond) // steps run on their own goroutine
		mu.Lock()
		defer mu.Unlock()
		return append([]model.Command{}, sent...)
	}
}

func TestADeviceTriggerFiresOnceWhenItBecomesTrue(t *testing.T) {
	hot := Automation{ID: "1", Name: "Cool the office", Enabled: true,
		Triggers: []Trigger{{Type: "device", DeviceID: "sensor", Key: "temperature", Op: "gt", Value: 25.0}},
		Steps:    []Step{{DeviceID: "fan", Action: "turn_on"}}}
	a, sent := testAutomations(t, hot)
	at := func(v float64) model.Device {
		return model.Device{ID: "sensor", State: map[string]any{"temperature": v}}
	}
	prev := at(24)
	for _, v := range []float64{26, 27, 28} { // crosses once, then stays above
		cur := at(v)
		a.deviceChanged(&prev, cur)
		prev = cur
	}
	if got := sent(); len(got) != 1 || got[0].DeviceID != "fan" || got[0].Action != "turn_on" {
		t.Fatalf("want one turn_on, got %v", got)
	}
}

func TestATimeTriggerRunsOncePerMinuteOnItsDays(t *testing.T) {
	night := Automation{ID: "2", Name: "Lights off", Enabled: true,
		Triggers: []Trigger{{Type: "time", At: "23:00", Days: []int{0}}}, // Sundays
		Steps:    []Step{{DeviceID: "lamp", Action: "turn_off"}}}
	a, sent := testAutomations(t, night)
	sunday := time.Date(2026, 10, 25, 23, 0, 5, 0, time.Local)
	a.now = func() time.Time { return sunday }
	a.tick()
	a.tick()
	monday := sunday.AddDate(0, 0, 1)
	a.now = func() time.Time { return monday }
	a.tick()
	if got := sent(); len(got) != 1 {
		t.Fatalf("want one run on Sunday only, got %d", len(got))
	}
}

func TestATimeWindowCanCrossMidnight(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.Local) }
	if !inWindow(at(23, 30), "22:00", "06:30") || !inWindow(at(5, 0), "22:00", "06:30") || inWindow(at(12, 0), "22:00", "06:30") {
		t.Fatal("22:00–06:30 should hold at night and not at noon")
	}
}

func TestAnAutomationWithoutStepsIsRefusedInWords(t *testing.T) {
	err := validate(Automation{Name: "Nothing", Triggers: []Trigger{{Type: "time", At: "07:00"}}})
	if err == nil || err.Error() != "Add at least one thing for it to do." {
		t.Fatalf("got %v", err)
	}
}

func TestSunTimesForHome(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	// 21 June: long day; 21 December: short day (home is ~53.6°N, 2.6°W).
	for _, c := range []struct {
		day              time.Time
		riseFrom, riseTo string
		setFrom, setTo   string
	}{
		{time.Date(2026, 6, 21, 12, 0, 0, 0, london), "04:30", "04:55", "21:30", "21:55"},
		{time.Date(2026, 12, 21, 12, 0, 0, 0, london), "08:15", "08:40", "15:40", "16:05"},
	} {
		rise, set, ok := sunTimes(c.day, 53.56, -2.64)
		r, s := rise.Format("15:04"), set.Format("15:04")
		if !ok || r < c.riseFrom || r > c.riseTo || s < c.setFrom || s > c.setTo {
			t.Errorf("%s: sunrise %s sunset %s", c.day.Format("Jan 2"), r, s)
		}
	}
}

func TestRetriggerRestartsDelayedSteps(t *testing.T) {
	// Motion → light on, off after 1s. Fresh motion before the delay ends restarts it,
	// so the light is switched off once, after the last motion.
	auto := Automation{ID: "m", Name: "Hall light", Enabled: true,
		Triggers: []Trigger{{Type: "device", DeviceID: "pir", Key: "motion", Op: "eq", Value: true}},
		Steps: []Step{{DeviceID: "light", Action: "turn_on"},
			{DeviceID: "light", Action: "turn_off", DelaySeconds: 1}}}
	a, sent := testAutomations(t, auto)
	pir := func(m bool) model.Device { return model.Device{ID: "pir", State: map[string]any{"motion": m}} }
	off, on := pir(false), pir(true)
	a.deviceChanged(&off, on)
	time.Sleep(600 * time.Millisecond)
	a.deviceChanged(&on, off)
	a.deviceChanged(&off, on) // motion again before the light went off
	time.Sleep(1300 * time.Millisecond)
	var offs, ons int
	for _, c := range sent() {
		if c.Action == "turn_off" {
			offs++
		} else {
			ons++
		}
	}
	if ons != 2 || offs != 1 {
		t.Errorf("turn_on %d, turn_off %d; want 2 and 1", ons, offs)
	}
}

func TestASceneNeedsNoTrigger(t *testing.T) {
	scene := Automation{Name: "Movie night", Scene: true, Steps: []Step{{DeviceID: "tv", Action: "turn_on"}}}
	if err := validate(scene); err != nil {
		t.Fatalf("scene refused: %v", err)
	}
	scene.Scene = false
	if validate(scene) == nil {
		t.Fatal("an automation without a trigger should be refused")
	}
}
