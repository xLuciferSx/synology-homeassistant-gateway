package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func describeFixture() *mapper {
	st := func(eid, name, class string) haState {
		return haState{EntityID: eid, Attributes: map[string]any{"friendly_name": name, "device_class": class}}
	}
	m := &mapper{states: map[string]haState{}}
	for _, s := range []haState{
		st("binary_sensor.front_door_entry_door", "Front Door", "door"),
		st("binary_sensor.patio_door_entry_door", "Patio Door", "door"),
		st("binary_sensor.conservatory_entry_door", "Conservatory Door", "door"),
		st("binary_sensor.playroom_entry_door", "Playroom Door", "door"),
		st("switch.wifi_smart_switch", "Boiler Controller", ""),
		st("climate.a", "Living Room Thermostat 1", ""), st("climate.b", "Office Thermostat", ""),
		st("climate.c", "Playroom Thermostat", ""), st("climate.d", "Bathroom Thermostat", ""), st("climate.e", "Downstairs Thermostat", ""),
	} {
		m.states[s.EntityID] = s
	}
	return m
}

func describeJSON(t *testing.T, m *mapper, cfg string) (when, do []string) {
	var c map[string]any
	if err := json.Unmarshal([]byte(cfg), &c); err != nil {
		t.Fatal(err)
	}
	out := m.describeAutomation(c)
	return out["when"].([]string), out["do"].([]string)
}

func TestDescribeDoorLeftOpen(t *testing.T) {
	doors := `["binary_sensor.front_door_entry_door","binary_sensor.patio_door_entry_door","binary_sensor.conservatory_entry_door","binary_sensor.playroom_entry_door"]`
	when, do := describeJSON(t, describeFixture(), `{"triggers":[
		{"trigger":"state","entity_id":`+doors+`,"to":"on","for":{"minutes":10},"id":"open10"},
		{"trigger":"state","entity_id":`+doors+`,"to":"on","for":{"minutes":30},"id":"open30"},
		{"trigger":"state","entity_id":`+doors+`,"from":"on","to":"off","id":"closed"}],
	 "actions":[{"choose":[
		{"conditions":[{"condition":"trigger","id":["open10","open30"]},{"condition":"numeric_state","entity_id":"weather.forecast_home","attribute":"temperature","below":15}],
		 "sequence":[{"action":"notify.mobile_app_x","data":{"title":"🚪 {{ trigger.to_state.name }} door open","message":"x"}}]},
		{"conditions":[{"condition":"trigger","id":"closed"}],"sequence":[{"action":"notify.mobile_app_x","data":{"message":"clear_notification"}}]}]}]}`)
	want := []string{
		"Front, Patio, Conservatory or Playroom door has been open for 10 min",
		"Front, Patio, Conservatory or Playroom door has been open for 30 min",
		"Front, Patio, Conservatory or Playroom door closes",
	}
	if strings.Join(when, "|") != strings.Join(want, "|") {
		t.Errorf("when = %q", when)
	}
	wantDo := []string{
		"If Front, Patio, Conservatory or Playroom door has been open for 10 min or 30 min and it's below 15° outside",
		"→ Notify X: “🚪 … door open”",
		"If Front, Patio, Conservatory or Playroom door closes",
		"→ Clear the notification on X",
	}
	if strings.Join(do, "|") != strings.Join(wantDo, "|") {
		t.Errorf("do = %q", do)
	}
}

