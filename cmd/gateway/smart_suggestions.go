package main

import (
	"fmt"

	"hearth/internal/model"
)

// smartSuggestions offers automations that come from what the home already knows, not just
// from repeated taps: getting the car ready for when it usually leaves, and tidying up when the
// last person goes out.
func (h *habits) smartSuggestions(devices []model.Device, existing []Automation) []Suggestion {
	var out []Suggestion
	automates := func(deviceID string) bool {
		for _, a := range existing {
			for _, s := range a.Steps {
				if s.DeviceID == deviceID {
					return true
				}
			}
			for _, t := range a.Triggers {
				if t.DeviceID == deviceID {
					return true
				}
			}
		}
		return false
	}
	can := func(d model.Device, action string) bool {
		for _, c := range d.Capabilities {
			if c == action {
				return true
			}
		}
		return false
	}

	for _, car := range devices {
		if car.Kind != model.KindEVCharger || car.Hidden || !can(car, model.ActSetChargeMode) || automates(car.ID) {
			continue
		}
		week := h.carRoutine(car.ID)["weekdays"]
		leaves, ok := minuteOf(week.Leaves)
		if !ok {
			continue
		}
		ready := (leaves - 15 + 1440) % 1440 / 5 * 5
		readyAt := fmt.Sprintf("%02d:%02d", ready/60, ready%60)
		steps := []Step{{DeviceID: car.ID, Action: model.ActSetChargeMode, Params: map[string]any{"mode": "smart_charge"}}}
		if can(car, model.ActSetChargeTo) {
			steps = append(steps, Step{DeviceID: car.ID, Action: model.ActSetChargeTo, Params: map[string]any{"value": 80.0}})
		}
		if can(car, model.ActSetReadyBy) {
			steps = append(steps, Step{DeviceID: car.ID, Action: model.ActSetReadyBy, Params: map[string]any{"value": readyAt}})
		}
		out = append(out, Suggestion{
			ID: "smart-car-" + car.ID, DeviceID: car.ID, Name: car.Name, On: true, At: readyAt, Seen: week.Seen + 10,
			Title:  "Have the car ready by " + readyAt,
			Detail: fmt.Sprintf("You usually leave around %s. When it's plugged in, charge in the cheapest hours to 80%% by %s.", week.Leaves, readyAt),
			Symbol: "bolt.car.fill",
			Automation: Automation{Name: "Car ready by " + readyAt, Icon: "bolt.car.fill", Enabled: true,
				Triggers: []Trigger{{Type: "device", DeviceID: car.ID, Key: "plugged", Op: "eq", Value: true}},
				Steps:    steps},
		})
	}

	for _, energy := range devices {
		if energy.Kind != model.KindEnergy {
			continue
		}
		if _, ok := energy.State["saving_session"]; !ok || automates(energy.ID) {
			continue
		}
		for _, car := range devices {
			if car.Kind != model.KindEVCharger || car.Hidden || !can(car, model.ActSetChargeMode) {
				continue
			}
			out = append(out, Suggestion{
				ID: "smart-saving-" + car.ID, DeviceID: energy.ID, Name: "Saving sessions", Seen: 15,
				Title:  "Pause the car during saving sessions",
				Detail: "Octopus pays you to use less during a saving session. Pause charging, then go back to Smart an hour later.",
				Symbol: "leaf.fill",
				Automation: Automation{Name: "Saving session: pause the car", Icon: "leaf.fill", Enabled: true,
					Triggers: []Trigger{{Type: "device", DeviceID: energy.ID, Key: "saving_session", Op: "eq", Value: true}},
					Steps: []Step{
						{DeviceID: car.ID, Action: model.ActSetChargeMode, Params: map[string]any{"mode": "paused"}},
						{DeviceID: car.ID, Action: model.ActSetChargeMode, Params: map[string]any{"mode": "smart_charge"}, DelaySeconds: 3600},
					}},
			})
			break
		}
	}

	for _, people := range devices {
		if people.Kind != model.KindPresence || automates(people.ID) {
			continue
		}
		if _, ok := people.State["anyone_home"]; !ok {
			continue
		}
		var steps []Step
		for _, d := range devices {
			if d.Hidden || !d.Online {
				continue
			}
			switch {
			case d.Kind == model.KindLight && can(d, model.ActTurnOff):
				steps = append(steps, Step{DeviceID: d.ID, Action: model.ActTurnOff})
			case d.Kind == model.KindLock && can(d, model.ActLock):
				steps = append(steps, Step{DeviceID: d.ID, Action: model.ActLock})
			}
		}
		if len(steps) < 2 {
			continue
		}
		out = append(out, Suggestion{
			ID: "smart-away-" + people.ID, DeviceID: people.ID, Name: "Everyone out", Seen: 20,
			Title:  "When the last person leaves",
			Detail: "Turn off the lights and lock the doors once nobody's home.",
			Symbol: "figure.walk.departure",
			Automation: Automation{Name: "Everyone's out", Icon: "figure.walk.departure", Enabled: true,
				Triggers: []Trigger{{Type: "device", DeviceID: people.ID, Key: "anyone_home", Op: "eq", Value: false}},
				Steps:    steps},
		})
	}
	return out
}

func minuteOf(hhmm string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(hhmm, "%d:%d", &h, &m); err != nil {
		return 0, false
	}
	return h*60 + m, true
}
