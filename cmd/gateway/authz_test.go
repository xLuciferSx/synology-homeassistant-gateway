package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminTokenOnlyFromHomeNetwork(t *testing.T) {
	h := authz{token: "secret"}.wrap(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	cases := []struct {
		name   string
		remote string
		header string
		want   int
	}{
		{"home network", "192.168.1.10:5000", "", http.StatusOK},
		{"through the tunnel", "192.168.1.20:5000", "203.0.113.9", http.StatusUnauthorized},
		{"public address", "203.0.113.9:5000", "", http.StatusUnauthorized},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/home", nil)
		r.RemoteAddr = c.remote
		r.Header.Set("Authorization", "Bearer secret")
		if c.header != "" {
			r.Header.Set("CF-Connecting-IP", c.header)
		}
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, w.Code, c.want)
		}
	}
}
