package server

// zz_mine_98l1_test.go — MY OWN verification of round 98 leg 1's findings: the
// nameless entry, the id-less call, and the empty upstream channel, on the
// translated surfaces.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/fs/ggml"
	"github.com/ollama/ollama/llm"
	"github.com/ollama/ollama/middleware"
	"github.com/ollama/ollama/openai"
)

const zzTmpl = "{{ messages[0]['content'] }}{{/* tools tool_call tool_response */}}"

func zzArgs(k string, v any) api.ToolCallFunctionArguments {
	a := api.NewToolCallFunctionArguments()
	a.Set(k, v)
	return a
}

func zzBody(surface string, stream bool, model string) string {
	s := "false"
	if stream {
		s = "true"
	}
	switch surface {
	case "openai":
		return `{"model":"` + model + `","stream":` + s + `,"messages":[{"role":"user","content":"hi"}],` +
			`"tools":[{"type":"function","function":{"name":"Bash","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}]}`
	case "responses":
		return `{"model":"` + model + `","stream":` + s + `,"input":[{"role":"user","content":"hi"}],` +
			`"tools":[{"type":"function","name":"Bash","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	default:
		return `{"model":"` + model + `","max_tokens":64,"stream":` + s + `,"messages":[{"role":"user","content":"hi"}],` +
			`"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	}
}

// zzRunChunks answers one surface's client body from a fixed chunk list.
func zzRunChunks(t *testing.T, chunks []llm.ChatResponse, surface, model string, stream bool) (int, string, *Server) {
	t.Helper()
	t.Setenv("OLLAMA_CONTEXT_LENGTH", "4096")
	t.Setenv("OLLAMA_GO_TEMPLATE", "")
	gin.SetMode(gin.TestMode)
	setTestHome(t, t.TempDir())
	runner := &mockRunner{ChatFn: func(_ context.Context, _ llm.ChatRequest, fn func(llm.ChatResponse)) error {
		for _, c := range chunks {
			fn(c)
		}
		return nil
	}}
	s := newServerWithMockRunner(t, runner)
	createMinimalGGUFModel(t, s, model, ggml.KV{"tokenizer.chat_template": zzTmpl}, "",
		map[string]any{"capabilities": []string{"completion", "tools"}})
	code, body := zzPost(t, s, surface, stream, zzBody(surface, stream, model))
	return code, body, s
}

func zzPost(t *testing.T, s *Server, surface string, stream bool, body string) (int, string) {
	t.Helper()
	r := gin.New()
	path := "/api/chat"
	switch surface {
	case "openai":
		r.Use(middleware.ChatMiddleware())
		path = "/v1/chat/completions"
	case "responses":
		r.Use(middleware.ResponsesMiddleware())
		path = "/v1/responses"
	case "anthropic":
		r.Use(middleware.AnthropicMessagesMiddleware())
		path = "/v1/messages"
	}
	r.POST(path, s.ChatHandler)
	srv := httptest.NewServer(r)
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	return resp.StatusCode, string(out)
}

// zzCallIDs pulls the ids and names of the tool calls the OpenAI chat wire
// stated, both arms.
func zzCallIDs(t *testing.T, body string, stream bool) (text string, ids []string, names []string) {
	t.Helper()
	var read func(raw string)
	read = func(raw string) {
		var c struct {
			Choices []struct {
				Message struct {
					Content   any `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
				Delta struct {
					Content   any `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(raw), &c) != nil {
			return
		}
		for _, ch := range c.Choices {
			if s, ok := ch.Message.Content.(string); ok {
				text += s
			}
			if s, ok := ch.Delta.Content.(string); ok {
				text += s
			}
			for _, tc := range append(ch.Message.ToolCalls, ch.Delta.ToolCalls...) {
				ids = append(ids, tc.ID)
				names = append(names, tc.Function.Name)
			}
		}
	}
	if stream {
		for _, line := range strings.Split(body, "\n") {
			if d, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok && d != "[DONE]" {
				read(d)
			}
		}
		return text, ids, names
	}
	read(body)
	return text, ids, names
}

// zzFirstCallID answers the first `call_…` id the Anthropic arm stated, on
// either arm (the streamed one carries it JSON-escaped inside its SSE data).
func zzFirstCallID(body string) string {
	re := regexp.MustCompile(`call_[^"\\]+`)
	return re.FindString(body)
}

// zzResponseNames pulls the function-call names and call ids the Responses wire
// stated, both arms.
func zzResponseNames(t *testing.T, body string, stream bool) (text string, ids []string) {
	t.Helper()
	read := func(items []any) {
		for _, raw := range items {
			it, _ := raw.(map[string]any)
			switch it["type"] {
			case "function_call":
				id, _ := it["call_id"].(string)
				ids = append(ids, id)
			case "message":
				content, _ := it["content"].([]any)
				for _, c := range content {
					cm, _ := c.(map[string]any)
					s, _ := cm["text"].(string)
					text += s
				}
			}
		}
	}
	if stream {
		for _, line := range strings.Split(body, "\n") {
			d, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
			if !ok {
				continue
			}
			var ev struct {
				Type     string         `json:"type"`
				Response map[string]any `json:"response"`
			}
			if json.Unmarshal([]byte(d), &ev) != nil || ev.Type != "response.completed" {
				continue
			}
			items, _ := ev.Response["output"].([]any)
			read(items)
		}
		return text, ids
	}
	var resp openai.ResponsesResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", nil
	}
	b, _ := json.Marshal(resp.Output)
	var items []any
	_ = json.Unmarshal(b, &items)
	read(items)
	return text, ids
}

// TestMine98NamelessEntryOnTheTranslatedSurfaces is F98-L1-1: an entry the
// upstream never named is not a call — its bytes are the model's own output.
func TestMine98NamelessEntryOnTheTranslatedSurfaces(t *testing.T) {
	chunks := []llm.ChatResponse{
		{Message: api.Message{Role: "assistant", Content: "Let me look."}},
		{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "call_srv1",
			Function: api.ToolCallFunction{Name: "", Arguments: zzArgs("cmd", "ls")}}}}},
		{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonStop, PromptEvalCount: 5, EvalCount: 3},
	}
	for _, stream := range []bool{false, true} {
		_, anthropic, _ := zzRunChunks(t, chunks, "anthropic", "zz-nl-a", stream)
		// Premise: the Anthropic arm of this same handler relays the entry's
		// bytes as text and builds no tool_use. The buffered arm joins them to
		// the prose; the streamed arm sends them as a text_delta.
		if !strings.Contains(anthropic, "Let me look.") || strings.Contains(anthropic, "tool_use") {
			t.Fatalf("premise: this leg's own rule answered %q", anthropic)
		}
		if !stream && !strings.Contains(anthropic, `Let me look.{\"cmd\":\"ls\"}`) {
			t.Fatalf("premise: the buffered Anthropic arm answered %q", anthropic)
		}
		_, oa, _ := zzRunChunks(t, chunks, "openai", "zz-nl-o", stream)
		text, ids, names := zzCallIDs(t, oa, stream)
		t.Logf("openai stream=%v text=%q ids=%v names=%v", stream, text, ids, names)
		if len(ids) != 0 || text != `Let me look.{"cmd":"ls"}` {
			t.Errorf("openai stream=%v answered text=%q calls=%v/%v — an entry the upstream never named is not a call (round 77, F77-L1-1): its bytes are the model's own output and the Anthropic arm of the same handler relays them as TEXT (%q) (2026-09-29 audit, round 98, F98-L1-1)",
				stream, text, ids, names, `Let me look.{"cmd":"ls"}`)
		}
		_, rs, _ := zzRunChunks(t, chunks, "responses", "zz-nl-r", stream)
		rtext, rids := zzResponseNames(t, rs, stream)
		t.Logf("responses stream=%v text=%q ids=%v", stream, rtext, rids)
		if len(rids) != 0 || rtext != `Let me look.{"cmd":"ls"}` {
			t.Errorf("responses stream=%v answered text=%q calls=%v — the same rule (2026-09-29 audit, round 98, F98-L1-1)", stream, rtext, rids)
		}
	}
}

