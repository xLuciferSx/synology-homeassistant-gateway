package main

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"hearth/internal/model"
)

// execute translates a product-level command into Home Assistant service calls.
func execute(ctx context.Context, c *haClient, p *product, cmd model.Command) error {
	prim := p.refs["primary"]
	dom := domainOf(prim)
	target := func(eid string) map[string]any { return map[string]any{"entity_id": eid} }
	call := func(domain, service string, eid string, data map[string]any) error {
		return c.callService(ctx, domain, service, target(eid), data)
	}
	param := func(k string) any { return cmd.Params[k] }

	// A TV with Android TV Remote: its remote entity is what reliably powers it, plays and
	// pauses native apps (Cast only controls what is cast) and opens apps.
	atv := p.refs["remote_platform"] == "androidtv_remote" && p.refs["remote"] != ""
	if atv {
		rem := p.refs["remote"]
		key := func(k string) error { return call("remote", "send_command", rem, map[string]any{"command": k}) }
		switch cmd.Action {
		case model.ActTurnOn, model.ActTurnOff:
			return call("remote", cmd.Action, rem, nil)
		case model.ActPlayPause:
			return key("MEDIA_PLAY_PAUSE")
		case model.ActNext:
			return key("MEDIA_FAST_FORWARD")
		case model.ActPrevious:
			return key("MEDIA_REWIND")
		case model.ActLaunchApp, model.ActSelectSource:
			name := fmt.Sprint(param("app"))
			if name == "<nil>" {
				name = fmt.Sprint(param("source"))
			}
			return call("remote", "turn_on", rem, map[string]any{"activity": androidTVActivity(name)})
		}
	}

	// Fire TV and other Android boxes added with the ADB integration: keys go in as Android
	// keycodes over ADB, which also covers volume the media player itself can't change.
	if code, ok := adbKeycode(p, cmd); ok {
		return sendKeycodes(ctx, c, p, []string{code})
	}

	if cmd.Action == model.ActPlayURL && dom == "media_player" {
		url := fmt.Sprint(param("url"))
		if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
			return fmt.Errorf("that isn't a stream address")
		}
		player := prim
		if p.refs["stream"] != "" {
			player = p.refs["stream"]
		}
		return call("media_player", "play_media", player, map[string]any{"media_content_id": url, "media_content_type": "music"})
	}

	if cmd.Action == model.ActAnnounce || cmd.Action == model.ActSpeak {
		text := strings.TrimSpace(fmt.Sprint(param("text")))
		if text == "" || text == "<nil>" {
			return fmt.Errorf("say what to announce")
		}
		eid := p.refs[cmd.Action]
		if eid == "" {
			return fmt.Errorf("%s can't %s", p.dev.Name, cmd.Action)
		}
		return c.callService(ctx, "notify", "send_message", target(eid), map[string]any{"message": text})
	}

	if p.dev.Kind == model.KindDishwasher {
		switch cmd.Action {
		case model.ActStart:
			return c.callService(ctx, "button", "press", target(p.refs["start"]), nil)
		case model.ActStop:
			return c.callService(ctx, "button", "press", target(p.refs["abort"]), nil)
		}
	}

	if wake := p.refs["wake"]; wake != "" {
		on, _ := p.dev.State["power"].(bool)
		if cmd.Action == model.ActTurnOn || cmd.Action == model.ActToggle && !on {
			return c.callService(ctx, "button", "press", map[string]any{"entity_id": strings.Split(wake, ",")}, nil)
		}
	}

	switch cmd.Action {
	case model.ActTurnOn, model.ActTurnOff, model.ActToggle:
		if p.dev.Kind == model.KindPrinter3D && p.refs["chamber_light"] != "" {
			return call("light", cmd.Action, p.refs["chamber_light"], nil)
		}
		if dom == "climate" {
			mode := "heat"
			if cmd.Action == model.ActTurnOff {
				mode = "off"
			}
			return call("climate", "set_hvac_mode", prim, map[string]any{"hvac_mode": mode})
		}
		return call(dom, cmd.Action, prim, nil)
	case model.ActSetBrightness:
		return call("light", "turn_on", prim, map[string]any{"brightness_pct": param("value")})
	case model.ActSetTemperature:
		return call("climate", "set_temperature", prim, map[string]any{"temperature": param("value")})
	case model.ActSetMode:
		return call("climate", "set_hvac_mode", prim, map[string]any{"hvac_mode": param("mode")})
	case model.ActEntity:
		return entityCommand(ctx, c, p, cmd)
	case model.ActSetSpeed:
		return call("fan", "set_percentage", prim, map[string]any{"percentage": param("value")})
	case model.ActSetPreset:
		return call("fan", "set_preset_mode", prim, map[string]any{"preset_mode": param("preset")})

	case model.ActPlayPause:
		if can, ok := p.dev.State["can_pause"].(bool); ok && !can {
			if on, _ := p.dev.State["power"].(bool); on {
				return call("media_player", "turn_off", prim, nil)
			}
			return call("media_player", "turn_on", prim, nil)
		}
		return call("media_player", "media_play_pause", prim, nil)
	case model.ActNext:
		return call("media_player", "media_next_track", prim, nil)
	case model.ActPrevious:
		return call("media_player", "media_previous_track", prim, nil)
	case model.ActVolumeUp, model.ActVolumeDown:
		if level, ok := stepVolume(p, cmd.Action == model.ActVolumeUp); ok {
			return call("media_player", "volume_set", prim, map[string]any{"volume_level": level})
		}
		return call("media_player", cmd.Action, prim, nil)
	case model.ActVolumeSet:
		return call("media_player", "volume_set", prim, map[string]any{"volume_level": param("value")})
	case model.ActMute:
		return call("media_player", "volume_mute", prim, map[string]any{"is_volume_muted": param("value")})
	case model.ActSelectSource, model.ActLaunchApp:
		src := param("source")
		if src == nil {
			src = param("app")
		}
		return call("media_player", "select_source", prim, map[string]any{"source": src})
	case model.ActRemote:
		if p.refs["remote"] == "" {
			return fmt.Errorf("%s has no remote", p.dev.Name)
		}
		data := map[string]any{"command": remoteKey(p.refs["remote_platform"], fmt.Sprint(param("key")))}
		if n, ok := cmd.Params["repeat"]; ok {
			data["num_repeats"] = n
		}
		return call("remote", "send_command", p.refs["remote"], data)

	case model.ActStart:
		return call("vacuum", "start", prim, nil)
	case model.ActDock:
		return call("vacuum", "return_to_base", prim, nil)
	case model.ActPause, model.ActResume, model.ActStop:
		if p.dev.Kind == model.KindPrinter3D {
			role := map[string]string{model.ActPause: "pause_print", model.ActResume: "resume_print", model.ActStop: "stop_print"}[cmd.Action]
			if p.refs[role] == "" {
				return fmt.Errorf("printer can't %s", cmd.Action)
			}
			return call("button", "press", p.refs[role], nil)
		}
		if dom == "cover" {
			return call("cover", "stop_cover", prim, nil)
		}
		svc := map[string]string{model.ActPause: "pause", model.ActResume: "start", model.ActStop: "stop"}[cmd.Action]
		return call(dom, svc, prim, nil)

	case model.ActLock, model.ActUnlock:
		return call("lock", cmd.Action, prim, nil)
	case model.ActArmHome, model.ActArmAway, model.ActArmNight, model.ActArmVacation, model.ActDisarm:
		var data map[string]any
		if code, ok := cmd.Params["code"].(string); ok && code != "" {
			data = map[string]any{"code": code}
		}
		svc := map[string]string{model.ActArmHome: "alarm_arm_home", model.ActArmAway: "alarm_arm_away", model.ActArmNight: "alarm_arm_night",
			model.ActArmVacation: "alarm_arm_vacation", model.ActDisarm: "alarm_disarm"}[cmd.Action]
		return call("alarm_control_panel", svc, prim, data)
	case model.ActSetChargeMode:
		return call("select", "select_option", p.refs["charge_mode"], map[string]any{"option": fmt.Sprint(param("mode"))})
	case model.ActSetChargeTo:
		return call("number", "set_value", p.refs["target_percentage"], map[string]any{"value": param("value")})
	case model.ActSetReadyBy:
		t := fmt.Sprint(param("value"))
		if len(t) == 5 {
			t += ":00"
		}
		return call("time", "set_value", p.refs["target_time"], map[string]any{"time": t})
	case model.ActOpen:
		return call("cover", "open_cover", prim, nil)
	case model.ActClose:
		return call("cover", "close_cover", prim, nil)
	}
	return fmt.Errorf("action %q not supported for %s", cmd.Action, p.dev.Kind)
}

