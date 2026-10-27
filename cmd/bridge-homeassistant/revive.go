package main

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// Home Connect's event stream sometimes goes quiet while the appliance carries on: the dishwasher starts its
// delayed wash, but Home Assistant keeps saying "starts in 2 h 31 min" for hours. Its countdowns tick every
// minute, so one that hasn't moved for a quarter of an hour while the machine is waiting or running means the
// stream is stuck. Reloading the integration reconnects it and brings the real state back.

const (
	countdownStale = 15 * time.Minute
	reviveGap      = 30 * time.Minute
)

// stuckAppliances lists the operation-state entities whose countdown has stopped ticking.
func stuckAppliances(states map[string]haState, now time.Time) []string {
	var out []string
	for id, s := range states {
		base, ok := strings.CutSuffix(id, "_operation_state")
		if !ok || !strings.HasPrefix(id, "sensor.") {
			continue
		}
		countdown := ""
		switch s.State {
		case "delayedstart":
			countdown = base + "_start_in"
		case "run":
			countdown = base + "_remaining_program_time"
		default:
			continue
		}
		c, ok := states[countdown]
		if !ok || c.State == "unavailable" || c.State == "unknown" {
			continue
		}
		if t, ok := c.updatedAt(); ok && now.Sub(t) > countdownStale {
			out = append(out, id)
		}
	}
	return out
}

func (br *bridge) reviveStuck(ctx context.Context, c *haClient) {
	br.mu.Lock()
	var stuck []string
	if br.m != nil {
		stuck = stuckAppliances(br.m.states, time.Now())
	}
	if br.revived == nil {
		br.revived = map[string]time.Time{}
	}
	now := time.Now()
	todo := []string{}
	for _, id := range stuck {
		if now.Sub(br.revived[id]) > reviveGap {
			br.revived[id] = now
			todo = append(todo, id)
		}
	}
	br.mu.Unlock()
	for _, id := range todo {
		slog.Info("appliance updates stalled, reloading its integration", "entity", id)
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := c.callService(cctx, "homeassistant", "reload_config_entry", map[string]any{"entity_id": id}, nil); err != nil {
			slog.Warn("reload integration", "entity", id, "err", err)
		}
		cancel()
	}
}
