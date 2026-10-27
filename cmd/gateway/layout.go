package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"

	"hearth/internal/model"
)

// layout is the household's own organisation of the home, made in the app:
// names, which room a device is in, hidden items, custom rooms and order.
// It lives on the hub (DATA_DIR/layout.json), so every family member sees
// the same home, and it is applied on top of what the bridges report.
type layout struct {
	path string
	mu   sync.RWMutex
	data layoutData
}

type layoutData struct {
	Devices    map[string]deviceLayout `json:"devices"`
	Rooms      map[string]roomLayout   `json:"rooms"`
	ExtraRooms []model.Room            `json:"extra_rooms"`
}

type deviceLayout struct {
	Name   string  `json:"name,omitempty"`
	RoomID *string `json:"room_id,omitempty"`
	Hidden bool    `json:"hidden,omitempty"`
	Order  int     `json:"order,omitempty"`
}

type roomLayout struct {
	Name   string `json:"name,omitempty"`
	Icon   string `json:"icon,omitempty"`
	Hidden bool   `json:"hidden,omitempty"`
	Order  int    `json:"order,omitempty"`
}

func loadLayout(dir string) *layout {
	l := &layout{path: filepath.Join(dir, "layout.json"),
		data: layoutData{Devices: map[string]deviceLayout{}, Rooms: map[string]roomLayout{}}}
	if b, err := os.ReadFile(l.path); err == nil {
		_ = json.Unmarshal(b, &l.data)
	}
	if l.data.Devices == nil {
		l.data.Devices = map[string]deviceLayout{}
	}
	if l.data.Rooms == nil {
		l.data.Rooms = map[string]roomLayout{}
	}
	return l
}

func (l *layout) save() error {
	b, err := json.MarshalIndent(l.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

func (l *layout) applyDevice(d model.Device) model.Device {
	l.mu.RLock()
	c, ok := l.data.Devices[d.ID]
	l.mu.RUnlock()
	if !ok {
		return d
	}
	if c.Name != "" && c.Name != d.Name {
		d.OriginalName, d.Name = d.Name, c.Name
	}
	if c.RoomID != nil {
		d.RoomID = *c.RoomID
	}
	d.Hidden, d.Order = c.Hidden, c.Order
	return d
}

func (l *layout) applyRooms(rooms []model.Room) []model.Room {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]model.Room, 0, len(rooms)+len(l.data.ExtraRooms))
	for _, r := range append(append([]model.Room{}, rooms...), l.data.ExtraRooms...) {
		if c, ok := l.data.Rooms[r.ID]; ok {
			if c.Name != "" {
				r.Name = c.Name
			}
			if c.Icon != "" {
				r.Icon = c.Icon
			}
			r.Hidden, r.Order = c.Hidden, c.Order
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteRune('_')
		}
	}
	return b.String()
}

// --- HTTP ---

func (l *layout) routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, st *store) {
	changed := func(w http.ResponseWriter) {
		if err := l.save(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Couldn't save the change."})
			return
		}
		st.broadcastSnapshot()
		w.WriteHeader(http.StatusNoContent)
	}

	// PATCH /api/devices/{id}  {"name"?, "room_id"?, "hidden"?}  (empty name/room = back to default)
	mux.HandleFunc("PATCH /api/devices/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := st.rawDevice(id); !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown device"})
			return
		}
		var req struct {
			Name   *string `json:"name"`
			RoomID *string `json:"room_id"`
			Hidden *bool   `json:"hidden"`
		}
		if !decode(w, r, &req) {
			return
		}
		l.mu.Lock()
		c := l.data.Devices[id]
		if req.Name != nil {
			c.Name = strings.TrimSpace(*req.Name)
			if len(c.Name) > 60 {
				c.Name = c.Name[:60]
			}
		}
		if req.RoomID != nil {
			if *req.RoomID == "" {
				c.RoomID = nil
			} else {
				rid := *req.RoomID
				c.RoomID = &rid
			}
		}
		if req.Hidden != nil {
			c.Hidden = *req.Hidden
		}
		l.data.Devices[id] = c
		l.mu.Unlock()
		changed(w)
	}))

	// PATCH /api/rooms/{id}  {"name"?, "icon"?, "hidden"?}
	mux.HandleFunc("PATCH /api/rooms/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name   *string `json:"name"`
			Icon   *string `json:"icon"`
			Hidden *bool   `json:"hidden"`
		}
		if !decode(w, r, &req) {
			return
		}
		id := r.PathValue("id")
		l.mu.Lock()
		c := l.data.Rooms[id]
		if req.Name != nil {
			c.Name = strings.TrimSpace(*req.Name)
		}
		if req.Icon != nil {
			c.Icon = *req.Icon
		}
		if req.Hidden != nil {
			c.Hidden = *req.Hidden
		}
		l.data.Rooms[id] = c
		l.mu.Unlock()
		changed(w)
	}))

	// POST /api/rooms  {"name"}  → a room of your own (e.g. "Garden")
	mux.HandleFunc("POST /api/rooms", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name, Icon string }
		if !decode(w, r, &req) {
			return
		}
		name := strings.TrimSpace(req.Name)
		id := "custom_" + slugify(name)
		if name == "" || len(name) > 40 || id == "custom_" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Give the room a name (up to 40 characters)."})
			return
		}
		l.mu.Lock()
		for _, x := range l.data.ExtraRooms {
			if x.ID == id {
				l.mu.Unlock()
				writeJSON(w, http.StatusConflict, map[string]string{"error": "A room with that name already exists."})
				return
			}
		}
		room := model.Room{ID: id, Name: name, Icon: req.Icon, Custom: true}
		l.data.ExtraRooms = append(l.data.ExtraRooms, room)
		l.mu.Unlock()
		if err := l.save(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Couldn't save the room."})
			return
		}
		st.broadcastSnapshot()
		writeJSON(w, http.StatusCreated, room)
	}))

	// DELETE /api/rooms/{id}  (rooms made in the app only; their devices go back to their own rooms)
	mux.HandleFunc("DELETE /api/rooms/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		l.mu.Lock()
		kept := l.data.ExtraRooms[:0]
		found := false
		for _, x := range l.data.ExtraRooms {
			if x.ID == id {
				found = true
				continue
			}
			kept = append(kept, x)
		}
		l.data.ExtraRooms = kept
		if found {
			delete(l.data.Rooms, id)
			for did, c := range l.data.Devices {
				if c.RoomID != nil && *c.RoomID == id {
					c.RoomID = nil
					l.data.Devices[did] = c
				}
			}
		}
		l.mu.Unlock()
		if !found {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Only rooms you created can be deleted. Hide other rooms instead."})
			return
		}
		changed(w)
	}))

	// PUT /api/order  {"rooms": [ids in order]} and/or {"devices": [ids in order]}
	mux.HandleFunc("PUT /api/order", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Rooms   []string `json:"rooms"`
			Devices []string `json:"devices"`
		}
		if !decode(w, r, &req) {
			return
		}
		l.mu.Lock()
		for i, id := range req.Rooms {
			c := l.data.Rooms[id]
			c.Order = i + 1
			l.data.Rooms[id] = c
		}
		for i, id := range req.Devices {
			c := l.data.Devices[id]
			c.Order = i + 1
			l.data.Devices[id] = c
		}
		l.mu.Unlock()
		changed(w)
	}))
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return false
	}
	return true
}
