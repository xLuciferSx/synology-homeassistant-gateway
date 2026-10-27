package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"hearth/internal/model"
)

// Raw Home Assistant registry/state shapes (only the fields we need).
type haArea struct {
	AreaID  string `json:"area_id"`
	Name    string `json:"name"`
	FloorID string `json:"floor_id"`
	Icon    string `json:"icon"`
}

type haDevice struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	NameByUser   string     `json:"name_by_user"`
	Manufacturer string     `json:"manufacturer"`
	Model        string     `json:"model"`
	AreaID       string     `json:"area_id"`
	DisabledBy   string     `json:"disabled_by"`
	Identifiers  [][]any    `json:"identifiers"`
	Connections  [][]string `json:"connections"`
}

// handOffZigbee leaves Zigbee2MQTT devices to bridge-zigbee (which talks to
// Zigbee2MQTT directly) and only shares their HA room assignment.
var handOffZigbee = false

// z2mIEEE returns the IEEE address of a device that came from Zigbee2MQTT.
func z2mIEEE(d haDevice) string {
	for _, id := range d.Identifiers {
		if len(id) == 2 && fmt.Sprint(id[0]) == "mqtt" {
			if v, ok := strings.CutPrefix(fmt.Sprint(id[1]), "zigbee2mqtt_"); ok && strings.HasPrefix(v, "0x") {
				return v
			}
		}
	}
	return ""
}

type haEntity struct {
	ID             string `json:"id"`
	EntityID       string `json:"entity_id"`
	DeviceID       string `json:"device_id"`
	AreaID         string `json:"area_id"`
	Platform       string `json:"platform"`
	DisabledBy     string `json:"disabled_by"`
	HiddenBy       string `json:"hidden_by"`
	EntityCategory string `json:"entity_category"`
}

type haState struct {
	EntityID    string         `json:"entity_id"`
	State       string         `json:"state"`
	Attributes  map[string]any `json:"attributes"`
	LastChanged string         `json:"last_changed,omitempty"`
	LastUpdated string         `json:"last_updated,omitempty"`
}

// updatedAt is when Home Assistant last heard anything for this entity.
func (s haState) updatedAt() (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s.LastUpdated)
	return t, err == nil
}

// countdownEnd turns a "hours left" reading into a moment, measured from when it was read, so a reading that
// stops updating still points at the right time.
func countdownEnd(s haState, hours float64) string {
	t, ok := s.updatedAt()
	if !ok {
		return ""
	}
	return t.Add(time.Duration(hours * float64(time.Hour))).UTC().Format(time.RFC3339)
}

// changedAt is when the state last changed, as plain RFC 3339 (Home Assistant sends microseconds).
func (s haState) changedAt() any {
	t, err := time.Parse(time.RFC3339Nano, s.LastChanged)
	if err != nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// alarmCapabilities offers only the arm modes the panel supports. Panels that don't say keep home and away.
func alarmCapabilities(k model.Kind, s haState, caps []string) []string {
	f, ok := s.Attributes["supported_features"].(float64)
	if k != model.KindAlarm || !ok || f == 0 {
		return caps
	}
	out := []string{model.ActDisarm}
	for _, m := range alarmModes {
		if int(f)&m.bit != 0 {
			out = append(out, m.action)
		}
	}
	return out
}

// Home Assistant's alarm_control_panel supported_features bits.
var alarmModes = []struct {
	bit    int
	action string
}{{1, model.ActArmHome}, {2, model.ActArmAway}, {4, model.ActArmNight}, {32, model.ActArmVacation}}

// Integrations that are not "products" in the house (phones, network gear,
// tariffs, add-on plumbing…). They stay out of the app.
var skipPlatforms = map[string]bool{
	"mobile_app": true, "eero": true, "synology_dsm": true, "octopus_energy": true,
	"hacs": true, "met": true, "sun": true, "backup": true, "hassio": true,
	"min_max": true, "template": true, "integration": true, "browser_mod": true,
	"matter_binding_helper": true, "shopping_list": true, "person": true,
}

// group is one HA device and its entities, before it is classified.
type group struct {
	dev      haDevice
	entities []haEntity
}

// product is one physical device plus the HA entities that belong to it.
type product struct {
	dev    model.Device
	refs   map[string]string // role -> entity_id (primary, remote, battery, …)
	all    []string          // every entity_id of the device
	lostAt time.Time         // when HA first reported it unavailable, while we still show it online
}

// offlineGrace keeps a device online through HA's brief reconnects (a printer waking up,
// a Wi-Fi plug re-joining) instead of flashing "Offline" in the app.
var offlineGrace = 3 * time.Minute

// mapper turns HA registries + states into products and keeps them current.
type mapper struct {
	areas    map[string]haArea
	products map[string]*product // by model device id
	byEntity map[string]string   // entity_id -> model device id
	states   map[string]haState
	zigbee   map[string]string // ieee -> area id, for handed-off devices
	category map[string]string // entity_id -> "config" | "diagnostic" | ""
	// Products folded into another (the second half of a split TV). Their ids may still be
	// on the bus from before the merge, so each sync removes them.
	retired []string

	registryIDs map[string]string         // entity registry id -> entity_id
	deviceNames map[string]string         // HA device id -> name
	zigbeeIDs   map[string]string         // HA device id -> Hearth id of the same Zigbee device
	entityDevs  map[string]string         // entity_id -> HA device id
	autoConfigs map[string]map[string]any // automation entity_id -> its config
}

func devID(haDeviceID string) string { return "ha-" + haDeviceID }

func domainOf(entityID string) string {
	d, _, _ := strings.Cut(entityID, ".")
	return d
}

func (m *mapper) rooms() []model.Room {
	rooms := make([]model.Room, 0, len(m.areas))
	for _, a := range m.areas {
		rooms = append(rooms, model.Room{ID: a.AreaID, Name: a.Name, Floor: a.FloorID, Icon: a.Icon})
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].Name < rooms[j].Name })
	return rooms
}