// entityCommand runs one of a device's own entities. Only entities that belong
// to this device are accepted, and only safe service calls per domain.
func entityCommand(ctx context.Context, c *haClient, p *product, cmd model.Command) error {
	eid, _ := cmd.Params["entity_id"].(string)
	op, _ := cmd.Params["op"].(string)
	if !slices.Contains(p.all, eid) {
		return fmt.Errorf("%s is not part of %s", eid, p.dev.Name)
	}
	dom := domainOf(eid)
	target := map[string]any{"entity_id": eid}
	call := func(service string, data map[string]any) error { return c.callService(ctx, dom, service, target, data) }
	switch op {
	case "trigger":
		if dom == "automation" {
			return call("trigger", map[string]any{"skip_condition": true})
		}
	case "rename", "delete":
		if dom == "automation" {
			return c.editAutomation(ctx, eid, op, cmd.Params["value"])
		}
	case "turn_on", "turn_off", "toggle":
		switch dom {
		case "automation":
			return call(op, nil)
		case "switch", "light", "fan", "siren", "input_boolean", "humidifier", "media_player", "climate":
			return call(op, nil)
		}
	case "set_value":
		switch dom {
		case "number", "input_number", "text":
			return call("set_value", map[string]any{"value": cmd.Params["value"]})
		case "time": // "07:30" or "07:30:00", e.g. the Ohme's ready-by time
			return call("set_value", map[string]any{"time": cmd.Params["value"]})
		case "light":
			return call("turn_on", map[string]any{"brightness_pct": cmd.Params["value"]})
		case "fan":
			return call("set_percentage", map[string]any{"percentage": cmd.Params["value"]})
		case "climate":
			return call("set_temperature", map[string]any{"temperature": cmd.Params["value"]})
		}
	case "select":
		switch dom {
		case "select", "input_select":
			return call("select_option", map[string]any{"option": cmd.Params["value"]})
		case "fan":
			return call("set_preset_mode", map[string]any{"preset_mode": cmd.Params["value"]})
		}
	case "press":
		if dom == "button" || dom == "input_button" {
			return call("press", nil)
		}
	case "open", "close", "stop":
		if dom == "cover" || dom == "valve" {
			return call(op+"_"+dom, nil)
		}
	case "lock", "unlock":
		if dom == "lock" {
			return call(op, nil)
		}
	}
	return fmt.Errorf("%s can't do %q", eid, op)
}

