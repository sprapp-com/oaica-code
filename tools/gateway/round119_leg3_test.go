package main

// Round 119 leg 3 (2026-09-29 audit): the upstream is sent JSON labelled as JSON (F119-L3-1), and
// the bridge's re-encoding does not grow a body past the cap it already passed (F119-L3-2).

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRound119UpstreamGetsJSONContentType(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Content-Type"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer up.Close()
	srv, _ := r107Gw(t, up, []gwKey{{SHA256: keyHash("sk"), Label: "k"}})
	for _, ct := range []string{"application/x-www-form-urlencoded", "text/plain", ""} {
		for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
			body := `{"model":"kat-awq","max_tokens":5,"messages":[{"role":"user","content":"go"}]}`
			req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer sk")
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 6 {
		t.Fatalf("premise: upstream saw %d requests, want 6: %v", len(seen), seen)
	}
	for _, s := range seen {
		if !strings.HasSuffix(s, " application/json") {
			t.Errorf("upstream received %q, want the gateway's own application/json", s)
		}
	}
}

func TestRound119MarkupHeavyBridgedBodyIsNotRefusedForItsEscapes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer up.Close()
	srv, _ := r107Gw(t, up, []gwKey{{SHA256: keyHash("sk"), Label: "k"}})
	// 5 MiB of '<': the client sends it as one byte each, encoding/json writes it as six.
	text := strings.Repeat("<", 5<<20)
	chat := `{"model":"kat-awq","max_tokens":5,"messages":[{"role":"user","content":"` + text + `"}]}`
	messages := chat
	for name, tc := range map[string]struct{ path, body string }{"chat": {"/v1/chat/completions", chat}, "messages": {"/v1/messages", messages}} {
		code, _, rb := r107Post(t, srv, tc.path, tc.body, 0)
		if code == http.StatusRequestEntityTooLarge {
			t.Errorf("%s: a %d-byte client body under the %d cap answered 413 %.80s", name, len(tc.body), maxBodyBytes, rb)
		}
	}
}
