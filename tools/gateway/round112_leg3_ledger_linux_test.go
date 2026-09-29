package main

// round112_leg3_ledger_linux_test.go — leg 3, round 112 (2026-09-29 audit), F112-L3-1.
// Linux-only on purpose (the _linux suffix is the build constraint): the fault is
// injected with RLIMIT_FSIZE, which is how a full disk that later frees up looks to a
// writer that keeps its file open.
//
// writeLedger did one Write of the row and its newline and only logged an error. A
// write that got PART of a row onto the disk and then failed left a newline-less
// fragment at the end of the file, and the next successful row was appended straight
// onto it: the failed row was lost, which the code accepts, and the row AFTER it became
// unparseable as well, because the fragment and the row shared a line. Four requests
// were served and billed and two ledger rows parsed. The file is now cut back to the
// row boundary after a failed write; if even that fails, the next row starts on a
// fresh line.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

func r112Settle(t *testing.T, path string, n int) {
	t.Helper()
	for i := 0; i < 300; i++ {
		b, _ := os.ReadFile(path)
		if strings.Count(string(b), "\n") >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMine112APartialLedgerWriteDoesNotDestroyTheNextRow(t *testing.T) {
	signal.Ignore(syscall.SIGXFSZ)
	t.Cleanup(func() { signal.Reset(syscall.SIGXFSZ) })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	t.Cleanup(up.Close)
	srv, ledger := r107Gw(t, up, nil)
	body := `{"model":"kat-awq","messages":[{"role":"user","content":"hello"}]}`
	post := func() {
		if code, _, b := r107Post(t, srv, "/v1/chat/completions", body, 0); code != 200 {
			t.Fatalf("premise: a request answered %d %s", code, b)
		}
	}
	post()
	r112Settle(t, ledger, 1)
	st, err := os.Stat(ledger)
	if err != nil {
		t.Fatal(err)
	}
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	// Room for 60 bytes of the second row and no more: a write that lands in part.
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: uint64(st.Size()) + 60, Max: old.Max}); err != nil {
		t.Fatal(err)
	}
	post()
	time.Sleep(300 * time.Millisecond)
	_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old) // the disk has room again
	post()
	r112Settle(t, ledger, 2)
	post()
	time.Sleep(300 * time.Millisecond)

	b, _ := os.ReadFile(ledger)
	good, bad := 0, 0
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			good++
		} else {
			bad++
		}
	}
	if bad != 0 {
		t.Errorf("%d unparseable line(s) remain in the ledger — a failed partial write is cut back to the row boundary and leaves no fragment (2026-09-29 audit, round 112, F112-L3-1)\n%s", bad, b)
	}
	// Four requests were served; one row was lost to the failed write, which is
	// accepted. The rows after it must survive.
	if good < 3 {
		t.Errorf("%d of 4 ledger rows parse — a failed partial write must cost its own row only, not the next one (2026-09-29 audit, round 112, F112-L3-1)\n%s", good, b)
	}
}