// adbKeycode is the Android keycode for a command to a Fire TV or other Android box added
// with the ADB integration, whose keys go in over ADB.
func adbKeycode(p *product, cmd model.Command) (string, bool) {
	if p.refs["remote_platform"] != "androidtv" || domainOf(p.refs["primary"]) != "media_player" {
		return "", false
	}
	switch cmd.Action {
	case model.ActRemote:
		return adbKey(fmt.Sprint(cmd.Params["key"])), true
	case model.ActVolumeUp:
		return "VOLUME_UP", true
	case model.ActVolumeDown:
		return "VOLUME_DOWN", true
	case model.ActMute:
		return "VOLUME_MUTE", true
	case model.ActPlayPause:
		return "MEDIA_PLAY_PAUSE", true
	case model.ActNext:
		return "MEDIA_FAST_FORWARD", true
	case model.ActPrevious:
		return "MEDIA_REWIND", true
	}
	return "", false
}

// scancodes are the Linux key codes behind the Android keys, for Fire TV's
// built-in "amzkeyboard" input device (amzkeyboard.kl).
var scancodes = map[string]int{
	"DPAD_UP": 103, "DPAD_DOWN": 108, "DPAD_LEFT": 105, "DPAD_RIGHT": 106, "DPAD_CENTER": 353, "ENTER": 28,
	"BACK": 158, "HOME": 172, "MENU": 139, "VOLUME_UP": 115, "VOLUME_DOWN": 114, "VOLUME_MUTE": 113,
	"MEDIA_PLAY_PAUSE": 164, "MEDIA_FAST_FORWARD": 208, "MEDIA_REWIND": 168,
}

// keyScript presses keys by writing straight to the input device, which takes
// a few milliseconds a key; "input keyevent" starts a Java VM and takes about a
// second. Keys without a scancode, or boxes without amzkeyboard, use input keyevent.
func keyScript(codes []string) string {
	fallback := make([]string, len(codes))
	var events []string
	for i, code := range codes {
		fallback[i] = "KEYCODE_" + code
		sc, ok := scancodes[code]
		if !ok {
			events = nil
			break
		}
		events = append(events, fmt.Sprintf("sendevent $d 1 %d 1;sendevent $d 0 0 0;sendevent $d 1 %d 0;sendevent $d 0 0 0", sc, sc))
	}
	slow := "input keyevent " + strings.Join(fallback, " ")
	if len(events) != len(codes) {
		return slow
	}
	return `d=$(getevent -pl 2>/dev/null|grep -B1 '"amzkeyboard"'|grep -o '/dev/input/event[0-9]*');` +
		`if [ -n "$d" ];then ` + strings.Join(events, ";") + `;else ` + slow + `;fi`
}

// sendKeycodes presses keys in one ADB call, so a burst of taps goes in together.
func sendKeycodes(ctx context.Context, c *haClient, p *product, codes []string) error {
	return c.callService(ctx, "androidtv", "adb_command", map[string]any{"entity_id": p.refs["primary"]},
		map[string]any{"command": keyScript(codes)})
}

// tvVolume remembers the level last set on each TV, so quick taps add up even
// before the TV reports its new volume.
var tvVolume = struct {
	sync.Mutex
	level map[string]float64
	at    map[string]time.Time
}{level: map[string]float64{}, at: map[string]time.Time{}}

// stepVolume moves a TV's volume by one step (1 of 100). Home Assistant's own
// volume_up jumps 10 for TVs that have no volume step of their own.
func stepVolume(p *product, up bool) (float64, bool) {
	if p.dev.Kind != model.KindTV {
		return 0, false
	}
	tvVolume.Lock()
	defer tvVolume.Unlock()
	level, ok := p.dev.State["volume"].(float64)
	if last, recent := tvVolume.level[p.dev.ID]; recent && time.Since(tvVolume.at[p.dev.ID]) < 4*time.Second {
		level, ok = last, true
	}
	if !ok {
		return 0, false
	}
	if up {
		level += 0.01
	} else {
		level -= 0.01
	}
	level = math.Round(math.Max(0, math.Min(1, level))*100) / 100
	tvVolume.level[p.dev.ID], tvVolume.at[p.dev.ID] = level, time.Now()
	return level, true
}
