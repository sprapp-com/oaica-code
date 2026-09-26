package launch

// proxy_tool_call_id_integrity_test.go — a non-streaming tool call with no
// upstream id was forwarded with no id at all (2026-09-26 audit, thirteenth
// round).
//
// Several OpenAI-compatible GGUF backends send tool calls without an `id`.
// The streaming path learned to synthesize one from the call's own identity
// (name + arguments — two genuinely different parallel calls differ in
// arguments, and two identical ones are indistinguishable anyway), so the
// client can name the call back in its tool_result. The non-streaming path
// passed tc.ID through verbatim, and ContentBlock.ID is omitempty, so the
// client received a tool_use block with NO id: it cannot name the call back,
// its tool_result then carries tool_use_id "" into the next request as an
// empty OpenAI tool_call_id, and the turn either errors upstream or silently
// loses tool routing — on the one wire where the streaming path works.
//
// The fix is the same rule in the same place: one exported helper in the
// anthropic package, used by both paths.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// toolCallBlocks returns the tool_use content blocks from an Anthropic
// Messages response.
func toolCallBlocks(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var got struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	var out []map[string]any
	for _, b := range got.Content {
		if b["type"] == "tool_use" {
			out = append(out, b)
		}
	}
	return out
}

func TestANonStreamToolCallWithoutAnUpstreamIDGetsASynthesizedOne(t *testing.T) {
	up := jsonUpstream(t, `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/tmp/x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`)
	defer up.Close()
	proxy := startCalibProxy(t, up.URL, "sess-tool-id-missing")

	resp, err := http.Post(proxy+"/v1/messages", "application/json", strings.NewReader(string(calibMessagesBody(t, 32, 64, false))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d\n%s", resp.StatusCode, raw)
	}
	blocks := toolCallBlocks(t, raw)
	if len(blocks) != 1 {
		t.Fatalf("got %d tool_use block(s), want 1\n%s", len(blocks), raw)
	}
	id, _ := blocks[0]["id"].(string)
	if id == "" {
		t.Errorf("the tool_use block carries no id for an upstream call that sent none: the client cannot name the call back, its tool_result then carries tool_use_id \"\" into the next request as an empty OpenAI tool_call_id, and the turn either errors upstream or silently loses tool routing\n%s", raw)
	}
	if name, _ := blocks[0]["name"].(string); name != "Read" {
		t.Errorf("tool name = %q, want Read\n%s", name, raw)
	}
}

// Control: an id the upstream DID send is preserved verbatim — the client's
// tool_result echoes it, and the upstream's own correlation must not be
// replaced.
func TestAStatedToolCallIDIsPreserved(t *testing.T) {
	up := jsonUpstream(t, `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_abc123","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/tmp/x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`)
	defer up.Close()
	proxy := startCalibProxy(t, up.URL, "sess-tool-id-stated")

	resp, err := http.Post(proxy+"/v1/messages", "application/json", strings.NewReader(string(calibMessagesBody(t, 32, 64, false))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	blocks := toolCallBlocks(t, raw)
	if len(blocks) != 1 {
		t.Fatalf("got %d tool_use block(s), want 1\n%s", len(blocks), raw)
	}
	if id, _ := blocks[0]["id"].(string); id != "call_abc123" {
		t.Errorf("tool_use id = %q, want the upstream's own call_abc123\n%s", id, raw)
	}
}

// Two parallel calls differing only in name/arguments must get DIFFERENT ids:
// an id shared between two calls is a tool_result the client cannot route.
func TestTwoDistinctCallsWithoutIDsGetDistinctIDs(t *testing.T) {
	up := jsonUpstream(t, `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/a\"}"}},{"type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/b\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`)
	defer up.Close()
	proxy := startCalibProxy(t, up.URL, "sess-tool-id-distinct")

	resp, err := http.Post(proxy+"/v1/messages", "application/json", strings.NewReader(string(calibMessagesBody(t, 32, 64, false))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	blocks := toolCallBlocks(t, raw)
	if len(blocks) != 2 {
		t.Fatalf("got %d tool_use block(s), want 2\n%s", len(blocks), raw)
	}
	a, _ := blocks[0]["id"].(string)
	b, _ := blocks[1]["id"].(string)
	if a == "" || b == "" {
		t.Fatalf("ids = %q, %q — both must be non-empty\n%s", a, b, raw)
	}
	if a == b {
		t.Errorf("two distinct parallel calls got the SAME id %q: the client cannot route their tool_results apart\n%s", a, raw)
	}
}

// Two IDENTICAL id-less calls must not leave the client with two tool_use
// blocks sharing one id: that shape is a protocol violation (Anthropic never
// emits it), and the tool_result round-trip becomes ambiguous — one
// tool_result satisfies both blocks, and the follow-up request carries two
// OpenAI tool messages with the same tool_call_id.
//
// The streaming path already answers this exact body with ONE block: its
// accumulator keys on name+arguments (`anthropic.go`, toolCallsSent), so the
// duplicate is skipped. Both paths translate the same upstream body, so they
// must agree; the duplicate-id shape is the half that cannot be right
// (2026-09-26 audit, fourteenth round).
func TestTwoIdenticalCallsWithoutIDsDoNotShareOneID(t *testing.T) {
	up := jsonUpstream(t, `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"Bash","arguments":"{}"}},{"type":"function","function":{"name":"Bash","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`)
	defer up.Close()
	proxy := startCalibProxy(t, up.URL, "sess-tool-id-identical")

	resp, err := http.Post(proxy+"/v1/messages", "application/json", strings.NewReader(string(calibMessagesBody(t, 32, 64, false))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	blocks := toolCallBlocks(t, raw)
	if len(blocks) != 1 {
		t.Errorf("got %d tool_use block(s) for two identical id-less calls, want 1 — the streaming path emits one for this same body, and two blocks sharing one synthesized id is a protocol violation\n%s", len(blocks), raw)
	}
	if len(blocks) > 1 {
		a, _ := blocks[0]["id"].(string)
		b, _ := blocks[1]["id"].(string)
		if a == b {
			t.Errorf("both tool_use blocks carry id %q; a tool_result for it satisfies both\n%s", a, raw)
		}
	}
}
