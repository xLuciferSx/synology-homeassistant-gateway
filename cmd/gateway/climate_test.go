package main

import (
	"time"

	"testing"

	"hearth/internal/model"
)

func bedroom(aqara []float64, dreoCurrent, dreoTarget float64) []model.Device {
	var list []model.Device
	for i, t := range aqara {
		list = append(list, model.Device{ID: "aqara" + string(rune('a'+i)), Kind: model.KindSensor, RoomID: "bed", Online: true,
			State: map[string]any{"temperature": t}})
	}
	return append(list, model.Device{ID: "dreo", Kind: model.KindHeater, RoomID: "bed", Online: true,
		State: map[string]any{"current": dreoCurrent, "target": dreoTarget, "mode": "heat", "min": 5.0, "max": 30.0, "step": 1.0}})
}

func TestRoomAverageWeighsTheHeatersOwnReadingHalf(t *testing.T) {
	avg, n, ok := roomAverage(bedroom([]float64{19, 20, 21}, 24, 21), "bed")
	if !ok || n != 3 || avg != (19+20+21+24*0.5)/3.5 {
		t.Fatalf("average %v from %d sensors", avg, n)
	}
}

func TestTheHeaterIsSetAboveTargetUntilTheRoomCatchesUp(t *testing.T) {
	zone := ClimateZone{RoomID: "bed", Target: 21, Enabled: true}
	devices := bedroom([]float64{19, 19.5, 19}, 22, 21)
	avg, _, _ := roomAverage(devices, "bed")
	offset, set := decide(zone, avg, devices[3])
	if set <= 21 || offset <= 0 {
		t.Fatalf("cold room should push the setpoint up: set %v offset %v (avg %v)", set, offset, avg)
	}
	if set != float64(int(set)) {
		t.Fatalf("setpoint should follow the heater's 1° step: %v", set)
	}

	warm := bedroom([]float64{21.8, 21.7, 21.9}, 24, 25)
	avg, _, _ = roomAverage(warm, "bed")
	if _, set := decide(ClimateZone{Target: 21, Offset: 3}, avg, warm[3]); set != 5 {
		t.Fatalf("a room well over target should let the heater idle, got %v", set)
	}

	// Offset is bounded however long the room stays cold.
	z := ClimateZone{Target: 21}
	for i := 0; i < 200; i++ {
		z.Offset, _ = decide(z, 15, devices[3])
	}
	if z.Offset != climateMaxOffset {
		t.Fatalf("offset should cap at %v, got %v", climateMaxOffset, z.Offset)
	}
}

func TestClimateTickSendsOnlyWhenTheSetpointChanges(t *testing.T) {
	devices := bedroom([]float64{19, 19, 19}, 21, 21)
	var sent []model.Command
	c := loadClimate(t.TempDir(), func(cmd model.Command) error { sent = append(sent, cmd); return nil }, func() []model.Device { return devices })
	c.zones["bed"] = &ClimateZone{RoomID: "bed", Target: 21, Enabled: true}
	c.tick()
	if len(sent) != 1 || sent[0].DeviceID != "dreo" || sent[0].Action != model.ActSetTemperature {
		t.Fatalf("sent %+v", sent)
	}
	devices[3].State["target"] = sent[0].Params["value"]
	c.zones["bed"].Offset -= climateLearn * 2 // same error next tick moves the offset back to where it was
	c.tick()
	if len(sent) != 1 {
		t.Fatalf("no change, nothing to send: %+v", sent)
	}
	c.zones["bed"].Enabled = false
	devices[0].State["temperature"] = 10.0
	c.tick()
	if len(sent) != 1 {
		t.Fatal("a disabled zone shouldn't touch the heater")
	}
}

func TestAnUnsetRoomStartsFromItsHeatersTarget(t *testing.T) {
	devices := bedroom([]float64{20}, 21, 21.5)
	c := loadClimate(t.TempDir(), func(model.Command) error { return nil }, func() []model.Device { return devices })
	if st := c.status("bed"); st.Target != 21.5 || st.Enabled {
		t.Fatalf("unset room: %+v", st)
	}
	if st := c.status("nowhere"); st.Target != 20 {
		t.Fatalf("a room with no heater should default to 20: %+v", st)
	}
}

