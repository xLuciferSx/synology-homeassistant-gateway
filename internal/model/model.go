// Package model is the shared, provider-independent description of the home.
// Every bridge translates its own world (Home Assistant, Zigbee2MQTT, Matter…)
// into these types, and the app only ever sees these types.
package model

import "time"

// Kind is the product type. The app picks a dedicated screen per Kind.
type Kind string

const (
	KindLight       Kind = "light"
	KindSwitch      Kind = "switch"
	KindThermostat  Kind = "thermostat"   // radiator valve
	KindHeater      Kind = "heater"       // electric heater (e.g. Dreo)
	KindTV          Kind = "tv"           // TV or streaming stick
	KindSpeaker     Kind = "speaker"      // HomePod, radio…
	KindVacuum      Kind = "vacuum"       // robot hoover
	KindAirPurifier Kind = "air_purifier" // Blueair…
	KindFan         Kind = "fan"
	KindLock        Kind = "lock"
	KindAlarm       Kind = "alarm"
	KindPrinter3D   Kind = "printer_3d"
	KindWasher      Kind = "washer"
	KindEVCharger   Kind = "ev_charger"
	KindCover       Kind = "cover"
	KindCamera      Kind = "camera"
	KindSensor      Kind = "sensor"   // temperature / motion / door / anything read-only
	KindEnergy      Kind = "energy"   // tariff, usage and cost (Octopus)
	KindPresence    Kind = "presence" // who is home
	KindAutomations Kind = "automations"
	KindWeather     Kind = "weather"
	KindButton      Kind = "button"
	KindDishwasher  Kind = "dishwasher" // Home Assistant's own automations
)

// Room groups devices the way the house is laid out.
type Room struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Floor string `json:"floor,omitempty"`
	Icon  string `json:"icon,omitempty"`
	// Set by the gateway from the household's own layout:
	Order  int  `json:"order,omitempty"`
	Hidden bool `json:"hidden,omitempty"`
	Custom bool `json:"custom,omitempty"` // created in the app, not by a bridge
}

// Device is one physical product (a TV, a radiator valve, a hoover…),
// not an individual entity. Related readings (battery, temperature, filter
// life…) are folded into State.
type Device struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	RoomID       string         `json:"room_id,omitempty"`
	Kind         Kind           `json:"kind"`
	Manufacturer string         `json:"manufacturer,omitempty"`
	Model        string         `json:"model,omitempty"`
	Source       string         `json:"source"` // which bridge owns it
	Online       bool           `json:"online"`
	Capabilities []string       `json:"capabilities"`
	State        map[string]any `json:"state"`
	UpdatedAt    time.Time      `json:"updated_at"`
	// Set by the gateway from the household's own layout:
	OriginalName string `json:"original_name,omitempty"` // bridge's name when renamed
	Order        int    `json:"order,omitempty"`
	Hidden       bool   `json:"hidden,omitempty"`
}

// Command is an action the app asks a device to perform.
// Action names are product-level (e.g. "set_temperature", "remote", "dock"),
// never provider-specific; each bridge translates them.
type Command struct {
	DeviceID string         `json:"device_id"`
	Action   string         `json:"action"`
	Params   map[string]any `json:"params,omitempty"`
}

// Well-known actions. Bridges may support a subset; a device lists what it
// supports in Capabilities.
const (
	ActTurnOn         = "turn_on"
	ActTurnOff        = "turn_off"
	ActToggle         = "toggle"
	ActSetBrightness  = "set_brightness"  // params: value 0-100
	ActSetTemperature = "set_temperature" // params: value (°C)
	ActSetMode        = "set_mode"        // params: mode
	ActPlayPause      = "play_pause"
	ActNext           = "next"
	ActPrevious       = "previous"
	ActVolumeUp       = "volume_up"
	ActVolumeDown     = "volume_down"
	ActVolumeSet      = "volume_set" // params: value 0-1
	ActMute           = "mute"       // params: value bool
	ActSelectSource   = "select_source"
	ActRemote         = "remote"     // params: key (e.g. UP, DOWN, OK, BACK, HOME)
	ActLaunchApp      = "launch_app" // params: app
	ActStart          = "start"
	ActPause          = "pause"
	ActStop           = "stop"
	ActResume         = "resume"
	ActDock           = "dock"
	ActLock           = "lock"
	ActUnlock         = "unlock"
	ActArmHome        = "arm_home"
	ActArmAway        = "arm_away"
	ActDisarm         = "disarm"
	ActArmNight       = "arm_night"         // params: code (optional)
	ActArmVacation    = "arm_vacation"      // params: code (optional)
	ActSetChargeMode  = "set_charge_mode"   // EV charger; params: mode (smart_charge|max_charge|paused)
	ActSetChargeTo    = "set_charge_target" // EV charger; params: value (percent)
	ActSetReadyBy     = "set_ready_by"      // EV charger; params: value ("07:30")
	ActEntity         = "entity"            // params: entity_id, op (turn_on|turn_off|toggle|set_value|select|press|open|close|stop|lock|unlock), value
	ActSetSpeed       = "set_speed"         // params: value (percentage 0-100)
	ActSetPreset      = "set_preset"        // params: preset
	ActOpen           = "open"
	ActClose          = "close"
	ActAnnounce       = "announce" // params: text; plays a chime first (Echo)
	ActPlayURL        = "play_url" // params: url, title; a radio stream
	ActSpeak          = "speak"    // params: text; spoken without a chime
)
