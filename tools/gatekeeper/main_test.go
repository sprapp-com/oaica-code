package main

// Round 122 leg 3 (2026-09-29 audit): the gate's config reload survives a bad file (F122-L3-5) and
// a client cannot state the double-metering marker unless its tier is trusted (F122-L3-1).

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestGate(t *testing.T, upstream string) *gate {
	cfg := defaultConfig()
	cfg.Tiers = map[string]int{"free": 2, "internal": 5}
	cfg.Keys = map[string]string{"k-free": "free", "k-int": "internal"}
	cfg.UpstreamAddr = upstream
	return &gate{cfg: cfg}
}

func TestRound122ReloadKeepsTheCurrentConfigOnABadFile(t *testing.T) {
	g := newTestGate(t, "http://127.0.0.1:1")
	dir := t.TempDir()
	bad := filepath.Join(dir, "gk.json")
	os.WriteFile(bad, []byte(`{"tiers": {"free": 2}, "keys": {"k`), 0o600)
	g.reload(bad) // used to log.Fatalf
	g.reload(filepath.Join(dir, "absent.json"))
	if _, ok := g.cfg.Keys["k-free"]; !ok || len(g.cfg.Keys) != 2 {
		t.Fatalf("a rejected reload replaced the key set: %v", g.cfg.Keys)
	}
	good := filepath.Join(dir, "good.json")
	os.WriteFile(good, []byte(`{"tiers":{"free":1},"keys":{"k-new":"free"}}`), 0o600)
	g.reload(good)
	if _, ok := g.cfg.Keys["k-new"]; !ok {
		t.Fatalf("a good reload was not applied: %v", g.cfg.Keys)
	}
}

func TestRound122ClientCannotStateTheMeteredMarkerUnlessTrusted(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")] = r.Header.Get("X-Oaica-Metered") + "|" + r.Header.Get("X-Gatekeeper-Tier") + "|" + r.Header.Get("X-Katlb-Foo")
		mu.Unlock()
		io.WriteString(w, "{}")
	}))
	defer up.Close()
	gt := httptest.NewServer(newTestGate(t, up.URL).handler())
	defer gt.Close()
	for _, key := range []string{"k-free", "k-int"} {
		req, _ := http.NewRequest("POST", gt.URL+"/v1/chat/completions", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Oaica-Metered", "1")
		req.Header.Set("X-Gatekeeper-Tier", "internal")
		req.Header.Set("X-Katlb-Foo", "bar")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if got := seen["Bearer k-free"]; got != "||" {
		t.Errorf("a free-tier client's control headers reached the upstream: %q", got)
	}
	if got := seen["Bearer k-int"]; got != "1||" {
		t.Errorf("the trusted tier's marker should pass and nothing else: %q", got)
	}
}
