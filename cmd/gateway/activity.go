package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"hearth/internal/model"
)

// Activity is the home's history: doors, locks, motion, people coming and
// going, who controlled what, automations — and alerts the hub raises itself
// (door left open, unlocked at night, low battery, leak, washer done, car
// charged, room too cold or hot). Kept on the hub so it works with every phone off.

type Entry struct {
	ID       int64     `json:"id"`
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`  // device, command, automation, alert
	Level    string    `json:"level"` // info, warning, alert
	DeviceID string    `json:"device_id,omitempty"`
	Room     string    `json:"room,omitempty"`
	Icon     string    `json:"icon,omitempty"` // SF Symbol hint
	Text     string    `json:"text"`
	Who      string    `json:"who,omitempty"`
}

const maxEntries = 3000

// offlineGrace: Wi-Fi devices that drop off for a moment aren't worth mentioning.
const offlineGrace = 5 * time.Minute

type activity struct {
	path  string
	mu    sync.Mutex
	list  []Entry
	next  int64
	dirty bool
	emit  func(Entry) // to the phones
	now   func() time.Time

	// alert state
	openSince  map[string]time.Time // door/window open since
	offSince   map[string]time.Time // device offline since, until reported
	offTold    map[string]bool      // offline was reported, so "back online" is worth saying
	alerted    map[string]time.Time // alert key → last raised
	lastMotion map[string]time.Time // motion entries throttled per device
	roomName   func(id string) string
	devices    func() []model.Device
}

func loadActivity(dir string) *activity {
	a := &activity{path: filepath.Join(dir, "activity.json"), emit: func(Entry) {}, now: time.Now,
		openSince: map[string]time.Time{}, offSince: map[string]time.Time{}, offTold: map[string]bool{}, alerted: map[string]time.Time{}, lastMotion: map[string]time.Time{},
		roomName: func(string) string { return "" }, devices: func() []model.Device { return nil }}
	if b, err := os.ReadFile(a.path); err == nil {
		_ = json.Unmarshal(b, &a.list)
	}
	for _, e := range a.list {
		if e.ID >= a.next {
			a.next = e.ID + 1
		}
	}
	return a
}

func (a *activity) add(e Entry) {
	a.mu.Lock()
	e.ID = a.next
	a.next++
	if e.Time.IsZero() {
		e.Time = a.now()
	}
	if e.Level == "" {
		e.Level = "info"
	}
	a.list = append(a.list, e)
	if len(a.list) > maxEntries {
		a.list = append([]Entry{}, a.list[len(a.list)-maxEntries:]...)
	}
	a.dirty = true
	a.mu.Unlock()
	a.emit(e)
}

// flush writes the history to disk (called every 30 s and on shutdown).
func (a *activity) flush() {
	a.mu.Lock()
	if !a.dirty {
		a.mu.Unlock()
		return
	}
	b, err := json.Marshal(a.list)
	a.dirty = false
	a.mu.Unlock()
	if err == nil {
		tmp := a.path + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, a.path)
		}
	}
}

// alertOnce raises an alert at most once per `every` for the same key.
func (a *activity) alertOnce(key string, every time.Duration, e Entry) {
	a.mu.Lock()
	last, seen := a.alerted[key]
	if seen && a.now().Sub(last) < every {
		a.mu.Unlock()
		return
	}
	a.alerted[key] = a.now()
	a.mu.Unlock()
	e.Kind, e.Level = "alert", "alert"
	a.add(e)
}

func boolOf(v any) (bool, bool)   { x, ok := v.(bool); return x, ok }
func numOf(v any) (float64, bool) { x, ok := v.(float64); return x, ok }
func strOf(v any) string          { x, _ := v.(string); return x }

// deviceChanged records interesting changes and raises immediate alerts.
func (a *activity) deviceChanged(prev *model.Device, cur model.Device) {
	if prev == nil {
		return
	}
	room := a.roomName(cur.RoomID)
	add := func(icon, text string) {
		a.add(Entry{Kind: "device", DeviceID: cur.ID, Room: room, Icon: icon, Text: text})
	}
	was, now := prev.State, cur.State

	if prev.Online && !cur.Online {
		a.mu.Lock()
		a.offSince[cur.ID] = a.now()
		a.mu.Unlock()
	} else if !prev.Online && cur.Online {
		a.mu.Lock()
		told := a.offTold[cur.ID]
		delete(a.offSince, cur.ID)
		delete(a.offTold, cur.ID)
		a.mu.Unlock()
		if told {
			add("wifi", cur.Name+" is back online")
		}
	}

	switch cur.Kind {
	case model.KindLock:
		if l, ok := boolOf(now["locked"]); ok {
			if pl, _ := boolOf(was["locked"]); pl != l {
				if l {
					add("lock.fill", cur.Name+" locked")
				} else if a.alarmArmed() {
					a.add(Entry{Kind: "device", Level: "alert", DeviceID: cur.ID, Room: room, Icon: "lock.open.fill", Text: cur.Name + " unlocked while the alarm is armed"})
				} else {
					a.add(Entry{Kind: "device", Level: "warning", DeviceID: cur.ID, Room: room, Icon: "lock.open.fill", Text: cur.Name + " unlocked"})
				}
			}
		}
	case model.KindAlarm:
		if st, pst := strOf(now["status"]), strOf(was["status"]); st != pst && st != "" {
			level, text := "info", "Alarm "+strings.ReplaceAll(st, "_", " ")
			switch st {
			case "triggered":
				level, text = "alert", "Alarm triggered"
			case "pending":
				level, text = "alert", "Alarm entry delay · disarm now"
			}
			a.add(Entry{Kind: "device", Level: level, DeviceID: cur.ID, Icon: "shield.lefthalf.filled", Text: text})
		}
	case model.KindWasher:
		if strOf(was["status"]) == "run" && strOf(now["status"]) != "run" {
			a.alertOnce("washer:"+cur.ID, 30*time.Minute, Entry{DeviceID: cur.ID, Room: room, Icon: "washer.fill", Text: cur.Name + " has finished"})
		}
	case model.KindDishwasher:
		if strOf(now["status"]) == "finished" && strOf(was["status"]) != "finished" && strOf(was["status"]) != "" {
			a.alertOnce("dishes:"+cur.ID, 30*time.Minute, Entry{DeviceID: cur.ID, Room: room, Icon: "dishwasher.fill", Text: "The dishes are clean"})
		}
	case model.KindPrinter3D:
		if strOf(was["status"]) == "printing" && strOf(now["status"]) != "printing" {
			pct, _ := numOf(was["progress"])
			text := cur.Name + " stopped printing"
			if pct >= 95 || strOf(now["status"]) == "complete" {
				text = cur.Name + " finished the print"
			}
			a.alertOnce("print:"+cur.ID, 10*time.Minute, Entry{DeviceID: cur.ID, Room: room, Icon: "printer.fill", Text: text})
		}
	case model.KindEVCharger:
		ps, ns := strOf(was["status"]), strOf(now["status"])
		if ps != ns && ns == "charging" {
			add("bolt.car.fill", "Car started charging")
		}
		if ps == "charging" && ns != "charging" {
			bat, _ := numOf(now["car_battery"])
			a.alertOnce("ev:"+cur.ID, 30*time.Minute, Entry{DeviceID: cur.ID, Icon: "bolt.car.fill",
				Text: fmt.Sprintf("Car finished charging (%.0f%%)", bat)})
		}
	case model.KindVacuum:
		ps, ns := strOf(was["status"]), strOf(now["status"])
		if ps != ns && ns == "cleaning" {
			add("robotic.vacuum.fill", cur.Name+" started cleaning")
		} else if ps == "cleaning" && (ns == "docked" || ns == "returning") {
			add("robotic.vacuum.fill", cur.Name+" finished cleaning")
		} else if ns == "error" && ps != "error" {
			a.alertOnce("vac:"+cur.ID, time.Hour, Entry{DeviceID: cur.ID, Room: room, Icon: "exclamationmark.triangle.fill", Text: cur.Name + " needs help"})
		}
	case model.KindPresence:
		ph, nh := toSet(was["home"]), toSet(now["home"])
		for n := range nh {
			if !ph[n] {
				add("house.fill", n+" arrived home")
			}
		}
		for n := range ph {
			if !nh[n] {
				add("figure.walk", n+" left home")
			}
		}
	}

	// sensors on any device
	if o, ok := boolOf(now["open"]); ok {
		if po, _ := boolOf(was["open"]); po != o {
			a.mu.Lock()
			if o {
				a.openSince[cur.ID] = a.now()
			} else {
				delete(a.openSince, cur.ID)
			}
			a.mu.Unlock()
			if o && a.alarmArmed() {
				a.add(Entry{Kind: "device", Level: "alert", DeviceID: cur.ID, Room: room, Icon: "door.left.hand.open",
					Text: cur.Name + " opened while the alarm is armed"})
			} else if o {
				add("door.left.hand.open", cur.Name+" opened")
			} else {
				add("door.left.hand.closed", cur.Name+" closed")
			}
		}
	}
	if m, _ := boolOf(now["motion"]); m {
		if pm, _ := boolOf(was["motion"]); !pm {
			a.mu.Lock()
			last := a.lastMotion[cur.ID]
			throttle := a.now().Sub(last) < 10*time.Minute
			if !throttle {
				a.lastMotion[cur.ID] = a.now()
			}
			a.mu.Unlock()
			if !throttle {
				where := room
				if where == "" {
					where = cur.Name
				}
				add("figure.walk.motion", "Motion in "+where)
			}
		}
	}
	for _, k := range []string{"leak", "smoke"} {
		if v, _ := boolOf(now[k]); v {
			if pv, _ := boolOf(was[k]); !pv {
				what := map[string]string{"leak": "Water leak", "smoke": "Smoke"}[k]
				a.alertOnce(k+":"+cur.ID, 10*time.Minute, Entry{DeviceID: cur.ID, Room: room, Icon: "exclamationmark.triangle.fill",
					Text: what + " detected by " + cur.Name})
			}
		}
	}
}

func toSet(v any) map[string]bool {
	out := map[string]bool{}
	if l, ok := v.([]any); ok {
		for _, x := range l {
			out[fmt.Sprint(x)] = true
		}
	} else if l, ok := v.([]string); ok {
		for _, x := range l {
			out[x] = true
		}
	}
	return out
}

// check runs every minute for alerts about things that last too long.
func (a *activity) check() {
	now := a.now()
	night := now.Hour() >= 22 || now.Hour() < 6
	for _, d := range a.devices() {
		if d.Hidden {
			continue
		}
		room := a.roomName(d.RoomID)
		a.mu.Lock()
		since, open := a.openSince[d.ID]
		offAt, off := a.offSince[d.ID]
		report := off && !a.offTold[d.ID] && !d.Online && now.Sub(offAt) >= offlineGrace
		if report {
			a.offTold[d.ID] = true
		}
		a.mu.Unlock()
		if report {
			a.add(Entry{Kind: "device", Level: "warning", DeviceID: d.ID, Room: room, Icon: "wifi.slash", Text: d.Name + " went offline"})
		}
		if open && now.Sub(since) > 10*time.Minute {
			a.alertOnce("open:"+d.ID, time.Hour, Entry{DeviceID: d.ID, Room: room, Icon: "door.left.hand.open",
				Text: fmt.Sprintf("%s has been open for %d minutes", d.Name, int(now.Sub(since).Minutes()))})
		}
		if d.Kind == model.KindLock && night {
			if l, ok := boolOf(d.State["locked"]); ok && !l {
				a.alertOnce("unlocked-night:"+d.ID, 2*time.Hour, Entry{DeviceID: d.ID, Room: room, Icon: "lock.open.fill",
					Text: d.Name + " is unlocked at night"})
			}
		}
		if bat, ok := numOf(d.State["battery"]); ok && bat < 15 && d.Online {
			a.alertOnce("battery:"+d.ID, 24*time.Hour, Entry{DeviceID: d.ID, Room: room, Icon: "battery.25percent",
				Text: fmt.Sprintf("%s battery is low (%.0f%%)", d.Name, bat)})
		}
		if d.Kind == model.KindSensor || d.Kind == model.KindThermostat {
			t, ok := numOf(d.State["temperature"])
			if !ok {
				t, ok = numOf(d.State["current"])
			}
			if ok && room != "" && (t < 12 || t > 30) {
				word := "cold"
				if t > 30 {
					word = "hot"
				}
				a.alertOnce("temp:"+room, 6*time.Hour, Entry{DeviceID: d.ID, Room: room, Icon: "thermometer.medium",
					Text: fmt.Sprintf("%s is very %s (%.1f°)", room, word, t)})
			}
		}
	}
}

func (a *activity) routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	// GET /api/activity?limit=100&before=<id>&level=alert
	mux.HandleFunc("GET /api/activity", auth(func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 500 {
			limit = 100
		}
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		level := r.URL.Query().Get("level")
		a.mu.Lock()
		out := []Entry{}
		for i := len(a.list) - 1; i >= 0 && len(out) < limit; i-- {
			e := a.list[i]
			if (before > 0 && e.ID >= before) || (level != "" && e.Level != level) {
				continue
			}
			out = append(out, e)
		}
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, out)
	}))
}

// alarmArmed: some alarm panel in the home is armed, so a door opening is worth waking someone for.
func (a *activity) alarmArmed() bool {
	if a.devices == nil {
		return false
	}
	for _, d := range a.devices() {
		if d.Kind == model.KindAlarm && strings.HasPrefix(strOf(d.State["status"]), "armed_") {
			return true
		}
	}
	return false
}
