package server

// round96_relay_surface_mark_test.go — leg 1, round 96 (2026-09-29 audit).
//
// F96-L1-1 and F96-L1-2 are one cause stated twice: the relay lane forwards the
// client's own `stream` flag to its peer, so the PEER merges the turn — and the
// marks that say "this client arrived on a translated surface" live in the gin
// context of the process that saw that client, which the peer's context is not.
// The peer therefore applied the native wire's reading of a request that
// declared no tools, and the client's own `stream` flag decided the turn:
//
//	openai chat  buffered runner relayed the call, relay dropped it
//	responses    buffered runner 1 call,             relay 0
//	anthropic    buffered runner 1 call,             relay 0
//	anthropic    buffered block order [tool_use,text] vs [text,tool_use],
//	             [text,tool_use,text] vs [text,tool_use],
//	             [thinking,text,thinking] vs [thinking,text]
//
// while every streamed arm of every surface already agreed with the runner lane.
// One client body, one upstream body, two turns — depending on which process
// happened to hold the mark.
//
// The relay that does know states the surface on the wire
// (relayedSurfaceHeader) and the peer that receives it applies the marks it
// would have set locally (applyRelayedSurfaceMark), so the peer serves the
// client on the far side of the relay the way this process serves it directly.
// The native wire carries no header and keeps upstream's own rule — pinned here
// as the control.

import (
	"encoding/json"
	"fmt"
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

// r96RelayCall is one tool-call chunk.
func r96RelayCall(id, name string, args map[string]any) api.ChatResponse {
	f := api.NewToolCallFunctionArguments()
	for k, v := range args {
		f.Set(k, v)
	}
	return api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant",
		ToolCalls: []api.ToolCall{{ID: id, Function: api.ToolCallFunction{Name: name, Arguments: f}}}}}
}

// r96RelayText is one prose chunk.
func r96RelayText(s string) api.ChatResponse {
	return api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: s}}
}

// r96RelayThink is one reasoning chunk.
func r96RelayThink(s string) api.ChatResponse {
	return api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Thinking: s}}
}

// r96RelayDone ends the turn.
func r96RelayDone() api.ChatResponse {
	return api.ChatResponse{Model: "m", Done: true, DoneReason: "stop", Message: api.Message{Role: "assistant"}}
}

// r96RelayLane serves a fixed chunk list the way a server with a model runner
// does — through the same writer the runner lane uses, so the lane under test
// and the lane it is compared against are driven by one piece of code.
func r96RelayLane(chunks []any) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req api.ChatRequest
		_ = c.ShouldBindJSON(&req)
		ch := make(chan any, len(chunks))
		for _, v := range chunks {
			ch <- v
		}
		close(ch)
		writeChatResponse(c, req, ch)
	}
}

