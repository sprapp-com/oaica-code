package launch

// round69_idless_pair_and_truncation_gate_integrity_test.go — leg 2, two reads
// that decided a turn by the wrong entry.
//
// (1) The truncation gate. Round 68 taught this leg's whole-document arm to
// take its VERDICT from the last finish_reason any choice stated, and left the
// gate that decides whether an unfinished argument fragment is a call reading
// Choices[0] eight lines above it — so on a two-choice completion the function
// contradicted itself: it relayed a half-written call as runnable under
// stop_reason tool_use, or dropped a call the turn's own last reason asked for.
// This leg's fragment arm, which folds the reason across every frame, answers
// max_tokens for the same body (2026-09-28 audit, round 69).
//
// (2) The ID-LESS PAIR. Two identical calls that state an index and no id are
// two calls — A45-2's rule, which the whole-list parser honours — but the delta
// arm's restatement fold matched them (same name, same finished arguments, no
// id on either side) and answered ONE, so the model's second tool request
// vanished and the client ran the tool once where it asked twice.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// r69IdlessPair is one call the model asked for twice with no id on either
// entry, the index stated.
const r69IdlessPairCall = `{"index":0,"type":"function","function":{"name":"do_thing","arguments":"{}"}}`

// r69IdlessPairDoc is the whole completion for that turn.
func r69IdlessPairDoc() string {
	return `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"tool_calls",` +
		`"message":{"role":"assistant","tool_calls":[` + r69IdlessPairCall + `,` + r69IdlessPairCall + `]}}],` +
		`"usage":{"prompt_tokens":7,"completion_tokens":3}}`
}

// r69ToolUseIDs posts one turn through the proxy and returns the tool_use ids
// the client was handed, from either arm's shape. The streamed turn is spelled
// as real DELTA frames (one chunk listing both entries), not as a whole
// completion inside a frame: the adoption arm is a third spelling of this turn
// and is not what this finding is about.
func r69ToolUseIDs(t *testing.T, stream bool) []string {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	var up *httptest.Server
	if stream {
		up = streamUpstream(t,
			`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[`+r69IdlessPairCall+`,`+r69IdlessPairCall+`]}}]}`+"\n\n"+
				`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n"+
				`data: [DONE]`+"\n\n", true)
	} else {
		up = jsonUpstream(t, r69IdlessPairDoc())
	}
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", stream)
	if code != http.StatusOK {
		t.Fatalf("status %d\n%s", code, body)
	}
	if !stream {
		var out struct {
			Content []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, body)
		}
		var ids []string
		for _, b := range out.Content {
			if b.Type == "tool_use" {
				ids = append(ids, b.ID)
			}
		}
		return ids
	}
	blocks := sseToolUseBlocks(t, body)
	ids := make([]string, 0, len(blocks))
	for _, b := range blocks {
		id, _ := b["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// TestTheDeltaArmKeepsTwoIdenticalIdlessCalls is finding (2).
func TestTheDeltaArmKeepsTwoIdenticalIdlessCalls(t *testing.T) {
	streamed := r69ToolUseIDs(t, true)
	if len(streamed) != 2 {
		t.Errorf("the delta arm answered %d tool_use block(s) (%v) for two identical id-less calls; the non-stream arm answers %d: the model asked for the tool twice and this arm folded the two into one, so the client ran it once (A45-2; 2026-09-28 audit, round 69)",
			len(streamed), streamed, len(r69ToolUseIDs(t, false)))
	}
}

// TestTheDocumentArmAlsoKeepsTwoIdenticalIdlessCalls is the control that was
// already right, and must stay so.
func TestTheDocumentArmAlsoKeepsTwoIdenticalIdlessCalls(t *testing.T) {
	if ids := r69ToolUseIDs(t, false); len(ids) != 2 {
		t.Errorf("the non-stream arm answered %d tool_use block(s) (%v) for two identical id-less calls, want 2 (A45-2)", len(ids), ids)
	}
}

// r69TruncDoc is a completion whose FIRST choice carries a call the model was
// still writing and whose SECOND choice states the token limit.
func r69TruncDoc() string {
	return `{"id":"c","object":"chat.completion","choices":[` +
		`{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":"}}]},"finish_reason":"stop"},` +
		`{"index":1,"message":{"role":"assistant","content":""},"finish_reason":"length"}],` +
		`"usage":{"prompt_tokens":7,"completion_tokens":3}}`
}

// TestTheTruncationGateReadsTheReasonAnyChoiceStated is finding (1): the last
// stated reason is the token limit, so the unfinished call is not a call.
func TestTheTruncationGateReadsTheReasonAnyChoiceStated(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	up := jsonUpstream(t, r69TruncDoc())
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})

	code, body := round54PostMessage(t, proxy, "glm-5.3", false)
	if code != http.StatusOK {
		t.Fatalf("status %d\n%s", code, body)
	}
	var out struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	for _, b := range out.Content {
		if b.Type == "tool_use" {
			t.Errorf("the whole-document arm relayed a tool_use for a turn whose last stated reason is the token limit: the call was still being written, and this leg's fragment arm drops it (2026-09-28 audit, round 69)\n%s", body)
			break
		}
	}
	if out.StopReason != "max_tokens" {
		t.Errorf("the whole-document arm answered stop_reason %q, want max_tokens from the last choice that stated one\n%s", out.StopReason, body)
	}
}
