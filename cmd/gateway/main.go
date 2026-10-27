// Command gateway is the only service the apps talk to.
//
//	GET  /healthz                       liveness (no auth)
//	GET  /api/home                      rooms + devices snapshot
//	GET  /api/devices/{id}              one device
//	POST /api/devices/{id}/command      {"action": "...", "params": {...}}
//	GET  /api/live                      WebSocket: snapshot, then live changes
//	GET  /api/hub                       hub id, name, whether it is set up (no auth)
//	GET  /pair                          pairing code page, home network only (no auth)
//
// Auth: "Authorization: Bearer <token>" (or ?token= for WebSockets), where the
// token is either a Hearth account access token for a member of this hub
// (when HOMEAPP_HUBS_URL is set) or the local admin HOMEAPP_TOKEN, which is
// only accepted from the home network, never through the tunnel.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo; TZ=Europe/London needs this

	"github.com/coder/websocket"

	"hearth/internal/bus"
	"hearth/internal/config"
	"hearth/internal/hubproto"
	"hearth/internal/model"
)

var version = "dev" // set with -ldflags "-X main.version=..."

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	addr := config.Get("LISTEN_ADDR", ":8780")
	az := authz{token: config.Get("HOMEAPP_TOKEN", "")}
	if hubs := config.Get("HOMEAPP_HUBS_URL", ""); hubs != "" {
		c, err := newCloud(strings.TrimSuffix(hubs, "/"), config.Must("AUTH_JWKS_URL"), config.Get("HUB_NAME", "Home"),
			config.Get("HUB_LAN_URL", ""), config.Get("HUB_PUBLIC_URL", ""), config.Get("DATA_DIR", "/data"))
		if err != nil {
			slog.Error("hub identity", "err", err)
			os.Exit(1)
		}
		az.cloud = c
		go c.run(context.Background())
		slog.Info("hub identity", "hub", c.id)
	}
	if az.token != "" && len(az.token) < 32 {
		slog.Error("HOMEAPP_TOKEN is too short: use at least 32 characters, e.g. openssl rand -hex 32")
		os.Exit(2)
	}
	if az.token == "" && az.cloud == nil {
		slog.Error("set HOMEAPP_HUBS_URL (account sign-in) and/or HOMEAPP_TOKEN (local admin token)")
		os.Exit(2)
	}
	auth := az.wrap
	bc := config.LoadBus()

	dataDir := config.Get("DATA_DIR", "/data")
	st := newStore(loadLayout(dataDir))
	// Set once the bus is up; device updates (and so automations) can start before that.
	var hubBus atomic.Pointer[bus.Bus]
	habit := loadHabits(dataDir)
	autos := loadAutomations(dataDir, func(c model.Command) error {
		habit.automated(c.DeviceID)
		b := hubBus.Load()
		if b == nil {
			return fmt.Errorf("the hub is not connected yet")
		}
		return b.SendCommand(c)
	}, st.rawDevice)
	climate := loadClimate(dataDir, autos.send, func() []model.Device { return st.snapshot().Devices })
	if lat, err1 := strconv.ParseFloat(config.Get("HOME_LAT", ""), 64); err1 == nil {
		if lon, err2 := strconv.ParseFloat(config.Get("HOME_LON", ""), 64); err2 == nil {
			autos.lat, autos.lon, autos.hasLoc = lat, lon, true
		}
	}
	// Tell the phones about automation runs and failed commands (shown as banners).
	act := loadActivity(dataDir)
	st.people = loadPresence(dataDir)
	awayTemp, _ := strconv.ParseFloat(config.Get("AWAY_TEMP", "16"), 64)
	awayMode := loadAway(dataDir, awayTemp)
	awayMode.send = func(c model.Command) error {
		habit.automated(c.DeviceID)
		b := hubBus.Load()
		if b == nil {
			return fmt.Errorf("the hub is not connected yet")
		}
		return b.SendCommand(c)
	}
	awayMode.devices = func() []model.Device { return st.snapshot().Devices }
	awayMode.changed = st.refreshPresence
	st.people.awayOn = awayMode.on
	hist := loadHistory(dataDir)
	energy := loadEnergy(dataDir)
	energy.heat = loadHeatingTime(dataDir)
	pushes := loadPush(dataDir, config.Get("APNS_KEY_FILE", ""), config.Get("APNS_KEY_ID", ""),
		config.Get("APNS_TEAM_ID", ""), config.Get("APNS_TOPIC", "com.example.hearth"))
	pushes.critical = config.Get("APNS_CRITICAL", "") == "1"
	if az.cloud != nil {
		pushes.allowed = func(user string) bool { return user == "admin" || az.cloud.isMember(user) }
	}
	act.emit = func(e Entry) {
		pushes.notify(e)
		st.broadcast(map[string]any{"type": "activity", "entry": e})
		if e.Level == "alert" {
			st.broadcast(map[string]any{"type": "event", "name": "alert", "data": e})
		}
	}
	act.roomName = func(id string) string {
		for _, r := range st.snapshot().Rooms {
			if r.ID == id {
				return r.Name
			}
		}
		return ""
	}
	act.devices = func() []model.Device { return st.snapshot().Devices }
	st.onChange = func(prev *model.Device, cur model.Device) {
		hist.record(cur)
		energy.record(cur)
		energy.heat.record(cur)
		if awayMode.presenceChanged(cur) {
			act.add(Entry{Kind: "device", Icon: "house.fill", Text: "Welcome home. Away mode is off and the heating is back to normal"})
		}
		habit.deviceChanged(prev, cur)
		autos.deviceChanged(prev, cur)
		var p *model.Device
		if prev != nil {
			x := st.layout.applyDevice(*prev)
			p = &x
		}
		act.deviceChanged(p, st.layout.applyDevice(cur)) // the household's names and rooms
	}
	autos.notify = func(name string, data any) {
		st.broadcast(map[string]any{"type": "event", "name": name, "data": data})
		if m, ok := data.(map[string]any); ok && name == "automation_ran" {
			e := Entry{Kind: "automation", Icon: "sparkles", Text: fmt.Sprint(m["name"]) + " ran"}
			if errText := fmt.Sprint(m["error"]); errText != "" && errText != "<nil>" {
				e.Level, e.Text = "warning", fmt.Sprint(m["name"])+" ran with a problem: "+errText
			}
			act.add(e)
		}
	}
	go func() {
		for range time.Tick(time.Minute) {
			act.check()
		}
	}()
	go func() {
		for range time.Tick(30 * time.Second) {
			act.flush()
			hist.flush()
		}
	}()
	b, err := bus.Connect(bc.URL, "homeapp-gateway", bc.User, bc.Pass, func(b *bus.Bus) {
		if err := b.OnRooms(st.setRooms); err != nil {
			slog.Error("subscribe rooms", "err", err)
		}
		if err := b.OnDevice(st.setDevice); err != nil {
			slog.Error("subscribe devices", "err", err)
		}
		if err := b.OnEvent(func(name string, data json.RawMessage) {
			st.broadcast(map[string]any{"type": "event", "name": name, "data": data})
		}); err != nil {
			slog.Error("subscribe events", "err", err)
		}
	})
	if err != nil {
		slog.Error("bus", "err", err)
		os.Exit(1)
	}

	hubBus.Store(b)
	go func() {
		for range time.Tick(10 * time.Second) {
			autos.tick()
		}
	}()
	go func() {
		for range time.Tick(climateEvery) {
			climate.tick()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /api/hub", func(w http.ResponseWriter, r *http.Request) {
		info := map[string]any{"version": version, "cloud": az.cloud != nil}
		if az.cloud != nil && fromHomeNetwork(r) { // name and id only for phones on the home network
			owned, _ := az.cloud.status()
			info["hub_id"], info["name"], info["owned"] = az.cloud.id, az.cloud.name, owned
		}
		writeJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("GET /pair", func(w http.ResponseWriter, r *http.Request) { pairPage(w, r, az.cloud) })
	mux.HandleFunc("GET /api/home", auth(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, st.snapshot())
	}))
	mux.HandleFunc("GET /api/devices/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		d, ok := st.device(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown device"})
			return
		}
		writeJSON(w, http.StatusOK, d)
	}))
	mux.HandleFunc("GET /api/presence", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"available": autos.hasLoc, "lat": autos.lat, "lon": autos.lon, "radius": 150})
	}))
	mux.HandleFunc("POST /api/presence", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Home bool `json:"home"`
		}
		who := whoFrom(r)
		if json.NewDecoder(r.Body).Decode(&body) != nil || who == "" || who == "Hub admin" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Sign in with your account to share where you are."})
			return
		}
		if st.people.report(who, body.Home, time.Now()) {
			st.refreshPresence()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	mux.HandleFunc("GET /api/devices/{id}/history", auth(func(w http.ResponseWriter, r *http.Request) {
		hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
		if hours <= 0 || hours > 24*7 {
			hours = 24
		}
		writeJSON(w, http.StatusOK, hist.get(r.PathValue("id"), r.URL.Query().Get("key"), time.Duration(hours)*time.Hour))
	}))
	mux.HandleFunc("POST /api/devices/{id}/command", auth(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := st.device(id); !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown device"})
			return
		}
		var cmd model.Command
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&cmd); err != nil || cmd.Action == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"action\": ..., \"params\": {...}}"})
			return
		}
		cmd.DeviceID = id
		if err := b.SendCommand(cmd); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		if d, ok := st.device(id); ok {
			if text := describeCommand(d, cmd); text != "" {
				act.add(Entry{Kind: "command", DeviceID: id, Room: act.roomName(d.RoomID), Icon: "hand.tap.fill",
					Who: whoFrom(r), Text: text})
			}
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
	}))
	st.layout.routes(mux, auth, st)
	autos.routes(mux, auth)
	act.routes(mux, auth)
	pushes.routes(mux, auth)
	awayMode.routes(mux, auth, act.add, whoFrom)
	energy.routes(mux, auth)
	loadDiagnostics(dataDir).routes(mux, auth)
	habit.routes(mux, auth, st, autos)
	habit.carRoutes(mux, auth)
	climate.routes(mux, auth)
	mux.HandleFunc("GET /api/live", auth(func(w http.ResponseWriter, r *http.Request) {
		live(w, r, st)
	}))

	srv := &http.Server{Addr: addr, Handler: logRequests(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		slog.Info("gateway listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http", "err", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// live streams the snapshot and then every change over a WebSocket.
func live(w http.ResponseWriter, r *http.Request, st *store) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context()) // we don't expect client messages; detect close

	ch := st.subscribe()
	defer st.unsubscribe(ch)

	first, _ := json.Marshal(st.snapshot())
	if err := write(ctx, c, first); err != nil {
		return
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			if err := write(ctx, c, msg); err != nil {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func write(ctx context.Context, c *websocket.Conn, b []byte) error {
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}

type authz struct {
	token string // local admin token (optional, home network only)
	cloud *cloud // account sign-in (optional)
}

func (a authz) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		if a.token != "" && fromHomeNetwork(r) && subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1 {
			ctx := context.WithValue(r.Context(), whoKey{}, "Hub admin")
			next(w, r.WithContext(context.WithValue(ctx, userKey{}, "admin")))
			return
		}
		if a.cloud != nil && got != "" {
			c, err := a.cloud.authorize(r.Context(), got)
			if err == nil {
				who := c.Name
				if who == "" {
					who = c.Email
				}
				ctx := context.WithValue(r.Context(), whoKey{}, who)
				next(w, r.WithContext(context.WithValue(ctx, userKey{}, c.Subject)))
				return
			}
			if errors.Is(err, errNoAccess) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "You don't have access to this hub. Ask its owner for an invite."})
				return
			}
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Please sign in again."})
	}
}

