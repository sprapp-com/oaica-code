package launch

// round70_delta_arm_reserved_stated_ids_integrity_test.go — leg 2, the ids an
// entry states on a call the flush never hands over.
//
// A minted id must never land on an id the turn already stated (A46-4). The
// whole-list arm reserves every stated id before its walk; the delta arm
// reserved only the ids of the accumulators it HANDS to the converter, so an
// entry the flush drops — the nameless fragment, whose arguments relay as text,
// or the unfinished fragment the truncation gate refuses — left the id it
// stated unclaimed. An id-less call later in the same turn whose mint happens
// to be that string then took it, and the same upstream body reached the client
// under a different id depending on which arm answered it (2026-09-28 audit,
// round 70).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ollama/ollama/anthropic"
)

// r70Mint is the id this leg mints for an id-less "Read" call carrying
// {"a":1}. Both spells below state exactly this string on an entry the flush
// drops, which is what makes the collision reachable at all: the wire states an
// id that a mint would otherwise produce.
var r70Mint = anthropic.ToolCallIDFor("Read", `{"a":1}`)

// r70NamelessEntry is the first spell: an entry the upstream never named, so no
// block is built from it and its arguments reach the client as text.
func r70NamelessEntry() string {
	return `{"index":0,"type":"function","id":"` + r70Mint + `","function":{"name":"","arguments":"{\"a\":1}"}}`
}

// r70TruncatedEntry is the second: a call the token limit cut off mid-JSON, so
// the truncation gate drops it whole.
func r70TruncatedEntry() string {
	return `{"index":0,"type":"function","id":"` + r70Mint + `","function":{"name":"Read","arguments":"{\"a\":"}}`
}

// r70IdlessCall is the call that collides: no id of its own, so the arm mints
// one, and the mint is r70Mint.
func r70IdlessCall() string {
	return `{"index":1,"type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}`
}

// r70Doc is the whole completion a document arm answers — the control, since
// the whole-list arm already reserves every stated id.
func r70Doc(first string, finish string) string {
	return `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"` + finish + `",` +
		`"message":{"role":"assistant","tool_calls":[` + first + `,` + r70IdlessCall() + `]}}],` +
		`"usage":{"prompt_tokens":7,"completion_tokens":3}}`
}

// r70ToolUseIDs posts one turn through the proxy and returns the tool_use ids
// the client was handed. The streamed turn is spelled as real DELTA frames, one
// chunk listing both entries — the adoption arm is a third spelling of this
// turn and is not what this finding is about.
func r70ToolUseIDs(t *testing.T, stream bool, first, finish string) []string {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	var up *httptest.Server
	if stream {
		up = streamUpstream(t,
			`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[`+first+`,`+r70IdlessCall()+`]}}]}`+"\n\n"+
				`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"`+finish+`"}]}`+"\n\n"+
				`data: [DONE]`+"\n\n", true)
	} else {
		up = jsonUpstream(t, r70Doc(first, finish))
	}
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", stream)
	if code != http.StatusOK {
		t.Fatalf("status %d\n%s", code, body)
	}
	if stream {
		var ids []string
		for _, b := range sseToolUseBlocks(t, body) {
			id, _ := b["id"].(string)
			ids = append(ids, id)
		}
		return ids
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

// r70AssertReserved is the shared claim: the delta arm must answer the same id
// the whole-list arm answers, and neither may hand the client the id the wire
// stated for an entry the client never sees.
func r70AssertReserved(t *testing.T, first, finish string) {
	t.Helper()
	document := r70ToolUseIDs(t, false, first, finish)
	delta := r70ToolUseIDs(t, true, first, finish)
	if len(document) != 1 || len(delta) != 1 {
		t.Fatalf("want one tool_use block per arm, got document=%v delta=%v", document, delta)
	}
	if delta[0] == r70Mint {
		t.Errorf("the delta arm handed the client %s, the id the wire stated for an entry it dropped: a minted id landed on a stated one (A46-4)", delta[0])
	}
	if delta[0] != document[0] {
		t.Errorf("arms disagree on the id-less call: document=%v delta=%v", document, delta)
	}
}

func TestTheDeltaArmReservesTheIDANamelessEntryStates(t *testing.T) {
	r70AssertReserved(t, r70NamelessEntry(), "tool_calls")
}

// The same rule for the other drop: a fragment the truncation gate refuses
// states its id too, and the client never sees that entry either.
func TestTheDeltaArmReservesTheIDATruncatedEntryStates(t *testing.T) {
	r70AssertReserved(t, r70TruncatedEntry(), "length")
}
