package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

// loadAutomationConfigs reads every automation's config so the app can show it in words
// and, where it only uses things Hearth understands, offer it as a Hearth automation.
func (m *mapper) loadAutomationConfigs(ctx context.Context, c *haClient) {
	p := m.products["ha-automations"]
	if p == nil {
		return
	}
	for _, eid := range p.all {
		var out struct {
			Config map[string]any `json:"config"`
		}
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.call(qctx, map[string]any{"type": "automation/config", "entity_id": eid}, &out)
		cancel()
		if err != nil || out.Config == nil {
			continue
		}
		m.autoConfigs[eid] = out.Config
	}
	m.refresh(p)
}

// editAutomation renames or deletes an automation through Home Assistant's config API.
func (c *haClient) editAutomation(ctx context.Context, eid, op string, value any) error {
	var out struct {
		Config map[string]any `json:"config"`
	}
	if err := c.call(ctx, map[string]any{"type": "automation/config", "entity_id": eid}, &out); err != nil {
		return err
	}
	id, _ := out.Config["id"].(string)
	if id == "" {
		return fmt.Errorf("this automation is written in YAML, so it can only be changed in Home Assistant")
	}
	url := strings.TrimRight(c.baseURL, "/") + "/api/config/automation/config/" + id
	var req *http.Request
	var err error
	switch op {
	case "delete":
		req, err = http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	default:
		name, _ := value.(string)
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("give the automation a name")
		}
		out.Config["alias"] = name
		body, _ := json.Marshal(out.Config)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if req != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("home assistant said %s", resp.Status)
	}
	slog.Info("automation edited", "entity", eid, "op", op)
	return nil
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case []any:
		parts := []string{}
		for _, e := range x {
			parts = append(parts, str(e))
		}
		return strings.Join(parts, ", ")
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func list(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case nil:
		return nil
	}
	return []any{v}
}

func obj(v any) map[string]any { o, _ := v.(map[string]any); return o }

func (m *mapper) name(eid string) string {
	if strings.Contains(eid, "{") {
		return "the chosen items"
	}
	if s, ok := m.states[eid]; ok {
		if n, _ := s.Attributes["friendly_name"].(string); n != "" {
			return tidyName(n)
		}
	}
	return capFirst(strings.ReplaceAll(strings.TrimPrefix(eid, domainOf(eid)+"."), "_", " "))
}

var serialInName = regexp.MustCompile(`\s*\([^)]*\d{5,}[^)]*\)`)

// tidyName drops meter serials and the doubled words integrations leave behind: "Washing Machine Machine state"
// reads "Washing Machine state".
func tidyName(n string) string {
	n = serialInName.ReplaceAllString(n, "")
	words := strings.Fields(n)
	out := []string{}
	for i, w := range words {
		if i > 0 && strings.EqualFold(w, words[i-1]) {
			continue
		}
		out = append(out, w)
	}
	return strings.Join(out, " ")
}

// domainNoun names a group of entities by what they are when their names don't share a word.
func domainNoun(domain string) string {
	return map[string]string{
		"climate": "thermostats", "light": "lights", "switch": "switches", "binary_sensor": "sensors", "sensor": "sensors",
		"media_player": "speakers and TVs", "vacuum": "hoovers", "lock": "locks", "cover": "blinds", "fan": "fans",
		"person": "people", "device_tracker": "phones", "update": "updates", "automation": "automations",
	}[domain]
}

func (m *mapper) names(v any) string { return m.nameList(v, "or") }

// nameList reads a list of entities the way a person would say it: "Front, Patio or Playroom door" when they
// share a last word, "any of 10 thermostats" (or "all 10" for actions) when there are too many to read out.
func (m *mapper) nameList(v any, conj string) string {
	parts, domains := []string{}, map[string]bool{}
	for _, e := range list(v) {
		parts = append(parts, m.name(str(e)))
		domains[domainOf(str(e))] = true
	}
	out := joinNames(parts, conj)
	if strings.HasSuffix(out, " devices") && len(domains) == 1 {
		for d := range domains {
			if n := domainNoun(d); n != "" {
				out = strings.TrimSuffix(out, "devices") + n
			}
		}
	}
	return out
}

func joinNames(parts []string, conj string) string {
	switch len(parts) {
	case 0:
		return "something"
	case 1:
		return parts[0]
	}
	last := func(s string) string {
		f := strings.Fields(s)
		if len(f) < 2 {
			return ""
		}
		return f[len(f)-1]
	}
	noun := last(parts[0])
	for _, p := range parts[1:] {
		if !strings.EqualFold(last(p), noun) {
			noun = ""
			break
		}
	}
	if len(parts) > 4 {
		many := "devices"
		if common := commonNoun(parts); common != "" {
			many = plural(strings.ToLower(common))
		}
		if conj == "and" {
			return fmt.Sprintf("all %d %s", len(parts), many)
		}
		return fmt.Sprintf("any of %d %s", len(parts), many)
	}
	if noun != "" {
		heads := make([]string, len(parts))
		short := true
		for i, p := range parts {
			heads[i] = strings.TrimSpace(p[:len(p)-len(noun)])
			short = short && len(strings.Fields(heads[i])) <= 2
		}
		if short {
			return listJoin(heads, conj) + " " + strings.ToLower(noun)
		}
	}
	return listJoin(parts, conj)
}

