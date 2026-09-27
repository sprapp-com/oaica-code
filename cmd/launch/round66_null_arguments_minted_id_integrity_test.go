package launch

// round66_null_arguments_minted_id_integrity_test.go — round 66's finding on the
// client proxy, and the one round 65 could not have caught from here.
//
// ONE ID-LESS CALL, TWO MINTED IDS. Round 65 folded the JSON literal null into
// the empty object on the way to the client, and this leg's whole-list arm has
// always handed the client `{}` for it. The ID, though, is minted from the
// argument text — and the two arms of this one leg read two different texts:
// the whole-list parser marshals the map it unmarshalled into, which for the
// literal is `null`, while the streaming arm runs through the local converter,
// which round 65 had just taught to seed the hash from the FOLDED shape. So one
// body, one call, one `stream` flag: `"arguments":"null"` reached the client as
// call_261f4946 whole and call_781541ff streamed. The id is what the client
// echoes back as tool_result.tool_use_id, so the two framings are two different
// agent loops — and the gateway leg numbers the same call from `null` (its
// canonicalCallArgs returns the literal, round 49), which is what the whole-list
// arm agreeing with it here means.
//
// Round 65's own test for the fold states an id ("id":"a"), so the mint was
// never exercised: the arms agreed because neither of them had to number the
// call at all. This one leaves the id out, which is the wire several
// OpenAI-compatible backends send.

import (
	"testing"

	"github.com/ollama/ollama/anthropic"
)

// r66L2Whole is the non-SSE answer to one id-less call whose arguments the
// upstream spelled as raw spells them.
func r66L2Whole(raw string) string {
	return `{"id":"c","object":"chat.completion","model":"glm-5.3",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[` +
		`{"index":0,"type":"function","function":{"name":"Bash","arguments":"` + raw + `"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`
}

// TestANullArgumentCallIsNumberedOneIDOnBothArms is the finding. The id is
// asserted as a VALUE — the one the gateway leg and this leg's whole-list arm
// already answer with — because agreement between the two arms is exactly what
// the folding bug preserved when both arms were folded together.
func TestANullArgumentCallIsNumberedOneIDOnBothArms(t *testing.T) {
	wantID := anthropic.ToolCallIDFor("Bash", "null")
	if wantID == anthropic.ToolCallIDFor("Bash", "{}") {
		t.Fatalf("premise: the two argument texts must mint different ids, both gave %s", wantID)
	}
	frag, wh := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"function":{"name":"Bash","arguments":"null"}}`),
		r59L2Fin, `data: [DONE]`,
	}, r66L2Whole("null"))

	if len(wh) != 1 {
		t.Fatalf("PREMISE: the whole-list arm answers %v, want one call", wh)
	}
	if got, _ := wh[0]["id"].(string); got != wantID {
		t.Fatalf("PREMISE: the whole-list arm numbered the call %s, want %s — the text the upstream stated is what every leg hashes", got, wantID)
	}
	if in, ok := wh[0]["input"].(map[string]any); !ok || len(in) != 0 {
		t.Fatalf("PREMISE: the whole-list arm's input is %#v, want the empty object", wh[0]["input"])
	}
	r59L2SameAsWhole(t, frag, wh)
	if len(frag) == 1 {
		if got, _ := frag[0]["id"].(string); got != wantID {
			t.Errorf("one body with one `stream` flag reached the client as %s streamed and %s whole: the id is minted from the arguments the upstream stated, the whole-list arm already answers with the literal, and the gateway leg numbers the same call the same way (2026-09-28 audit, round 66, F66-L2-1)", got, wantID)
		}
	}
}

// TestTheEmptyArgumentCallKeepsItsOwnID is the control: the empty object is a
// different argument text from the literal and keeps the id it has always had,
// so the fix cannot be satisfied by folding the literal into it.
func TestTheEmptyArgumentCallKeepsItsOwnID(t *testing.T) {
	wantID := anthropic.ToolCallIDFor("Bash", "{}")
	frag, wh := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"function":{"name":"Bash","arguments":"{}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, r66L2Whole("{}"))
	r59L2SameAsWhole(t, frag, wh)
	for arm, blocks := range map[string][]map[string]any{"streamed": frag, "whole": wh} {
		if len(blocks) != 1 {
			t.Fatalf("premise: the %s arm answers %v, want one call", arm, blocks)
		}
		if got, _ := blocks[0]["id"].(string); got != wantID {
			t.Errorf("an id-less call whose arguments the upstream spelled `{}` is numbered %s on the %s arm, want %s", got, arm, wantID)
		}
	}
}