func mainBedroom(avg float64) (ClimateZone, []model.Device) {
	dreo := model.Device{ID: "dreo", Kind: model.KindHeater, RoomID: "bed", Online: true,
		State: map[string]any{"current": avg, "mode": "heat", "min": 5.0, "max": 29.0, "step": 1.0}}
	trv := model.Device{ID: "trv", Kind: model.KindThermostat, RoomID: "bed", Online: true,
		State: map[string]any{"current": avg, "mode": "heat", "min": 5.0, "max": 30.0, "step": 0.5}}
	return ClimateZone{RoomID: "bed", Target: 21, Enabled: true}, []model.Device{dreo, trv}
}

func setpoints(plans []heaterPlan) map[string]float64 {
	out := map[string]float64{}
	for _, p := range plans {
		out[p.device.ID] = p.setpoint
	}
	return out
}

func TestTheRadiatorHeatsWhenGasIsCheaper(t *testing.T) {
	zone, heaters := mainBedroom(20.5)
	_, plans, reason := plan(zone, 20.5, heaters, energyNow{elecP: 35.9, gasP: 8.3}, false)
	got := setpoints(plans)
	if got["dreo"] != 5 || got["trv"] <= 21 {
		t.Fatalf("peak electricity: dreo should idle and the radiator heat: %v", got)
	}
	if reason != "Using the radiator: gas is cheaper now" {
		t.Fatalf("reason %q", reason)
	}

	_, plans, reason = plan(zone, 20.5, heaters, energyNow{elecP: 6.6, gasP: 8.3}, false)
	if got := setpoints(plans); got["dreo"] <= 21 {
		t.Fatalf("off-peak electricity is cheaper than gas: the Dreo should heat: %v (%s)", got, reason)
	}

	_, plans, _ = plan(zone, 19.5, heaters, energyNow{elecP: 35.9, gasP: 8.3}, false)
	if got := setpoints(plans); got["dreo"] <= 21 {
		t.Fatalf("1.5° under target the Dreo should boost even at peak: %v", got)
	}
}

func TestAnOpenWindowPausesHeatingAndLearnsNothing(t *testing.T) {
	zone, heaters := mainBedroom(19)
	zone.Offset = 1.5
	offset, plans, reason := plan(zone, 19, heaters, energyNow{elecP: 6.6, gasP: 8.3}, true)
	if got := setpoints(plans); got["dreo"] != 5 || got["trv"] != 5 || offset != 1.5 || reason != "Window open: heating paused" {
		t.Fatalf("open window: %v offset %v %q", got, offset, reason)
	}
}

func TestAwayAndSavingSessionsAimLower(t *testing.T) {
	zone, heaters := mainBedroom(20)
	_, home, _ := plan(zone, 20, heaters[1:], energyNow{}, false)
	_, away, reason := plan(zone, 20, heaters[1:], energyNow{away: true, knownPresence: true}, false)
	if away[0].setpoint >= home[0].setpoint || reason != "Nobody's home: aiming lower" {
		t.Fatalf("away %v vs home %v (%q)", away[0].setpoint, home[0].setpoint, reason)
	}
	_, saving, reason := plan(zone, 20, heaters, energyNow{elecP: 6.6, gasP: 8.3, saving: true}, false)
	if got := setpoints(saving); got["dreo"] != 5 || reason != "Saving session: 1° lower" {
		t.Fatalf("saving session: %v %q", got, reason)
	}
}

func TestOpenSinceDecidesAWindowIsOpen(t *testing.T) {
	now := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	door := func(at time.Time) []model.Device {
		return []model.Device{{ID: "w", Kind: model.KindSensor, RoomID: "bed", State: map[string]any{"open": true, "open_at": at.Format(time.RFC3339)}}}
	}
	if windowOpen(door(now.Add(-time.Minute)), "bed", now) {
		t.Fatal("open for a minute is just someone passing through")
	}
	if !windowOpen(door(now.Add(-5*time.Minute)), "bed", now) {
		t.Fatal("open five minutes should pause heating")
	}
}
