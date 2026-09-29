package launch

// round103_leg2_choice_per_chunk_test.go — leg 2, round 103 (2026-09-29 audit),
// F103-L2-1 (fixed) and F103-L2-2 (recorded).
//
// Round 67 cured a chunk carrying two choices by reading only the FIRST element
// of the chunk. That is positional, and it holds only for the spelling round 67
// measured. A vendor that streams each choice in a chunk of its own puts the
// alternative at position 0 of its chunk, and it was spliced into the client's
// single message: "AB" streamed where the document arm states "A", or a second
// choice's tool call handed to the agent as a call to run. The turn's choice is
// the one its first element names.
//
// F103-L2-2 is what the fix leaves standing and is RECORDED: a document whose
// FIRST choice is empty and whose second states the answer is refused 502 "empty
// completion" by the document arms, and now by the fragment arm as well — the
// arms agree, on a reading the round-68 comment ("the message stays the first
// entry's") states on purpose.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func r103Chunk(index int, delta string) string {
	return `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":` +
		strings.TrimSpace(string(rune('0'+index))) + `,"delta":` + delta + `}]}`
}

// TestMine103AChoiceInAChunkOfItsOwnDoesNotJoinTheFirst is F103-L2-1's pin.
func TestMine103AChoiceInAChunkOfItsOwnDoesNotJoinTheFirst(t *testing.T) {
	streamed, whole := r67Texts(t, []string{
		r103Chunk(0, `{"content":"A"}`),
		r103Chunk(1, `{"content":"B"}`),
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, r67TwoChoiceWhole)
	if whole != "A" {
		t.Fatalf("premise: the document arm states %q, want \"A\"", whole)
	}
	if streamed != "A" {
		t.Errorf("the fragment arm states %q, want \"A\" — the turn's choice is the one its first element names, not the element that opens its own chunk (2026-09-29 audit, round 103, F103-L2-1)", streamed)
	}
}

// TestMine103AChoiceNumberedFromOneIsStillTheTurn keeps round 67's other half:
// a vendor numbering its choices from 1 is relayed the way the whole arm relays
// it, so the fix cannot be `choice.Index == 0`.
func TestMine103AChoiceNumberedFromOneIsStillTheTurn(t *testing.T) {
	streamed, _ := r67Texts(t, []string{
		r103Chunk(1, `{"content":"A"}`),
		r103Chunk(2, `{"content":"B"}`),
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":1,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, r67TwoChoiceWhole)
	if streamed != "A" {
		t.Errorf("the fragment arm states %q for a vendor numbering from 1, want \"A\" (2026-09-29 audit, round 103, F103-L2-1)", streamed)
	}
}

// TestMine103ATurnWhoseFirstChoiceIsEmptyIsRefusedOnBothArms is F103-L2-2's
// record: the arms agree (502 "empty completion") on a turn whose second choice
// states the answer, and that agreement is what a later round must not break by
// moving one arm alone.
func TestMine103ATurnWhoseFirstChoiceIsEmptyIsRefusedOnBothArms(t *testing.T) {
	whole := `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[` +
		`{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"},` +
		`{"index":1,"message":{"role":"assistant","content":"second"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":1}}`
	frames := []string{
		r103Chunk(0, `{"role":"assistant"}`),
		r103Chunk(1, `{"content":"second"}`),
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	for _, stream := range []bool{false, true} {
		code, body := r103PostChoiceTurn(t, frames, whole, stream)
		if code != 502 || !strings.Contains(body, "empty completion") {
			t.Errorf("stream=%v answered %d %q, want the 502 \"empty completion\" both arms state — F103-L2-2 is a recorded reading, not a fix (2026-09-29 audit, round 103)", stream, code, strings.TrimSpace(body))
		}
	}
}

func r103PostChoiceTurn(t *testing.T, frames []string, whole string, stream bool) (int, string) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	script := strings.Join(frames, "\n\n") + "\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, script)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, whole)
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	return round54PostMessage(t, proxy, "glm-5.3", stream)
}