// TestMine98IdlessCallKeepsAnEchoableId is F98-L1-2: a call the upstream never
// gave an id is minted the id this leg's other arms mint for it, so a client
// that echoes the id it was handed is paired back with the call's name.
func TestMine98IdlessCallKeepsAnEchoableId(t *testing.T) {
	chunks := []llm.ChatResponse{
		{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "",
			Function: api.ToolCallFunction{Name: "Bash", Arguments: zzArgs("cmd", "ls")}}}}},
		{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonStop, PromptEvalCount: 5, EvalCount: 3},
	}
	for _, stream := range []bool{false, true} {
		_, anthropic, _ := zzRunChunks(t, chunks, "anthropic", "zz-id-a", stream)
		want := zzFirstCallID(anthropic)
		if want == "" {
			t.Fatalf("premise: the Anthropic arm minted nothing for the id-less call: %q", anthropic)
		}
		_, oa, _ := zzRunChunks(t, chunks, "openai", "zz-id-o", stream)
		_, ids, names := zzCallIDs(t, oa, stream)
		_, rs, _ := zzRunChunks(t, chunks, "responses", "zz-id-r", stream)
		_, rids := zzResponseNames(t, rs, stream)
		t.Logf("openai stream=%v ids=%v names=%v responses ids=%v anthropic mint=%s", stream, ids, names, rids, want)
		if len(rids) != 1 || rids[0] != want {
			t.Errorf("the Responses surface stream=%v stated the id-less call under %v, where the Anthropic arm of the same handler minted %q — a client answers a call by the `call_id` it was given (2026-09-29 audit, round 98, F98-L1-2)",
				stream, rids, want)
		}
		if len(ids) == 1 && ids[0] != "" && ids[0] != want {
			t.Errorf("openai stream=%v handed the client %q for the id-less call, where the Anthropic arm of the same handler handed %q — one body, one id (2026-09-29 audit, round 98, F98-L1-2)",
				stream, ids[0], want)
		}
		if len(ids) != 1 || ids[0] == "" {
			t.Errorf("openai stream=%v handed the client the call %v/%v with no id — a client can only answer a call by the id it was given, and the mint is what this leg's own arms and the other two legs put there (2026-09-29 audit, round 98, F98-L1-2)\n  anthropic %s", stream, ids, names, anthropic)
			continue
		}
		// The echo: the client's next request carries the id it was handed.
		last := zzBody("openai", false, "zz-id-o")
		last = strings.Replace(last, `{"role":"user","content":"hi"}`,
			`{"role":"user","content":"hi"},`+
				`{"role":"assistant","content":"","tool_calls":[{"id":"`+ids[0]+`","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},`+
				`{"role":"tool","tool_call_id":"`+ids[0]+`","content":"file1"}`, 1)
		var req openai.ChatCompletionRequest
		if err := json.Unmarshal([]byte(last), &req); err != nil {
			t.Fatal(err)
		}
		out, err := openai.FromChatRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		got := out.Messages[len(out.Messages)-1]
		if got.ToolName != "Bash" {
			t.Errorf("openai stream=%v: a client echoing the id %q it was handed was read back with toolName=%q, want Bash — the pairing is by that id (2026-09-29 audit, round 98, F98-L1-2)",
				stream, ids[0], got.ToolName)
		}
	}
}

