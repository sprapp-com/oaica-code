package server

// round97_relayed_surface_thinking_test.go — leg 1, round 97 (2026-09-29 audit).
//
// F97-L1-1: round 96 taught the relay to state which surface its client arrived
// on (`X-Oaica-Surface`), and the peer to apply the marks that surface sets —
// but the Anthropic middleware sets TWO marks, and only one was carried. It sets
// `anthropic_messages` (which wire this is) and `relax_thinking` (its own rule
// that lets an Anthropic client ask for thinking against a model that cannot
// think — the claude-code case, middleware/anthropic.go:1679-1680). The relay
// branch returns before this tree's capability gate (routes.go:3045-3061), so
// the PEER's gate decides a relayed turn and it had one of the two marks:
//
//	one Anthropic client body, one model with no thinking capability
//	  runner lane /v1/messages : 200 (no content, stop_reason end_turn)
//	  relay  lane /v1/messages : 400 {"type":"error","error":{"type":
//	                             "invalid_request_error","message":
//	                             "\"…\" does not support thinking"}}
//
// and isolated to the single mark, on one native body and one model:
//
//	mark absent  (what the peer had) : 400 {"error":"\"…\" does not support thinking"}
//	mark present (what the runner has): 200
//
// One client body, two verdicts, decided by which process happened to hold the
// mark. The header states a SURFACE, so it now carries everything that surface
// sets locally — the pair, not one of them.
//
// The two lanes are compared by VERDICT: the gate decides before any turn is
// written, and both lanes answer the same 200 for the same client body — the
// relay lane through a peer that only knows what the header told it.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/fs/ggml"
	"github.com/ollama/ollama/llm"
	"github.com/ollama/ollama/middleware"
)

// r97ChatTemplate is what makes this harness take the chat route at all: a
// model rendered through `tokenizer.chat_template` (the Go template lane is off)
// calls the runner's Chat, which is where r97Runner answers. A model with no
// template answers an empty turn without ever reaching the runner, which would
// leave neither lane with a turn to compare.
const r97ChatTemplate = "{{ messages[0]['content'] }}"

// r97Runner answers every turn with one finished response.
func r97Runner() *mockRunner {
	return &mockRunner{
		ChatFn: func(_ context.Context, _ llm.ChatRequest, fn func(llm.ChatResponse)) error {
			fn(llm.ChatResponse{Message: api.Message{Role: "assistant", Content: "hi there"},
				Done: true, DoneReason: llm.DoneReasonStop})
			return nil
		},
	}
}

// r97Do asks one server one body, with optional headers.
func r97Do(t *testing.T, srv *httptest.Server, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(out)
}

// r97ThinkBody is an Anthropic client asking for thinking.
func r97ThinkBody(model string) string {
	return `{"model":"` + model + `","max_tokens":64,"stream":false,` +
		`"thinking":{"type":"enabled","budget_tokens":1024},` +
		`"messages":[{"role":"user","content":"hi"}]}`
}

// r97NativeThinkBody is the native body the relay POSTs to its peer for that
// client: the same turn, no tools, thinking on.
func r97NativeThinkBody(model string) string {
	return `{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"hi"}],"think":true}`
}

// An Anthropic client that asks for thinking against a remote model that cannot
// think is answered the verdict the runner lane answers: the peer that decides a
// relayed turn is told the whole rule, not half of it.
func TestARelayedAnthropicTurnAsksForThinkingTheWayTheRunnerLaneDoes(t *testing.T) {
	t.Setenv("OLLAMA_CONTEXT_LENGTH", "4096")
	t.Setenv("OLLAMA_GO_TEMPLATE", "")
	gin.SetMode(gin.TestMode)
	setTestHome(t, t.TempDir())

	s := newServerWithMockRunner(t, r97Runner())
	createMinimalGGUFModel(t, s, "r97-nothink", ggml.KV{"tokenizer.chat_template": r97ChatTemplate}, "", nil)

	peerR := gin.New()
	peerR.POST("/api/chat", s.ChatHandler)
	peer := httptest.NewServer(peerR)
	t.Cleanup(peer.Close)

	u, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_REMOTES", u.Hostname())
	stream := true
	w := createRequest(t, s.CreateHandler, api.CreateRequest{
		Model: "r97-relay", RemoteHost: peer.URL, From: "r97-nothink",
		Info: map[string]any{"capabilities": []string{"completion"}}, Stream: &stream,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("premise: creating the remote-host model answered %d: %s", w.Code, w.Body.String())
	}

	r := gin.New()
	r.Use(middleware.AnthropicMessagesMiddleware())
	r.POST("/v1/messages", s.ChatHandler)
	local := httptest.NewServer(r)
	t.Cleanup(local.Close)

	rc, rb := r97Do(t, local, "/v1/messages", r97ThinkBody("r97-nothink"), nil)
	lc, lb := r97Do(t, local, "/v1/messages", r97ThinkBody("r97-relay"), nil)
	if rc != http.StatusOK {
		t.Fatalf("premise: the runner lane answered %d for a model with no thinking capability: %s", rc, rb)
	}
	if lc != rc {
		t.Errorf("one client body, two verdicts across lanes (2026-09-29 audit, round 97, F97-L1-1):\n  runner lane %d\n  relay lane  %d\n%s", rc, lc, lb)
	}
	t.Logf("runner lane %d | relay lane %d", rc, lc)
}

// The stated surface is the whole surface: a peer handed an Anthropic client's
// native body relaxes thinking for it, and one handed the same body with no
// surface, or with another surface's, keeps this tree's refusal.
func TestAStatedAnthropicSurfaceCarriesTheSurfacesWholeRule(t *testing.T) {
	t.Setenv("OLLAMA_CONTEXT_LENGTH", "4096")
	t.Setenv("OLLAMA_GO_TEMPLATE", "")
	gin.SetMode(gin.TestMode)
	setTestHome(t, t.TempDir())

	s := newServerWithMockRunner(t, r97Runner())
	createMinimalGGUFModel(t, s, "r97-gate", ggml.KV{"tokenizer.chat_template": r97ChatTemplate}, "", nil)

	r := gin.New()
	r.Use(func(c *gin.Context) { applyRelayedSurfaceMark(c); c.Next() })
	r.POST("/api/chat", s.ChatHandler)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	body := r97NativeThinkBody("r97-gate")
	for _, tc := range []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"no stated surface", nil, http.StatusBadRequest},
		{"another surface", map[string]string{relayedSurfaceHeader: "openai"}, http.StatusBadRequest},
		{"the anthropic surface", map[string]string{relayedSurfaceHeader: "anthropic"}, http.StatusOK},
	} {
		got, out := r97Do(t, srv, "/api/chat", body, tc.hdr)
		if got != tc.want {
			t.Errorf("%s: answered %d, want %d (2026-09-29 audit, round 97, F97-L1-1):\n%s", tc.name, got, tc.want, out)
		}
		t.Logf("%-20s %d", tc.name, got)
	}
}
