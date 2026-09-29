package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// A real success is recorded at wall time T; the host clock is then stepped back one hour
// (NTP correction of a fast clock). Equivalently: lastOKAt holds a unix second one hour in
// the "future". The upstream is dead.
func TestRound115HealthAfterClockStepBack(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer dead.Close()
	g := &gateway{}
	if err := g.apply(gwConfig{UpstreamAddr: dead.URL, ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "l"),
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}}, Models: []gwModel{{ID: "kat-awq"}}}); err != nil {
		t.Fatal(err)
	}
	g.lastOKAt.Store(time.Now().Add(time.Hour).Unix())
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/health")
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("/health with every upstream answering 502, last success stamped before a 1h clock step back -> %d %s", resp.StatusCode, b)
	if resp.StatusCode == 200 {
		t.Errorf("health reports ok for a dead upstream for the length of the clock step (+5 min)")
	}
}
