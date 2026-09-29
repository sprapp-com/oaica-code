package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A ledger that a previous process left ending mid-row (crash, OOM kill, power loss,
// or a pre-round-112 binary's partial write on a full disk) is opened O_APPEND as is.
func TestRound117StartupFragmentFusesFirstRow(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer up.Close()
	dir := t.TempDir()
	ledger := filepath.Join(dir, "l.jsonl")
	os.WriteFile(ledger, []byte(`{"ts":"2026-09-29T00:00:00Z","request_id":"old1","key_label":"cust"}`+"\n"+`{"ts":"2026-09-29T00:00:01Z","request_id":"old2","key_la`), 0o600)
	cfgPath := filepath.Join(dir, "gw.json")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(`{"upstream_addr":%q,"ledger_path":%q,"upstream_error_log_path":%q,
"api_keys":[{"sha256":%q,"label":"cust"}],
"models":[{"id":"kat-awq"}]}`, up.URL, ledger, filepath.Join(dir, "e.jsonl"), keyHash("sk"))), 0o600)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	for i := 0; i < 2; i++ {
		code, _, _ := r107Post(t, srv, "/v1/chat/completions", `{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`, 0)
		fmt.Printf("completion %d -> %d\n", i, code)
	}
	var b []byte
	for i := 0; i < 200; i++ {
		b, _ = os.ReadFile(ledger)
		if strings.Count(string(b), "req_") >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	parsed := 0
	for i, l := range lines {
		var m map[string]any
		err := json.Unmarshal([]byte(l), &m)
		fmt.Printf("line %d parses=%v: %.140s\n", i, err == nil, l)
		if err == nil && m["request_id"] != "old1" {
			parsed++
		}
	}
	if parsed != 2 /* want both */ {
		t.Errorf("two served turns, %d of their ledger rows parse", parsed)
	}
}

// One key whose subscription is canceled, one body, two doors.
func TestRound117EntitlementBeforeBody(t *testing.T) {
	var upstreamHits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer up.Close()
	var hubHits int
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubHits++
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/subscribers/get") {
			io.WriteString(w, `{"status":"canceled"}`)
			return
		}
		w.WriteHeader(204)
	}))
	defer hub.Close()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gw.json")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(`{"upstream_addr":%q,"ledger_path":%q,"upstream_error_log_path":%q,
"meterhub_addr":%q,"entitlement_enabled":true,
"api_keys":[{"sha256":%q,"label":"cust"}],
"models":[{"id":"kat-awq","max_completion_tokens":100}]}`, up.URL, filepath.Join(dir, "l.jsonl"), filepath.Join(dir, "e.jsonl"), hub.URL, keyHash("sk"))), 0o600)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	big := `{"model":"kat-awq","max_tokens":10,"messages":[{"role":"user","content":"` + strings.Repeat("a", 17<<20) + `"}]}`
	bodies := map[string]string{
		"bad-json":      `{`,
		"no-max_tokens": `{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`,
		"17MiB":         big,
	}
	verdict := map[string]map[string]int{}
	for _, name := range []string{"bad-json", "no-max_tokens", "17MiB"} {
		verdict[name] = map[string]int{}
		for _, door := range []string{"/v1/chat/completions", "/v1/messages"} {
			code, _, body := r107Post(t, srv, door, bodies[name], 0)
			verdict[name][door] = code
			fmt.Printf("%-14s %-22s -> %d %.160s\n", name, door, code, strings.TrimSpace(body))
		}
	}
	fmt.Printf("upstream hits=%d\n", upstreamHits)
	for name, v := range verdict {
		if v["/v1/chat/completions"] != v["/v1/messages"] {
			t.Errorf("%s: canceled key answered %d on chat and %d on messages", name, v["/v1/chat/completions"], v["/v1/messages"])
		}
	}
}

// A SIGHUP reload that lands while shutdown is flushing meterhub reports.
func TestRound117FlushSentinelAcrossReload(t *testing.T) {
	release := make(chan struct{})
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(204)
	}))
	defer hub.Close()
	defer close(release)
	dir := t.TempDir()
	mk := func(hubAddr string) gwConfig {
		cfg := defaultConfig()
		cfg.UpstreamAddr = "http://127.0.0.1:1"
		cfg.LedgerPath = filepath.Join(dir, "l.jsonl")
		cfg.UpstreamErrorLogPath = filepath.Join(dir, "e.jsonl")
		cfg.MeterHubAddr = hubAddr
		cfg.APIKeys = []gwKey{{SHA256: keyHash("sk"), Label: "cust"}}
		cfg.Models = []gwModel{{ID: "m"}}
		return cfg
	}
	g := &gateway{}
	if err := g.apply(mk(hub.URL)); err != nil {
		t.Fatal(err)
	}
	g.reportUsage(ledgerEntry{RequestID: "r1"}) // the reporter takes this one and blocks on the hub
	time.Sleep(100 * time.Millisecond)
	var logBuf strings.Builder
	logSink(t, &logBuf)
	start := time.Now()
	done := make(chan struct{})
	go func() { g.flushMeterReports(3 * time.Second); close(done) }()
	time.Sleep(100 * time.Millisecond)
	if err := g.apply(mk("")); err != nil { // operator drops meterhub_addr and HUPs
		t.Fatal(err)
	}
	<-done
	fmt.Printf("flush returned after %v\nlog:\n%s", time.Since(start).Round(100*time.Millisecond), logBuf.String())
	if strings.Contains(logBuf.String(), "dropped 1 queued usage report") {
		t.Errorf("no usage report was queued behind the one in flight; the line counts the shutdown sentinel as a dropped billing report")
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  *strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func logSink(t *testing.T, b *strings.Builder) {
	log.SetOutput(&lockedBuf{b: b})
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
}

// The upstream error log is closed off the same way (F117-L3-2).
func TestRound117ErrorLogFragmentIsClosedOff(t *testing.T) {
	dir := t.TempDir()
	errLog := filepath.Join(dir, "err.jsonl")
	if err := os.WriteFile(errLog, []byte(`{"ts":"x","half`), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &gateway{}
	if err := g.apply(gwConfig{UpstreamAddr: "http://127.0.0.1:1", ListenAddr: ":0", LedgerPath: filepath.Join(dir, "l.jsonl"),
		UpstreamErrorLogPath: errLog, APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}}, Models: []gwModel{{ID: "m"}}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(errLog)
	if len(b) == 0 || b[len(b)-1] != '\n' {
		t.Errorf("a fragment left in the upstream error log was not closed off: %q", b)
	}
}
