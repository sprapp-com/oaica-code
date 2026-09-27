package launch

// Round 56, leg 2 (the client proxy). The same two wires the gateway leg was
// fixed for: an upstream that lists a call it has already stated, once more,
// either on later chunks of the same slot (the id arriving on a chunk of its
// own, so the restatement carries none) or as a second entry of the same delta.
// This leg appended the second listing to the first's arguments, and the client
// got one tool_use whose input was `{"a":1}{"a":1}` wrapped as {"_raw":…}.

import (
	"net/http"
	"strings"
	"testing"
)

// r56Wire serves the given raw SSE frames and posts one Messages request
// through a proxy in front of it.
func r56Wire(t *testing.T, frames ...string) (int, string) {
	t.Helper()
	up := streamUpstream(t, strings.Join(frames, "\n\n")+"\n\n", false)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	return round54PostMessage(t, proxy, "glm-5.3", true)
}

const (
	r56L2Head = `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`
	r56L2ID   = `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","id":"call_1"}]},"finish_reason":null}]}`
	r56L2Name = `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"Bash"}}]},"finish_reason":null}]}`
	r56L2Args = `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`
	r56L2Fin  = `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	r56L2Done = `data: [DONE]`
	// One delta listing the same slot twice; the second entry states no id.
	r56L2Dup = `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},{"index":0,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`
)

// TestAL2CallRestatedInALaterChunkOfItsSlotIsOneCall is F1.
func TestAL2CallRestatedInALaterChunkOfItsSlotIsOneCall(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	code, body := r56Wire(t, r56L2Head, r56L2ID, r56L2Name, r56L2Args, r56L2Fin, r56L2Done)
	if code != http.StatusOK {
		t.Fatalf("status %d\n%s", code, body)
	}
	blocks := sseToolUseBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("the model's one call reached the client as %d blocks:\n%s", len(blocks), body)
	}
	if got := blocks[0]["input"]; !r56IsOneArgObject(got) {
		t.Errorf("the client accumulated %v as this call's input, want {\"a\":1}\n"+
			"the arguments were restated by a later chunk of the same slot and were appended as if they were more of the object: two finished argument objects do not concatenate into JSON, so the client held an input it could not parse, under a stop_reason of tool_use\n%s", got, body)
	}
}

// TestAL2SlotListedTwiceInOneDeltaIsOneCall is F2.
func TestAL2SlotListedTwiceInOneDeltaIsOneCall(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	code, body := r56Wire(t, r56L2Dup, r56L2Fin, r56L2Done)
	if code != http.StatusOK {
		t.Fatalf("status %d\n%s", code, body)
	}
	blocks := sseToolUseBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("the slot the upstream listed twice reached the client as %d blocks:\n%s", len(blocks), body)
	}
	if got := blocks[0]["input"]; !r56IsOneArgObject(got) {
		t.Errorf("the client accumulated %v as this call's input, want {\"a\":1}\n%s", got, body)
	}
}

// r56IsOneArgObject reports whether sseToolUseBlocks decoded exactly {"a":1} —
// and not the {"_raw": …} shape this leg keeps for text that is not a JSON
// object, which is what the concatenation was answered with.
func r56IsOneArgObject(input any) bool {
	m, ok := input.(map[string]any)
	if !ok || len(m) != 1 {
		return false
	}
	v, ok := m["a"]
	if !ok {
		return false
	}
	f, ok := v.(float64)
	return ok && f == 1
}
