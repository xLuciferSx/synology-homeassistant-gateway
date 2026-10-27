// Command bridge-homeassistant publishes Home Assistant devices onto the bus
// as products and executes product commands through Home Assistant.
// It is a temporary provider: device types move to native bridges over time.
package main

import (
	_ "time/tzdata"

	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"hearth/internal/bus"
	"hearth/internal/config"
	"hearth/internal/model"
)

const source = "homeassistant"

type bridge struct {
	b         *bus.Bus
	haURL     string
	haToken   string
	mu        sync.Mutex
	client    *haClient
	m         *mapper
	resync    chan struct{}
	commands  chan model.Command
	published map[string]bool      // device ids seen on the bus with our prefix
	revived   map[string]time.Time // appliance → when its integration was last reloaded for stalling
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	handOffZigbee = config.Get("HA_HANDOFF_ZIGBEE", "false") == "true"
	br := &bridge{
		haURL:     config.Must("HA_URL"),
		haToken:   config.Must("HA_TOKEN"),
		resync:    make(chan struct{}, 1),
		commands:  make(chan model.Command, 64),
		published: map[string]bool{},
	}
	bc := config.LoadBus()
	var err error
	br.b, err = bus.Connect(bc.URL, "homeapp-bridge-ha", bc.User, bc.Pass, func(b *bus.Bus) {
		if err := b.OnCommand("ha-", func(c model.Command) { br.commands <- c }); err != nil {
			slog.Error("subscribe commands", "err", err)
		}
		// Learn which devices this bridge published before (retained on the bus),
		// so ones it no longer owns are cleaned up even across restarts.
		if err := b.OnDevice(func(id string, d *model.Device) {
			if !strings.HasPrefix(id, "ha-") {
				return
			}
			br.mu.Lock()
			if d == nil {
				delete(br.published, id)
			} else {
				br.published[id] = true
			}
			br.mu.Unlock()
		}); err != nil {
			slog.Error("subscribe devices", "err", err)
		}
	})
	if err != nil {
		slog.Error("bus", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go br.commandLoop(ctx)

	backoff := time.Second
	for ctx.Err() == nil {
		if err := br.session(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("home assistant session ended", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

// session runs one connection: full sync, then live updates until it drops.
func (br *bridge) session(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	c, err := dialHA(dctx, br.haURL, br.haToken)
	cancel()
	if err != nil {
		return err
	}
	defer c.close()
	slog.Info("connected to home assistant", "url", br.haURL)

	if err := br.fullSync(ctx, c); err != nil {
		return err
	}
	for _, ev := range []string{"state_changed", "device_registry_updated", "entity_registry_updated", "area_registry_updated"} {
		if err := c.call(ctx, map[string]any{"type": "subscribe_events", "event_type": ev}, nil); err != nil {
			return err
		}
	}
	br.mu.Lock()
	br.client = c
	br.mu.Unlock()
	defer func() { br.mu.Lock(); br.client = nil; br.mu.Unlock() }()

	periodic := time.NewTicker(10 * time.Minute) // safety net against missed events
	defer periodic.Stop()
	recheck := time.NewTicker(30 * time.Second)
	defer recheck.Stop()
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-c.events:
			if !ok {
				return c.err
			}
			if ev.EventType == "state_changed" {
				br.onStateChanged(ev.Data)
			} else {
				debounce = time.After(3 * time.Second) // registry changed: rebuild soon
			}
		case <-debounce:
			if err := br.fullSync(ctx, c); err != nil {
				return err
			}
		case <-recheck.C:
			br.publishLost()
		case <-periodic.C:
			if err := br.fullSync(ctx, c); err != nil {
				return err
			}
			go br.reviveStuck(ctx, c)
		}
	}
}

func (br *bridge) fullSync(ctx context.Context, c *haClient) error {
	var (
		areas    []haArea
		devices  []haDevice
		entities []haEntity
		states   []haState
	)
	for _, q := range []struct {
		typ string
		out any
	}{
		{"config/area_registry/list", &areas},
		{"config/device_registry/list", &devices},
		{"config/entity_registry/list", &entities},
		{"get_states", &states},
	} {
		qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err := c.call(qctx, map[string]any{"type": q.typ}, q.out)
		cancel()
		if err != nil {
			return err
		}
	}
	m := build(areas, devices, entities, states)
	m.loadAutomationConfigs(ctx, c)

	br.mu.Lock()
	old := br.m
	br.m = m
	br.mu.Unlock()

	if err := br.b.PublishRooms(source, m.rooms()); err != nil {
		slog.Warn("publish rooms", "err", err)
	}
	if handOffZigbee {
		if err := br.b.PublishRetained(bus.TopicZigbeeRooms, m.zigbee); err != nil {
			slog.Warn("publish zigbee rooms", "err", err)
		}
	}
	for _, p := range m.products {
		_ = br.b.PublishDevice(p.dev)
	}
	for _, id := range m.retired {
		_ = br.b.RemoveDevice(id)
	}
	if old != nil { // remove products that disappeared
		for id := range old.products {
			if _, still := m.products[id]; !still {
				_ = br.b.RemoveDevice(id)
			}
		}
	}
	// …and anything published before a restart that isn't a product any more
	// (handed off to bridge-zigbee, deleted in HA, split into several tiles).
	br.mu.Lock()
	var stale []string
	for id := range br.published {
		if _, ok := m.products[id]; !ok {
			stale = append(stale, id)
		}
	}
	br.mu.Unlock()
	for _, id := range stale {
		_ = br.b.RemoveDevice(id)
	}
	if len(stale) > 0 {
		slog.Info("removed stale devices", "count", len(stale))
	}
	slog.Info("synced", "rooms", len(m.areas), "products", len(m.products))
	return nil
}

func (br *bridge) publishLost() {
	br.mu.Lock()
	var devs []model.Device
	if br.m != nil {
		for _, p := range br.m.recheckLost() {
			devs = append(devs, p.dev)
		}
	}
	br.mu.Unlock()
	for _, d := range devs {
		_ = br.b.PublishDevice(d)
	}
}

func (br *bridge) onStateChanged(data json.RawMessage) {
	var d struct {
		EntityID string   `json:"entity_id"`
		NewState *haState `json:"new_state"`
	}
	if json.Unmarshal(data, &d) != nil {
		return
	}
	br.mu.Lock()
	m := br.m
	var p *product
	if m != nil {
		if d.NewState == nil {
			p = m.apply(haState{EntityID: d.EntityID}, true)
		} else {
			p = m.apply(*d.NewState, false)
		}
	}
	var dev model.Device
	if p != nil {
		dev = p.dev
	}
	br.mu.Unlock()
	if p != nil {
		_ = br.b.PublishDevice(dev)
	}
}

func (br *bridge) commandLoop(ctx context.Context) {
	queues := map[string]chan model.Command{}
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-br.commands:
			q, ok := queues[cmd.DeviceID]
			if !ok {
				q = make(chan model.Command, 64)
				queues[cmd.DeviceID] = q
				go br.deviceWorker(ctx, q)
			}
			select {
			case q <- cmd:
			default:
				slog.Warn("command queue full", "device", cmd.DeviceID, "action", cmd.Action)
			}
		}
	}
}

// coalescing actions only need their latest value: a burst of slider moves becomes one call.
var coalescing = map[string]bool{
	model.ActSetTemperature: true, model.ActSetBrightness: true, model.ActVolumeSet: true, model.ActSetSpeed: true,
}

// deviceWorker runs one device's commands in order, so a slow device never holds up another.
func (br *bridge) deviceWorker(ctx context.Context, q chan model.Command) {
	for {
		var cmd model.Command
		select {
		case <-ctx.Done():
			return
		case cmd = <-q:
		}
		if br.pressKeys(ctx, cmd, q) {
			continue
		}
		if coalescing[cmd.Action] {
		drain:
			for {
				select {
				case next := <-q:
					if next.Action != cmd.Action {
						br.run(ctx, cmd)
					}
					cmd = next
					if !coalescing[cmd.Action] {
						break drain
					}
				default:
					break drain
				}
			}
		}
		br.run(ctx, cmd)
	}
}

// pressKeys sends a remote key and any keys already queued behind it as one batch.
// It reports false when cmd isn't a key for an ADB box.
func (br *bridge) pressKeys(ctx context.Context, cmd model.Command, q chan model.Command) bool {
	br.mu.Lock()
	c, m := br.client, br.m
	var p *product
	if m != nil {
		p = m.products[cmd.DeviceID]
	}
	br.mu.Unlock()
	if c == nil || p == nil {
		return false
	}
	code, ok := adbKeycode(p, cmd)
	if !ok {
		return false
	}
	codes := []string{code}
	var rest *model.Command
collect:
	for len(codes) < 10 {
		select {
		case next := <-q:
			if more, ok := adbKeycode(p, next); ok {
				codes = append(codes, more)
				continue
			}
			rest = &next
			break collect
		default:
			break collect
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	if err := sendKeycodes(cctx, c, p, codes); err != nil {
		slog.Warn("command failed", "device", p.dev.Name, "action", cmd.Action, "err", err)
	}
	cancel()
	if rest != nil {
		br.run(ctx, *rest)
	}
	return true
}

func (br *bridge) run(ctx context.Context, cmd model.Command) {
	br.mu.Lock()
	c, m := br.client, br.m
	var p *product
	if m != nil {
		p = m.products[cmd.DeviceID]
	}
	br.mu.Unlock()
	if c == nil || p == nil {
		slog.Warn("command dropped", "device", cmd.DeviceID, "action", cmd.Action)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err := execute(cctx, c, p, cmd)
	cancel()
	if err != nil {
		slog.Warn("command failed", "device", p.dev.Name, "action", cmd.Action, "err", err)
		_ = br.b.PublishEvent("command_failed", map[string]any{"device_id": cmd.DeviceID, "action": cmd.Action, "error": err.Error()})
	} else {
		slog.Info("command", "device", p.dev.Name, "action", cmd.Action)
	}
}