// build creates the full product list from scratch.
func build(areas []haArea, devices []haDevice, entities []haEntity, states []haState) *mapper {
	m := &mapper{
		areas:    map[string]haArea{},
		products: map[string]*product{},
		byEntity: map[string]string{},
		states:   map[string]haState{},
		category: map[string]string{},
		zigbee:   map[string]string{},

		registryIDs: map[string]string{},
		deviceNames: map[string]string{},
		zigbeeIDs:   map[string]string{},
		entityDevs:  map[string]string{},
		autoConfigs: map[string]map[string]any{},
	}
	for _, e := range entities {
		if e.ID != "" {
			m.registryIDs[e.ID] = e.EntityID
		}
		if e.DeviceID != "" {
			m.entityDevs[e.EntityID] = e.DeviceID
		}
	}
	for _, d := range devices {
		if ieee := z2mIEEE(d); ieee != "" {
			m.zigbeeIDs[d.ID] = "zb-" + ieee
		}
		if d.NameByUser != "" {
			m.deviceNames[d.ID] = d.NameByUser
		} else {
			m.deviceNames[d.ID] = d.Name
		}
	}
	for _, a := range areas {
		m.areas[a.AreaID] = a
	}
	for _, s := range states {
		m.states[s.EntityID] = s
	}

	groups := map[string]*group{}
	for _, d := range devices {
		if d.DisabledBy != "" || (strings.EqualFold(d.Manufacturer, "Zigbee2MQTT") && strings.Contains(strings.ToLower(d.Model), "bridge")) {
			continue // Zigbee2MQTT's own bridge is plumbing, not a device in the home
		}
		if ieee := z2mIEEE(d); handOffZigbee && ieee != "" {
			m.zigbee[ieee] = d.AreaID
			continue
		}
		groups[d.ID] = &group{dev: d}
	}
	for _, e := range entities {
		// Entities hidden in Home Assistant (e.g. the switches behind "switch as light"
		// helpers) stay out, like they do in HA's own UI.
		if e.DisabledBy != "" || e.HiddenBy != "" || e.DeviceID == "" || skipPlatforms[e.Platform] {
			continue
		}
		m.category[e.EntityID] = e.EntityCategory
		if g := groups[e.DeviceID]; g != nil {
			g.entities = append(g.entities, e)
		}
	}

	for _, id := range mergeSplitMedia(groups) {
		m.retired = append(m.retired, devID(id))
	}
	for _, id := range mergeStreamOnly(groups) {
		m.retired = append(m.retired, devID(id))
	}
	for _, id := range splitMultiGang(groups, m.states) {
		m.retired = append(m.retired, devID(id))
	}

	for _, g := range groups {
		p := classify(g.dev, g.entities, m.states)
		if p == nil {
			continue
		}
		room := g.dev.AreaID
		if room == "" {
			if e, ok := firstWithArea(g.entities, p.refs["primary"]); ok {
				room = e
			}
		}
		p.dev.RoomID = room
		m.products[p.dev.ID] = p
		for _, eid := range p.all {
			m.byEntity[eid] = p.dev.ID
		}
		m.refresh(p)
	}
	m.addSynthetic()
	return m
}

// addSynthetic adds products that aren't one device in Home Assistant:
// "Energy" from the Octopus tariff/usage sensors and "People" from person entities.
func (m *mapper) addSynthetic() {
	energy := &product{
		dev:  model.Device{ID: "ha-energy", Name: "Energy", Kind: model.KindEnergy, Source: source, Manufacturer: "Octopus Energy", Capabilities: []string{}},
		refs: map[string]string{},
	}
	people := &product{
		dev:  model.Device{ID: "ha-people", Name: "People", Kind: model.KindPresence, Source: source, Capabilities: []string{}},
		refs: map[string]string{},
	}
	weather := &product{
		dev:  model.Device{ID: "ha-weather", Name: "Weather", Kind: model.KindWeather, Source: source, Capabilities: []string{}},
		refs: map[string]string{},
	}
	automations := &product{
		dev:  model.Device{ID: "ha-automations", Name: "Home Assistant automations", Kind: model.KindAutomations, Source: source, Capabilities: []string{model.ActEntity}},
		refs: map[string]string{},
	}
	for eid := range m.states {
		switch {
		case strings.HasPrefix(eid, "sensor.octopus_energy_electricity_"), strings.HasPrefix(eid, "binary_sensor.octopus_energy_electricity_"),
			strings.HasPrefix(eid, "sensor.octopus_energy_gas_"), strings.HasPrefix(eid, "sensor.octopus_energy_a_"),
			strings.HasPrefix(eid, "calendar.octopus_energy_a_"), strings.HasPrefix(eid, "event.octopus_energy_electricity_"):
			energy.all = append(energy.all, eid)
		case strings.HasPrefix(eid, "person.") && eid != "person.mqtt_user":
			people.all = append(people.all, eid)
		case strings.HasPrefix(eid, "weather.") && len(weather.all) == 0:
			weather.all = append(weather.all, eid)
		case strings.HasPrefix(eid, "automation."):
			automations.all = append(automations.all, eid)
		}
	}
	for _, p := range []*product{energy, people, automations, weather} {
		if len(p.all) == 0 {
			continue
		}
		sort.Strings(p.all)
		m.products[p.dev.ID] = p
		for _, eid := range p.all {
			m.byEntity[eid] = p.dev.ID
		}
		m.refresh(p)
	}
}

