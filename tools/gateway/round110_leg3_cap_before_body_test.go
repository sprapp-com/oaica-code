package main

// round110_leg3_cap_before_body_test.go — leg 3, round 110 (2026-09-29 audit), F110-L3-2.
//
// A key at its MaxConcurrent gets its 429 before a byte of the body is read on the
// OpenAI doors, and only after the body had been read, parsed and translated on
// /v1/messages, whose bridge reaches the check last. So the refusal cost nothing on
// one door and a full 16 MiB buffer-and-convert on the other, per connection, with
// no limit on how many ran at once, and a caller at its cap got body-content
// feedback (a 400 for bad JSON) instead of the refusal. Round 108 fixed the same
// ordering for authentication. The cap is now peeked before the body; the atomic
// admission in completionHandler is unchanged.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMine110AKeyAtItsCapIsRefusedBeforeItsBodyIsRead(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	entered := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		once.Do(func() { close(entered) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	t.Cleanup(up.Close)
	srv, _ := r107Gw(t, up, []gwKey{{SHA256: keyHash("sk"), Label: "k", MaxConcurrent: 1}})
	// Registered after the gateway so it runs first: srv.Close waits for the held
	// request, which waits for this.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	go r107Post(t, srv, "/v1/chat/completions", `{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`, 0)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("premise: the request holding the key's only slot never reached the upstream")
	}

	const want = `{"error":{"code":"concurrency_limited","message":"key \"k\" has 1 concurrent requests in flight (limit 1); wait for one to finish","type":"concurrency_limited"},"type":"error"}`
	for name, body := range map[string]string{
		"valid":    `{"model":"kat-awq","max_tokens":5,"messages":[{"role":"user","content":"go"}]}`,
		"bad json": `{not json`,
		"no model": `{"max_tokens":5,"messages":[]}`,
		"huge":     `{"model":"kat-awq","pad":"` + strings.Repeat("x", 15<<20) + `"}`,
	} {
		code, hdr, got := r107Post(t, srv, "/v1/messages", body, 8*time.Second)
		if code != 429 || strings.TrimSpace(got) != want || hdr.Get("Retry-After") != "1" || hdr.Get("x-ratelimit-remaining-requests") != "0" {
			t.Errorf("%s: a key at its cap answered %d %q (Retry-After %q) on /v1/messages, want the 429 the OpenAI doors give before reading the body (2026-09-29 audit, round 110, F110-L3-2)", name, code, strings.TrimSpace(got), hdr.Get("Retry-After"))
		}
	}

	// The control: once the slot is free the door admits again, so the peek is a
	// refusal at the cap and not a refusal for having ever been busy.
	releaseOnce.Do(func() { close(release) })
	var code int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		code, _, _ = r107Post(t, srv, "/v1/messages", `{"model":"kat-awq","max_tokens":5,"messages":[{"role":"user","content":"go"}]}`, 8*time.Second)
		if code == 200 {
			return
		}
	}
	t.Errorf("/v1/messages still answers %d after the key's slot was released — the peek must refuse only at the cap (2026-09-29 audit, round 110, F110-L3-2)", code)
}
