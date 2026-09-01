package main

// messages_test.go — Anthropic /v1/messages wire translation tests. Each
// case exercises the full handler chain (auth, conversion, metering,
// translation) against a fake OpenAI upstream, like main_test.go does for
// the native wire.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postMessages(t *testing.T, g *gateway, apiKey string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.messagesHandler(w, req)
	return w
}

func testGatewayForMessages(t *testing.T, got *map[string]any) (*gateway, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		if got != nil {
			*got = body
		}
		stream, _ := body["stream"].(bool)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"chatcmpl-abc","choices":[{"finish_reason":"stop","message":{"content":"Hello!"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		f.Flush()
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n\n")
		f.Flush()
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		f.Flush()
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":5}}\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	t.Cleanup(srv.Close)
	g := &gateway{}
	cfg := gwConfig{
		UpstreamAddr: srv.URL, ListenAddr: ":0",
		LedgerPath: t.TempDir() + "/ledger.jsonl",
		APIKeys:    []gwKey{{SHA256: keyHash("sk-test"), Label: "test"}},
		Models: []gwModel{{
			ID: "oaica-35b-a3b-vision", UpstreamID: "oaica-35b-a3b-vision", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
		}},
	}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return g, srv
}

func TestMessages_NonStreamTranslation(t *testing.T) {
	var got map[string]any
	g, _ := testGatewayForMessages(t, &got)

	w := postMessages(t, g, "sk-test", map[string]any{
		"model":      "oaica-35b-a3b-vision",
		"max_tokens": 100,
		"system":     "be terse",
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not Anthropic JSON: %v\n%s", err, w.Body.String())
	}
	if resp.Type != "message" || resp.Role != "assistant" {
		t.Fatalf("type/role = %s/%s, want message/assistant", resp.Type, resp.Role)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "Hello!" {
		t.Fatalf("content = %+v, want one text block \"Hello!\"", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 3 {
		t.Errorf("usage = %d/%d, want 7/3 (metering must survive translation)", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
	// The request that reached the upstream must be OpenAI-shaped.
	if m, _ := got["model"].(string); m != "oaica-35b-a3b-vision" {
		t.Errorf("upstream model = %v, want rewritten upstream id", got["model"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("upstream got %d messages, want 2 (system + user)", len(msgs))
	}
	if first, _ := msgs[0].(map[string]any); first["role"] != "system" {
		t.Errorf("first message role = %v, want system", first["role"])
	}
}

func TestMessages_StreamTranslation(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		f.Flush()
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		f.Flush()
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		f.Flush()
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":5}}\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer up.Close()
	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: up.URL, ListenAddr: ":0",
		LedgerPath: t.TempDir() + "/ledger.jsonl",
		APIKeys:    []gwKey{{SHA256: keyHash("sk-test"), Label: "openrouter"}},
		Models:     []gwModel{{ID: "oaica-35b-a3b-vision", UpstreamID: "oaica-35b-a3b-vision", OwnedBy: "oaica"}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	w := postMessages(t, g, "sk-test", map[string]any{
		"model": "oaica-35b-a3b-vision", "max_tokens": 100, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE missing %q:\n%s", want, body)
		}
	}
	// The two text deltas must land as text_delta events.
	if got := strings.Count(body, `"text_delta"`); got != 2 {
		t.Errorf("text_delta count = %d, want 2:\n%s", got, body)
	}
	if !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Errorf("message_delta missing end_turn:\n%s", body)
	}
	// Real usage must land in the final message_delta.
	if !strings.Contains(body, `"input_tokens":11`) || !strings.Contains(body, `"output_tokens":5`) {
		t.Errorf("final usage missing from message_delta:\n%s", body)
	}
}

func TestMessages_ToolUseRoundTrip(t *testing.T) {
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-t","choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[{"id":"call_1","function":{"name":"get_weather","arguments":"{\"city\":\"JB\"}"}}]}}],"usage":{"prompt_tokens":20,"completion_tokens":9}}`)
	}))
	defer up.Close()
	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: up.URL, ListenAddr: ":0",
		LedgerPath: t.TempDir() + "/ledger.jsonl",
		APIKeys:    []gwKey{{SHA256: keyHash("sk-test"), Label: "openrouter"}},
		Models:     []gwModel{{ID: "m1", UpstreamID: "m1", OwnedBy: "oaica"}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	w := postMessages(t, g, "sk-test", map[string]any{
		"model": "m1", "max_tokens": 100,
		"messages": []map[string]any{
			{"role": "user", "content": "weather in JB?"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": map[string]any{"city": "JB"}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "call_1", "content": "32C sunny"},
			}},
		},
		"tools": []map[string]any{
			{"name": "get_weather", "description": "weather", "input_schema": map[string]any{"type": "object"}},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Content []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" || resp.Content[0].Name != "get_weather" {
		t.Fatalf("content = %+v, want one tool_use get_weather block", resp.Content)
	}
	if resp.Content[0].Input["city"] != "JB" {
		t.Errorf("tool input = %+v, want city=JB", resp.Content[0].Input)
	}
	// Upstream must have received tool messages + tool_calls, not blocks.
	if _, ok := got["tools"].([]any); !ok {
		t.Errorf("tools dropped in conversion: upstream body = %v", got)
	}
	msgs, _ := got["messages"].([]any)
	var sawTool, sawToolCall bool
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "tool" {
			sawTool = true
		}
		if _, ok := mm["tool_calls"]; ok {
			sawToolCall = true
		}
	}
	if !sawTool || !sawToolCall {
		t.Errorf("upstream messages missing tool/tool_calls: %v", msgs)
	}
}

func TestMessages_ErrorShapeTranslation(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"type":"ModelError","message":"Model x is not supported"}}`)
	}))
	defer up.Close()
	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: up.URL, ListenAddr: ":0",
		LedgerPath: t.TempDir() + "/ledger.jsonl",
		APIKeys:    []gwKey{{SHA256: keyHash("sk-test"), Label: "openrouter"}},
		Models:     []gwModel{{ID: "m1", UpstreamID: "m1", OwnedBy: "oaica"}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	w := postMessages(t, g, "sk-test", map[string]any{
		"model": "m1", "max_tokens": 10,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 passthrough, body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Type != "error" || resp.Error.Message == "" {
		t.Fatalf("error not in Anthropic envelope: %s", w.Body.String())
	}
}

func TestMessages_MeteredLikeNativeWire(t *testing.T) {
	g, _ := testGatewayForMessages(t, nil)

	postMessages(t, g, "sk-test", map[string]any{
		"model": "oaica-35b-a3b-vision", "max_tokens": 100,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	// The ledger row must exist with real usage — translation must not
	// open a billing bypass.
	rows := readLedger(t, g.cfg.LedgerPath)
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(rows))
	}
	if rows[0].PromptTokens != 7 || rows[0].CompletionTokens != 3 {
		t.Errorf("metered usage = %d/%d, want 7/3", rows[0].PromptTokens, rows[0].CompletionTokens)
	}
	// The proxied path is pinned to /v1/chat/completions (the upstream must
	// answer on the OpenAI wire — see messagesHandler), so the ledger row
	// records the upstream wire, not the client-facing one. Metering is the
	// point: translation must not open a billing bypass.
	if rows[0].Path != "/v1/chat/completions" {
		t.Errorf("ledger path = %q, want /v1/chat/completions", rows[0].Path)
	}
}