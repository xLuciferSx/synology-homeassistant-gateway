package main

import (
	"encoding/json"
	"sort"
	"sync"

	"hearth/internal/model"
)

// store is the gateway's in-memory picture of the home, fed from the bus.
// Live subscribers (app WebSockets) receive every change as a JSON frame.
type store struct {
	mu      sync.RWMutex
	rooms   map[string][]model.Room // per source
	devices map[string]model.Device
	subs    map[chan []byte]struct{}
	layout  *layout
	people  *presence
	raw     map[string]model.Device
	// Told about every device change with the state before it, for automations.
	onChange func(prev *model.Device, cur model.Device)
}

func newStore(l *layout) *store {
	return &store{
		layout:  l,
		rooms:   map[string][]model.Room{},
		devices: map[string]model.Device{},
		subs:    map[chan []byte]struct{}{},
		raw:     map[string]model.Device{},
	}
}

type snapshot struct {
	Type    string         `json:"type"`
	Rooms   []model.Room   `json:"rooms"`
	Devices []model.Device `json:"devices"`
}

func (s *store) snapshot() snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	rooms := []model.Room{}
	for _, list := range s.rooms {
		for _, r := range list {
			if !seen[r.ID] {
				seen[r.ID] = true
				rooms = append(rooms, r)
			}
		}
	}
	rooms = s.layout.applyRooms(rooms)
	devs := make([]model.Device, 0, len(s.devices))
	for _, d := range s.devices {
		devs = append(devs, s.layout.applyDevice(d))
	}
	sort.Slice(devs, func(i, j int) bool {
		if devs[i].Order != devs[j].Order {
			return devs[i].Order < devs[j].Order
		}
		return devs[i].Name < devs[j].Name
	})
	return snapshot{Type: "snapshot", Rooms: rooms, Devices: devs}
}

func (s *store) device(id string) (model.Device, bool) {
	d, ok := s.rawDevice(id)
	if ok {
		d = s.layout.applyDevice(d)
	}
	return d, ok
}

// rawDevice is the device exactly as its bridge reports it.
func (s *store) rawDevice(id string) (model.Device, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.devices[id]
	return d, ok
}

// broadcastSnapshot re-sends the whole home after a layout change.
func (s *store) broadcastSnapshot() { s.broadcast(s.snapshot()) }

func (s *store) setRooms(source string, rooms []model.Room) {
	s.mu.Lock()
	s.rooms[source] = rooms
	s.mu.Unlock()
	s.broadcast(map[string]any{"type": "rooms", "rooms": s.snapshot().Rooms})
}

func (s *store) setDevice(id string, d *model.Device) {
	s.mu.Lock()
	var prev *model.Device
	if old, ok := s.devices[id]; ok {
		prev = &old
	}
	if d == nil {
		delete(s.devices, id)
		delete(s.raw, id)
	} else {
		if d.Kind == model.KindPresence && s.people != nil {
			s.raw[id] = *d
			merged := s.people.apply(*d)
			d = &merged
		}
		s.devices[id] = *d
	}
	s.mu.Unlock()
	if d == nil {
		s.broadcast(map[string]any{"type": "device_removed", "id": id})
	} else {
		s.broadcast(map[string]any{"type": "device", "device": s.layout.applyDevice(*d)})
		if s.onChange != nil {
			s.onChange(prev, *d)
		}
	}
}

func (s *store) subscribe() chan []byte {
	ch := make(chan []byte, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *store) unsubscribe(ch chan []byte) {
	s.mu.Lock()
	delete(s.subs, ch)
	s.mu.Unlock()
}

func (s *store) broadcast(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for ch := range s.subs {
		select {
		case ch <- b:
		default: // slow client: drop; it will resync on reconnect
		}
	}
}

// refreshPresence re-applies the phones' reports to the People devices.
func (s *store) refreshPresence() {
	s.mu.RLock()
	raws := []model.Device{}
	for _, d := range s.raw {
		raws = append(raws, d)
	}
	s.mu.RUnlock()
	for _, d := range raws {
		x := d
		s.setDevice(x.ID, &x)
	}
}