// refreshSynthetic fills the normalized state of the Energy and People products.
func (m *mapper) refreshSynthetic(p *product) map[string]any {
	st := map[string]any{}
	find := func(contains ...string) (haState, bool) {
		for _, eid := range p.all {
			ok := true
			for _, c := range contains {
				ok = ok && strings.Contains(eid, c)
			}
			if ok {
				return m.states[eid], true
			}
		}
		return haState{}, false
	}
	num := func(contains ...string) any {
		if s, ok := find(contains...); ok {
			return parseNum(s.State)
		}
		return nil
	}
	pence := func(v any) any { // Octopus reports £/kWh; people think in pence
		if f, ok := v.(float64); ok {
			return math.Round(f*1000) / 10
		}
		return nil
	}
	switch p.dev.Kind {
	case model.KindEnergy:
		st["elec_rate_p"] = pence(num("electricity", "_current_rate"))
		st["elec_next_rate_p"] = pence(num("electricity", "_next_rate"))
		st["elec_standing_p"] = pence(num("electricity", "current_standing_charge"))
		st["elec_yesterday_kwh"] = num("electricity", "previous_accumulative_consumption")
		st["elec_yesterday_cost"] = num("electricity", "previous_accumulative_cost")
		if s, ok := find("electricity", "previous_accumulative_cost"); ok {
			if day, peak, off := octopusDay(s); day != "" {
				st["elec_day"], st["elec_peak_kwh"], st["elec_offpeak_kwh"] = day, peak, off
			}
		}
		if s, ok := find("binary_sensor", "off_peak"); ok {
			st["off_peak"] = s.State == "on"
		}
		st["gas_rate_p"] = pence(num("gas", "_current_rate"))
		st["gas_standing_p"] = pence(num("gas", "current_standing_charge"))
		st["gas_yesterday_kwh"] = num("gas", "previous_accumulative_consumption_kwh")
		st["gas_yesterday_cost"] = num("gas", "previous_accumulative_cost")
		if s, ok := find("gas", "previous_accumulative_cost"); ok {
			if day, _, _ := octopusDay(s); day != "" {
				st["gas_day"] = day
			}
		}
		st["octoplus_points"] = num("octoplus_points")
		if s, ok := find("calendar", "saving_sessions"); ok {
			st["saving_session"] = s.State == "on"
		}
		if s, ok := find("event.", "electricity", "current_day_rates"); ok {
			st["rates_today"] = rateBands(s.Attributes["rates"])
		}
		if s, ok := find("event.", "electricity", "next_day_rates"); ok {
			if b := rateBands(s.Attributes["rates"]); len(b) > 0 {
				st["rates_tomorrow"] = b
			}
		}
		p.dev.Online = st["elec_rate_p"] != nil || st["gas_rate_p"] != nil
	case model.KindWeather:
		if len(p.all) > 0 {
			s := m.states[p.all[0]]
			st["condition"] = s.State
			for _, k := range []string{"temperature", "humidity", "wind_speed", "uv_index"} {
				st[k] = s.Attributes[k]
			}
			p.dev.Online = s.State != "unavailable"
		}
	case model.KindPresence:
		home, away := []string{}, []string{}
		for _, eid := range p.all {
			s := m.states[eid]
			name, _ := s.Attributes["friendly_name"].(string)
			if s.State == "home" {
				home = append(home, name)
			} else if s.State != "unknown" && s.State != "unavailable" {
				away = append(away, name)
			}
		}
		st["home"], st["away"] = home, away
		st["anyone_home"] = len(home) > 0
		st["everyone_home"] = len(away) == 0 && len(home) > 0
		st["count_home"] = float64(len(home))
		p.dev.Online = true
	}
	return st
}

// splitMultiGang turns a device with several independent lights (a 2-gang wall
// switch: "Main Lights" and "Side Lights") into one product per light, so each
// gets its own tile. Plain multi-channel switch boxes are split the same way
// when they have nothing else (no climate, media, vacuum…). Returns the ids it replaced.
func splitMultiGang(groups map[string]*group, states map[string]haState) (replaced []string) {
	for id, g := range groups {
		var lights, switches, others []haEntity
		hasMain := false
		for _, e := range g.entities {
			switch dom := domainOf(e.EntityID); {
			case e.EntityCategory != "":
				others = append(others, e)
			case dom == "light":
				lights = append(lights, e)
			case dom == "switch":
				switches = append(switches, e)
			default:
				if map[string]bool{"climate": true, "media_player": true, "vacuum": true, "fan": true, "lock": true,
					"cover": true, "camera": true, "alarm_control_panel": true}[dom] {
					hasMain = true
				}
				// An appliance (dishwasher, oven…): its option switches aren't separate devices.
				if strings.HasSuffix(e.EntityID, "_operation_state") || strings.HasSuffix(e.EntityID, "_selected_program") {
					hasMain = true
				}
				others = append(others, e)
			}
		}
		parts := lights
		if len(lights) == 1 && len(switches) >= 1 && len(switches) <= 3 && !hasMain {
			// A wall switch whose second channel Home Assistant lists as a plain switch.
			parts = append(append([]haEntity{}, lights...), switches...)
			switches = nil
		}
		if len(parts) < 2 {
			if hasMain || len(lights) > 0 || len(switches) < 2 {
				continue
			}
			// Only real channels: each switch needs its own name ("Socket 1", "Socket 2").
			names := map[string]bool{}
			for _, e := range switches {
				n, _ := states[e.EntityID].Attributes["friendly_name"].(string)
				names[n] = true
			}
			if len(names) < len(switches) {
				continue
			}
			parts = switches
		} else {
			others = append(others, switches...)
		}
		delete(groups, id)
		replaced = append(replaced, id)
		for i, e := range parts {
			d := g.dev
			_, object, _ := strings.Cut(e.EntityID, ".")
			d.ID = g.dev.ID + "_" + object
			if name, _ := states[e.EntityID].Attributes["friendly_name"].(string); name != "" {
				d.Name, d.NameByUser = name, ""
			}
			if i > 0 && genericName(d.Name) {
				first, _ := states[parts[0].EntityID].Attributes["friendly_name"].(string)
				if base, ok := strings.CutSuffix(first, " 1"); ok {
					d.Name = fmt.Sprintf("%s %d", base, i+1)
				}
			}
			if e.AreaID != "" {
				d.AreaID = e.AreaID
			}
			ents := []haEntity{e}
			if i == 0 {
				ents = append(ents, others...) // shared settings/diagnostics live with the first light
			}
			groups[d.ID] = &group{dev: d, entities: ents}
		}
	}
	return replaced
}

