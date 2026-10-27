package main

import "strings"

// The app sends one set of key names (UP, DOWN, LEFT, RIGHT, OK, BACK, HOME, MENU, PLAY_PAUSE,
// VOLUME_UP, VOLUME_DOWN, MUTE, POWER); each remote integration wants its own.
var remoteKeys = map[string]map[string]string{
	// Android key codes without the KEYCODE_ prefix.
	"androidtv_remote": {
		"UP": "DPAD_UP", "DOWN": "DPAD_DOWN", "LEFT": "DPAD_LEFT", "RIGHT": "DPAD_RIGHT", "OK": "DPAD_CENTER",
		"SELECT": "DPAD_CENTER", "BACK": "BACK", "HOME": "HOME", "MENU": "MENU", "PLAY_PAUSE": "MEDIA_PLAY_PAUSE",
		"VOLUME_UP": "VOLUME_UP", "VOLUME_DOWN": "VOLUME_DOWN", "MUTE": "VOLUME_MUTE", "POWER": "POWER",
		"REWIND": "MEDIA_REWIND", "FAST_FORWARD": "MEDIA_FAST_FORWARD", "INFO": "INFO", "SETTINGS": "SETTINGS",
	},
	// Sony's IRCC names.
	"braviatv": {
		"UP": "Up", "DOWN": "Down", "LEFT": "Left", "RIGHT": "Right", "OK": "Confirm", "SELECT": "Confirm",
		"BACK": "Return", "HOME": "Home", "MENU": "Options", "PLAY_PAUSE": "Play", "VOLUME_UP": "VolumeUp",
		"VOLUME_DOWN": "VolumeDown", "MUTE": "Mute", "POWER": "TvPower",
	},
	"apple_tv": {
		"UP": "up", "DOWN": "down", "LEFT": "left", "RIGHT": "right", "OK": "select", "SELECT": "select",
		"BACK": "menu", "HOME": "home", "MENU": "menu", "PLAY_PAUSE": "play_pause",
		"VOLUME_UP": "volume_up", "VOLUME_DOWN": "volume_down",
	},
	"samsungtv": {
		"UP": "KEY_UP", "DOWN": "KEY_DOWN", "LEFT": "KEY_LEFT", "RIGHT": "KEY_RIGHT", "OK": "KEY_ENTER",
		"SELECT": "KEY_ENTER", "BACK": "KEY_RETURN", "HOME": "KEY_HOME", "MENU": "KEY_MENU",
		"PLAY_PAUSE": "KEY_PLAY", "VOLUME_UP": "KEY_VOLUP", "VOLUME_DOWN": "KEY_VOLDOWN", "MUTE": "KEY_MUTE",
	},
}

// remoteKey turns the app's key name into the one platform's remote expects. Unknown keys
// pass through untouched, so a native name sent on purpose still works.
func remoteKey(platform, key string) string {
	if m, ok := remoteKeys[platform]; ok {
		// The swipe pad speaks Android ("DPAD_UP", "DPAD_CENTER"); read those as the plain names.
		name := strings.TrimPrefix(strings.ToUpper(key), "DPAD_")
		if name == "CENTER" {
			name = "OK"
		}
		if k, ok := m[name]; ok {
			return k
		}
	}
	return key
}

// adbKey turns the app's key name into an Android keycode name for "input keyevent".
func adbKey(key string) string {
	name := strings.TrimPrefix(strings.ToUpper(key), "DPAD_")
	switch name {
	case "UP", "DOWN", "LEFT", "RIGHT":
		return "DPAD_" + name
	case "OK", "SELECT", "CENTER":
		return "DPAD_CENTER"
	case "PLAY_PAUSE":
		return "MEDIA_PLAY_PAUSE"
	case "MUTE":
		return "VOLUME_MUTE"
	case "REWIND", "FAST_FORWARD":
		return "MEDIA_" + name
	}
	return name
}

// Apps Android TV Remote can open, by the name shown and the package it launches.
var androidTVApps = []struct{ name, pkg string }{
	{"Netflix", "com.netflix.ninja"},
	{"YouTube", "com.google.android.youtube.tv"},
	{"Prime Video", "com.amazon.amazonvideo.livingroom"},
	{"Disney+", "com.disney.disneyplus"},
	{"BBC iPlayer", "uk.co.bbc.iplayer"},
	{"ITVX", "air.ITVMobilePlayer"},
	{"Spotify", "com.spotify.tv.android"},
	{"Plex", "com.plexapp.android"},
	{"Kinopub", "com.kinopub"},
}

func androidTVAppNames() []string {
	names := make([]string, len(androidTVApps))
	for i, a := range androidTVApps {
		names[i] = a.name
	}
	return names
}

// androidTVActivity is the package to launch for an app name, or the name itself when it
// already is a package or a link.
func androidTVActivity(name string) string {
	for _, a := range androidTVApps {
		if strings.EqualFold(a.name, name) {
			return a.pkg
		}
	}
	return name
}

// friendlyApp turns a package name ("com.kinopub") into a name a person would say.
func friendlyApp(pkg string) string {
	for _, a := range androidTVApps {
		if a.pkg == pkg {
			return a.name
		}
	}
	if pkg == "" || pkg == "<nil>" {
		return ""
	}
	parts := strings.Split(pkg, ".")
	last := parts[len(parts)-1]
	if last == "" {
		return pkg
	}
	return strings.ToUpper(last[:1]) + last[1:]
}
