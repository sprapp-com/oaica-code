package anthropic

// round55_call_charge_system_and_ref_integrity_test.go — round 55's findings on
// the local leg (leg 1), all three of them the round-53/54 rule ("the estimate
// charges exactly what the converter WRITES") still not held.
//
//   - A5: the tool_use and server_tool_use arms of the estimate charged the
//     block AS THE CLIENT SENT IT, while the converter that writes the prompt
//     reads only the call's type, id, name and input. A stray "text" key beside
//     the input of a call — a field the wire's own tool_use block does not have,
//     which a client can put there by accident — was billed for bytes no
//     converter ever wrote: a 41 000-byte string on one call moved the estimate
//     from 4 KB to 45 KB for a prompt that did not change. The estimate seeds
//     the client-visible input_tokens whenever the upstream states no usage, so
//     the charge was the client's window arithmetic.
//
//   - A6: the object form of a block's source — {"source":{"ref":"http://…"}}
//     — was dropped on decode. convertMessage's search_result and document arms
//     read s.Ref and write it into the passage they build, so the reference the
//     body stated reached no prompt on this leg while the identical body on the
//     gateway leg's arm carried it: the two legs sent different bytes for one
//     body, and a prompt cache keyed on them could never hit across the two.
//
//   - A7: an EMPTY text block in the system array took the "\n\n" separator.
//     convertMessage writes a system message by joining the blocks it actually
//     writes, and it writes nothing for an empty text — so systemBytes charged
//     two bytes the converter never wrote, on every request that carried a
//     trailing or interleaved empty system block (Claude Code emits them).

import (
	"encoding/json"
	"strings"
	"testing"
)

// round55Request decodes a body with the given user content.
func round55Request(t *testing.T, content string) MessagesRequest {
	t.Helper()
	var req MessagesRequest
	body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":` + content + `}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode %s: %v", content, err)
	}
	return req
}

// round55ConvertedBytes is what the converter writes for a request, which is
// what the estimate must charge.
func round55ConvertedBytes(t *testing.T, req MessagesRequest) int {
	t.Helper()
	conv, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	n := 0
	for _, m := range conv.Messages {
		n += len(m.Content)
	}
	return n
}

// round55ConvertedSystemBytes is the system message the converter writes, and
// nothing else — the user message beside it is not what systemBytes charges.
func round55ConvertedSystemBytes(t *testing.T, req MessagesRequest) int {
	t.Helper()
	conv, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	for _, m := range conv.Messages {
		if m.Role == "system" {
			return len(m.Content)
		}
	}
	return 0
}

// A5: a key the converter never writes must not be charged.
func TestAStrayKeyOnAToolCallIsNotCharged(t *testing.T) {
	stray := strings.Repeat("x", 41000)

	for _, tc := range []struct {
		name  string
		clean string
		stray string
	}{
		{
			name:  "tool_use",
			clean: `[{"type":"tool_use","id":"c1","name":"Bash","input":{"cmd":"ls"}}]`,
			stray: `[{"type":"tool_use","id":"c1","name":"Bash","input":{"cmd":"ls"},"text":"` + stray + `"}]`,
		},
		{
			name:  "server_tool_use",
			clean: `[{"type":"server_tool_use","id":"c1","name":"web_search","input":{"q":"x"}}]`,
			stray: `[{"type":"server_tool_use","id":"c1","name":"web_search","input":{"q":"x"},"text":"` + stray + `"}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanReq := round55Request(t, tc.clean)
			strayReq := round55Request(t, tc.stray)
			clean, got := EstimateInputTokens(cleanReq), EstimateInputTokens(strayReq)
			if clean != got {
				t.Errorf("a stray %q key on a %s call moved the charge %d -> %d; the converter writes the call's type, id, name and input only, so the two bodies produce byte-identical prompts and the estimate must not see the difference (2026-09-28 audit, round 55)",
					"text", tc.name, clean, got)
			}
			// The control: the charge is still the converter's own output for the
			// call that IS written, so the fix cannot be "charge nothing".
			if want := round55ConvertedBytes(t, cleanReq); clean < want {
				t.Errorf("the %s call was charged %d for a prompt the converter writes in %d bytes", tc.name, clean, want)
			}
		})
	}
}

// A7: the converter writes no separator for an empty system text block.
func TestAnEmptySystemBlockTakesNoSeparator(t *testing.T) {
	var req MessagesRequest
	body := `{"model":"m","max_tokens":1,"system":[{"type":"text","text":"aaa"},{"type":"text","text":""}],"messages":[{"role":"user","content":"go"}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	conv, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, m := range conv.Messages {
		if m.Role == "system" {
			want = len(m.Content)
		}
	}
	if got := systemBytes(req.System); got != want {
		t.Errorf("systemBytes charged %d bytes for a system the converter writes in %d: the empty block is joined with the \"\\n\\n\" separator here and written by nobody, so the estimate overstated the prompt it seeds (2026-09-28 audit, round 55)",
			got, want)
	}

	// The control: two blocks that DO carry text still take their separator.
	var both MessagesRequest
	if err := json.Unmarshal([]byte(`{"model":"m","max_tokens":1,"system":[{"type":"text","text":"aaa"},{"type":"text","text":"bbb"}],"messages":[{"role":"user","content":"go"}]}`), &both); err != nil {
		t.Fatal(err)
	}
	if got, want := systemBytes(both.System), round55ConvertedSystemBytes(t, both); got != want {
		t.Errorf("two written system blocks were charged %d for a system the converter writes in %d", got, want)
	}
}

// A6: the object-form ref the wire states must reach the prompt the converter
// builds — the same bytes the gateway leg's arm writes for the same body.
func TestAnObjectFormRefReachesThePrompt(t *testing.T) {
	req := round55Request(t, `[{"type":"search_result","title":"T","source":{"ref":"http://ref"},"content":[{"type":"text","text":"p"}]}]`)
	conv, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for _, m := range conv.Messages {
		got.WriteString(m.Content)
	}
	if !strings.Contains(got.String(), "http://ref") {
		t.Errorf("the object-form source the body stated was dropped on decode, so this leg sent a prompt without it while the metered gateway relays the same body with it — one body, two different prompts, and a prefix cache keyed on either can never hit the other (2026-09-28 audit, round 55)\nconverted: %q",
			got.String())
	}
}