// mergeSplitMedia joins the halves of one TV that Home Assistant lists as two devices:
// Google Cast knows what is playing (and that it is paused), Android TV Remote has the
// remote, power and app launching. Same maker, model, room and name means the same set.
// The Cast half keeps its id, so a layout saved for it (room, hidden, order) still applies,
// and its media player stays first, so it is the one whose state the product shows.
func mergeSplitMedia(groups map[string]*group) (absorbed []string) {
	byKey := map[string][]string{}
	for id, g := range groups {
		hasMedia := false
		for _, e := range g.entities {
			if domainOf(e.EntityID) == "media_player" {
				hasMedia = true
			}
		}
		if !hasMedia || g.dev.Model == "" {
			continue
		}
		name := g.dev.NameByUser
		if name == "" {
			name = g.dev.Name
		}
		key := strings.ToLower(strings.Join([]string{g.dev.Manufacturer, g.dev.Model, g.dev.AreaID, name}, "|"))
		byKey[key] = append(byKey[key], id)
	}
	for _, ids := range byKey {
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		base := ids[0]
		for _, id := range ids {
			for _, e := range groups[id].entities {
				if e.Platform == "cast" && domainOf(e.EntityID) == "media_player" {
					base = id
				}
			}
		}
		for _, id := range ids {
			if id == base {
				continue
			}
			groups[base].entities = append(groups[base].entities, groups[id].entities...)
			delete(groups, id)
			absorbed = append(absorbed, id)
		}
	}
	return absorbed
}

// mergeStreamOnly folds a DLNA renderer into the same speaker's native integration
// (e.g. a Roberts radio seen by both Frontier Silicon and DLNA): HA names both after the
// hardware, so they share a registry name. The DLNA side is kept only to play stream links.
func mergeStreamOnly(groups map[string]*group) (absorbed []string) {
	platformOf := func(g *group) string {
		for _, e := range g.entities {
			if domainOf(e.EntityID) == "media_player" {
				return e.Platform
			}
		}
		return ""
	}
	native := map[string]string{}
	for id, g := range groups {
		if p := platformOf(g); p != "" && p != "dlna_dmr" && g.dev.Name != "" {
			native[strings.ToLower(g.dev.Name)] = id
		}
	}
	for id, g := range groups {
		if platformOf(g) != "dlna_dmr" {
			continue
		}
		base, ok := native[strings.ToLower(g.dev.Name)]
		if !ok || base == id {
			continue
		}
		groups[base].entities = append(groups[base].entities, g.entities...)
		delete(groups, id)
		absorbed = append(absorbed, id)
	}
	return absorbed
}

func firstWithArea(entities []haEntity, primary string) (string, bool) {
	for _, e := range entities {
		if e.EntityID == primary && e.AreaID != "" {
			return e.AreaID, true
		}
	}
	return "", false
}