// fromHomeNetwork is true for direct requests from a private address (not
// through a tunnel or proxy), so the pairing code is never shown publicly.
func fromHomeNetwork(r *http.Request) bool {
	if r.Header.Get("CF-Connecting-IP") != "" || r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback())
}

func pairPage(w http.ResponseWriter, r *http.Request, c *cloud) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	msg := "This hub is not linked to Hearth accounts (HOMEAPP_HUBS_URL is not set)."
	code := ""
	switch {
	case c == nil:
	case !fromHomeNetwork(r):
		w.WriteHeader(http.StatusForbidden)
		msg = "Open this page from your home Wi-Fi to see the pairing code."
	default:
		owned, pc := c.status()
		if owned {
			msg = "This hub is set up. Ask its owner to invite you from the app."
		} else if pc != "" {
			msg, code = "In Hearth, choose Add hub and enter:", hubproto.FormatCode(pc)
		} else {
			msg = "Connecting to Hearth… refresh in a few seconds."
		}
	}
	fmt.Fprintf(w, `<!doctype html><meta name=viewport content="width=device-width,initial-scale=1"><title>Pair hub</title>
<style>body{font:18px -apple-system,system-ui,sans-serif;display:grid;place-items:center;min-height:90vh;margin:0;background:#0b0b12;color:#eee;text-align:center}
.code{font:600 56px ui-monospace,monospace;letter-spacing:6px;margin:20px 0;color:#ffb35c}</style>
<div><h2>Hearth hub</h2><p>%s</p><div class=code>%s</div></div>`, html.EscapeString(msg), html.EscapeString(code))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		if r.URL.Path != "/healthz" {
			slog.Info("request", "method", r.Method, "path", r.URL.Path, "took", time.Since(start).Round(time.Millisecond))
		}
	})
}

