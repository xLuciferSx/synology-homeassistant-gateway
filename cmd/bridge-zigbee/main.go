// Command bridge-zigbee turns Zigbee2MQTT devices into products on the bus
// and sends product commands back to them.
//
// Zigbee2MQTT can run anywhere: its broker is Z2M_MQTT_URL (e.g. the Home
// Assistant Mosquitto add-on today), defaulting to the homeapp bus. One
// coordinator is shared with every platform through Zigbee2MQTT's MQTT API.
//
// Rooms come from the friendly-name convention "Room/Device name"
// (e.g. "Office/Motion sensor"); otherwise from Home Assistant's area for the
// device (homeapp/hints/zigbee-rooms) while HA still exists.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"hearth/internal/bus"
	"hearth/internal/config"
	"hearth/internal/model"
)

const source = "zigbee"

type bridge struct {
	b     *bus.Bus // homeapp bus
	z     *bus.Bus // Zigbee2MQTT's broker
	base  string   // Zigbee2MQTT base topic
	mu    sync.Mutex
	byID  map[string]*product
	byTop map[string]*product // friendly_name → product
	// availability/state that arrived before the device list
	pendingState map[string]map[string]any
	pendingAvail map[string]bool
	haRooms      map[string]string // ieee → room id from Home Assistant
	// last reading per friendly name, saved so a restart doesn't blank battery sensors that only
	// report every so often (Zigbee2MQTT doesn't retain their state)
	cachePath  string
	cacheDirty bool
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	br := &bridge{
		base:         strings.TrimSuffix(config.Get("Z2M_BASE_TOPIC", "zigbee2mqtt"), "/"),
		byID:         map[string]*product{},
		byTop:        map[string]*product{},
		pendingState: map[string]map[string]any{},
		pendingAvail: map[string]bool{},
		haRooms:      map[string]string{},
		cachePath:    filepath.Join(config.Get("DATA_DIR", "/data"), "zigbee-state.json"),
	}
	br.loadCache()
	go func() {
		for range time.Tick(30 * time.Second) {
			br.saveCache()
		}
	}()
	bc := config.LoadBus()
	var err error
	br.b, err = bus.Connect(bc.URL, "homeapp-bridge-zigbee", bc.User, bc.Pass, func(b *bus.Bus) {
		if err := b.Subscribe(bus.TopicZigbeeRooms, br.onRoomHints); err != nil {
			slog.Error("subscribe room hints", "err", err)
		}
		if err := b.OnCommand("zb-", br.onCommand); err != nil {
			slog.Error("subscribe commands", "err", err)
		}
	})
	if err != nil {
		slog.Error("bus", "err", err)
		os.Exit(1)
	}
	zURL := config.Get("Z2M_MQTT_URL", bc.URL)
	zUser := config.Get("Z2M_MQTT_USER", bc.User)
	if zURL != bc.URL && zUser == "" {
		// Waiting for setup is not an error: idle instead of crash-looping.
		slog.Warn("Zigbee is waiting for setup: set Z2M_MQTT_USER and Z2M_MQTT_PASS in .env (an MQTT login for Zigbee2MQTT's broker)", "broker", zURL)
		select {}
	}
	for wait := 10 * time.Second; ; wait = min(wait*2, 5*time.Minute) {
		br.z, err = bus.Connect(zURL, "homeapp-bridge-zigbee-z2m", zUser, config.Get("Z2M_MQTT_PASS", bc.Pass), func(z *bus.Bus) {
			if err := z.Subscribe(br.base+"/#", br.onMessage); err != nil {
				slog.Error("subscribe zigbee2mqtt", "err", err)
			}
		})
		if err == nil {
			break
		}
		slog.Error("zigbee2mqtt broker unreachable, retrying", "err", err, "retry_in", wait)
		time.Sleep(wait)
	}
	slog.Info("zigbee bridge running", "broker", zURL, "base_topic", br.base)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}

func (br *bridge) onMessage(topic string, payload []byte) {
	rest := strings.TrimPrefix(topic, br.base+"/")
	switch {
	case rest == "bridge/devices":
		var devs []z2mDevice
		if err := json.Unmarshal(payload, &devs); err != nil {
			slog.Warn("bad device list", "err", err)
			return
		}
		br.setDevices(devs)
	case strings.HasPrefix(rest, "bridge/"):
	case strings.HasSuffix(rest, "/availability"):
		br.setAvailability(strings.TrimSuffix(rest, "/availability"), payload)
	case strings.HasSuffix(rest, "/set"), strings.HasSuffix(rest, "/get"):
	default:
		var st map[string]any
		if json.Unmarshal(payload, &st) == nil {
			br.setState(rest, st)
		}
	}
}