// TestMine98TheMintsAgreeWithTheAnthropicArm pins the halves of the mint rule
// that a single id-less call does not exercise: the second occurrence of one
// call identity takes a distinct id, a mint never lands on an id the turn
// states, and a stated id reused for a DIFFERENT call is re-minted rather than
// delivered under an id that names the first.
func TestMine98TheMintsAgreeWithTheAnthropicArm(t *testing.T) {
	stated := func(id, name string, cmd string) api.ToolCall {
		return api.ToolCall{ID: id, Function: api.ToolCallFunction{Name: name, Arguments: zzArgs("cmd", cmd)}}
	}
	mintOf := func(name, args string) string {
		return anthropic.ToolCallIDFor(name, args)
	}
	done := llm.ChatResponse{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonStop, PromptEvalCount: 5, EvalCount: 3}

	for _, tc := range []struct {
		name   string
		calls  []api.ToolCall
		reason string
	}{
		{
			name:   "one identity stated twice",
			calls:  []api.ToolCall{stated("", "Bash", "ls"), stated("", "Bash", "ls")},
			reason: "the second occurrence of one identity is a second call and takes a distinct id (round 40, A40-6)",
		},
		{
			name:   "a mint that would land on a stated id",
			calls:  []api.ToolCall{stated(mintOf("Bash", `{"cmd":"ls"}`), "Read", "cat"), stated("", "Bash", "ls")},
			reason: "a synthesized id must not land on an id another call in the same turn states (round 46, A46-4)",
		},
		{
			name:   "one id stated for two different calls",
			calls:  []api.ToolCall{stated("call_shared", "Bash", "ls"), stated("call_shared", "Read", "cat")},
			reason: "one id reused for two calls reaches the client under one id, and cannot be answered separately (round 45, A45-3)",
		},
	} {
		chunks := []llm.ChatResponse{{Message: api.Message{Role: "assistant", ToolCalls: tc.calls}}, done}
		_, anthropicBody, _ := zzRunChunks(t, chunks, "anthropic", "zz-mint-a", false)
		want := regexp.MustCompile(`"id":"(call_[^"\\]+)"`).FindAllStringSubmatch(anthropicBody, -1)
		var wantIDs []string
		for _, m := range want {
			wantIDs = append(wantIDs, m[1])
		}
		_, oa, _ := zzRunChunks(t, chunks, "openai", "zz-mint-o", false)
		_, ids, _ := zzCallIDs(t, oa, false)
		t.Logf("%s: openai %v anthropic %v", tc.name, ids, wantIDs)
		if len(wantIDs) != 2 {
			t.Fatalf("premise: the Anthropic arm stated %v for %s", wantIDs, tc.name)
		}
		if len(ids) != len(wantIDs) {
			t.Errorf("%s: the OpenAI chat surface stated %d calls %v where the Anthropic arm of the same handler stated %d %v (2026-09-29 audit, round 98, F98-L1-2)",
				tc.name, len(ids), ids, len(wantIDs), wantIDs)
			continue
		}
		for i := range ids {
			if ids[i] != wantIDs[i] {
				t.Errorf("%s: call %d reached the OpenAI chat surface as %q and the Anthropic arm as %q — one body, one id, and %s (2026-09-29 audit, round 98, F98-L1-2)",
					tc.name, i, ids[i], wantIDs[i], tc.reason)
			}
		}
		if ids[0] == ids[1] {
			t.Errorf("%s: both calls reached the client under %q — two calls under one id cannot be answered separately", tc.name, ids[0])
		}
	}
}