func TestDescribeBoiler(t *testing.T) {
	trvs := `["climate.a","climate.b","climate.c","climate.d","climate.e"]`
	when, do := describeJSON(t, describeFixture(), `{"triggers":[
		{"trigger":"state","entity_id":`+trvs+`,"attribute":"hvac_action","to":"heating"},
		{"trigger":"state","entity_id":`+trvs+`,"attribute":"temperature"},
		{"trigger":"state","entity_id":"switch.wifi_smart_switch","to":"off","for":{"seconds":30},"id":"switched_off"},
		{"trigger":"time_pattern","minutes":"/5"},
		{"trigger":"homeassistant","event":"start"},
		{"trigger":"template","value_template":"{{ x }}","for":{"minutes":15},"id":"no_demand"}],
	 "actions":[{"variables":{"demand":"x"}},{"choose":[
		{"conditions":[{"condition":"template","value_template":"{{ demand }}"},{"condition":"state","entity_id":"switch.wifi_smart_switch","state":"off"}],
		 "sequence":[{"action":"switch.turn_on","target":{"entity_id":"switch.wifi_smart_switch"}},
		   {"if":[{"condition":"trigger","id":"switched_off"}],"then":[{"action":"notify.mobile_app_x","data":{"title":"🔥 Boiler switched back on","message":"m"}}]}]},
		{"conditions":[{"condition":"trigger","id":"no_demand"},{"condition":"state","entity_id":"switch.wifi_smart_switch","state":"on"}],
		 "sequence":[{"action":"switch.turn_off","target":{"entity_id":"switch.wifi_smart_switch"}}]}]}]}`)
	want := []string{
		"Any of 5 thermostats starts heating",
		"Any of 5 thermostats changes its target temperature",
		"Boiler Controller has been off for 30 sec",
		"Every 5 minutes",
		"Home Assistant starts",
		"“No demand” is true for 15 min",
	}
	if strings.Join(when, "|") != strings.Join(want, "|") {
		t.Errorf("when = %q", when)
	}
	wantDo := []string{
		"If “demand” is true and Boiler Controller is off",
		"→ Turn on Boiler Controller",
		"→ If Boiler Controller has been off for 30 sec",
		"→ → Notify X: “🔥 Boiler switched back on”",
		"If “No demand” is true for 15 min and Boiler Controller is on",
		"→ Turn off Boiler Controller",
	}
	if strings.Join(do, "|") != strings.Join(wantDo, "|") {
		t.Errorf("do = %q", do)
	}
}

func TestDescribeHomeAndWaits(t *testing.T) {
	m := describeFixture()
	m.states["zone.home"] = haState{EntityID: "zone.home", Attributes: map[string]any{"friendly_name": "Home"}}
	m.states["sensor.wm"] = haState{EntityID: "sensor.wm", Attributes: map[string]any{"friendly_name": "Washing Machine Machine state"}}
	m.deviceNames = map[string]string{"p": "Alex's iPhone"}
	when, do := describeJSON(t, m, `{"triggers":[
		{"trigger":"numeric_state","entity_id":"zone.home","below":1,"for":{"minutes":10}},
		{"trigger":"numeric_state","entity_id":"zone.home","above":0},
		{"trigger":"event","event_type":"mobile_app_notification_action"},
		{"trigger":"state","entity_id":"sensor.wm","to":"stop"}],
	 "actions":[{"action":"notify.mobile_app_alexs_iphone","data":{"message":"Hi"}},
		{"wait_for_trigger":[{"trigger":"state","entity_id":"binary_sensor.front_door_entry_door","to":"off"}],"timeout":"00:05:00"},
		{"repeat":{"for_each":["a","b"],"sequence":[{"action":"switch.turn_off","target":{"entity_id":"switch.wifi_smart_switch"}}]}},
		{"if":[{"condition":"template","value_template":"{{ is_state('switch.wifi_smart_switch','on') }}"}],"then":[{"action":"vacuum.start","target":{"entity_id":"{{ hoovers }}"}}]}]}`)
	want := "Everyone has left home for 10 min|Someone is home|You tap a button on a notification|Washing Machine state becomes stop"
	if strings.Join(when, "|") != want {
		t.Errorf("when = %q", when)
	}
	wantDo := "Notify Alex's iPhone: “Hi”|Wait until Front Door closes (up to 5 min)|For each of 2 items|→ Turn off Boiler Controller|If Boiler Controller is on|→ Start the chosen hoovers"
	if strings.Join(do, "|") != wantDo {
		t.Errorf("do = %q", do)
	}
}