// r96RelayPeer stands in for a remote oaica: the native merge over a fixed chunk
// list, with the mark a real ChatHandler applies to a relayed request.
func r96RelayPeer(t *testing.T, chunks []any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gin.SetMode(gin.TestMode)
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		applyRelayedSurfaceMark(c)
		r96RelayLane(chunks)(c)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// r96RelayRouter is one surface's handler, with or without its translation
// middleware.
func r96RelayRouter(t *testing.T, h gin.HandlerFunc, mw gin.HandlerFunc) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if mw != nil {
		r.Use(mw)
	}
	r.POST("/x", h)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// r96RelayLocal is the local server whose model is remote-host, for one surface.
func r96RelayLocal(t *testing.T, peer *httptest.Server, mw gin.HandlerFunc) *httptest.Server {
	t.Helper()
	setTestHome(t, t.TempDir())
	u, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_REMOTES", u.Hostname())
	s := Server{}
	yes := true
	w := createRequest(t, s.CreateHandler, api.CreateRequest{
		Model: "r96-relay", RemoteHost: peer.URL, From: "test",
		Info: map[string]any{"capabilities": []string{"completion"}}, Stream: &yes,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("premise: creating the remote-host model answered %d: %s", w.Code, w.Body.String())
	}
	return r96RelayRouter(t, s.ChatHandler, mw)
}

// r96RelayPost asks one server one client body.
func r96RelayPost(t *testing.T, srv *httptest.Server, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/x", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(out)
}

// r96RelayCalls counts the tool calls a body states, in any of the four
// envelopes and on either arm.
func r96RelayCalls(body string) int {
	n := 0
	count := func(v any) {
		if list, ok := v.([]any); ok {
			n += len(list)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			line = strings.TrimPrefix(line, "data: ")
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		count(m["tool_calls"])
		if msg, ok := m["message"].(map[string]any); ok {
			count(msg["tool_calls"])
		}
		if choices, ok := m["choices"].([]any); ok {
			for _, ch := range choices {
				if cm, ok := ch.(map[string]any); ok {
					if msg, ok := cm["message"].(map[string]any); ok {
						count(msg["tool_calls"])
					}
					if d, ok := cm["delta"].(map[string]any); ok {
						count(d["tool_calls"])
					}
				}
			}
		}
		if item, ok := m["item"].(map[string]any); ok && item["type"] == "function_call" {
			n++
		}
		count(m["output"])
		for _, b := range r96RelayList(m["content"]) {
			if bm, ok := b.(map[string]any); ok && bm["type"] == "tool_use" {
				n++
			}
		}
	}
	return n
}

// r96RelayList reads a JSON value that should be a list.
func r96RelayList(v any) []any {
	l, _ := v.([]any)
	return l
}

// r96RelayBlocks reduces an Anthropic envelope or stream to the block kinds in
// order — which is what "the same block order on every arm" asks about.
func r96RelayBlocks(body string) string {
	var kinds []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev["type"] == "content_block_start" {
			if cb, ok := ev["content_block"].(map[string]any); ok {
				kinds = append(kinds, fmt.Sprint(cb["type"]))
			}
		}
	}
	if len(kinds) > 0 {
		return strings.Join(kinds, ",")
	}
	var doc struct {
		Content []map[string]any `json:"content"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &doc) != nil {
		return "UNPARSEABLE:" + body
	}
	for _, b := range doc.Content {
		kinds = append(kinds, fmt.Sprint(b["type"]))
	}
	return strings.Join(kinds, ",")
}

// r96RelaySurface is one client surface: its translation middleware and the
// client body it accepts.
type r96RelaySurface struct {
	name string
	mw   gin.HandlerFunc
	body func(stream bool) string
}

func r96RelaySurfaces() []r96RelaySurface {
	openai := func(s bool) string {
		return fmt.Sprintf(`{"model":"r96-relay","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, s)
	}
	return []r96RelaySurface{
		{"native", nil, openai},
		{"openai chat", middleware.ChatMiddleware(), openai},
		{"responses", middleware.ResponsesMiddleware(), openai},
		{"anthropic", middleware.AnthropicMessagesMiddleware(), func(s bool) string {
			return fmt.Sprintf(`{"model":"r96-relay","max_tokens":64,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, s)
		}},
	}
}

// One client body that declares no tools, over one upstream body that states a
// call: the relay lane answers the turn the runner lane answers. The native wire
// is the control — it declares neither mark, carries no header, and keeps
// upstream's own rule on both lanes.
func TestARelayedTurnKeepsTheCallTheClientDidNotDeclare(t *testing.T) {
	chunks := []any{r96RelayText("let me look"), r96RelayCall("call_aaa", "Read", map[string]any{"path": "/a"}), r96RelayDone()}
	for _, surf := range r96RelaySurfaces() {
		for _, stream := range []bool{false, true} {
			runner := r96RelayRouter(t, r96RelayLane(chunks), surf.mw)
			rc, rb := r96RelayPost(t, runner, surf.body(stream))
			relay := r96RelayLocal(t, r96RelayPeer(t, chunks), surf.mw)
			lc, lb := r96RelayPost(t, relay, surf.body(stream))

			wantRunner, wantRelay := r96RelayCalls(rb), r96RelayCalls(rb)
			if surf.mw == nil {
				// The native wire's own reading, which the relay lane must not
				// change: a request that declared no tools is not handed the
				// model's calls BUFFERED — upstream's gate belongs to that wire's
				// merge — while its streamed arm relays every chunk (round 73,
				// F73-L1-1; round 92, F92-L1-2).
				wantRunner, wantRelay = 0, 0
				if stream {
					wantRunner, wantRelay = 1, 1
				}
			}
			if rc != http.StatusOK || lc != http.StatusOK {
				t.Fatalf("%s stream=%v: runner %d, relay %d, want 200 on both (%q / %q)", surf.name, stream, rc, lc, rb, lb)
			}
			if n := r96RelayCalls(rb); n != wantRunner {
				t.Errorf("%s stream=%v: the runner lane states %d calls, want %d — the premise moved:\n%s", surf.name, stream, n, wantRunner, rb)
			}
			if got := r96RelayCalls(lb); got != wantRelay {
				t.Errorf("%s stream=%v: one client body, two turns (2026-09-29 audit, round 96, F96-L1-1):\n  runner lane states %d calls\n  relay lane states  %d calls\n%s",
					surf.name, stream, wantRunner, got, lb)
			}
			t.Logf("%-12s stream=%-5v runner calls=%d | relay calls=%d", surf.name, stream, r96RelayCalls(rb), r96RelayCalls(lb))
		}
	}
}

// One Anthropic turn's block order is a property of the turn, not of the lane
// that merged it.
func TestARelayedAnthropicTurnStatesTheBlockOrderItsStreamedArmStates(t *testing.T) {
	body := func(s bool) string {
		return fmt.Sprintf(`{"model":"r96-relay","max_tokens":64,"stream":%v,"messages":[{"role":"user","content":"hi"}],`+
			`"tools":[{"name":"Read","description":"read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`, s)
	}
	for _, sh := range []struct {
		name   string
		chunks []any
	}{
		{"call then text", []any{r96RelayCall("call_aaa", "Read", map[string]any{"path": "/a"}), r96RelayText("done"), r96RelayDone()}},
		{"text then call", []any{r96RelayText("let me look"), r96RelayCall("call_aaa", "Read", map[string]any{"path": "/a"}), r96RelayDone()}},
		{"text call text", []any{r96RelayText("one"), r96RelayCall("call_aaa", "Read", map[string]any{"path": "/a"}), r96RelayText("two"), r96RelayDone()}},
		{"think text think", []any{r96RelayThink("t1"), r96RelayText("c"), r96RelayThink("t2"), r96RelayDone()}},
		{"think call think", []any{r96RelayThink("t1"), r96RelayCall("call_aaa", "Read", map[string]any{"path": "/a"}), r96RelayThink("t2"), r96RelayDone()}},
		{"two calls", []any{r96RelayCall("call_a1", "Read", map[string]any{"path": "/a"}), r96RelayCall("call_b2", "Read", map[string]any{"path": "/b"}), r96RelayDone()}},
	} {
		for _, stream := range []bool{false, true} {
			runner := r96RelayRouter(t, r96RelayLane(sh.chunks), middleware.AnthropicMessagesMiddleware())
			rc, rb := r96RelayPost(t, runner, body(stream))
			relay := r96RelayLocal(t, r96RelayPeer(t, sh.chunks), middleware.AnthropicMessagesMiddleware())
			lc, lb := r96RelayPost(t, relay, body(stream))

			if rc != http.StatusOK || lc != http.StatusOK {
				t.Fatalf("%s stream=%v: runner %d, relay %d, want 200 on both", sh.name, stream, rc, lc)
			}
			if r96RelayBlocks(rb) != r96RelayBlocks(lb) {
				t.Errorf("%s stream=%v: one client body, two block orders (2026-09-29 audit, round 96, F96-L1-2):\n  runner lane [%s]\n  relay lane  [%s]",
					sh.name, stream, r96RelayBlocks(rb), r96RelayBlocks(lb))
			}
			t.Logf("%-16s stream=%-5v runner [%s] | relay [%s]", sh.name, stream, r96RelayBlocks(rb), r96RelayBlocks(lb))
		}
	}
}

// The header is the whole difference: a direct client that states it is served
// the turn the surface it names would be served — the same reading, without a
// relay in between.
func TestAStatedSurfaceIsServedTheTurnThatSurfaceEarns(t *testing.T) {
	chunks := []any{r96RelayText("let me look"), r96RelayCall("call_aaa", "Read", map[string]any{"path": "/a"}), r96RelayDone()}
	srv := r96RelayRouter(t, func(c *gin.Context) {
		applyRelayedSurfaceMark(c)
		r96RelayLane(chunks)(c)
	}, nil)

	_, plain := r96RelayPost(t, srv, `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if n := r96RelayCalls(plain); n != 0 {
		t.Errorf("a request stating no surface states %d calls, want 0 — the native wire's rule is upstream's", n)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/x", strings.NewReader(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(relayedSurfaceHeader, "openai")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if n := r96RelayCalls(string(out)); n != 1 {
		t.Errorf("a request that states the openai surface states %d calls, want the one the model made (2026-09-29 audit, round 96, F96-L1-1):\n%s", n, out)
	}
}
