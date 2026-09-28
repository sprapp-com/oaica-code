package server

// round94_responses_contentless_turn_test.go — leg 1, F94-L1-2 (2026-09-29
// audit, round 94), end to end.
//
// The unit pin (openai/round94_contentless_turn_test.go) holds the two
// Responses arms to the same output array. This one holds the surface to its
// siblings: a turn that stated no text — a model that stops without saying
// anything — is answered by the Responses surface with no items at all, and by
// the Anthropic surface with no content part at all, on both arms. Before the
// fix the Responses surface answered `[{"type":"message","content":[{"text":
// ""}]}]` when the client did not stream and `null` when it did, where the
// Anthropic surface answered `"content": []` and the streamed events said
// nothing — one upstream body, one surface contradicting itself and its
// neighbour (2026-09-29 audit, round 94, F94-L1-2).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"strings"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/middleware"
)

// r94ContentlessPeer answers every turn with one line: a turn that stated
// nothing.
func r94ContentlessPeer(t *testing.T) *httptest.Server {
	t.Helper()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"test","created_at":"2026-01-01T00:00:00Z","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","eval_count":1,"prompt_eval_count":3}`+"\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(peer.Close)
	return peer
}

// splitSSE is the data payload of every event in an SSE body.
func splitSSE(body string) []string {
	var out []string
	for _, ev := range strings.Split(body, "\n\n") {
		var payload string
		for _, l := range strings.Split(ev, "\n") {
			if strings.HasPrefix(l, "data: ") {
				payload = strings.TrimPrefix(l, "data: ")
			}
		}
		if payload != "" && payload != "[DONE]" {
			out = append(out, payload)
		}
	}
	return out
}

// r94ContentlessRelayModel registers the remote-host model r93RelayModel builds,
// under this pin's name.
func r94ContentlessRelayModel(t *testing.T, remote *httptest.Server, name string) Server {
	t.Helper()
	setTestHome(t, t.TempDir())
	u, err := url.Parse(remote.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_REMOTES", u.Hostname())
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

// r94ContentlessTurn drives one contentless turn through one surface on one arm
// and reports the status and the raw body.
func r94ContentlessTurn(t *testing.T, path string, mw gin.HandlerFunc, stream bool) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	peer := r94ContentlessPeer(t)
	s := r94ContentlessRelayModel(t, peer, "r94-contentless")
	router := gin.New()
	router.Use(mw)
	router.POST(path, s.ChatHandler)
	local := httptest.NewServer(router)
	t.Cleanup(local.Close)
	lit := "false"
	if stream {
		lit = "true"
	}
	var body string
	if path == "/v1/messages" {
		body = `{"model":"r94-contentless","stream":` + lit + `,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	} else {
		body = `{"model":"r94-contentless","stream":` + lit + `,"input":"hi"}`
	}
	resp, err := http.Post(local.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(out)
}

// r94ResponsesItems is the terminal output array of a Responses body, streamed
// or not.
func r94ResponsesItems(t *testing.T, stream bool, body string) []any {
	t.Helper()
	if !stream {
		var doc map[string]any
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("reading the buffered Responses body: %v", err)
		}
		items, ok := doc["output"].([]any)
		if !ok {
			t.Fatalf("the buffered Responses body states output = %v, want an array (2026-09-29 audit, round 94, F94-L1-2)", doc["output"])
		}
		return items
	}
	for _, ev := range splitSSE(body) {
		var d map[string]any
		if err := json.Unmarshal([]byte(ev), &d); err != nil {
			continue
		}
		if d["type"] != "response.completed" {
			continue
		}
		response, ok := d["response"].(map[string]any)
		if !ok {
			t.Fatalf("response.completed states no response object: %s", ev)
		}
		items, ok := response["output"].([]any)
		if !ok {
			t.Fatalf("the streamed Responses body states output = %v, want an array (2026-09-29 audit, round 94, F94-L1-2)", response["output"])
		}
		return items
	}
	t.Fatalf("the streamed Responses body stated no response.completed event: %s", body)
	return nil
}

// A turn that said nothing states no items, on both arms, through the whole
// surface.
func TestAContentlessTurnStatesNoItemsOnEitherArm(t *testing.T) {
	for _, stream := range []bool{false, true} {
		code, body := r94ContentlessTurn(t, "/v1/responses", middleware.ResponsesMiddleware(), stream)
		if code != http.StatusOK {
			t.Fatalf("stream=%v: answered %d: %s", stream, code, body)
		}
		if items := r94ResponsesItems(t, stream, body); len(items) != 0 {
			t.Errorf("stream=%v: the Responses surface states %v, want no items — a turn that stated no text has no message item to state, and the Anthropic surface answers the same turn no content part (2026-09-29 audit, round 94, F94-L1-2)", stream, items)
		}
	}
}

// The Anthropic surface, whose `content` is the same kind of array, states no
// part on either arm — the reading the Responses surface now matches.
func TestAContentlessTurnStatesNoContentPartOnTheAnthropicSurface(t *testing.T) {
	code, body := r94ContentlessTurn(t, "/v1/messages", middleware.AnthropicMessagesMiddleware(), false)
	if code != http.StatusOK {
		t.Fatalf("buffered: answered %d: %s", code, body)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("reading the buffered Anthropic body: %v", err)
	}
	if content, ok := doc["content"].([]any); !ok || len(content) != 0 {
		t.Errorf("buffered: the Anthropic surface states content = %v, want no parts", doc["content"])
	}

	code, body = r94ContentlessTurn(t, "/v1/messages", middleware.AnthropicMessagesMiddleware(), true)
	if code != http.StatusOK {
		t.Fatalf("streamed: answered %d: %s", code, body)
	}
	for _, ev := range splitSSE(body) {
		var d map[string]any
		if err := json.Unmarshal([]byte(ev), &d); err != nil {
			continue
		}
		if d["type"] == "content_block_start" {
			t.Errorf("streamed: the Anthropic surface opened a content block for a turn that stated no text: %s", ev)
		}
	}
}