func (br *bridge) setDevices(devs []z2mDevice) {
	br.mu.Lock()
	defer br.mu.Unlock()
	seen := map[string]bool{}
	roomNames := map[string]string{}
	for _, d := range devs {
		if !include(d) {
			continue
		}
		p := newProduct(d)
		seen[p.dev.ID] = true
		if old, ok := br.byID[p.dev.ID]; ok {
			p.raw, p.available = old.raw, old.available // keep last known state across renames
			delete(br.byTop, old.name)
		}
		if st, ok := br.pendingState[p.name]; ok {
			p.raw = st
			delete(br.pendingState, p.name)
		}
		if a, ok := br.pendingAvail[p.name]; ok {
			p.available = &a
			delete(br.pendingAvail, p.name)
		}
		br.placeInRoom(p)
		p.refresh()
		br.byID[p.dev.ID], br.byTop[p.name] = p, p
		if p.nameRoom != "" {
			roomNames[p.nameRoom], _ = splitName(d.FriendlyName)
		}
		br.publish(p)
	}
	for id, p := range br.byID {
		if !seen[id] {
			delete(br.byID, id)
			delete(br.byTop, p.name)
			_ = br.b.RemoveDevice(id)
		}
	}
	rooms := make([]model.Room, 0, len(roomNames))
	for id, name := range roomNames {
		rooms = append(rooms, model.Room{ID: id, Name: name})
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].Name < rooms[j].Name })
	if err := br.b.PublishRooms(source, rooms); err != nil {
		slog.Warn("publish rooms", "err", err)
	}
	slog.Info("zigbee devices synced", "devices", len(br.byID), "rooms", len(rooms))
}

func (br *bridge) setState(name string, st map[string]any) {
	br.mu.Lock()
	defer br.mu.Unlock()
	p, ok := br.byTop[name]
	if !ok {
		br.pendingState[name] = st
		return
	}
	for k, v := range st { // devices may send partial updates
		p.raw[k] = v
	}
	br.cacheDirty = true
	if a, _ := st["action"].(string); a != "" && p.dev.Kind == model.KindButton {
		p.raw["last_pressed"] = time.Now().UTC().Format(time.RFC3339)
		p.raw["last_press"] = a
		p.refresh()
		br.publish(p)
		_ = br.b.PublishEvent("button", map[string]any{"device_id": p.dev.ID, "press": a})
		// A press is momentary: clear it so the same press again counts as a new change.
		p.raw["action"] = ""
		go func(id string) {
			time.Sleep(time.Second)
			br.mu.Lock()
			defer br.mu.Unlock()
			if q, ok := br.byID[id]; ok {
				q.refresh()
				br.publish(q)
			}
		}(p.dev.ID)
		return
	}
	p.refresh()
	br.publish(p)
}

func (br *bridge) setAvailability(name string, payload []byte) {
	// {"state":"online"} (current) or plain "online" (legacy)
	var obj struct{ State string }
	s := strings.Trim(string(payload), `"`)
	if json.Unmarshal(payload, &obj) == nil && obj.State != "" {
		s = obj.State
	}
	online := s == "online"
	br.mu.Lock()
	defer br.mu.Unlock()
	p, ok := br.byTop[name]
	if !ok {
		br.pendingAvail[name] = online
		return
	}
	p.available = &online
	p.refresh()
	br.publish(p)
}

func (br *bridge) publish(p *product) {
	if err := br.b.PublishDevice(p.dev); err != nil {
		slog.Warn("publish device", "device", p.dev.Name, "err", err)
	}
}

func (br *bridge) onCommand(cmd model.Command) {
	br.mu.Lock()
	p, ok := br.byID[cmd.DeviceID]
	var payload map[string]any
	var err error
	if ok {
		if !slices.Contains(p.dev.Capabilities, cmd.Action) {
			err = fmt.Errorf("%s not supported by %s", cmd.Action, p.dev.Name)
		} else {
			payload, err = p.setPayload(cmd)
		}
	}
	br.mu.Unlock()
	if !ok {
		return
	}
	if err == nil {
		err = br.z.PublishRaw(br.base+"/"+p.name+"/set", payload)
	}
	if err != nil {
		slog.Warn("command failed", "device", p.dev.Name, "action", cmd.Action, "err", err)
		_ = br.b.PublishEvent("command_failed", map[string]any{"device_id": cmd.DeviceID, "action": cmd.Action, "error": err.Error()})
		return
	}
	slog.Info("command", "device", p.dev.Name, "action", cmd.Action)
}

// placeInRoom uses the "Room/Device" name, else Home Assistant's area.
func (br *bridge) placeInRoom(p *product) {
	p.dev.RoomID = p.nameRoom
	if p.dev.RoomID == "" {
		p.dev.RoomID = br.haRooms[p.ieee]
	}
}

func (br *bridge) onRoomHints(_ string, payload []byte) {
	hints := map[string]string{}
	if len(payload) > 0 && json.Unmarshal(payload, &hints) != nil {
		return
	}
	br.mu.Lock()
	defer br.mu.Unlock()
	br.haRooms = hints
	for _, p := range br.byID {
		before := p.dev.RoomID
		if br.placeInRoom(p); p.dev.RoomID != before {
			br.publish(p)
		}
	}
}

func (br *bridge) loadCache() {
	b, err := os.ReadFile(br.cachePath)
	if err != nil {
		return
	}
	var cached map[string]map[string]any
	if json.Unmarshal(b, &cached) != nil {
		return
	}
	for name, st := range cached {
		delete(st, "action") // a button press is momentary, never a state to restore
		br.pendingState[name] = st
	}
	slog.Info("restored last zigbee readings", "devices", len(cached))
}

func (br *bridge) saveCache() {
	br.mu.Lock()
	if !br.cacheDirty {
		br.mu.Unlock()
		return
	}
	out := map[string]map[string]any{}
	for name, p := range br.byTop {
		if len(p.raw) > 0 {
			out[name] = p.raw
		}
	}
	b, err := json.Marshal(out)
	br.cacheDirty = false
	br.mu.Unlock()
	if err == nil {
		_ = os.MkdirAll(filepath.Dir(br.cachePath), 0o700)
		_ = os.WriteFile(br.cachePath, b, 0o600)
	}
}