// classify decides what kind of product a device is and which entities matter.
func classify(d haDevice, ents []haEntity, states map[string]haState) *product {
	name := d.NameByUser
	if name == "" {
		name = d.Name
	}
	refs := map[string]string{}
	var all []string
	byDomain := map[string][]string{}
	nativeMedia := false
	for _, e := range ents {
		if domainOf(e.EntityID) == "media_player" && e.Platform != "dlna_dmr" {
			nativeMedia = true
		}
	}
	for _, e := range ents {
		all = append(all, e.EntityID)
		dom := domainOf(e.EntityID)
		if dom == "media_player" && e.Platform == "dlna_dmr" && nativeMedia {
			refs["stream"] = e.EntityID // plays stream links; the native player does the rest
			continue
		}
		if e.EntityCategory == "" { // skip config/diagnostic entities for roles
			byDomain[dom] = append(byDomain[dom], e.EntityID)
		}
		// supporting readings, whatever their category
		s := states[e.EntityID]
		dc, _ := s.Attributes["device_class"].(string)
		switch {
		case dom == "sensor" && dc == "battery" && refs["battery"] == "":
			refs["battery"] = e.EntityID
		case dom == "sensor" && dc == "temperature" && refs["temperature"] == "":
			refs["temperature"] = e.EntityID
		case dom == "sensor" && dc == "humidity" && refs["humidity"] == "":
			refs["humidity"] = e.EntityID
		case dom == "sensor" && dc == "signal_strength" && refs["signal"] == "":
			refs["signal"] = e.EntityID
		case dom == "binary_sensor" && (dc == "motion" || dc == "occupancy" || dc == "presence") && refs["motion"] == "":
			refs["motion"] = e.EntityID
		case dom == "binary_sensor" && (dc == "door" || dc == "window" || dc == "opening" || dc == "garage_door") && refs["contact"] == "":
			refs["contact"] = e.EntityID
		case dom == "binary_sensor" && dc == "moisture" && refs["leak"] == "":
			refs["leak"] = e.EntityID
		case dom == "binary_sensor" && (dc == "smoke" || dc == "gas" || dc == "carbon_monoxide") && refs["smoke"] == "":
			refs["smoke"] = e.EntityID
		case dom == "sensor" && dc == "illuminance" && refs["illuminance"] == "":
			refs["illuminance"] = e.EntityID
		case dom == "remote" && refs["remote"] == "":
			refs["remote"] = e.EntityID
			refs["remote_platform"] = e.Platform
		case dom == "media_player" && e.Platform == "androidtv_remote" && refs["tv_control"] == "":
			// Android TV Remote's own player: reliable power, the package of the app in front.
			refs["tv_control"] = e.EntityID
		case dom == "camera" && refs["camera"] == "":
			refs["camera"] = e.EntityID
		}
		// product-specific readings by entity id suffix
		for _, suffix := range []string{"pm_2_5", "filter_life", "print_status", "percent_complete",
			"remaining_print_time", "nozzle_temperature", "bed_temperature", "file_name",
			"machine_state", "job_state", "completion_time", "vehicle_battery", "status", "power",
			"charge_mode", "target_percentage", "target_time", "charge_slots",
			"valve_position", "pause_print", "resume_print", "stop_print", "chamber_light", "clean_mode",
			"announce", "speak", "do_not_disturb",
			"operation_state", "selected_program", "active_program", "program_progress", "program_phase",
			"remaining_program_time", "start_in", "rinse_aid", "salt", "door", "machinecare_remaining_runs", "abort", "start"} {
			if strings.HasSuffix(e.EntityID, "_"+suffix) && refs[suffix] == "" {
				refs[suffix] = e.EntityID
			}
		}
	}

	first := func(dom string) string {
		if l := byDomain[dom]; len(l) > 0 {
			return l[0]
		}
		return ""
	}
	maker := strings.ToLower(d.Manufacturer + " " + d.Model)
	var kind model.Kind
	primary := ""

	switch {
	case refs["print_status"] != "" || strings.Contains(maker, "elegoo"):
		kind, primary = model.KindPrinter3D, refs["print_status"]
	case refs["operation_state"] != "" && strings.Contains(maker+" "+strings.ToLower(name), "dish"):
		kind, primary = model.KindDishwasher, refs["operation_state"]
	case refs["machine_state"] != "":
		kind, primary = model.KindWasher, refs["machine_state"]
	case strings.Contains(maker, "ohme") || refs["vehicle_battery"] != "":
		kind, primary = model.KindEVCharger, refs["status"]
	case first("alarm_control_panel") != "":
		kind, primary = model.KindAlarm, first("alarm_control_panel")
	case first("lock") != "":
		kind, primary = model.KindLock, first("lock")
	case first("climate") != "":
		primary = first("climate")
		kind = model.KindThermostat
		if strings.Contains(maker, "dreo") || strings.Contains(primary, "heater") || strings.Contains(primary, "heather") {
			kind = model.KindHeater
		}
	case first("vacuum") != "":
		kind, primary = model.KindVacuum, first("vacuum")
	case refs["announce"] != "" && first("media_player") == "":
		kind, primary = model.KindSpeaker, refs["announce"]
	case first("media_player") != "":
		primary = first("media_player")
		dc, _ := states[primary].Attributes["device_class"].(string)
		kind = model.KindSpeaker
		if dc == "tv" || refs["remote"] != "" || strings.Contains(maker, "tv") || strings.Contains(maker, "bravia") || strings.Contains(maker, "fire") {
			kind = model.KindTV
		}
		// The Apple TV integration also gives HomePods a remote; they are speakers. HomePods often report
		// their model as "AudioAccessory5,1", and any Apple device that isn't an Apple TV is a speaker.
		for _, w := range []string{"homepod", "audioaccessory", "sonos", "speaker", "echo", "nest audio", "istream"} {
			if strings.Contains(maker, w) {
				kind = model.KindSpeaker
			}
		}
		if dc == "speaker" || (strings.Contains(maker, "apple") && !strings.Contains(maker, "apple tv") && !strings.Contains(maker, "appletv")) {
			kind = model.KindSpeaker
		}
	case first("fan") != "":
		primary = first("fan")
		kind = model.KindFan
		if refs["pm_2_5"] != "" || strings.Contains(primary, "purifier") || strings.Contains(maker, "blueair") {
			kind = model.KindAirPurifier
		}
	case first("cover") != "":
		kind, primary = model.KindCover, first("cover")
	case first("light") != "":
		kind, primary = model.KindLight, first("light")
	case first("switch") != "":
		kind, primary = model.KindSwitch, first("switch")
	case first("camera") != "":
		kind, primary = model.KindCamera, first("camera")
	case refs["temperature"] != "" || refs["motion"] != "" || refs["contact"] != "" || refs["humidity"] != "" || refs["leak"] != "" || refs["smoke"] != "":
		kind = model.KindSensor
		for _, r := range []string{"temperature", "motion", "contact", "humidity", "leak", "smoke"} {
			if refs[r] != "" {
				primary = refs[r]
				break
			}
		}
	default:
		return nil // not a product we show (bridges, hubs, network gear…)
	}
	if primary == "" {
		return nil
	}
	refs["primary"] = primary
	// TVs that drop off the network when off (Samsung) are woken by the Wake on LAN
	// integration's buttons for their MAC addresses.
	if domainOf(primary) == "media_player" {
		var wake []string
		for _, c := range d.Connections {
			if len(c) == 2 && c[0] == "mac" {
				b := "button.wake_on_lan_" + strings.ReplaceAll(strings.ToLower(c[1]), ":", "_")
				if _, ok := states[b]; ok {
					wake = append(wake, b)
				}
			}
		}
		if len(wake) > 0 {
			refs["wake"] = strings.Join(wake, ",")
		}
	}
	entityName, _ := states[primary].Attributes["friendly_name"].(string)
	if (kind == model.KindLight || kind == model.KindSwitch) && entityName != "" && genericName(name) && !genericName(entityName) {
		name = entityName
	}
	if kind == model.KindSensor && primary == refs["contact"] && strings.HasSuffix(strings.ToLower(name), "entry") {
		if own := strings.TrimSpace(strings.TrimPrefix(entityName, name)); own != "" {
			name = own
		}
	}
	if kind == model.KindSwitch && looksLikeLight(name) {
		kind = model.KindLight
	}

	return &product{
		dev: model.Device{
			ID:           devID(d.ID),
			Name:         name,
			Kind:         kind,
			Manufacturer: d.Manufacturer,
			Model:        d.Model,
			Source:       "homeassistant",
			Capabilities: alarmCapabilities(kind, states[primary], capabilitiesFor(kind, refs, dimmable(states[primary]))),
			State:        map[string]any{},
		},
		refs: refs,
		all:  all,
	}
}

// dimmable says whether a light can change brightness (not just on/off).
func dimmable(s haState) bool {
	if domainOf(s.EntityID) != "light" {
		return false
	}
	modes, _ := s.Attributes["supported_color_modes"].([]any)
	for _, m := range modes {
		if m != "onoff" {
			return true
		}
	}
	return len(modes) == 0 && s.Attributes["brightness"] != nil
}

