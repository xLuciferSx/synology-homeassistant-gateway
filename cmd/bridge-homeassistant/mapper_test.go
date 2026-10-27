package main

import (
	"strings"
	"testing"

	"hearth/internal/model"
)

func fixture() *mapper {
	areas := []haArea{{AreaID: "living_room", Name: "Living Room"}, {AreaID: "office", Name: "Office"}}
	devices := []haDevice{
		{ID: "tv1", Name: "Living Room TV", Manufacturer: "Sony", Model: "BRAVIA VH1", AreaID: "living_room"},
		{ID: "trv1", Name: "Office Thermostat", Manufacturer: "TP-Link", Model: "KE100", AreaID: "office"},
		{ID: "air1", Name: "Gabi Air Purifier", Manufacturer: "Blueair", Model: "Blue Pure 411i Max", AreaID: "living_room"},
		{ID: "pr1", Name: "Centauri Carbon", Manufacturer: "Elegoo", Model: "Centauri Carbon", AreaID: "office"},
		{ID: "phone", Name: "Alex's iPhone", Manufacturer: "Apple"},
	}
	entities := []haEntity{
		{EntityID: "media_player.living_room_tv", DeviceID: "tv1"},
		{EntityID: "remote.living_room_tv", DeviceID: "tv1"},
		{EntityID: "climate.office_thermostat", DeviceID: "trv1"},
		{EntityID: "sensor.office_thermostat_battery", DeviceID: "trv1", EntityCategory: "diagnostic"},
		{EntityID: "switch.office_thermostat_child_lock", DeviceID: "trv1", EntityCategory: "config"},
		{EntityID: "fan.gabi_air_purifier_fan", DeviceID: "air1"},
		{EntityID: "sensor.gabi_air_purifier_pm_2_5", DeviceID: "air1"},
		{EntityID: "sensor.gabi_air_purifier_filter_life", DeviceID: "air1"},
		{EntityID: "sensor.centauri_carbon_print_status", DeviceID: "pr1"},
		{EntityID: "sensor.centauri_carbon_percent_complete", DeviceID: "pr1"},
		{EntityID: "button.centauri_carbon_pause_print", DeviceID: "pr1"},
		{EntityID: "sensor.alexs_iphone_battery_level", DeviceID: "phone", Platform: "mobile_app"},
	}
	states := []haState{
		{EntityID: "media_player.living_room_tv", State: "playing", Attributes: map[string]any{"device_class": "tv", "volume_level": 0.3, "app_name": "Netflix"}},
		{EntityID: "remote.living_room_tv", State: "on"},
		{EntityID: "climate.office_thermostat", State: "heat", Attributes: map[string]any{"current_temperature": 18.9, "temperature": 21.0, "hvac_action": "heating"}},
		{EntityID: "sensor.office_thermostat_battery", State: "100", Attributes: map[string]any{"device_class": "battery"}},
		{EntityID: "fan.gabi_air_purifier_fan", State: "on", Attributes: map[string]any{"preset_mode": "auto", "preset_modes": []any{"auto", "night"}}},
		{EntityID: "sensor.gabi_air_purifier_pm_2_5", State: "3"},
		{EntityID: "sensor.gabi_air_purifier_filter_life", State: "100.0"},
		{EntityID: "sensor.centauri_carbon_print_status", State: "printing"},
		{EntityID: "sensor.centauri_carbon_percent_complete", State: "45"},
		{EntityID: "sensor.alexs_iphone_battery_level", State: "80", Attributes: map[string]any{"device_class": "battery"}},
	}
	return build(areas, devices, entities, states)
}

func TestClassify(t *testing.T) {
	m := fixture()
	want := map[string]model.Kind{
		"ha-tv1": model.KindTV, "ha-trv1": model.KindThermostat,
		"ha-air1": model.KindAirPurifier, "ha-pr1": model.KindPrinter3D,
	}
	for id, kind := range want {
		p := m.products[id]
		if p == nil {
			t.Fatalf("%s missing", id)
		}
		if p.dev.Kind != kind {
			t.Errorf("%s kind = %s, want %s", id, p.dev.Kind, kind)
		}
	}
	if m.products["ha-phone"] != nil {
		t.Error("phones (mobile_app) must not become products")
	}
}

