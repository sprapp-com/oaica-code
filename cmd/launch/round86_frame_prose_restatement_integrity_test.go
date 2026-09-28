package launch

// round86_frame_prose_restatement_integrity_test.go — leg 2, F86-L2-1
// (2026-09-28 audit, round 86).
//
// A whole-completion frame is folded as a delta when it says something the
// stream does not already hold (round 85). The gate that decides "something the
// stream does not already hold" asks about the FRAME, and a frame carrying prose
// answers yes on that prose alone — so every call the frame restated was folded
// as well, and the model's one call reached the client twice under two ids.
// Round 85's fold comment states the intent the gate could not keep: "one that
// only restates what the stream already holds is still left alone".
//
// Two spellings of the same restatement escaped even the per-entry question:
// `{"a": 1}` against `{"a":1}` is one call to this leg's own restatement folds
// (`restatesAccumulatedCall` compares `canonicalArgs`) but was two by the gate's
// byte comparison, and a FREEFORM restatement — the model's whole command,
// `ls -la`, delivered once as a fragment and once as a document — had the two
// statements' bytes concatenated into a command the model never wrote, under a
// minted id.
//
// Measured on 2026-09-28 before the fix, one fragment `Bash {"a":1}` and one
// frame restating it: fragment alone `[call_7ff51383]`, frame alone
// `[call_7ff51383]`, frame with no prose `[call_7ff51383]`, frame with prose
// `[call_7ff51383 call_1f1f3503]` — the prose decided the call count. Freeform:
// fragment `{"_raw":"ls -la"}` vs mixture `{"_raw":"ls -lals -la"}`.

import (
	"encoding/json"
	"strings"
	"testing"
)

// r86RestatedCalls reads the turn's tool_use blocks in order, each as the name,
// the id it wears and the argument bytes it assembled.
func r86RestatedCalls(t *testing.T, body string) []string {
	t.Helper()
	type ev struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	type open struct {
		name, id string
		parts    strings.Builder
	}
	blocks := map[int]*open{}
	var order []int
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var e ev
		if json.Unmarshal([]byte(raw), &e) != nil {
			continue
		}
		switch {
		case e.Type == "content_block_start" && e.ContentBlock.Type == "tool_use":
			blocks[e.Index] = &open{name: e.ContentBlock.Name, id: e.ContentBlock.ID}
			order = append(order, e.Index)
		case e.Type == "content_block_delta" && e.Delta.Type == "input_json_delta":
			if b := blocks[e.Index]; b != nil {
				b.parts.WriteString(e.Delta.PartialJSON)
			}
		}
	}
	var out []string
	for _, idx := range order {
		b := blocks[idx]
		out = append(out, b.name+"/"+b.id+" "+b.parts.String())
	}
	return out
}

// r86CallFragment is one delta naming Bash with the argument bytes given.
func r86CallFragment(args string) string {
	return `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"Bash","arguments":"` + args + `"}}]}}]}`
}

// r86CallFrame is a whole completion restating that call, with prose and
// reasoning of its own when given.
func r86CallFrame(content, reasoning, args string) string {
	return `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"` + content +
		`","reasoning_content":"` + reasoning + `","tool_calls":[{"type":"function","function":{"name":"Bash","arguments":"` + args + `"}}]},"finish_reason":"tool_calls"}]}`
}

// TestAFramesProseDoesNotDoubleTheCallItRestates is the F86-L2-1 pin: the frame's
// prose is what the frame adds, and the call it restates is still the stream's
// one call.
func TestAFramesProseDoesNotDoubleTheCallItRestates(t *testing.T) {
	fragment := r86CallFragment(`{\"a\":1}`)
	frame := r86CallFrame("", "", `{\"a\":1}`)

	alone := r86RestatedCalls(t, r85Body(t, []string{fragment, r85CallsFin, r81Done}))
	for _, tc := range []struct {
		name   string
		frames []string
	}{
		{"the frame alone", []string{r86CallFrame("hi", "", `{\"a\":1}`), r85CallsFin, r81Done}},
		{"the frame restating it with no prose", []string{fragment, frame, r85CallsFin, r81Done}},
		{"the frame restating it with prose", []string{fragment, r86CallFrame("hi", "", `{\"a\":1}`), r85CallsFin, r81Done}},
		{"the frame restating it with reasoning", []string{fragment, r86CallFrame("", "why", `{\"a\":1}`), r85CallsFin, r81Done}},
		{"the frame restating it in other spacing", []string{
			r86CallFragment(`{\"a\": 1}`), frame, r85CallsFin, r81Done}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := r86RestatedCalls(t, r85Body(t, tc.frames))
			if len(got) != len(alone) {
				t.Errorf("the turn carries %d call(s) %v; the same statements written as one fragment carry %v — a frame's prose is what the frame adds, and a call it restates is the call the stream already holds (2026-09-28 audit, round 86, F86-L2-1)",
					len(got), got, alone)
			}
			if len(got) > 0 && len(alone) > 0 && got[0] != alone[0] {
				t.Errorf("the turn's first call is %q, want %q (2026-09-28 audit, round 86, F86-L2-1)", got[0], alone[0])
			}
		})
	}
}

// TestAFreeformRestatementIsNotConcatenated is F86-L2-1's other face: the two
// statements of one freeform call are one call, not two commands.
func TestAFreeformRestatementIsNotConcatenated(t *testing.T) {
	fragment := r86CallFragment("ls -la")
	alone := r86RestatedCalls(t, r85Body(t, []string{fragment, r85CallsFin, r81Done}))
	mixed := r86RestatedCalls(t, r85Body(t, []string{fragment, r86CallFrame("hi", "", "ls -la"), r85CallsFin, r81Done}))
	if len(mixed) != len(alone) || mixed[0] != alone[0] {
		t.Errorf("a freeform call stated as a fragment and restated by a frame reached the client as %v; the same statements as one fragment give %v — the model wrote that command once and the client must run it once, with its own bytes (2026-09-28 audit, round 86, F86-L2-1)",
			mixed, alone)
	}
}