type whoKey struct{}
type userKey struct{}

// userFrom is the account id behind a request ("admin" for the hub token).
func userFrom(r *http.Request) string {
	u, _ := r.Context().Value(userKey{}).(string)
	return u
}

// whoFrom is the person (or "Hub admin") who made the request.
func whoFrom(r *http.Request) string {
	w, _ := r.Context().Value(whoKey{}).(string)
	return w
}

// describeCommand words a command for the activity log ("turned off Living Room TV").
// Volume, remote keys and other fiddly actions aren't worth recording.
func describeCommand(d model.Device, c model.Command) string {
	switch c.Action {
	case model.ActTurnOn:
		return "turned on " + d.Name
	case model.ActTurnOff:
		return "turned off " + d.Name
	case model.ActToggle:
		return "switched " + d.Name
	case model.ActSetTemperature:
		return fmt.Sprintf("set %s to %v°", d.Name, c.Params["value"])
	case model.ActSetBrightness:
		return fmt.Sprintf("set %s to %v%%", d.Name, c.Params["value"])
	case model.ActLock:
		return "locked " + d.Name
	case model.ActUnlock:
		return "unlocked " + d.Name
	case model.ActArmHome, model.ActArmAway, model.ActArmNight, model.ActArmVacation:
		return strings.ReplaceAll(c.Action, "_", " ") + " the alarm"
	case model.ActDisarm:
		return "disarmed the alarm"
	case model.ActSetChargeMode:
		mode := map[string]string{"smart_charge": "Smart charging", "max_charge": "full power", "paused": "paused"}[fmt.Sprint(c.Params["mode"])]
		return "set " + d.Name + " to " + mode
	case model.ActSetChargeTo:
		return fmt.Sprintf("set %s to charge to %v%%", d.Name, c.Params["value"])
	case model.ActSetReadyBy:
		return fmt.Sprintf("set %s to be ready by %v", d.Name, c.Params["value"])
	case model.ActStart:
		return "started " + d.Name
	case model.ActDock:
		return "sent " + d.Name + " to its dock"
	case model.ActSetPreset:
		return fmt.Sprintf("set %s to %v", d.Name, c.Params["preset"])
	case model.ActSetMode:
		return fmt.Sprintf("set %s to %v", d.Name, c.Params["mode"])
	case model.ActOpen:
		return "opened " + d.Name
	case model.ActClose:
		return "closed " + d.Name
	case model.ActPlayURL:
		return fmt.Sprintf("played %v on %s", c.Params["title"], d.Name)
	case model.ActAnnounce, model.ActSpeak:
		return fmt.Sprintf("announced “%v” on %s", c.Params["text"], d.Name)
	}
	return ""
}