func capabilitiesFor(k model.Kind, refs map[string]string, canDim bool) []string {
	c := []string{}
	switch k {
	case model.KindLight:
		c = []string{model.ActTurnOn, model.ActTurnOff, model.ActToggle}
		if canDim {
			c = append(c, model.ActSetBrightness)
		}
	case model.KindSwitch:
		c = []string{model.ActTurnOn, model.ActTurnOff, model.ActToggle}
	case model.KindThermostat, model.KindHeater:
		c = []string{model.ActSetTemperature, model.ActSetMode, model.ActTurnOn, model.ActTurnOff}
	case model.KindTV, model.KindSpeaker:
		if refs["announce"] != "" && domainOf(refs["primary"]) == "notify" {
			return []string{model.ActAnnounce, model.ActSpeak}
		}
		c = []string{model.ActTurnOn, model.ActTurnOff, model.ActPlayPause, model.ActNext, model.ActPrevious, model.ActPlayURL,
			model.ActVolumeUp, model.ActVolumeDown, model.ActVolumeSet, model.ActMute, model.ActSelectSource, model.ActLaunchApp}
		if refs["remote"] != "" && k == model.KindTV { // a HomePod's remote entity has no d-pad worth showing
			c = append(c, model.ActRemote)
		}
	case model.KindVacuum:
		c = []string{model.ActStart, model.ActPause, model.ActStop, model.ActDock}
	case model.KindAirPurifier, model.KindFan:
		c = []string{model.ActTurnOn, model.ActTurnOff, model.ActToggle, model.ActSetPreset, model.ActSetSpeed}
	case model.KindEVCharger:
		for role, act := range map[string]string{"charge_mode": model.ActSetChargeMode, "target_percentage": model.ActSetChargeTo, "target_time": model.ActSetReadyBy} {
			if refs[role] != "" {
				c = append(c, act)
			}
		}
		sort.Strings(c)
	case model.KindLock:
		c = []string{model.ActLock, model.ActUnlock}
	case model.KindAlarm:
		c = []string{model.ActArmHome, model.ActArmAway, model.ActDisarm}
	case model.KindPrinter3D:
		for role, act := range map[string]string{"pause_print": model.ActPause, "resume_print": model.ActResume, "stop_print": model.ActStop} {
			if refs[role] != "" {
				c = append(c, act)
			}
		}
		if refs["chamber_light"] != "" {
			c = append(c, model.ActToggle)
		}
		sort.Strings(c)
	case model.KindCover:
		c = []string{model.ActOpen, model.ActClose, model.ActStop}
	case model.KindDishwasher:
		if refs["start"] != "" {
			c = append(c, model.ActStart)
		}
		if refs["abort"] != "" {
			c = append(c, model.ActStop)
		}
	}
	return c
}