// commonNoun is the last word every name shares, skipping numbers: "Living Room Thermostat 1" → "Thermostat".
func commonNoun(parts []string) string {
	noun := ""
	for i, p := range parts {
		f := strings.Fields(p)
		for len(f) > 0 && strings.Trim(f[len(f)-1], "0123456789") == "" {
			f = f[:len(f)-1]
		}
		if len(f) < 2 {
			return ""
		}
		if w := f[len(f)-1]; i == 0 {
			noun = w
		} else if !strings.EqualFold(w, noun) {
			return ""
		}
	}
	return noun
}

func plural(s string) string {
	if strings.HasSuffix(s, "y") && !strings.ContainsAny(s[len(s)-2:len(s)-1], "aeiou") {
		return s[:len(s)-1] + "ies"
	}
	for _, end := range []string{"s", "sh", "ch", "x"} {
		if strings.HasSuffix(s, end) {
			return s + "es"
		}
	}
	return s + "s"
}

func listJoin(p []string, conj string) string {
	if len(p) == 1 {
		return p[0]
	}
	return strings.Join(p[:len(p)-1], ", ") + " " + conj + " " + p[len(p)-1]
}

func (m *mapper) targetNames(a map[string]any) string {
	t := obj(a["target"])
	for _, src := range []map[string]any{t, obj(a["data"]), a} {
		if src == nil {
			continue
		}
		if e := src["entity_id"]; e != nil {
			return m.names(e)
		}
		if d := src["device_id"]; d != nil {
			parts := []string{}
			for _, id := range list(d) {
				parts = append(parts, m.deviceNames[str(id)])
			}
			return strings.Join(parts, ", ")
		}
		if ar := src["area_id"]; ar != nil {
			parts := []string{}
			for _, id := range list(ar) {
				parts = append(parts, m.areas[str(id)].Name)
			}
			return "everything in " + strings.Join(parts, ", ")
		}
	}
	return ""
}

