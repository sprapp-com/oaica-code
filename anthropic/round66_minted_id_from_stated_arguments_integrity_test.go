package anthropic

// round66_minted_id_from_stated_arguments_integrity_test.go — round 66's
// finding on leg 1, and the contract it had drifted from.
//
// AN ID-LESS CALL IS NUMBERED FROM THE ARGUMENTS THE UPSTREAM STATED, NOT FROM
// THE SHAPE THIS LEG WRITES. Round 65 folded the JSON literal null into the
// empty object on the way TO THE CLIENT — right, and kept here: the block's own
// input is an object on every leg, and the block's own delta must not spell the
// literal, or the client accumulates `{}` followed by `null`. It folded the
// SAME value into the id's seed, which is a different question: the id every
// leg mints for the same call must be one string, and the other two legs hash
// the text they were handed — the client proxy's whole-list parser marshals the
// map it unmarshalled into (a null text marshals back to `null`, an empty one to
// `{}`) and the gateway's canonicalCallArgs returns the literal for exactly this
// reason. So one id-less call with `"arguments":"null"` reached the client as
// call_261f4946 through the client proxy's whole-list arm and through the
// gateway, and as call_781541ff through this leg — and, because the proxy's
// streaming arm runs THROUGH this converter, as call_781541ff on the other arm
// of that same leg, from one body with one `stream` flag (2026-09-28 audit,
// round 66, F66-L2-1).
//
// The fix is one variable: the seed is `json.Marshal(tc.Function.Arguments)`,
// which is `{}` for an absent or empty text and `null` for the literal, while
// the delivered bytes keep round 65's fold.

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/api"
)

// r66Args parses an upstream argument text into the ordered map a
// ChatResponse carries: the literal null leaves it a NIL map, which is what
// marshals back to `null`, while an empty text leaves the zero value that
// marshals to `{}`.
func r66Args(t *testing.T, raw string) api.ToolCallFunctionArguments {
	t.Helper()
	var args api.ToolCallFunctionArguments
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			t.Fatalf("unmarshal %q: %v", raw, err)
		}
	}
	return args
}

// r66Call is a ChatResponse carrying one id-less call, the wire several
// OpenAI-compatible backends send.
func r66Call(raw string) api.ChatResponse {
	return api.ChatResponse{
		Model: "glm-5.3",
		Message: api.Message{
			Role: "assistant",
			ToolCalls: []api.ToolCall{{
				Function: api.ToolCallFunction{Name: "Bash"},
			}},
		},
		Done:       true,
		DoneReason: "tool_calls",
	}
}

// r66WithArgs is r66Call with its one call's arguments set to the given text.
func r66WithArgs(t *testing.T, raw string) api.ChatResponse {
	t.Helper()
	r := r66Call(raw)
	r.Message.ToolCalls[0].Function.Arguments = r66Args(t, raw)
	return r
}

// r66StreamID runs one ChatResponse through the streaming converter and
// returns the id of the tool_use block it opened, with the bytes it delivered
// as that block's input_json_delta.
func r66StreamID(t *testing.T, r api.ChatResponse) (string, string) {
	t.Helper()
	conv := NewStreamConverter("msg", r.Model, 0)
	id, partial := "", ""
	for _, ev := range conv.Process(r) {
		switch ev.Event {
		case "content_block_start":
			if blk, ok := ev.Data.(ContentBlockStartEvent); ok && blk.ContentBlock.Type == "tool_use" {
				id = blk.ContentBlock.ID
			}
		case "content_block_delta":
			if d, ok := ev.Data.(ContentBlockDeltaEvent); ok && d.Delta.Type == "input_json_delta" {
				partial = d.Delta.PartialJSON
			}
		}
	}
	return id, partial
}

// TestANullArgumentTextMintsTheIDTheUpstreamStated is the finding: an id-less
// call whose argument text is the JSON literal null must be numbered the way
// the other two legs number it, on both of this leg's arms, while the bytes it
// DELIVERS stay the empty object round 65 pinned.
func TestANullArgumentTextMintsTheIDTheUpstreamStated(t *testing.T) {
	wantID := ToolCallIDFor("Bash", "null")
	if wantID == ToolCallIDFor("Bash", "{}") {
		t.Fatalf("premise: the two argument texts must mint different ids, both gave %s", wantID)
	}
	r := r66WithArgs(t, "null")

	streamID, partial := r66StreamID(t, r)
	if streamID != wantID {
		t.Errorf("the streaming arm numbered an id-less call with `null` arguments %s, and the same call is %s through the gateway leg and the client proxy's whole-list arm — one body, one call, and a `tool_result` written against one leg's answer must still match when the retry goes through another (2026-09-28 audit, round 66, F66-L2-1)", streamID, wantID)
	}
	if partial != "{}" {
		t.Errorf("the streaming arm delivered %q as the call's arguments; the block's own input must be the empty object every leg states (round 65, F65-L2-1)", partial)
	}

	resp := ToMessagesResponse("msg", r)
	if len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" {
		t.Fatalf("premise: the whole-document arm must answer one tool_use block, got %+v", resp.Content)
	}
	if resp.Content[0].ID != wantID {
		t.Errorf("the whole-document arm numbered the same call %s and the streaming arm %s — the id must not depend on whether the client asked for `stream` (2026-09-28 audit, round 66, F66-L2-1)", resp.Content[0].ID, streamID)
	}
	if b, err := json.Marshal(resp.Content[0].Input); err != nil || string(b) != "{}" {
		t.Errorf("the whole-document arm's input is %s (err %v); it is the empty object on every leg", b, err)
	}
}

// TestTheNullAndEmptyArgumentTextsKeepTheirOwnIDs pins the whole mapping, so
// the seed cannot quietly fold again: an absent or empty text is `{}`, the
// literal is `null`, and the two are different ids on both arms — while `{}`
// and an absent text are one id, because they are one argument list.
func TestTheNullAndEmptyArgumentTextsKeepTheirOwnIDs(t *testing.T) {
	empty := ToolCallIDFor("Bash", "{}")
	null := ToolCallIDFor("Bash", "null")
	if empty == null {
		t.Fatalf("premise: the two argument texts must mint different ids, both gave %s", empty)
	}
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"", empty},
		{"{}", empty},
		{"null", null},
	} {
		r := r66WithArgs(t, tc.raw)
		streamID, _ := r66StreamID(t, r)
		resp := ToMessagesResponse("msg", r)
		if len(resp.Content) != 1 {
			t.Fatalf("premise: %q must answer one block, got %+v", tc.raw, resp.Content)
		}
		if streamID != tc.want || resp.Content[0].ID != tc.want {
			t.Errorf("arguments %q: streaming arm %s, whole-document arm %s, want %s on both (2026-09-28 audit, round 66, F66-L2-1)", tc.raw, streamID, resp.Content[0].ID, tc.want)
		}
	}
}