// refresh recomputes a product's normalized state from the latest HA states.
func (m *mapper) refresh(p *product) {
	get := func(role string) (haState, bool) {
		eid := p.refs[role]
		if eid == "" {
			return haState{}, false
		}
		s, ok := m.states[eid]
		return s, ok
	}
	num := func(role string) any {
		if s, ok := get(role); ok {
			return parseNum(s.State)
		}
		return nil
	}
	str := func(role string) any {
		if s, ok := get(role); ok && s.State != "unknown" && s.State != "unavailable" {
			return s.State
		}
		return nil
	}

	if p.dev.Kind == model.KindAutomations {
		on := 0
		for _, eid := range p.all {
			if m.states[eid].State == "on" {
				on++
			}
		}
		p.dev.State = map[string]any{"count": float64(len(p.all)), "enabled": float64(on), "entities": m.entities(p)}
		p.dev.Online = true
		p.dev.UpdatedAt = time.Now().UTC()
		return
	}
	if p.dev.Kind == model.KindEnergy || p.dev.Kind == model.KindPresence || p.dev.Kind == model.KindWeather {
		st := m.refreshSynthetic(p)
		for k, v := range st {
			if v == nil {
				delete(st, k)
			}
		}
		p.dev.State = st
		p.dev.UpdatedAt = time.Now().UTC()
		return
	}
	prim, ok := get("primary")
	if ok && prim.State == "unavailable" && p.dev.Online {
		if p.lostAt.IsZero() {
			p.lostAt = time.Now()
		}
		if time.Since(p.lostAt) < offlineGrace {
			return
		}
	} else if !ok || prim.State != "unavailable" {
		p.lostAt = time.Time{}
	}
	st := map[string]any{}
	p.dev.Online = ok && prim.State != "unavailable"
	a := prim.Attributes

	switch p.dev.Kind {
	case model.KindDishwasher:
		st["status"] = prim.State
		st["program"] = str("active_program")
		if st["program"] == nil {
			st["program"] = str("selected_program")
		}
		st["phase"] = str("program_phase")
		st["progress"] = num("program_progress")
		if h, ok := num("remaining_program_time").(float64); ok {
			st["remaining_min"] = math.Round(h * 60)
		}
		if h, ok := num("start_in").(float64); ok && h > 0 {
			st["start_in_min"] = math.Round(h * 60)
			if s, ok := get("start_in"); ok && prim.State == "delayedstart" {
				if at := countdownEnd(s, h); at != "" {
					st["starts_at"] = at
				}
			}
		}
		if h, ok := num("remaining_program_time").(float64); ok && h > 0 && prim.State == "run" {
			if s, ok := get("remaining_program_time"); ok {
				if at := countdownEnd(s, h); at != "" {
					st["ends_at"] = at
				}
			}
		}
		if s, ok := get("door"); ok {
			st["door_open"] = s.State == "on"
		}
		st["rinse_aid"] = str("rinse_aid")
		st["salt"] = str("salt")
		st["machine_care_runs"] = num("machinecare_remaining_runs")
		if s, ok := get("start"); ok {
			st["remote_start"] = s.State != "unavailable"
		}
	case model.KindLight, model.KindSwitch:
		st["on"] = prim.State == "on"
		if b, ok := a["brightness"].(float64); ok {
			st["brightness"] = int(b / 255 * 100)
		}
	case model.KindThermostat, model.KindHeater:
		st["mode"] = prim.State
		st["current"] = a["current_temperature"]
		st["target"] = a["temperature"]
		st["action"] = a["hvac_action"]
		st["modes"] = a["hvac_modes"]
		st["min"], st["max"], st["step"] = a["min_temp"], a["max_temp"], a["target_temp_step"]
		st["valve_position"] = num("valve_position")
	case model.KindTV, model.KindSpeaker:
		if domainOf(p.refs["primary"]) == "notify" {
			st["power"] = true
			st["assistant"] = "alexa"
			if s, ok := get("do_not_disturb"); ok {
				st["dnd"] = s.State == "on"
			}
			st["temperature"] = num("temperature")
			break
		}
		st["power"] = prim.State != "off" && prim.State != "standby" && prim.State != "unavailable"
		// Android TV Remote knows the set is on even when nothing is cast to it.
		ctl, hasCtl := get("tv_control")
		if hasCtl && ctl.State == "on" {
			st["power"] = true
		}
		st["playing"] = prim.State == "playing"
		st["paused"] = prim.State == "paused"
		// Some players (e.g. Roberts radios) can't pause or play, only go to standby.
		if f, ok := a["supported_features"].(float64); ok {
			st["can_pause"] = int(f)&(1|16384) != 0
		}
		st["volume"] = a["volume_level"]
		st["muted"] = a["is_volume_muted"]
		st["source"] = a["source"]
		st["sources"] = a["source_list"]
		if p.refs["remote_platform"] == "androidtv_remote" && a["source_list"] == nil {
			st["sources"] = androidTVAppNames()
		}
		app := a["app_name"]
		if s, _ := app.(string); s == "" && hasCtl {
			app = friendlyApp(fmt.Sprint(ctl.Attributes["app_name"]))
		}
		st["app"] = app
		// Where the video is, for a progress bar: the app adds the time since the update.
		st["position"] = a["media_position"]
		st["duration"] = a["media_duration"]
		st["position_updated_at"] = a["media_position_updated_at"]
		st["title"] = a["media_title"]
		st["artist"] = a["media_artist"]
		st["artwork"] = a["entity_picture"]
	case model.KindVacuum:
		st["status"] = prim.State
		st["mode"] = str("clean_mode")
	case model.KindAirPurifier, model.KindFan:
		st["on"] = prim.State == "on"
		st["preset"] = a["preset_mode"]
		st["presets"] = a["preset_modes"]
		st["speed"] = a["percentage"]
		st["speed_step"] = a["percentage_step"] // e.g. 33.3 → 3 speeds
		st["pm25"] = num("pm_2_5")
		st["filter_life"] = num("filter_life")
	case model.KindLock:
		st["locked"] = prim.State == "locked"
		st["status"] = prim.State // locked, unlocked, locking, unlocking, jammed, open, opening
		st["changed_at"] = prim.changedAt()
		st["changed_by"] = a["changed_by"]
		if s, ok := get("contact"); ok {
			st["door_open"] = s.State == "on"
		}
	case model.KindAlarm:
		st["status"] = prim.State // disarmed, armed_*, arming, pending, triggered
		st["changed_at"] = prim.changedAt()
		st["changed_by"] = a["changed_by"]
		if f, ok := a["code_format"].(string); ok && f != "" {
			st["code_format"] = f // "number" or "text"; arming may need it too
			st["code_arm_required"] = a["code_arm_required"] != false
		}
	case model.KindPrinter3D:
		st["status"] = prim.State
		st["progress"] = num("percent_complete")
		st["remaining_min"] = num("remaining_print_time")
		st["nozzle"] = num("nozzle_temperature")
		st["bed"] = num("bed_temperature")
		st["file"] = str("file_name")
		if s, ok := get("chamber_light"); ok {
			st["light"] = s.State == "on"
		}
	case model.KindWasher:
		st["status"] = prim.State
		st["job"] = str("job_state")
		if job := str("job_state"); prim.State == "run" && (job == "finish" || job == "none") {
			st["status"] = "finish" // Samsung stays in "run" until the door is opened
		}
		st["finishes_at"] = str("completion_time")
	case model.KindEVCharger:
		st["status"] = prim.State
		st["plugged"] = prim.State != "unplugged" && prim.State != "unavailable" && prim.State != ""
		st["power_kw"] = num("power")
		st["car_battery"] = num("vehicle_battery")
		st["mode"] = str("charge_mode")
		st["target_percent"] = num("target_percentage")
		st["target_time"] = str("target_time")
		if s := str("charge_slots"); s != nil {
			st["slots"] = s
		}
	case model.KindCover:
		st["status"] = prim.State
		st["position"] = a["current_position"]
	case model.KindCamera:
		st["status"] = prim.State
	case model.KindSensor:
		if s, ok := get("motion"); ok {
			st["motion"] = s.State == "on"
			st["motion_at"] = s.changedAt()
		}
		if s, ok := get("contact"); ok {
			st["open"] = s.State == "on"
			st["open_at"] = s.changedAt()
		}
		if s, ok := get("leak"); ok {
			st["leak"] = s.State == "on"
		}
		if s, ok := get("smoke"); ok {
			st["smoke"] = s.State == "on"
		}
		if v := num("illuminance"); v != nil {
			st["illuminance"] = v
		}
	}
	// shared readings
	if v := num("battery"); v != nil {
		st["battery"] = v
	}
	if v := num("temperature"); v != nil {
		st["temperature"] = v
	}
	if v := num("humidity"); v != nil {
		st["humidity"] = v
	}
	if v := num("signal"); v != nil {
		st["signal"] = v
	}
	if ents := m.entities(p); len(ents) > 0 {
		st["entities"] = ents
	}
	if p.refs["camera"] != "" {
		st["camera_entity"] = p.refs["camera"] // the gateway can proxy snapshots later
	}
	if p.dev.Kind == model.KindEVCharger {
		delete(st, "battery") // that's the car's battery, already car_battery — not a charger battery to warn about
	}
	for k, v := range st {
		if v == nil {
			delete(st, k)
		}
	}
	p.dev.State = st
	p.dev.UpdatedAt = time.Now().UTC()
}

// recheckLost finishes the grace period for devices that never came back.
func (m *mapper) recheckLost() []*product {
	var out []*product
	for _, p := range m.products {
		if !p.lostAt.IsZero() && time.Since(p.lostAt) >= offlineGrace {
			m.refresh(p)
			out = append(out, p)
		}
	}
	return out
}

// apply records a state change and returns the product it affects (if any).
func (m *mapper) apply(s haState, removed bool) *product {
	if removed {
		delete(m.states, s.EntityID)
	} else {
		m.states[s.EntityID] = s
	}
	id, ok := m.byEntity[s.EntityID]
	if !ok {
		return nil
	}
	p := m.products[id]
	m.refresh(p)
	return p
}