func offsetText(v any) string {
	s := str(v)
	if s == "" || s == "0" || s == "00:00:00" {
		return ""
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	d := duration(s)
	if d == 0 {
		return ""
	}
	if neg {
		return humanDuration(d) + " before "
	}
	return humanDuration(d) + " after "
}

func duration(v any) time.Duration {
	switch x := v.(type) {
	case float64:
		return time.Duration(x) * time.Second
	case map[string]any:
		var d time.Duration
		for k, unit := range map[string]time.Duration{"hours": time.Hour, "minutes": time.Minute, "seconds": time.Second} {
			if n, ok := x[k].(float64); ok {
				d += time.Duration(n * float64(unit))
			}
		}
		return d
	case string:
		parts := strings.Split(x, ":")
		var d time.Duration
		units := []time.Duration{time.Hour, time.Minute, time.Second}
		if len(parts) == 2 {
			units = units[:2]
		}
		for i, p := range parts {
			if i >= len(units) {
				break
			}
			var n int
			fmt.Sscanf(p, "%d", &n)
			d += time.Duration(n) * units[i]
		}
		return d
	}
	return 0
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%d h", int(d.Hours()))
	case d >= time.Hour:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%d sec", int(d.Seconds()))
}

func hhmm(v any) string {
	s := str(v)
	if len(s) >= 5 && s[2] == ':' {
		return s[:5]
	}
	return s
}

var weekdays = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

func kind(cfg map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := cfg[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func (m *mapper) deviceClass(eid string) string {
	if s, ok := m.states[eid]; ok {
		c, _ := s.Attributes["device_class"].(string)
		return c
	}
	return ""
}

// sensorKind groups binary sensor classes that read the same way.
func (m *mapper) sensorKind(eid string) string {
	if domainOf(eid) != "binary_sensor" {
		return domainOf(eid)
	}
	switch m.deviceClass(eid) {
	case "door", "window", "garage_door", "opening":
		return "opening"
	case "motion", "occupancy", "presence":
		return "motion"
	case "moisture":
		return "water"
	case "smoke", "gas", "carbon_monoxide":
		return "smoke"
	case "connectivity":
		return "connectivity"
	}
	return "binary"
}

// stateAdj is a state as an adjective: "open", "clear", "away", for "has been open for 10 min" or "is off".
func (m *mapper) stateAdj(eid, s string) string {
	on := s == "on"
	switch m.sensorKind(eid) + "/" + s {
	case "opening/on", "opening/off":
		return map[bool]string{true: "open", false: "closed"}[on]
	case "motion/on":
		return "detecting someone"
	case "water/on":
		return "wet"
	case "water/off":
		return "dry"
	case "smoke/on":
		return "detecting smoke"
	case "motion/off", "smoke/off":
		return "clear"
	case "connectivity/on":
		return "online"
	case "connectivity/off":
		return "offline"
	}
	switch s {
	case "not_home":
		return "away"
	case "unavailable":
		return "offline"
	case "error":
		return "in error"
	}
	return strings.ReplaceAll(s, "_", " ")
}

// stateVerb is the moment of change: "opens", "detects motion", "turns off", "arrives home".
func (m *mapper) stateVerb(eid, s string) string {
	switch m.sensorKind(eid) + "/" + s {
	case "opening/on":
		return "opens"
	case "opening/off":
		return "closes"
	case "motion/on":
		return "detects motion"
	case "water/on":
		return "detects water"
	case "smoke/on":
		return "detects smoke"
	case "motion/off", "water/off", "smoke/off":
		return "clears"
	case "connectivity/on":
		return "comes online"
	case "connectivity/off":
		return "goes offline"
	}
	if domainOf(eid) == "climate" {
		return "switches to " + strings.ReplaceAll(s, "_", " ")
	}
	switch s {
	case "on":
		return "turns on"
	case "off":
		return "turns off"
	case "home":
		return "arrives home"
	case "not_home":
		return "leaves home"
	case "unavailable":
		return "goes offline"
	}
	return "becomes " + m.stateAdj(eid, s)
}

func attrName(a string) string {
	switch a {
	case "temperature":
		return "target temperature"
	case "current_temperature":
		return "temperature"
	case "hvac_action":
		return "activity"
	}
	return strings.ReplaceAll(a, "_", " ")
}

func attrChange(attr string, to any) string {
	if attr == "hvac_action" && to != nil {
		switch str(to) {
		case "heating":
			return "starts heating"
		case "cooling":
			return "starts cooling"
		case "idle":
			return "stops heating"
		}
	}
	if to == nil {
		return "changes its " + attrName(attr)
	}
	return "changes its " + attrName(attr) + " to " + strings.ReplaceAll(str(to), "_", " ")
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

func idWords(id string) string { return strings.ReplaceAll(strings.ReplaceAll(id, "_", " "), "-", " ") }

// describeTrigger reads one trigger without a leading "When" (the card already says it): "Front door opens",
// "Any of 10 thermostats starts heating", "Boiler Controller has been off for 30 sec".
func (m *mapper) describeTrigger(t map[string]any) string {
	return capFirst(m.triggerPhrase(t))
}

func (m *mapper) triggerPhrase(t map[string]any) string {
	d := duration(t["for"])
	forText := ""
	if d > 0 {
		forText = " for " + humanDuration(d)
	}
	switch kind(t, "trigger", "platform") {
	case "time":
		out := []string{}
		for _, a := range list(t["at"]) {
			if strings.Contains(str(a), ".") {
				out = append(out, "the time in "+m.name(str(a)))
			} else {
				out = append(out, hhmm(a))
			}
		}
		return "at " + strings.Join(out, " or ")
	case "sun":
		return "at " + offsetText(t["offset"]) + str(t["event"])
	case "state":
		eids := list(t["entity_id"])
		if len(eids) == 0 {
			return "something changes"
		}
		subject, first := m.names(t["entity_id"]), str(eids[0])
		if attr := str(t["attribute"]); attr != "" {
			return subject + " " + attrChange(attr, t["to"]) + forText
		}
		to := t["to"]
		if to == nil {
			if from := t["from"]; from != nil {
				return subject + " stops being " + m.stateAdj(first, str(from)) + forText
			}
			return subject + " changes"
		}
		adjs, verbs := []string{}, []string{}
		for _, s := range list(to) {
			adjs, verbs = append(adjs, m.stateAdj(first, str(s))), append(verbs, m.stateVerb(first, str(s)))
		}
		if d > 0 {
			return subject + " has been " + listJoin(adjs, "or") + forText
		}
		for _, lead := range []string{"becomes ", "switches to "} {
			if len(verbs) > 1 && strings.HasPrefix(verbs[0], lead) {
				return subject + " " + lead + listJoin(adjs, "or")
			}
		}
		return subject + " " + listJoin(verbs, "or")
	case "numeric_state":
		if eid := str(t["entity_id"]); domainOf(eid) == "zone" && str(t["attribute"]) == "" {
			place := strings.ToLower(m.name(eid))
			if b := t["below"]; b != nil && str(b) == "1" {
				return "everyone has left " + place + forText
			}
			if a := t["above"]; a != nil && str(a) == "0" {
				return "someone is " + map[bool]string{true: "home", false: "at " + place}[place == "home"] + forText
			}
		}
		subject := m.names(t["entity_id"])
		if attr := str(t["attribute"]); attr != "" {
			subject += "'s " + attrName(attr)
		}
		parts := []string{}
		if a := t["above"]; a != nil {
			parts = append(parts, "goes above "+str(a))
		}
		if b := t["below"]; b != nil {
			parts = append(parts, "goes below "+str(b))
		}
		return subject + " " + strings.Join(parts, " and ") + forText
	case "device":
		name := m.deviceNames[str(t["device_id"])]
		if name == "" {
			name = "a device"
		}
		what := strings.ReplaceAll(str(t["type"]), "_", " ")
		if sub := str(t["subtype"]); sub != "" {
			what = strings.ReplaceAll(sub, "_", " ") + " " + what
		}
		return name + " reports " + strings.TrimSpace(what)
	case "time_pattern":
		if mins := str(t["minutes"]); strings.HasPrefix(mins, "/") {
			return "every " + strings.TrimPrefix(mins, "/") + " minutes"
		}
		if h := str(t["hours"]); strings.HasPrefix(h, "/") {
			return "every " + strings.TrimPrefix(h, "/") + " hours"
		}
		return "on a repeating schedule"
	case "homeassistant":
		if str(t["event"]) == "shutdown" {
			return "Home Assistant shuts down"
		}
		return "Home Assistant starts"
	case "zone":
		return m.names(t["entity_id"]) + " " + str(t["event"]) + "s " + m.name(str(t["zone"]))
	case "template":
		if id := str(t["id"]); id != "" {
			return "“" + capFirst(idWords(id)) + "” is true" + forText
		}
		return "a custom check is true" + forText
	case "event":
		if str(t["event_type"]) == "mobile_app_notification_action" {
			return "you tap a button on a notification"
		}
		return "the " + strings.ReplaceAll(str(t["event_type"]), "_", " ") + " event happens"
	case "mqtt":
		return "an MQTT message arrives"
	case "calendar":
		return "a calendar event " + str(t["event"]) + "s"
	case "webhook":
		return "a webhook is called"
	}
	return "“" + idWords(kind(t, "trigger", "platform")) + "” happens"
}

// describer reads conditions and actions; it knows the automation's trigger ids, so "for trigger open10"
// can be read back as "Front door has been open for 10 min".
type describer struct {
	*mapper
	triggers map[string]string
}

func (m *describer) describeCondition(c map[string]any) string { return capFirst(m.cond(c)) }

func (m *describer) cond(c map[string]any) string {
	switch kind(c, "condition") {
	case "time":
		parts := []string{}
		if a := c["after"]; a != nil {
			parts = append(parts, "after "+hhmm(a))
		}
		if b := c["before"]; b != nil {
			parts = append(parts, "before "+hhmm(b))
		}
		if w := c["weekday"]; w != nil {
			days := []string{}
			for _, d := range list(w) {
				days = append(days, capFirst(str(d)))
			}
			parts = append(parts, "on "+listJoin(days, "or"))
		}
		return strings.Join(parts, " and ")
	case "state":
		eids := list(c["entity_id"])
		first := ""
		if len(eids) > 0 {
			first = str(eids[0])
		}
		subject := m.nameList(c["entity_id"], "and")
		if attr := str(c["attribute"]); attr != "" {
			vals := []string{}
			for _, v := range list(c["state"]) {
				vals = append(vals, strings.ReplaceAll(str(v), "_", " "))
			}
			return subject + "'s " + attrName(attr) + " is " + listJoin(vals, "or")
		}
		states := []string{}
		for _, s := range list(c["state"]) {
			states = append(states, m.stateAdj(first, str(s)))
		}
		verb := " is "
		if len(eids) > 1 {
			verb = " are "
		}
		return subject + verb + listJoin(states, "or")
	case "numeric_state":
		eid, attr := str(c["entity_id"]), str(c["attribute"])
		parts := []string{}
		unit := ""
		if domainOf(eid) == "weather" && attr == "temperature" {
			unit = "°"
		}
		if a := c["above"]; a != nil {
			parts = append(parts, "above "+str(a)+unit)
		}
		if b := c["below"]; b != nil {
			parts = append(parts, "below "+str(b)+unit)
		}
		if unit != "" {
			return "it's " + strings.Join(parts, " and ") + " outside"
		}
		subject := m.nameList(c["entity_id"], "and")
		if attr != "" {
			subject += "'s " + attrName(attr)
		}
		return subject + " is " + strings.Join(parts, " and ")
	case "sun":
		parts := []string{}
		if a := c["after"]; a != nil {
			parts = append(parts, "after "+str(a))
		}
		if b := c["before"]; b != nil {
			parts = append(parts, "before "+str(b))
		}
		return strings.Join(parts, " and ")
	case "and", "or", "not":
		inner := []string{}
		for _, x := range list(c["conditions"]) {
			inner = append(inner, m.cond(obj(x)))
		}
		switch kind(c, "condition") {
		case "or":
			return listJoin(inner, "or")
		case "not":
			if len(inner) == 1 && strings.Count(inner[0], " is ") == 1 {
				return strings.Replace(inner[0], " is ", " isn't ", 1)
			}
			return "not (" + listJoin(inner, "or") + ")"
		}
		return listJoin(inner, "and")
	case "template":
		return m.templateCheck(str(c["value_template"]))
	case "trigger":
		return m.firedBy(list(c["id"]))
	case "zone":
		return m.names(c["entity_id"]) + " is in " + m.name(str(c["zone"]))
	case "device":
		return m.deviceNames[str(c["device_id"])] + " is " + strings.ReplaceAll(str(c["type"]), "_", " ")
	}
	return "a " + kind(c, "condition") + " check passes"
}

// firedBy reads trigger ids back as what happened, merging "X has been open for 10 min" and "… for 30 min"
// into "X has been open for 10 min or 30 min".
func (m *describer) firedBy(ids []any) string {
	texts := []string{}
	for _, id := range ids {
		t := m.triggers[str(id)]
		if t == "" {
			t = "“" + idWords(str(id)) + "” set it off"
		}
		if strings.HasPrefix(t, "at ") {
			t = "it's " + strings.TrimPrefix(t, "at ")
		}
		texts = append(texts, t)
	}
	if len(texts) > 1 {
		head, tails := "", []string{}
		for _, t := range texts {
			i := strings.LastIndex(t, " for ")
			if i < 0 || (head != "" && t[:i] != head) {
				return listJoin(texts, "or")
			}
			head, tails = t[:i], append(tails, t[i+5:])
		}
		return head + " for " + listJoin(tails, "or")
	}
	return listJoin(texts, "or")
}

var verbs = map[string]string{
	"turn_on": "Turn on", "turn_off": "Turn off", "toggle": "Toggle", "lock": "Lock", "unlock": "Unlock",
	"open_cover": "Open", "close_cover": "Close", "start": "Start", "return_to_base": "Send to dock", "pause": "Pause",
	"media_play_pause": "Play/pause", "media_play": "Play", "media_pause": "Pause", "set_temperature": "Set temperature of",
	"set_hvac_mode": "Set mode of", "set_preset_mode": "Set mode of", "set_percentage": "Set speed of", "press": "Press",
	"select_option": "Choose an option on", "set_value": "Set", "trigger": "Run", "reload": "Reload",
}

var (
	varIsTrue  = regexp.MustCompile(`^\{\{\s*\(?\s*([a-z_]+)\s*(\|\s*bool\s*)?\)?\s*\}\}$`)
	varIsFalse = regexp.MustCompile(`^\{\{\s*not\s*\(?\s*([a-z_]+)\s*(\|\s*bool\s*)?\)?\s*\}\}$`)
	stateIs    = regexp.MustCompile(`^\{\{\s*(not\s+)?is_state\(\s*'([a-z_]+\.[a-z0-9_]+)'\s*,\s*'([^']+)'\s*\)\s*\}\}$`)
)

// templateCheck reads the simple templates people write by hand; anything else is "a custom check".
func (m *describer) templateCheck(t string) string {
	t = strings.TrimSpace(t)
	if g := varIsTrue.FindStringSubmatch(t); g != nil {
		return "“" + idWords(g[1]) + "” is true"
	}
	if g := varIsFalse.FindStringSubmatch(t); g != nil {
		return "“" + idWords(g[1]) + "” is false"
	}
	if g := stateIs.FindStringSubmatch(t); g != nil {
		verb := " is "
		if g[1] != "" {
			verb = " isn't "
		}
		return m.name(g[2]) + verb + m.stateAdj(g[2], g[3])
	}
	return "a custom check passes"
}

var jinja = regexp.MustCompile(`\{\{.*?\}\}|\{%.*?%\}`)

// plain hides template code in a message: "It's been open for {{ … }} minutes" reads "It's been open for … minutes".
func plain(s string) string {
	s = jinja.ReplaceAllString(s, "…")
	return strings.Join(strings.Fields(s), " ")
}

func (m *describer) condList(conds []any) string {
	parts := []string{}
	for _, c := range conds {
		if co := obj(c); co != nil {
			parts = append(parts, m.cond(co))
		} else {
			parts = append(parts, m.templateCheck(str(c)))
		}
	}
	return listJoin(parts, "and")
}

// phone names the phone a mobile_app notify service sends to: mobile_app_alexs_iphone → "Alex's iPhone".
func (m *describer) phone(service string) string {
	slug, ok := strings.CutPrefix(service, "mobile_app_")
	if !ok {
		return ""
	}
	clean := regexp.MustCompile(`[^a-z0-9]+`)
	for _, n := range m.deviceNames {
		if strings.Trim(clean.ReplaceAllString(strings.ToLower(strings.NewReplacer("'", "", "’", "").Replace(n)), "_"), "_") == slug {
			return n
		}
	}
	words := strings.Split(slug, "_")
	for i, w := range words {
		switch {
		case w == "iphone" || w == "ipad":
			words[i] = "i" + strings.ToUpper(w[1:2]) + w[2:]
		case i == 0 && len(words) > 1 && strings.HasSuffix(w, "s"):
			words[i] = capFirst(w[:len(w)-1]) + "'s"
		default:
			words[i] = capFirst(w)
		}
	}
	return strings.Join(words, " ")
}

// branch reads an If/Otherwise block as a heading line followed by its steps, each marked "→ " one level deeper.
func (m *describer) branch(conds []any, then []any) []string {
	parts := []string{}
	for _, c := range conds {
		if co := obj(c); co != nil {
			parts = append(parts, m.cond(co))
		} else {
			parts = append(parts, m.templateCheck(str(c)))
		}
	}
	out := []string{"If " + listJoin(parts, "and")}
	return append(out, m.nested(then)...)
}

func (m *describer) nested(steps []any) []string {
	out := []string{}
	for _, x := range steps {
		for _, line := range m.describeAction(obj(x)) {
			out = append(out, "→ "+line)
		}
	}
	return out
}

func (m *describer) describeAction(a map[string]any) []string {
	if svc := kind(a, "action", "service"); strings.Contains(svc, "{") {
		return []string{"Run the chosen action"}
	} else if svc != "" {
		dom, s, _ := strings.Cut(svc, ".")
		target := m.actionTargets(a)
		data := obj(a["data"])
		switch {
		case dom == "notify" || dom == "persistent_notification":
			msg := str(data["message"])
			to := m.phone(s)
			if msg == "clear_notification" {
				return []string{"Clear the notification" + ifNonEmpty(" on ", to, "")}
			}
			if t := str(data["title"]); t != "" {
				msg = t
			}
			msg = plain(msg)
			if r := []rune(msg); len(r) > 70 {
				msg = string(r[:70]) + "…"
			}
			lead := "Send a notification"
			if to != "" {
				lead = "Notify " + to
			}
			return []string{lead + ifNonEmpty(": “", msg, "”")}
		case svc == "weather.get_forecasts":
			return []string{"Check the forecast"}
		case svc == "scene.create":
			return []string{"Remember how things are set now"}
		case svc == "cast.show_lovelace_view":
			return []string{"Show the dashboard on " + m.name(str(data["entity_id"]))}
		case svc == "input_datetime.set_datetime":
			return []string{"Note the time in " + target}
		case dom == "backup":
			return []string{"Make a backup"}
		case dom == "update" && s == "install":
			if strings.HasPrefix(target, "the chosen") {
				return []string{"Install the waiting updates"}
			}
			return []string{"Install " + target}
		case dom == "alarm_control_panel":
			switch s {
			case "alarm_disarm":
				return []string{"Disarm " + target}
			default:
				return []string{"Arm " + target + " (" + strings.ReplaceAll(strings.TrimPrefix(s, "alarm_arm_"), "_", " ") + ")"}
			}
		case s == "set_hvac_mode" && data["hvac_mode"] != nil:
			return []string{"Set " + target + " to " + plain(str(data["hvac_mode"]))}
		case dom == "scene" && s == "turn_on":
			return []string{"Activate " + target}
		case dom == "script":
			if s == "turn_on" {
				return []string{"Run " + target}
			}
			return []string{"Run " + m.name(svc)}
		case s == "set_temperature" && data["temperature"] != nil:
			return []string{"Set " + target + " to " + plain(str(data["temperature"])) + "°"}
		case s == "turn_on" && data["brightness_pct"] != nil:
			return []string{"Dim " + target + " to " + str(data["brightness_pct"]) + "%"}
		case s == "set_preset_mode":
			return []string{"Set " + target + " to " + str(data["preset_mode"])}
		}
		verb := verbs[s]
		if verb == "" {
			verb = capFirst(strings.ReplaceAll(s, "_", " "))
		}
		return []string{strings.TrimSpace(verb + " " + target)}
	}
	switch {
	case a["delay"] != nil:
		return []string{"Wait " + humanDuration(duration(a["delay"]))}
	case a["wait_for_trigger"] != nil:
		events := []string{}
		for _, t := range list(a["wait_for_trigger"]) {
			if e := m.triggerPhrase(obj(t)); !slices.Contains(events, e) {
				events = append(events, e)
			}
		}
		w := "Wait until " + listJoin(events, "or")
		if d := duration(a["timeout"]); d > 0 {
			w += " (up to " + humanDuration(d) + ")"
		}
		return []string{w}
	case a["wait_template"] != nil:
		return []string{"Wait until a condition is true"}
	case a["device_id"] != nil && a["type"] != nil:
		target := m.deviceNames[str(a["device_id"])]
		if eid := m.registryIDs[str(a["entity_id"])]; eid != "" {
			target = m.name(eid)
		}
		verb := verbs[str(a["type"])]
		if verb == "" {
			verb = capFirst(strings.ReplaceAll(str(a["type"]), "_", " "))
		}
		return []string{verb + " " + target}
	case a["choose"] != nil:
		out := []string{}
		for _, o := range list(a["choose"]) {
			opt := obj(o)
			out = append(out, m.branch(list(opt["conditions"]), list(opt["sequence"]))...)
		}
		if d := list(a["default"]); len(d) > 0 {
			out = append(out, "Otherwise")
			out = append(out, m.nested(d)...)
		}
		return out
	case a["if"] != nil:
		out := m.branch(list(a["if"]), list(a["then"]))
		if e := list(a["else"]); len(e) > 0 {
			out = append(out, "Otherwise")
			out = append(out, m.nested(e)...)
		}
		return out
	case a["repeat"] != nil:
		r := obj(a["repeat"])
		head := "Repeat"
		switch {
		case r["for_each"] != nil:
			if items, ok := r["for_each"].([]any); ok {
				head = fmt.Sprintf("For each of %d items", len(items))
			} else {
				head = "For each item"
			}
		case r["count"] != nil:
			head = "Repeat " + plain(str(r["count"])) + " times"
		case r["while"] != nil:
			head = "Repeat while " + m.condList(list(r["while"]))
		case r["until"] != nil:
			head = "Repeat until " + m.condList(list(r["until"]))
		}
		return append([]string{head}, m.nested(list(r["sequence"]))...)
	case a["condition"] != nil:
		return []string{"Continue only if " + m.cond(a)}
	case a["scene"] != nil:
		return []string{"Activate " + m.name(str(a["scene"]))}
	case a["event"] != nil:
		return []string{"Fire the " + str(a["event"]) + " event"}
	case a["variables"] != nil:
		return nil
	case a["stop"] != nil:
		return []string{"Stop"}
	case a["parallel"] != nil:
		return []string{"Do several things at once"}
	case a["sequence"] != nil:
		out := []string{}
		for _, x := range list(a["sequence"]) {
			out = append(out, m.describeAction(obj(x))...)
		}
		return out
	}
	return []string{"Do something custom"}
}

// actionTargets names what an action acts on, reading a long list as "all 10 thermostats".
func (m *describer) actionTargets(a map[string]any) string {
	dom, _, _ := strings.Cut(kind(a, "action", "service"), ".")
	for _, src := range []map[string]any{obj(a["target"]), obj(a["data"]), a} {
		if e := src["entity_id"]; src != nil && e != nil {
			out := m.nameList(e, "and")
			if out == "the chosen items" && domainNoun(dom) != "" {
				out = "the chosen " + domainNoun(dom)
			}
			return out
		}
	}
	return m.targetNames(a)
}

func ifNonEmpty(pre, s, post string) string {
	if s == "" {
		return ""
	}
	return pre + s + post
}

func (m *mapper) describeAutomation(cfg map[string]any) map[string]any {
	if bp := obj(cfg["use_blueprint"]); bp != nil {
		path := str(bp["path"])
		name := strings.TrimSuffix(path[strings.LastIndex(path, "/")+1:], ".yaml")
		inputs := []string{}
		for k, v := range obj(bp["input"]) {
			val := str(v)
			if o := obj(v); o != nil {
				val = m.targetNames(map[string]any{"target": o})
			} else if strings.Contains(val, ".") && !strings.Contains(val, " ") {
				val = m.name(val)
			} else if d := duration(v); d > 0 && fmt.Sprint(v) == str(v) {
				if _, isNum := v.(float64); isNum {
					val = humanDuration(d)
				}
			}
			inputs = append(inputs, strings.ReplaceAll(k, "_", " ")+": "+val)
		}
		return map[string]any{"blueprint": strings.ReplaceAll(name, "_", " "), "do": inputs}
	}
	when, onlyIf, do := []string{}, []string{}, []string{}
	r := &describer{mapper: m, triggers: map[string]string{}}
	for _, t := range list(first(cfg, "triggers", "trigger")) {
		line := m.describeTrigger(obj(t))
		when = append(when, line)
		if id := str(obj(t)["id"]); id != "" {
			r.triggers[id] = m.triggerPhrase(obj(t))
		}
	}
	for _, c := range list(first(cfg, "conditions", "condition")) {
		onlyIf = append(onlyIf, r.describeCondition(obj(c)))
	}
	for _, a := range list(first(cfg, "actions", "action")) {
		do = append(do, r.describeAction(obj(a))...)
	}
	out := map[string]any{"when": when, "only_if": onlyIf, "do": do}
	if d := str(cfg["description"]); d != "" {
		out["description"] = d
	}
	return out
}

func first(cfg map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := cfg[k]; ok {
			return v
		}
	}
	return nil
}

// convertAutomation rewrites an automation as a Hearth one when every part of it maps onto
// Hearth devices and features; otherwise it returns nil and the app offers rename/delete only.
func (m *mapper) convertAutomation(cfg map[string]any) map[string]any {
	if cfg["use_blueprint"] != nil {
		return nil
	}
	triggers := []map[string]any{}
	var days []int
	conditions := []map[string]any{}
	for _, c := range list(first(cfg, "conditions", "condition")) {
		co := obj(c)
		switch kind(co, "condition") {
		case "time":
			if co["after"] != nil || co["before"] != nil {
				if co["after"] == nil || co["before"] == nil {
					return nil
				}
				conditions = append(conditions, map[string]any{"type": "time", "after": hhmm(co["after"]), "before": hhmm(co["before"])})
			}
			for _, w := range list(co["weekday"]) {
				days = append(days, weekdays[str(w)])
			}
		case "state":
			dev, key, val, ok := m.watch(str(list(co["entity_id"])[0]), co["state"])
			if !ok || len(list(co["entity_id"])) != 1 {
				return nil
			}
			conditions = append(conditions, map[string]any{"type": "device", "device_id": dev, "key": key, "op": "eq", "value": val})
		default:
			return nil
		}
	}
	for _, t := range list(first(cfg, "triggers", "trigger")) {
		to := obj(t)
		switch kind(to, "trigger", "platform") {
		case "time":
			for _, at := range list(to["at"]) {
				if strings.Contains(str(at), ".") {
					return nil
				}
				tr := map[string]any{"type": "time", "at": hhmm(at)}
				if len(days) > 0 {
					tr["days"] = days
				}
				triggers = append(triggers, tr)
			}
		case "sun":
			off := duration(strings.TrimPrefix(str(to["offset"]), "-"))
			mins := int(off.Minutes())
			if strings.HasPrefix(str(to["offset"]), "-") {
				mins = -mins
			}
			tr := map[string]any{"type": "sun", "event": str(to["event"]), "offset_minutes": mins}
			if len(days) > 0 {
				tr["days"] = days
			}
			triggers = append(triggers, tr)
		case "state":
			if to["for"] != nil || len(list(to["entity_id"])) != 1 {
				return nil
			}
			dev, key, val, ok := m.watch(str(list(to["entity_id"])[0]), to["to"])
			if !ok {
				return nil
			}
			triggers = append(triggers, map[string]any{"type": "device", "device_id": dev, "key": key, "op": "eq", "value": val})
		case "device":
			dev := m.zigbeeIDs[str(to["device_id"])]
			press := str(to["subtype"])
			if dev == "" || str(to["type"]) != "action" || press == "" {
				return nil
			}
			triggers = append(triggers, map[string]any{"type": "device", "device_id": dev, "key": "press", "op": "eq", "value": press})
		case "numeric_state":
			eid := str(list(to["entity_id"])[0])
			dev, key, ok := m.numericKey(eid)
			if !ok || (to["above"] != nil) == (to["below"] != nil) {
				return nil
			}
			op, v := "gt", to["above"]
			if to["below"] != nil {
				op, v = "lt", to["below"]
			}
			n, isNum := v.(float64)
			if !isNum {
				return nil
			}
			triggers = append(triggers, map[string]any{"type": "device", "device_id": dev, "key": key, "op": op, "value": n})
		default:
			return nil
		}
	}
	steps := []map[string]any{}
	delay := 0
	for _, a := range list(first(cfg, "actions", "action")) {
		ao := obj(a)
		if d := ao["delay"]; d != nil {
			delay += int(duration(d).Seconds())
			continue
		}
		svc := kind(ao, "action", "service")
		dom, s, _ := strings.Cut(svc, ".")
		if svc == "" && ao["device_id"] != nil && ao["type"] != nil {
			eid := m.registryIDs[str(ao["entity_id"])]
			dom, s = domainOf(eid), str(ao["type"])
			ao = map[string]any{"target": map[string]any{"entity_id": eid}}
		}
		action, params := hearthAction(dom, s, obj(ao["data"]))
		if action == "" {
			return nil
		}
		ents := list(obj(ao["target"])["entity_id"])
		if ents == nil {
			ents = list(obj(ao["data"])["entity_id"])
		}
		if ents == nil {
			ents = list(ao["entity_id"])
		}
		if len(ents) == 0 {
			return nil
		}
		for _, e := range ents {
			dev := m.byEntity[str(e)]
			p := m.products[dev]
			if p == nil {
				dev = m.zigbeeIDs[m.entityDevs[str(e)]]
				if dev == "" {
					return nil
				}
			} else if p.refs["primary"] != str(e) {
				return nil
			}
			step := map[string]any{"device_id": dev, "action": action}
			if params != nil {
				step["params"] = params
			}
			if delay > 0 {
				step["delay_seconds"] = delay
				delay = 0
			}
			steps = append(steps, step)
		}
	}
	if len(triggers) == 0 || len(steps) == 0 {
		return nil
	}
	out := map[string]any{"name": str(cfg["alias"]), "enabled": true, "triggers": triggers, "steps": steps, "icon": "sparkles"}
	if len(conditions) > 0 {
		out["conditions"] = conditions
	}
	return out
}

func hearthAction(dom, s string, data map[string]any) (string, map[string]any) {
	switch {
	case s == "turn_on" && data["brightness_pct"] != nil && dom == "light":
		return "set_brightness", map[string]any{"value": data["brightness_pct"]}
	case s == "turn_on" && len(data) > 0:
		return "", nil
	case (s == "turn_on" || s == "turn_off" || s == "toggle") && map[string]bool{"light": true, "switch": true, "fan": true, "media_player": true, "climate": true, "input_boolean": true}[dom]:
		return s, nil
	case dom == "climate" && s == "set_temperature" && data["temperature"] != nil && len(data) == 1:
		return "set_temperature", map[string]any{"value": data["temperature"]}
	case dom == "lock" && (s == "lock"):
		return "lock", nil
	case dom == "cover" && (s == "open_cover" || s == "close_cover"):
		return strings.TrimSuffix(s, "_cover"), nil
	case dom == "vacuum" && s == "start":
		return "start", nil
	case dom == "vacuum" && s == "return_to_base":
		return "dock", nil
	case dom == "media_player" && s == "media_play_pause":
		return "play_pause", nil
	}
	return "", nil
}

// watch maps an entity state onto the Hearth device key it shows up as.
func (m *mapper) watch(eid string, state any) (dev, key string, value bool, ok bool) {
	dev = m.byEntity[eid]
	p := m.products[dev]
	if p == nil {
		return "", "", false, false
	}
	s := str(state)
	switch {
	case eid == p.refs["motion"]:
		key = "motion"
	case eid == p.refs["contact"]:
		key = "open"
	case eid == p.refs["primary"] && domainOf(eid) == "lock":
		key = "locked"
		if s != "locked" && s != "unlocked" {
			return "", "", false, false
		}
		return dev, key, s == "locked", true
	case eid == p.refs["primary"] && domainOf(eid) == "media_player":
		key = "power"
	case eid == p.refs["primary"]:
		key = "on"
	default:
		return "", "", false, false
	}
	if s != "on" && s != "off" {
		return "", "", false, false
	}
	return dev, key, s == "on", true
}

func (m *mapper) numericKey(eid string) (dev, key string, ok bool) {
	dev = m.byEntity[eid]
	p := m.products[dev]
	if p == nil {
		return "", "", false
	}
	for _, k := range []string{"temperature", "humidity", "battery"} {
		if p.refs[k] == eid {
			return dev, k, true
		}
	}
	return "", "", false
}
