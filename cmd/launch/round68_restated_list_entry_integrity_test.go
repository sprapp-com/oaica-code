package launch

// round68_restated_list_entry_integrity_test.go — leg 2's non-stream list, and
// the restatement it used to mint a second id for.
//
// Round 56 fixed this leg's STREAM arm for a wire that lists one call twice —
// the second entry with no id — and recorded the rule as "both this leg's
// non-stream list and the gateway leg answer that wire with ONE call". The
// gateway leg does. parseOpenAIToolCalls did not: an id-less entry whose name
// and finished arguments equalled a call the list had already STATED was read
// as a second call with an id minted from the same name and arguments, so the
// model's one call reached the client as two tool_use blocks it could run
// twice, while the same body's two other arms answered one (2026-09-28 audit,
// round 68, F68-L2-3).
//
// A45-2 survives the fold: two identical calls that BOTH state no id are two
// calls, and the control below says so.

import (
	"encoding/json"
	"net/http"
	"testing"
)

// r68ListBody posts one non-stream request through a proxy in front of an
// upstream answering with the given whole document, and returns the ids the
// client was handed for tool_use blocks, in order.
func r68ListBody(t *testing.T, doc string) []string {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	up := jsonUpstream(t, doc)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", false)
	if code != http.StatusOK {
		t.Fatalf("status %d\n%s", code, body)
	}
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

// r68Doc builds a whole completion whose tool_calls list is the given entries.
func r68Doc(entries string) string {
	return `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"tool_calls",` +
		`"message":{"role":"assistant","tool_calls":[` + entries + `]}}],` +
		`"usage":{"prompt_tokens":7,"completion_tokens":3}}`
}

const (
	r68NamedCall  = `{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	r68IdlessSame = `{"index":0,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
)

// TestARestatedListEntryIsNotASecondCall is the finding: the list states the
// call, then lists it again with no id.
func TestARestatedListEntryIsNotASecondCall(t *testing.T) {
	ids := r68ListBody(t, r68Doc(r68NamedCall+`,`+r68IdlessSame))
	if len(ids) == 1 {
		if ids[0] != "call_1" {
			t.Errorf("the one call reached the client as id %q, want the id the upstream stated (%q)", ids[0], "call_1")
		}
		return
	}
	t.Errorf("the model's one call reached the client as %d tool_use blocks (%v): the id-less entry repeating the call this list already stated was minted as a second call, so the client runs the tool twice, while the same body's stream arm and the gateway leg answer one (2026-09-28 audit, round 68, F68-L2-3)",
		len(ids), ids)
}

// TestTwoIdlessIdenticalCallsAreStillTwoCalls is A45-2's control: the fold is
// for an entry that repeats a call the list STATED, not for two calls that
// stated nothing at all.
func TestTwoIdlessIdenticalCallsAreStillTwoCalls(t *testing.T) {
	ids := r68ListBody(t, r68Doc(r68IdlessSame+`,`+r68IdlessSame))
	if len(ids) != 2 {
		t.Fatalf("two identical id-less calls reached the client as %d blocks (%v), want 2 — the upstream asked for the tool twice and the client must be able to answer each (A45-2)", len(ids), ids)
	}
	if ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Errorf("the two calls were handed ids %v: each call needs its own, or one tool_result satisfies both", ids)
	}
}

// TestTheStreamArmAgreesWithTheListForTheRestatedEntry is the cross-arm half:
// the same call and the same restatement, spelled as one delta.
func TestTheStreamArmAgreesWithTheListForTheRestatedEntry(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	up := streamUpstream(t, `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[`+
		r68NamedCall+`,`+r68IdlessSame+`]}}]}`+"\n\n"+
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n"+
		`data: [DONE]`+"\n\n", true)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", true)
	if code != http.StatusOK {
		t.Fatalf("status %d\n%s", code, body)
	}
	blocks := sseToolUseBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("the stream arm answered the same body with %d blocks (%v), want 1 (round 56's F1/F2)\n%s", len(blocks), blocks, body)
	}
	if got, _ := blocks[0]["id"].(string); got != "call_1" {
		t.Errorf("the stream arm's one call carries id %q, want the stated %q", got, "call_1")
	}
}
