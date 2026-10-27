# Synology Home Assistant Gateway (Hearth hub)

A small, self-hosted smart-home hub. It brings devices from Home Assistant and
Zigbee2MQTT into one model, runs automations, keeps history, and serves a
simple HTTP + WebSocket API for apps on your phone.

It runs anywhere Docker runs: a Synology or other NAS, a Raspberry Pi, a mini
PC, or a cloud VM. Everything stays on your own hardware.

## What it does

- **One device model** across providers: lights, radiators and thermostats,
  heaters, locks, TVs and media, washers, dishwashers, vacuums, EV chargers,
  3D printers, sensors, energy tariffs and more.
- **Automations** with time, sun, device and presence triggers, conditions and
  scenes, evaluated on the hub.
- **Away mode**: lights off, doors locked, heating down; restored when someone
  gets home.
- **History and insights**: a week of readings per device, a daily energy
  ledger, heating time per radiator, habit-based automation suggestions.
- **Activity feed and push notifications** (APNs, with your own Apple key).
- **Reliable with flaky devices**: short provider reconnects don't flash
  devices offline.

## Works with any Home Assistant

Nothing is hard-coded to one house. On start the hub reads Home Assistant's own
registries and builds the home from them:

- **Rooms** come from your Home Assistant areas, with whatever names and layout
  you use. Devices without an area land in "Other".
- **Devices** are grouped the way Home Assistant groups entities into devices,
  and their type comes from standard entity domains (`light`, `climate`,
  `lock`, `media_player`, `cover`, `fan`, `vacuum`, `camera`, sensors by
  device class, and so on). Any brand that shows up in Home Assistant works.
- **Changes are picked up live**: add a device, rename a room or move something
  in Home Assistant and the hub follows within seconds.

Some integrations get richer controls on top of that: Octopus Energy tariffs
and usage, Samsung SmartThings washers, Home Connect dishwashers, Ohme EV
chargers, Elegoo 3D printers, Dreo heaters, Blueair purifiers, Fire TV and
Android TV remotes. Devices the hub doesn't recognise still appear, with their
readings and on/off controls.

## Services

| Service | What it does |
|---|---|
| `gateway` | The hub: API on port 8780, automations, history, activity |
| `bridge-homeassistant` | Reads Home Assistant over its WebSocket API and maps devices |
| `bridge-zigbee` | Reads Zigbee2MQTT directly |
| `mosquitto` | Internal message bus, not exposed outside the compose network |
| `auth`, `hubs` | Optional cloud services for account sign-in and sharing a hub |

## Quick start

```bash
git clone <this repository> hearth-hub
cd hearth-hub/deploy
cp .env.example .env
```

Edit `.env`: set `HOMEAPP_TOKEN` (`openssl rand -hex 32`), `HUB_LAN_URL`, `TZ`,
and `HA_URL` / `HA_TOKEN` if you use Home Assistant. Then:

```bash
docker compose up -d --build
curl -H "Authorization: Bearer $YOUR_TOKEN" http://localhost:8780/api/home
```

On a Synology, the same `deploy` folder works as a Container Manager project.

## API

All endpoints need `Authorization: Bearer <token>`. A few to start with:

| Endpoint | |
|---|---|
| `GET /api/home` | Rooms and devices |
| `GET /api/live` | WebSocket stream of changes |
| `POST /api/devices/{id}/command` | `{"action": "turn_on", "params": {}}` |
| `GET /api/devices/{id}/history?key=temperature&hours=24` | Readings |
| `GET/PUT /api/automations` | Automations |
| `GET/POST /api/away` | Away mode |
| `GET /api/energy/days` | Daily energy and heating time |
| `GET /api/activity` | Activity feed |

## Development

```bash
go test ./...
```

The `auth` and `hubs` tests need PostgreSQL (`createdb homeapp_test`, or set
`TEST_DATABASE_URL`); they skip without one.

## Security

Read [SECURITY.md](SECURITY.md) before making the hub reachable from the
internet.

## Author

Designed and developed by **Raivis Olehno**.

## Licence

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE). If you
redistribute the hub or build on it, keep the NOTICE file so the original
author stays credited.
