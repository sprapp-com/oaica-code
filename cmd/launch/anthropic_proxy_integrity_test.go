package launch

// anthropic_proxy_integrity_test.go — the Anthropic-to-OpenAI proxy leg
// (2026-09-26 audit, fourth round):
//
//   - a system prompt arriving as an array of text blocks (Claude Code's own
//     shape, one block carrying cache_control) was delivered to the model as
//     escaped JSON with the block metadata inline — every instruction present
//     and unrecognisable;
//   - top_k was parsed away: the Anthropic field has no OpenAI sibling, so a
//     client setting it got the provider's default and no signal that it had
//     been ignored;
//   - a stop_reason of tool_use could be reported for a turn with no tool_use
//     block.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func proxyUnderTest(t *testing.T, upstream http.Handler) (string, string) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	us := httptest.NewServer(upstream)
	t.Cleanup(us.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	token, err := StartAnthropicOpenAIProxy(ln, userRemote{Name: "t", BaseURL: us.URL, APIKey: "k"}, "m")
	if err != nil {
		t.Fatal(err)
	}
	return "http://" + ln.Addr().String(), token
}

func postMessagesBody(t *testing.T, url, token, body string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/v1/messages", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	return string(b)
}

// F1: a block-array system prompt is normalized to prose on the local leg.
// The normalize step is what `oaica serve` runs in front of llama-server —
// the Anthropic->OpenAI converter covers the same shape on the remote leg,
// and the test below this one checks the model-visible end of that path.
func TestNormalizeSystemMessagesFlattensBlockArrays(t *testing.T) {
	out, err := normalizeSystemMessages("/v1/messages", []byte(
		`{"model":"m","max_tokens":10,"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"Be brief."}],"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	sys, _ := m["system"].(string)
	if sys != "You are Claude Code.\n\nBe brief." {
		t.Errorf("normalizeSystemMessages left system = %q, want the two text blocks as prose — the JSON rendering keeps every instruction but makes it unrecognisable to the model", sys)
	}
	if strings.Contains(sys, "cache_control") || strings.Contains(sys, `{\"`) {
		t.Errorf("block metadata survived into the system prompt: %q", sys)
	}
}

// F1, end to end: what the model actually receives on the remote leg.
func TestProxyFlattensABlockArraySystemPrompt(t *testing.T) {
	var upstream map[string]any
	url, token := proxyUnderTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&upstream)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "c1", "model": "m",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"}}}})
	}))
	postMessagesBody(t, url, token,
		`{"model":"m","max_tokens":10,"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"Be brief."}],"messages":[{"role":"user","content":"hi"}]}`)

	// The OpenAI wire has no top-level system field: the proxy lands it as
	// the first message, role "system".
	msgs, _ := upstream["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("upstream got no messages: %v", upstream)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("first upstream message role = %v, want system: %v", first["role"], upstream)
	}
	sys, _ := first["content"].(string)
	if sys != "You are Claude Code.\n\nBe brief." {
		t.Errorf("upstream system = %q, want the two blocks joined as prose — a JSON rendering keeps every instruction but makes it unrecognisable to the model", sys)
	}
	if strings.Contains(sys, "cache_control") || strings.Contains(sys, `{\"`) {
		t.Errorf("block metadata survived into the prompt: %q", sys)
	}
}

// F9: top_k is forwarded rather than silently dropped.
func TestProxyForwardsTopK(t *testing.T) {
	var upstream map[string]any
	url, token := proxyUnderTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&upstream)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "c1", "model": "m",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"}}}})
	}))
	postMessagesBody(t, url, token,
		`{"model":"m","max_tokens":10,"top_k":17,"temperature":0.3,"messages":[{"role":"user","content":"hi"}]}`)

	if got, ok := toInt(upstream["top_k"]); !ok || got != 17 {
		t.Errorf("upstream top_k = %#v, want 17 — the field was parsed away, so a client that set it is silently served the provider default", upstream["top_k"])
	}
}

// F7a: a stream that ends with no upstream finish_reason still closes the
// turn with a legal stop_reason. The message_delta used to be able to carry
// an empty one, which (being omitempty) dropped the key from the JSON — a
// client then treats the turn as unfinished.
func TestStreamingTurnAlwaysClosesWithAStopReason(t *testing.T) {
	url, token := proxyUnderTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":24,\"completion_tokens\":2}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	body := postMessagesBody(t, url, token,
		`{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	legal := map[string]bool{"end_turn": true, "max_tokens": true, "stop_sequence": true, "tool_use": true, "pause_turn": true, "refusal": true}
	found := false
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil || ev.Type != "message_delta" {
			continue
		}
		found = true
		if !legal[ev.Delta.StopReason] {
			t.Errorf("message_delta stop_reason = %q — the field is omitempty, so an empty or unknown value either vanishes from the JSON or is a value no Anthropic client models; the turn is then treated as unfinished", ev.Delta.StopReason)
		}
	}
	if !found {
		t.Errorf("no message_delta was emitted at all:\n%s", body)
	}
}

// F7c: tool_calls with nothing attached is not tool_use.
func TestProxyDoesNotReportToolUseWithoutAToolCall(t *testing.T) {
	url, token := proxyUnderTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "c1", "model": "m",
			"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{"role": "assistant", "content": ""}}}})
	}))
	body := postMessagesBody(t, url, token,
		`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	var resp struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("not Anthropic JSON: %v\n%s", err, body)
	}
	if resp.StopReason == "tool_use" {
		t.Errorf("stop_reason = tool_use with %d content blocks (%s) — an agent reads that as a tool call arriving, finds none, and stalls", len(resp.Content), body)
	}
}
