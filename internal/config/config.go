// Package config reads settings from environment variables (12-factor style),
// so every service is configured from the docker-compose .env file.
package config

import (
	"log/slog"
	"os"
)

func Get(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Must returns a required setting or exits with a clear message.
func Must(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("missing required setting", "env", key)
		os.Exit(2)
	}
	return v
}

// Bus holds the common MQTT settings shared by all services.
type Bus struct {
	URL, User, Pass string
}

func LoadBus() Bus {
	return Bus{
		URL:  Get("MQTT_URL", "tcp://mosquitto:1883"),
		User: Get("MQTT_USER", ""),
		Pass: Get("MQTT_PASS", ""),
	}
}