// TestMine98TheResponsesArmStatesTheTurnInOneOrder is F98-L1-3 end to end: the
// buffered arm of the Responses surface must state the turn's items in the order
// its streamed sibling states them, which is the order the turn's runs arrived
// in (openai/round98_response_item_order_from_runs_test.go pins the arm itself;
// this pins the runs the handler hands it).
func TestMine98TheResponsesArmStatesTheTurnInOneOrder(t *testing.T) {
	done := llm.ChatResponse{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonStop, PromptEvalCount: 5, EvalCount: 3}
	for i, turn := range []struct {
		name   string
		chunks []llm.ChatResponse
	}{
		{"prose, then reasoning", []llm.ChatResponse{
			{Message: api.Message{Role: "assistant", Content: "A"}},
			{Message: api.Message{Role: "assistant", Thinking: "T"}},
		}},
		{"prose either side of the reasoning", []llm.ChatResponse{
			{Message: api.Message{Role: "assistant", Content: "A"}},
			{Message: api.Message{Role: "assistant", Thinking: "T"}},
			{Message: api.Message{Role: "assistant", Content: "B"}},
		}},
		{"reasoning, the call, then the prose", []llm.ChatResponse{
			{Message: api.Message{Role: "assistant", Thinking: "T"}},
			{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{Function: api.ToolCallFunction{Name: "Bash", Arguments: zzArgs("cmd", "ls")}}}}},
			{Message: api.Message{Role: "assistant", Content: "A"}},
		}},
	} {
		chunks := append(append([]llm.ChatResponse{}, turn.chunks...), done)
		model := fmt.Sprintf("zz-ord2-%d", i)
		_, doc, _ := zzRunChunks(t, chunks, "responses", model, false)
		_, str, _ := zzRunChunks(t, chunks, "responses", model, true)
		buffered, streamed := zzOrderDump(t, doc, false), zzOrderDump(t, str, true)
		if len(buffered) == 0 || len(streamed) == 0 {
			t.Fatalf("premise: %s answered %d/%d items\nbuffered %s\nstreamed %s", turn.name, len(buffered), len(streamed), doc, str)
		}
		if strings.Join(buffered, "|") != strings.Join(streamed, "|") {
			t.Errorf("%s: the buffered arm of the Responses surface states %v and its streamed arm states %v — one upstream body, one item order, and the order is the one the turn's runs arrived in (2026-09-29 audit, round 98, F98-L1-4)",
				turn.name, buffered, streamed)
		}
	}
}

