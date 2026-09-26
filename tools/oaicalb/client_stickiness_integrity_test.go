package main

// client_stickiness_integrity_test.go — sessionHandler's no-header fallback is
// documented as per-CLIENT stickiness, but it hashed r.RemoteAddr, which
// carries the ephemeral source port. Every TCP connection a client opened got
// a different key, so one conversation was scattered across all the replicas
// and the prefix cache the affinity exists to warm was never reused
// (2026-09-26 audit, fourth round).

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientAddrDropsTheEphemeralPort(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"10.0.0.7:54321", "10.0.0.7"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"10.0.0.7", "10.0.0.7"}, // no port: returned as-is, never emptied
		{"", ""},
	} {
		if got := clientAddr(tc.in); got != tc.want {
			t.Errorf("clientAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSessionFallbackPinsOneClientToOneBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	bs := []*backend{
		newBackend(srv.URL + "/a"),
		newBackend(srv.URL + "/b"),
		newBackend(srv.URL + "/c"),
	}
	h := sessionHandler(newStaticPool(bs), 0)

	// The same client, four connections, four ephemeral ports.
	var picked []string
	for _, port := range []string{"40001", "40002", "40003", "40004"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.RemoteAddr = "203.0.113.5:" + port
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		picked = append(picked, w.Header().Get("X-Katlb-Backend"))
	}

	// X-Katlb-Backend is set from the backend the request was pinned to.
	distinct := map[string]bool{}
	for _, p := range picked {
		distinct[p] = true
	}
	if len(distinct) != 1 {
		t.Errorf("one client, four connections landed on %d backends (%v) — the fallback key is the address, so a conversation must not be re-pinned per TCP connection", len(distinct), picked)
	}

	// A different client may still land elsewhere (the hash is over the
	// address, not a constant).
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "198.51.100.9:40001"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("X-Katlb-Backend"); got == "" {
		t.Error("no backend was chosen for a second client")
	}
}

func TestSessionHeaderStillWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	bs := []*backend{newBackend(srv.URL + "/a"), newBackend(srv.URL + "/b")}
	h := sessionHandler(newStaticPool(bs), 0)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "203.0.113.5:40001"
	req.Header.Set("X-Session-Id", "conv-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("X-Katlb-Backend"); got == "" {
		t.Error("an explicit X-Session-Id must still pin the request")
	}
}
