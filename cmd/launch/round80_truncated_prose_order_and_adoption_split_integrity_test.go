package launch

// round80_truncated_prose_order_and_adoption_split_integrity_test.go — leg 2,
// the places its fragment arm still answered one upstream body differently from
// its own document arms (2026-09-28 audit, round 80).
//
//  1. F80-L2-1. Round 79 taught the flush to relay the bytes of an entry that
//     names nothing when the call they were folded into is dropped at the token
//     limit (F79-L2-B). It relayed them from the WALK that decides the drops —
//     loop 1 for a slot whose accumulator still names nothing, the drop branch
//     for a slot whose call the gate drops — so a dropped call's prose could
//     only ever land after every other nameless entry's prose, and among dropped
//     calls in SLOT-ARRIVAL order, not wire order. That is exactly what round
//     77's F77-L2-2 closed one loop over: `[{index 0,id c1,args "ls "},{index
//     0,args "-la"},{index 1,args "-x"}]` under finish_reason "length" relayed
//     `-x-la` where the whole-list arm relays `-la-x`, and a wire that
//     introduced slot 1 first relayed `/etc-la` where the document says
//     `-la/etc`. The flush now writes the relayed prose from the one record that
//     is in wire order, after the walk has decided which slots relay.
//
//  2. F80-L2-2. Round 79 ruled that an EMPTY argument list is a COMPLETE one, so
//     a fragment naming the call again WITH arguments begins the NEXT call. The
//     clause was guarded by `adoptedCallAt[slot] == nil`, so at a slot an
//     adopted document had written — a call the client already holds, complete
//     and runnable without arguments — the fragment fell through to the
//     adoption's drop: one call on this arm, two on this leg's plain fragment
//     arm and two whole. The clause is asked of adopted slots too; the adopted
//     TAIL it is not asked of (the same call restated with DIFFERENT arguments
//     over a slot whose arguments are on the wire) is a different shape, and
//     still not split.
//
//  3. F80-L2-4. The adoption's drop reserved the id of the fragment it dropped
//     and relayed nothing, so the bytes of an entry that named nothing vanished
//     — the same loss F79-L2-B closed at the truncation drop. They are now held
//     in an accumulator of their own, which is what makes the flush relay them
//     as the text every document arm of that body makes of them.
//
// The remaining difference at an adopted slot — a nameless fragment arriving
// AFTER the adopted call's block is on the wire reaches the client after it,
// where the document arms lay prose before the calls — is the placement item
// round 77 recorded as carried: a closed block cannot be preceded by a new one
// on this path, and the adoption emits the document's call as its own turn.

import (
	"encoding/json"
	"strings"
	"testing"
)

// r80L2FragText reads the text the streaming arm relayed, in arrival order.
func r80L2FragText(raw string) string {
	var out strings.Builder
	for _, part := range strings.Split(raw, `"type":"text_delta","text":"`)[1:] {
		if i := strings.Index(part, `"`); i >= 0 {
			out.WriteString(part[:i])
		}
	}
	return out.String()
}

// r80L2DocText reads the text blocks of the non-streaming answer, in order.
func r80L2DocText(t *testing.T, raw string) string {
	t.Helper()
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("the whole-list arm wrote no answer: %v\n%s", err, raw)
	}
	var out strings.Builder
	for _, b := range resp.Content {
		if b.Type == "text" {
			out.WriteString(b.Text)
		}
	}
	return out.String()
}

// r80L2Body is the whole-completion document for a list of entries.
func r80L2Body(entries, fin string) string {
	return `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"finish_reason":"` + fin + `",` +
		`"message":{"role":"assistant","content":"","tool_calls":[` + entries + `]}}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":10}}`
}