func parseNum(s string) any {
	if s == "" || s == "unknown" || s == "unavailable" {
		return nil
	}
	var f float64
	var dot, neg bool
	var div float64 = 1
	for i, r := range s {
		switch {
		case r == '-' && i == 0:
			neg = true
		case r == '.' && !dot:
			dot = true
		case r >= '0' && r <= '9':
			f = f*10 + float64(r-'0')
			if dot {
				div *= 10
			}
		default:
			return s // not a number; keep the text (e.g. timestamps)
		}
	}
	f /= div
	if neg {
		f = -f
	}
	return f
}

// entityDomains are the HA entity types shown as a device's own controls and
// readings in the app ("Everything this device can do").
var entityDomains = map[string]bool{
	"switch": true, "light": true, "fan": true, "number": true, "select": true, "button": true,
	"sensor": true, "binary_sensor": true, "lock": true, "cover": true, "siren": true,
	"input_boolean": true, "input_number": true, "input_select": true, "input_button": true,
	"climate": true, "media_player": true, "vacuum": true, "humidifier": true, "valve": true,
	"text": true, "time": true, "update": true, "automation": true,
}

// entities lists every entity of a device with what the app needs to show
// or control it. Names drop the device's own name ("Blueair Night mode" → "Night mode").
func (m *mapper) entities(p *product) []map[string]any {
	out := []map[string]any{}
	devName := strings.ToLower(p.dev.Name)
	for _, eid := range p.all {
		s, ok := m.states[eid]
		dom := domainOf(eid)
		if !ok || !entityDomains[dom] {
			continue
		}
		a := s.Attributes
		name, _ := a["friendly_name"].(string)
		if strings.HasPrefix(strings.ToLower(name), devName) && len(name) > len(devName) {
			name = strings.TrimSpace(name[len(devName):])
		}
		if name == "" {
			name = strings.ReplaceAll(strings.TrimPrefix(eid, dom+"."), "_", " ")
		}
		e := map[string]any{"id": eid, "domain": dom, "name": name, "state": s.State,
			"available": s.State != "unavailable"}
		// Tell the app which entity is the device itself or just repeats its status,
		// so the screen doesn't list it twice.
		for role, ref := range p.refs {
			if ref == eid {
				e["role"] = role
				// Shown by the device's own screen already.
				if role == "primary" || role == "status" || role == "battery" ||
					(p.dev.Kind == model.KindEVCharger && (role == "power" || role == "vehicle_battery")) ||
					(p.dev.Kind == model.KindPrinter3D && (role == "percent_complete" || role == "nozzle_temperature" || role == "bed_temperature")) ||
					(p.dev.Kind == model.KindAirPurifier && (role == "pm_2_5" || role == "filter_life")) {
					e["redundant"] = true
				}
			}
		}
		if c := m.category[eid]; c != "" {
			e["category"] = c
		}
		if cfg, ok := m.autoConfigs[eid]; ok {
			e["config_id"] = cfg["id"]
			e["summary"] = m.describeAutomation(cfg)
			if draft := m.convertAutomation(cfg); draft != nil {
				e["draft"] = draft
			}
		}
		for _, k := range []string{"unit_of_measurement", "device_class", "options", "min", "max", "step", "mode",
			"brightness", "percentage", "percentage_step", "preset_mode", "preset_modes", "hvac_action", "temperature",
			"current_temperature", "latest_version", "installed_version", "last_triggered"} {
			if v, ok := a[k]; ok && v != nil {
				e[k] = v
			}
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ci, _ := out[i]["category"].(string)
		cj, _ := out[j]["category"].(string)
		if ci != cj {
			return ci < cj // controls first, then config, then diagnostic
		}
		return out[i]["name"].(string) < out[j]["name"].(string)
	})
	return out
}

// rateBands folds Octopus half-hour rates into bands of the same price, in local time
// ("00:00"–"05:30" 6.6p), which is all the app needs to draw the day.
func rateBands(v any) []map[string]any {
	list, _ := v.([]any)
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		loc = time.UTC
	}
	out := []map[string]any{}
	for _, r := range list {
		o, _ := r.(map[string]any)
		start, e1 := time.Parse(time.RFC3339, fmt.Sprint(o["start"]))
		end, e2 := time.Parse(time.RFC3339, fmt.Sprint(o["end"]))
		price, ok := o["value_inc_vat"].(float64)
		if e1 != nil || e2 != nil || !ok {
			continue
		}
		p := math.Round(price*1000) / 10
		from, to := start.In(loc).Format("15:04"), end.In(loc).Format("15:04")
		if n := len(out); n > 0 && out[n-1]["p"] == p && out[n-1]["to"] == from {
			out[n-1]["to"] = to
			continue
		}
		out = append(out, map[string]any{"from": from, "to": to, "p": p})
	}
	return out
}

// genericName is a hardware name nobody would choose ("Kitchen Wall Switch 1", "Office Smart Switch").
func genericName(name string) bool {
	n := strings.ToLower(name)
	for _, w := range []string{"switch", "plug", "unit", "module", "relay", "socket"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

func looksLikeLight(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "light") || strings.Contains(n, "lamp")
}

// octopusDay reads which day Octopus's "previous" figures cover (the meter data
// arrives a day or two late) and splits that day's electricity into the cheapest
// rate and everything else.
func octopusDay(s haState) (day string, peakKWh, offPeakKWh float64) {
	charges, _ := s.Attributes["charges"].([]any)
	type half struct{ rate, kwh float64 }
	var list []half
	cheapest := math.Inf(1)
	for _, c := range charges {
		m, _ := c.(map[string]any)
		start, _ := m["start"].(string)
		rate, _ := m["rate"].(float64)
		kwh, _ := m["consumption"].(float64)
		if day == "" && len(start) >= 10 {
			day = start[:10]
		}
		list = append(list, half{rate, kwh})
		cheapest = math.Min(cheapest, rate)
	}
	for _, h := range list {
		if h.rate <= cheapest+0.0001 {
			offPeakKWh += h.kwh
		} else {
			peakKWh += h.kwh
		}
	}
	return day, math.Round(peakKWh*100) / 100, math.Round(offPeakKWh*100) / 100
}