func TestState(t *testing.T) {
	m := fixture()
	tv := m.products["ha-tv1"].dev
	if tv.State["playing"] != true || tv.State["app"] != "Netflix" || tv.RoomID != "living_room" {
		t.Errorf("tv state = %+v room=%s", tv.State, tv.RoomID)
	}
	if !contains(tv.Capabilities, model.ActRemote) {
		t.Error("tv with a remote entity must offer the remote action")
	}
	trv := m.products["ha-trv1"].dev
	if trv.State["target"] != 21.0 || trv.State["battery"] != 100.0 || trv.State["action"] != "heating" {
		t.Errorf("thermostat state = %+v", trv.State)
	}
	air := m.products["ha-air1"].dev
	if air.State["pm25"] != 3.0 || air.State["preset"] != "auto" {
		t.Errorf("purifier state = %+v", air.State)
	}
	pr := m.products["ha-pr1"].dev
	if pr.State["progress"] != 45.0 || !contains(pr.Capabilities, model.ActPause) {
		t.Errorf("printer = %+v caps=%v", pr.State, pr.Capabilities)
	}
}

func TestLiveUpdate(t *testing.T) {
	m := fixture()
	p := m.apply(haState{EntityID: "sensor.office_thermostat_battery", State: "15", Attributes: map[string]any{"device_class": "battery"}}, false)
	if p == nil || p.dev.ID != "ha-trv1" || p.dev.State["battery"] != 15.0 {
		t.Fatalf("battery update not applied to its product: %+v", p)
	}
	if m.apply(haState{EntityID: "sensor.unrelated", State: "1"}, false) != nil {
		t.Error("unrelated entity must not map to a product")
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestZigbeeHandOff(t *testing.T) {
	devices := []haDevice{
		{ID: "z1", Name: "Hall motion", AreaID: "hallway", Identifiers: [][]any{{"mqtt", "zigbee2mqtt_0x00158d0004"}}},
		{ID: "k1", Name: "Kettle plug", AreaID: "kitchen", Identifiers: [][]any{{"tplink", "abc"}}},
	}
	entities := []haEntity{
		{EntityID: "binary_sensor.hall_motion_occupancy", DeviceID: "z1", Platform: "mqtt"},
		{EntityID: "switch.kettle", DeviceID: "k1", Platform: "tplink"},
	}
	states := []haState{{EntityID: "binary_sensor.hall_motion_occupancy", State: "off"}, {EntityID: "switch.kettle", State: "on"}}

	handOffZigbee = true
	defer func() { handOffZigbee = false }()
	m := build(nil, devices, entities, states)
	if _, ok := m.products["ha-z1"]; ok {
		t.Error("zigbee device should be left to bridge-zigbee")
	}
	if _, ok := m.products["ha-k1"]; !ok {
		t.Error("non-zigbee device missing")
	}
	if m.zigbee["0x00158d0004"] != "hallway" {
		t.Errorf("room hint = %v", m.zigbee)
	}
}

// The Sony as Home Assistant has it: Google Cast and Android TV Remote as two devices.
func TestASplitTVBecomesOneProductWithItsRemote(t *testing.T) {
	areas := []haArea{{AreaID: "living_room", Name: "Living Room"}}
	devices := []haDevice{
		{ID: "cast", Name: "Living Room TV", Manufacturer: "Sony", Model: "BRAVIA VH1", AreaID: "living_room"},
		{ID: "atv", Name: "Living Room TV", Manufacturer: "Sony", Model: "BRAVIA VH1", AreaID: "living_room"},
	}
	entities := []haEntity{
		{EntityID: "media_player.living_room_tv", DeviceID: "cast", Platform: "cast"},
		{EntityID: "media_player.living_room_tv_2", DeviceID: "atv", Platform: "androidtv_remote"},
		{EntityID: "remote.living_room_tv", DeviceID: "atv", Platform: "androidtv_remote"},
	}
	states := []haState{
		{EntityID: "media_player.living_room_tv", State: "paused", Attributes: map[string]any{"app_name": "Kinopub", "media_position": 545.0, "media_duration": 1452.4}},
		{EntityID: "media_player.living_room_tv_2", State: "on", Attributes: map[string]any{"app_name": "com.kinopub", "device_class": "tv"}},
		{EntityID: "remote.living_room_tv", State: "on"},
	}
	m := build(areas, devices, entities, states)
	if len(m.products) != 1 {
		t.Fatalf("want one TV, got %d products", len(m.products))
	}
	if len(m.retired) != 1 || m.retired[0] != "ha-atv" {
		t.Fatalf("the absorbed half should be retired: %v", m.retired)
	}
	p := m.products["ha-cast"]
	if p == nil || p.dev.Kind != model.KindTV {
		t.Fatalf("the Cast half should keep its id and be a TV: %+v", m.products)
	}
	if p.refs["primary"] != "media_player.living_room_tv" || p.refs["remote"] != "remote.living_room_tv" {
		t.Fatalf("refs: %v", p.refs)
	}
	st := p.dev.State
	if st["paused"] != true || st["power"] != true || st["app"] != "Kinopub" || st["position"] != 545.0 {
		t.Fatalf("state: %v", st)
	}
	if !contains(p.dev.Capabilities, model.ActRemote) {
		t.Fatalf("no remote capability: %v", p.dev.Capabilities)
	}
	if remoteKey("androidtv_remote", "OK") != "DPAD_CENTER" || remoteKey("androidtv_remote", "up") != "DPAD_UP" {
		t.Fatal("key names are not translated for Android TV")
	}
}

func TestSyntheticProducts(t *testing.T) {
	states := []haState{
		{EntityID: "sensor.octopus_energy_electricity_17p_161_current_rate", State: "0.066"},
		{EntityID: "sensor.octopus_energy_electricity_17p_161_next_rate", State: "0.358518"},
		{EntityID: "sensor.octopus_energy_electricity_17p_161_previous_accumulative_cost", State: "6.66"},
		{EntityID: "binary_sensor.octopus_energy_electricity_17p_161_off_peak", State: "on"},
		{EntityID: "sensor.octopus_energy_gas_g4p_151_current_rate", State: "0.082851"},
		{EntityID: "person.alex", State: "not_home", Attributes: map[string]any{"friendly_name": "Alex"}},
		{EntityID: "person.sam", State: "home", Attributes: map[string]any{"friendly_name": "Sam"}},
		{EntityID: "person.mqtt_user", State: "unknown"},
	}
	m := build(nil, nil, nil, states)
	e := m.products["ha-energy"]
	if e == nil || e.dev.State["elec_rate_p"] != 6.6 || e.dev.State["elec_next_rate_p"] != 35.9 || e.dev.State["off_peak"] != true || e.dev.State["gas_rate_p"] != 8.3 {
		t.Fatalf("energy = %+v", e)
	}
	p := m.products["ha-people"]
	if p == nil || p.dev.State["anyone_home"] != true || p.dev.State["everyone_home"] != false || p.dev.State["count_home"] != 1.0 {
		t.Fatalf("people = %+v", p)
	}
	// Alex comes home → everyone home
	got := m.apply(haState{EntityID: "person.alex", State: "home", Attributes: map[string]any{"friendly_name": "Alex"}}, false)
	if got == nil || got.dev.State["everyone_home"] != true {
		t.Errorf("after arrival: %+v", got)
	}
}

func TestMultiGangSplitsIntoOneTilePerLight(t *testing.T) {
	devices := []haDevice{{ID: "sw", Name: "Conservatory Wall Switch", Manufacturer: "SONOFF", AreaID: "conservatory"}}
	entities := []haEntity{
		{EntityID: "light.conservatory_wall_switch_switch_1", DeviceID: "sw", Platform: "switch_as_x"},
		{EntityID: "light.conservatory_wall_switch_switch_2", DeviceID: "sw", Platform: "switch_as_x"},
		{EntityID: "switch.conservatory_wall_switch_1", DeviceID: "sw", Platform: "sonoff", HiddenBy: "integration"},
		{EntityID: "switch.conservatory_wall_switch_2", DeviceID: "sw", Platform: "sonoff", HiddenBy: "integration"},
		{EntityID: "select.conservatory_power_on_1", DeviceID: "sw", Platform: "sonoff", EntityCategory: "config"},
	}
	states := []haState{
		{EntityID: "light.conservatory_wall_switch_switch_1", State: "on", Attributes: map[string]any{"friendly_name": "Conservatory Main Lights"}},
		{EntityID: "light.conservatory_wall_switch_switch_2", State: "off", Attributes: map[string]any{"friendly_name": "Conservatory Side Lights"}},
		{EntityID: "switch.conservatory_wall_switch_1", State: "on"},
		{EntityID: "switch.conservatory_wall_switch_2", State: "off"},
		{EntityID: "select.conservatory_power_on_1", State: "off"},
	}
	m := build(nil, devices, entities, states)
	main, side := m.products["ha-sw_conservatory_wall_switch_switch_1"], m.products["ha-sw_conservatory_wall_switch_switch_2"]
	if main == nil || side == nil || m.products["ha-sw"] != nil {
		t.Fatalf("products: %v", keys(m.products))
	}
	if main.dev.Name != "Conservatory Main Lights" || main.dev.Kind != model.KindLight || main.dev.State["on"] != true ||
		main.dev.RoomID != "conservatory" || side.dev.State["on"] != false {
		t.Errorf("main %+v / side %+v", main.dev, side.dev)
	}
	for _, p := range []*product{main, side} {
		for _, e := range p.all {
			if strings.HasPrefix(e, "switch.") {
				t.Errorf("hidden switch %s should be left out", e)
			}
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestAHomePodWithARemoteEntityIsASpeaker(t *testing.T) {
	devices := []haDevice{{ID: "pod", Name: "Living Room HomePod", Manufacturer: "Apple", Model: "AudioAccessory5,1"}}
	entities := []haEntity{
		{EntityID: "media_player.living_room_homepod", DeviceID: "pod", Platform: "apple_tv"},
		{EntityID: "remote.living_room_homepod", DeviceID: "pod", Platform: "apple_tv"},
	}
	states := []haState{
		{EntityID: "media_player.living_room_homepod", State: "idle", Attributes: map[string]any{"volume_level": 0.4}},
		{EntityID: "remote.living_room_homepod", State: "on"},
	}
	p := build(nil, devices, entities, states).products["ha-pod"]
	if p == nil || p.dev.Kind != model.KindSpeaker {
		t.Fatalf("a HomePod should be a speaker: %+v", p)
	}
	if contains(p.dev.Capabilities, model.ActRemote) {
		t.Fatalf("a speaker shouldn't offer a TV remote: %v", p.dev.Capabilities)
	}
}

func TestLeakSmokeAndGarageSensorsAreMapped(t *testing.T) {
	devices := []haDevice{{ID: "leak", Name: "Under sink"}, {ID: "garage", Name: "Garage door"}}
	entities := []haEntity{
		{EntityID: "binary_sensor.under_sink_water", DeviceID: "leak"},
		{EntityID: "sensor.under_sink_lux", DeviceID: "leak"},
		{EntityID: "binary_sensor.garage_door", DeviceID: "garage"},
	}
	states := []haState{
		{EntityID: "binary_sensor.under_sink_water", State: "on", Attributes: map[string]any{"device_class": "moisture"}},
		{EntityID: "sensor.under_sink_lux", State: "40", Attributes: map[string]any{"device_class": "illuminance"}},
		{EntityID: "binary_sensor.garage_door", State: "off", Attributes: map[string]any{"device_class": "garage_door"}, LastChanged: "2026-10-26T18:00:00.123456+00:00"},
	}
	m := build(nil, devices, entities, states)
	leak, garage := m.products["ha-leak"], m.products["ha-garage"]
	if leak == nil || leak.dev.Kind != model.KindSensor || leak.dev.State["leak"] != true || leak.dev.State["illuminance"] != 40.0 {
		t.Fatalf("leak sensor: %+v", leak)
	}
	if garage == nil || garage.dev.State["open"] != false || garage.dev.State["open_at"] != "2026-10-26T18:00:00Z" {
		t.Fatalf("garage door: %+v", garage)
	}
}

func TestTheOhmeCanBeAutomated(t *testing.T) {
	devices := []haDevice{{ID: "ohme", Name: "Ohme Home Pro", Manufacturer: "Ohme"}}
	entities := []haEntity{
		{EntityID: "sensor.ohme_status", DeviceID: "ohme"},
		{EntityID: "sensor.ohme_vehicle_battery", DeviceID: "ohme"},
		{EntityID: "select.ohme_charge_mode", DeviceID: "ohme"},
		{EntityID: "number.ohme_target_percentage", DeviceID: "ohme"},
		{EntityID: "time.ohme_target_time", DeviceID: "ohme"},
	}
	states := []haState{
		{EntityID: "sensor.ohme_status", State: "plugged_in"},
		{EntityID: "sensor.ohme_vehicle_battery", State: "40"},
		{EntityID: "select.ohme_charge_mode", State: "smart_charge"},
		{EntityID: "number.ohme_target_percentage", State: "80"},
		{EntityID: "time.ohme_target_time", State: "07:00:00"},
	}
	p := build(nil, devices, entities, states).products["ha-ohme"]
	if p == nil || p.dev.Kind != model.KindEVCharger {
		t.Fatalf("ohme: %+v", p)
	}
	for _, a := range []string{model.ActSetChargeMode, model.ActSetChargeTo, model.ActSetReadyBy} {
		if !contains(p.dev.Capabilities, a) {
			t.Fatalf("missing %s: %v", a, p.dev.Capabilities)
		}
	}
	if p.dev.State["plugged"] != true {
		t.Fatalf("plugged: %v", p.dev.State)
	}
}