// TestMine98EmptyChannelOnTheTranslatedSurfaces is F98-L1-4: an upstream
// channel that states nothing is refused the way round 73 pinned for the
// Anthropic surface.
func TestMine98EmptyChannelOnTheTranslatedSurfaces(t *testing.T) {
	for _, stream := range []bool{false, true} {
		_, anthropic, _ := zzRunChunks(t, nil, "anthropic", "zz-ec-a", stream)
		if !strings.Contains(anthropic, "upstream returned an empty stream") {
			t.Fatalf("premise: the anthropic arm answered %q", anthropic)
		}
		for _, surface := range []string{"openai", "responses"} {
			code, body, _ := zzRunChunks(t, nil, surface, "zz-ec-"+surface, stream)
			t.Logf("%s stream=%v %d %q", surface, stream, code, body)
			if code != http.StatusBadGateway || !strings.Contains(body, "upstream returned an empty stream") {
				t.Errorf("%s stream=%v answered %d %q for an upstream channel that never stated a chunk — the Anthropic arm of the same handler answers 502 with that cause, and so do the other two legs (round 73, F73-L1-2; 2026-09-29 audit, round 98, F98-L1-4)",
					surface, stream, code, body)
			}
		}
	}
}

// zzOrderDump reports each item the Responses wire stated as "kind:text", in the
// order it stated them, on either arm.
func zzOrderDump(t *testing.T, body string, stream bool) []string {
	t.Helper()
	var out []string
	read := func(items []any) {
		for _, raw := range items {
			it, _ := raw.(map[string]any)
			kind, _ := it["type"].(string)
			if kind == "message" {
				content, _ := it["content"].([]any)
				for _, c := range content {
					cm, _ := c.(map[string]any)
					s, _ := cm["text"].(string)
					out = append(out, "message:"+s)
				}
				continue
			}
			if kind == "reasoning" {
				sum, _ := it["summary"].([]any)
				s := ""
				for _, c := range sum {
					cm, _ := c.(map[string]any)
					x, _ := cm["text"].(string)
					s += x
				}
				out = append(out, "reasoning:"+s)
				continue
			}
			out = append(out, kind)
		}
	}
	if stream {
		for _, line := range strings.Split(body, "\n") {
			d, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
			if !ok {
				continue
			}
			var ev struct {
				Type     string         `json:"type"`
				Response map[string]any `json:"response"`
			}
			if json.Unmarshal([]byte(d), &ev) != nil || ev.Type != "response.completed" {
				continue
			}
			items, _ := ev.Response["output"].([]any)
			read(items)
		}
		return out
	}
	var resp openai.ResponsesResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil
	}
	b, _ := json.Marshal(resp.Output)
	var items []any
	_ = json.Unmarshal(b, &items)
	read(items)
	return out
}

func TestMine98OrderDump(t *testing.T) {
	chunks := []llm.ChatResponse{
		{Message: api.Message{Role: "assistant", Content: "A"}},
		{Message: api.Message{Role: "assistant", Thinking: "T"}},
		{Message: api.Message{Role: "assistant", Content: "B"}},
		{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonStop, PromptEvalCount: 5, EvalCount: 3},
	}
	for _, stream := range []bool{false, true} {
		_, rs, _ := zzRunChunks(t, chunks, "responses", "zz-ord-r", stream)
		t.Logf("responses stream=%v %v\n%s", stream, zzOrderDump(t, rs, stream), rs)
		_, an, _ := zzRunChunks(t, chunks, "anthropic", "zz-ord-a", stream)
		t.Logf("anthropic stream=%v %s", stream, an)
		_, oa, _ := zzRunChunks(t, chunks, "openai", "zz-ord-o", stream)
		t.Logf("openai stream=%v %s", stream, oa)
	}
}
