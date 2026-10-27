package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hearth/internal/model"
)

func TestLayout(t *testing.T) {
	st := newStore(loadLayout(t.TempDir()))
	st.setRooms("ha", []model.Room{{ID: "office", Name: "Office"}, {ID: "kitchen", Name: "Kitchen"}})
	st.setDevice("ha-1", &model.Device{ID: "ha-1", Name: "Office Smart Switch", RoomID: "office"})
	st.setDevice("ha-2", &model.Device{ID: "ha-2", Name: "Server Plug", RoomID: "office"})

	mux := http.NewServeMux()
	st.layout.routes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h }, st)
	do := func(method, path, body string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec.Code
	}

	if c := do("POST", "/api/rooms", `{"name":"Garden"}`); c != 201 {
		t.Fatalf("create room: %d", c)
	}
	if c := do("PATCH", "/api/devices/ha-1", `{"name":"Desk Lamp","room_id":"custom_garden"}`); c != 204 {
		t.Fatalf("patch device: %d", c)
	}
	do("PATCH", "/api/devices/ha-2", `{"hidden":true}`)
	do("PATCH", "/api/rooms/kitchen", `{"name":"Kitchen & Dining"}`)
	do("PUT", "/api/order", `{"rooms":["office","custom_garden","kitchen"]}`)

	snap := st.snapshot()
	d, _ := st.device("ha-1")
	if d.Name != "Desk Lamp" || d.OriginalName != "Office Smart Switch" || d.RoomID != "custom_garden" {
		t.Errorf("device = %+v", d)
	}
	if d2, _ := st.device("ha-2"); !d2.Hidden {
		t.Error("ha-2 should be hidden")
	}
	if len(snap.Rooms) != 3 || snap.Rooms[0].ID != "office" || snap.Rooms[1].ID != "custom_garden" || snap.Rooms[2].Name != "Kitchen & Dining" {
		t.Errorf("rooms = %+v", snap.Rooms)
	}

	// the layout survives a restart
	again := loadLayout(st.layout.path[:len(st.layout.path)-len("/layout.json")])
	if got := again.applyDevice(model.Device{ID: "ha-1", Name: "Office Smart Switch"}); got.Name != "Desk Lamp" {
		t.Errorf("reloaded name = %q", got.Name)
	}

	// deleting the custom room returns its devices to their own room
	if c := do("DELETE", "/api/rooms/custom_garden", ""); c != 204 {
		t.Fatalf("delete room: %d", c)
	}
	if d, _ := st.device("ha-1"); d.RoomID != "office" {
		t.Errorf("after delete room_id = %q", d.RoomID)
	}
	if c := do("DELETE", "/api/rooms/office", ""); c != 400 {
		t.Errorf("deleting a bridge room: %d", c)
	}
}