// TestTheTruncatedDropRelaysProseInWireOrder is F80-L2-1.
func TestTheTruncatedDropRelaysProseInWireOrder(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
	}{
		{
			"a dropped call's prose, then a nameless entry at another slot",
			[]string{
				`{"index":0,"id":"c1","function":{"name":"Bash","arguments":"ls "}}`,
				`{"index":0,"function":{"arguments":"-la"}}`,
				`{"index":1,"function":{"arguments":"-x"}}`,
			},
		},
		{
			"two dropped calls whose slots were introduced out of prose order",
			[]string{
				`{"index":1,"id":"c2","function":{"name":"Read","arguments":"cat "}}`,
				`{"index":0,"id":"c1","function":{"name":"Bash","arguments":"ls "}}`,
				`{"index":0,"function":{"arguments":"-la"}}`,
				`{"index":1,"function":{"arguments":"/etc"}}`,
			},
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			frames := make([]string, 0, len(tc.entries)+2)
			for _, e := range tc.entries {
				frames = append(frames, r59L2Frame(e))
			}
			frames = append(frames,
				`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
				`data: [DONE]`)
			fragRaw, wholeRaw := r79L2ArmsRaw(t, frames, r80L2Body(strings.Join(tc.entries, ","), "length"))
			frag, whole := r80L2FragText(fragRaw), r80L2DocText(t, wholeRaw)
			if frag != whole {
				t.Errorf("one body, two arms: the fragment arm relayed the model's prose %q and the whole-list arm %q — a nameless entry's bytes are relayed in the order the wire wrote them, and a call the token limit drops does not move them to the end (2026-09-28 audit, round 80, F80-L2-1)\nFRAG:\n%s\nWHOLE:\n%s",
					frag, whole, fragRaw, wholeRaw)
			}
		})
	}
}

// TestARestatedCallWithArgumentsIsTheNextCallAtAnAdoptedSlot is F80-L2-2.
func TestARestatedCallWithArgumentsIsTheNextCallAtAnAdoptedSlot(t *testing.T) {
	docFrame := `data: {"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"Read"}}]},` +
		`"finish_reason":"tool_calls"}]}`
	late := `{"index":0,"id":"c1","type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}`
	frames := []string{docFrame, r59L2Frame(late), r59L2Fin, `data: [DONE]`}
	whole := r80L2Body(`{"id":"c1","type":"function","function":{"name":"Read"}},`+late, "tool_calls")

	fragRaw, wholeRaw := r79L2ArmsRaw(t, frames, whole)
	frag := strings.Count(fragRaw, `"type":"tool_use"`)
	doc := strings.Count(wholeRaw, `"type":"tool_use"`)
	if frag != doc {
		t.Errorf("one body, two arms: the adoption arm answered %d call(s) and the whole-list arm %d — a call restated WITH arguments over a call that takes none is the NEXT call, on the adoption path too (2026-09-28 audit, round 80, F80-L2-2)\nFRAG:\n%s\nWHOLE:\n%s",
			frag, doc, fragRaw, wholeRaw)
	}
}

// TestTheAdoptionDropKeepsANamelessEntrysProse is F80-L2-4.
func TestTheAdoptionDropKeepsANamelessEntrysProse(t *testing.T) {
	docFrame := `data: {"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":"}}]},` +
		`"finish_reason":"tool_calls"}]}`
	late := `{"index":0,"function":{"arguments":"-la"}}`
	frames := []string{docFrame, r59L2Frame(late), r59L2Fin, `data: [DONE]`}
	whole := r80L2Body(`{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":"}},`+late, "tool_calls")

	fragRaw, wholeRaw := r79L2ArmsRaw(t, frames, whole)
	if got := r80L2DocText(t, wholeRaw); got != "-la" {
		t.Fatalf("the whole-list arm no longer relays the nameless entry's bytes — control drifted: %q", got)
	}
	if got := r80L2FragText(fragRaw); got != "-la" {
		t.Errorf("the adoption arm relayed %q where the whole-list arm relays the nameless entry's bytes as text — an entry that names nothing is not a call, and the block the adoption closed is not where they can be delivered (2026-09-28 audit, round 80, F80-L2-4)\nFRAG:\n%s", got, fragRaw)
	}
}
