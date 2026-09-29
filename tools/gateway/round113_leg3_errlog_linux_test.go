package main

// round113_leg3_errlog_linux_test.go — leg 3, round 113 (2026-09-29 audit), F113-L3-1.
// Linux-only on purpose (RLIMIT_FSIZE injects the full disk).
//
// The upstream-error log is F112-L3-1's sibling: logUpstreamError appended a row with one
// Write and discarded the error, so a write that landed in part left a fragment that the
// next row was appended onto, and nothing was logged. Its default path is on the same small
// volume as the ledger, and it is the sink the 400 context-overflow incidents are read from.
// Both files now go through one appendRow helper that cuts the file back to the row
// boundary after a failed write, and a failure is logged.

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMine113APartialErrorLogWriteDoesNotDestroyTheNextRow(t *testing.T) {
	signal.Ignore(syscall.SIGXFSZ)
	t.Cleanup(func() { signal.Reset(syscall.SIGXFSZ) })
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"This model's maximum context length is 100 tokens. However, you requested 500 tokens (400 in the messages, 100 in the completion)."}}`))
	}))
	t.Cleanup(up.Close)
	dir := t.TempDir()
	errLog := filepath.Join(dir, "errors.jsonl")
	g := &gateway{}
	if err := g.apply(gwConfig{UpstreamAddr: up.URL, ListenAddr: ":0", LedgerPath: filepath.Join(dir, "ledger.jsonl"), UpstreamErrorLogPath: errLog,
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models:  []gwModel{{ID: "kat-awq", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768, Pricing: gwPricing{Prompt: "0", Completion: "0"}}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	t.Cleanup(srv.Close)
	post := func() {
		r107Post(t, srv, "/v1/chat/completions", `{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`, 0)
	}
	rows := func() int {
		b, _ := os.ReadFile(errLog)
		return strings.Count(string(b), "\n")
	}
	settle := func(n int) {
		for i := 0; i < 300 && rows() < n; i++ {
			time.Sleep(10 * time.Millisecond)
		}
	}
	post()
	settle(1)
	st, err := os.Stat(errLog)
	if err != nil {
		t.Fatalf("premise: no error log row was written: %v", err)
	}
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: uint64(st.Size()) + 40, Max: old.Max}); err != nil {
		t.Fatal(err)
	}
	post()
	time.Sleep(300 * time.Millisecond)
	_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	post()
	settle(2)
	post()
	time.Sleep(300 * time.Millisecond)

	b, _ := os.ReadFile(errLog)
	good, bad := 0, 0
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			good++
		} else {
			bad++
		}
	}
	if !strings.Contains(logged.String(), "upstream error log write failed") {
		t.Errorf("the failed write was not logged — the error log's failures were silent (2026-09-29 audit, round 113, F113-L3-1)\n%s", logged.String())
	}
	if bad != 0 || good < 3 {
		t.Errorf("the error log has %d parseable and %d unparseable rows after a failed partial write — it must cost its own row only and leave no fragment (2026-09-29 audit, round 113, F113-L3-1)\n%s", good, bad, b)
	}
}
