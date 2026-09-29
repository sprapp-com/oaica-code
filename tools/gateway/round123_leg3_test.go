package main

// F123-L3-3 (2026-09-29 audit, round 123): one tool call streamed a token at a time is handled in
// linear time on /v1/messages, as it is on the chat door.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRound123ManyArgumentFragmentsOfOneCallAreLinear(t *testing.T) {
	const frags = 24000
	var sb strings.Builder
	sb.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c0\",\"type\":\"function\",\"function\":{\"name\":\"t\",\"arguments\":\"{\\\"a\\\":\\\"\"}}]}}]}\n\n")
	for i := 0; i < frags; i++ {
		sb.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"abcd\"}}]}}]}\n\n")
	}
	sb.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"}\"}}]}}]}\n\n")
	sb.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	body := sb.String()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
	defer up.Close()
	srv, _ := r107Gw(t, up, []gwKey{{SHA256: keyHash("sk"), Label: "k"}})
	reqBody := `{"model":"kat-awq","stream":true,"max_tokens":32768,"tools":[{"name":"t","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"go"}]}`
	start := time.Now()
	code, _, out := r107Post(t, srv, "/v1/messages", reqBody, 0)
	el := time.Since(start)
	if code != 200 || !strings.Contains(out, "tool_use") {
		t.Fatalf("status %d, no tool_use in %.200s", code, out)
	}
	if el > 5*time.Second {
		t.Errorf("%d argument fragments of one call took %s on /v1/messages: the finished-object check is quadratic", frags, el)
	}
}

// F123-L3-2: a per-model upstream that is OUR hop with its own reporter gets the marker; a
// third-party one still does not.
func TestRound123MeteredDownstreamModelGetsTheMarker(t *testing.T) {
	var got sync.Map
	mk := func(name string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			got.Store(name, r.Header.Get("X-Oaica-Metered"))
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
		}))
		t.Cleanup(s.Close)
		return s
	}
	def, own, third := mk("default"), mk("own"), mk("third")
	g := &gateway{}
	if err := g.apply(gwConfig{UpstreamAddr: def.URL, ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "l.jsonl"),
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models:  []gwModel{{ID: "m-def"}, {ID: "m-own", UpstreamAddr: own.URL, MeteredDownstream: true}, {ID: "m-third", UpstreamAddr: third.URL}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	for _, m := range []string{"m-def", "m-own", "m-third"} {
		if code, _, rb := r107Post(t, srv, "/v1/chat/completions", `{"model":"`+m+`","messages":[{"role":"user","content":"go"}]}`, 0); code != 200 {
			t.Fatalf("%s: %d %.100s", m, code, rb)
		}
	}
	for name, want := range map[string]string{"default": "1", "own": "1", "third": ""} {
		if v, _ := got.Load(name); v != want {
			t.Errorf("%s upstream received X-Oaica-Metered=%q, want %q", name, v, want)
		}
	}
}
