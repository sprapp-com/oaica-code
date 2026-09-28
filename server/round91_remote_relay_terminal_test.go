package server

// round91_remote_relay_terminal_test.go — leg 1, F91-L1-2 (2026-09-29 audit,
// round 91).
//
// The two remote-host relay branches (`/api/chat` and `/api/generate`, for a
// model whose config names another host) are the one path on this leg that does
// not go through the arm machinery: their `fn` writes each chunk straight to the
// writer, and the reader they read through — `api/client.go`'s `stream` — returns
// nil however the remote's body ends. A remote that closed its stream mid-turn,
// or answered with nothing at all, was therefore relayed as a 200 that simply
// STOPS: no terminal chunk, no error, nothing to retry on. Measured with a fake
// remote answering real ollama shapes: a truncated turn reached the client as
// `message_start`, `content_block_start` and two deltas with no
// `message_stop`/`error`, and an empty stream reached it as a 200 with an EMPTY
// body — while the runner path refuses that same empty event 502 "upstream
// returned an empty stream" and the same truncated event with the family's
// sentence.
//
// The producer is any remote whose build predates round 90's refusal in
// `llm/llama_server.go`, or any remote that stops answering between frames: this
// relay's verdict must not depend on which, so it asks the turn's own last word
// (`Done`) instead.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/middleware"
)

const r91RelaySentence = "upstream stream ended before the response was complete"

// r91RelayRemote answers one of the two remote endpoints with `chunks`, honouring
// the `stream` flag the way a real ollama does — one JSON value for stream:false,
// ndjson for stream:true — and then closes at a clean boundary: no done chunk.
func r91RelayRemote(t *testing.T, path string, chunks []api.ChatResponse) *httptest.Server {
	t.Helper()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("the relay asked the remote for %s, want %s", r.URL.Path, path)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var asked struct {
			Stream *bool `json:"stream"`
		}
		_ = json.Unmarshal(raw, &asked)
		if asked.Stream != nil && !*asked.Stream {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			whole := api.ChatResponse{Model: "test", Done: true, DoneReason: "stop"}
			for _, c := range chunks {
				whole.Message.Content += c.Message.Content
			}
			_ = json.NewEncoder(w).Encode(whole)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		for _, c := range chunks {
			_ = enc.Encode(c)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(remote.Close)
	return remote
}

// r91RelayModel creates the remote-host model on a fresh server and returns it
// with the remote's host registered as a permitted remote.
func r91RelayModel(t *testing.T, remote *httptest.Server, name string) Server {
	t.Helper()
	setTestHome(t, t.TempDir())
	p, err := url.Parse(remote.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_REMOTES", p.Hostname())
	s := Server{}
	yes := true
	w := createRequest(t, s.CreateHandler, api.CreateRequest{
		Model: name, RemoteHost: remote.URL, From: "test",
		Info: map[string]any{"capabilities": []string{"completion"}}, Stream: &yes,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("premise: creating the remote-host model answered %d: %s", w.Code, w.Body.String())
	}
	return s
}

func TestARemoteTurnThatEndsWithoutAFinalResponseIsAFailure(t *testing.T) {
	truncated := []api.ChatResponse{
		{Model: "test", Message: api.Message{Role: "assistant", Content: "half a "}},
		{Model: "test", Message: api.Message{Role: "assistant", Content: "turn"}},
	}
	for _, tc := range []struct {
		name   string
		path   string
		chunks []api.ChatResponse
		want   string
	}{
		{"chat, a turn cut short", "/api/chat", truncated, r91RelaySentence},
		{"chat, an empty stream", "/api/chat", nil, "upstream returned an empty stream"},
		{"generate, a turn cut short", "/api/generate", truncated, r91RelaySentence},
		{"generate, an empty stream", "/api/generate", nil, "upstream returned an empty stream"},
	} {
		gin.SetMode(gin.TestMode)
		remote := r91RelayRemote(t, tc.path, tc.chunks)
		s := r91RelayModel(t, remote, "r91-relay")
		router := gin.New()
		if tc.path == "/api/chat" {
			router.POST(tc.path, s.ChatHandler)
		} else {
			router.POST(tc.path, s.GenerateHandler)
		}
		local := httptest.NewServer(router)
		defer local.Close()

		body := `{"model":"r91-relay","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		if tc.path == "/api/generate" {
			body = `{"model":"r91-relay","stream":true,"prompt":"hi"}`
		}
		resp, err := http.Post(local.URL+tc.path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got := string(out)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: the client was handed %d %q — a relayed turn that never finished was sold as a turn (2026-09-29 audit, round 91, F91-L1-2)", tc.name, resp.StatusCode, got)
		}
		// A truncated turn has already put its frames on the wire, so its
		// failure is stated in the framing and the status stays 200. A stream
		// that held nothing never wrote, so it is refused as a whole document —
		// the same 502 the runner path answers an empty stream with.
		wantStatus := http.StatusOK
		if len(tc.chunks) == 0 {
			wantStatus = http.StatusBadGateway
		}
		if resp.StatusCode != wantStatus {
			t.Errorf("%s: answered %d, want %d", tc.name, resp.StatusCode, wantStatus)
		}
	}
}

// The same event through the Anthropic middleware: the relay's error frame is
// read by the arm machinery, so the client is handed this server's own error
// event carrying the same sentence — the arm's answer, not a stream that stops.
func TestARelayedFailureReachesTheAnthropicClientAsAnErrorEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	remote := r91RelayRemote(t, "/api/chat", []api.ChatResponse{
		{Model: "test", Message: api.Message{Role: "assistant", Content: "half a "}},
	})
	s := r91RelayModel(t, remote, "r91-relay-msg")
	router := gin.New()
	router.POST("/v1/messages", middleware.AnthropicMessagesMiddleware(), s.ChatHandler)
	local := httptest.NewServer(router)
	defer local.Close()

	body := `{"model":"r91-relay-msg","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(local.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	got := string(out)
	if !strings.Contains(got, "event: error") || !strings.Contains(got, r91RelaySentence) {
		t.Errorf("the Anthropic client was handed a stream that just stops (2026-09-29 audit, round 91, F91-L1-2):\n%s", got)
	}
}
