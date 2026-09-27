package main

// round58_indexed_slot_identity_integrity_test.go — round 58's two findings on
// the frame arm's index (F58-L3-1, F58-L3-2).
//
// The index is the upstream's slot identity, and this bridge keys a call by it.
// Two things the key was not told:
//
//   - F58-L3-1: a block the index opened can be RE-KEYED to an id the upstream
//     states on a later fragment (the B45-1 merge). The index then named a key
//     the map no longer held, so the next fragment stating that index opened a
//     SECOND block for a call already being written: its arguments were split
//     across two blocks and the tail reached the client as prose beside a
//     tool_use whose input was half an object.
//   - F58-L3-2: an index that is STATED and repeated for the turn's next call —
//     the vendor that writes index 0 for every call of the turn, which is
//     round 36's B-F2 with the index present instead of absent — folded that
//     call into the first one's block: its id discarded, its name dropped and
//     its arguments concatenated onto the first's partial_json, so two tools
//     reached the client as one call whose input is not JSON. Both other
//     translations of the same two calls — this leg's own document arm and the
//     client proxy's adoption arm — keep them apart, which is what the parity
//     cases below assert rather than this file's preference.
//
// The restatement round 56 pinned is the control: a fragment that is the call
// the slot already carries stays that call, and the ordinary OpenAI chunk order
// — which restates the name on a chunk of its own — is still one call.

import (
	"encoding/json"
	"strings"
	"testing"
)

// r58FrameCalls drives one frame wire and returns the tool_use blocks the
// client was handed.
func r58FrameCalls(t *testing.T, frames ...string) ([]string, string) {
	t.Helper()
	up := round45Frames(t, append(frames, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, `data: [DONE]`)...)
	srv, _ := round39Gateway(t, up, nil)
	_, body := round45Ask(t, srv, round45AskStream)
	ids := round46ToolUseIDs(t, body)
	return ids, body
}

// r58Partials returns the partial_json each tool_use block accumulated, in the
// order the blocks were opened. A block that got no arguments reports "".
func r58Partials(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	open := false
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(raw), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			open = ev.ContentBlock.Type == "tool_use"
			if open {
				out = append(out, "")
			}
		case "content_block_stop":
			open = false
		case "content_block_delta":
			if open && len(out) > 0 {
				out[len(out)-1] += ev.Delta.PartialJSON
			}
		}
	}
	return out
}

// F58-L3-1: the index must still name the block it opened after that block was
// re-keyed to the id a later fragment stated.
func TestAnIndexStillNamesTheBlockItOpened(t *testing.T) {
	ids, body := r58FrameCalls(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_x","function":{"arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
	)
	if len(ids) != 1 {
		t.Fatalf("the turn opened %d tool_use block(s), want the model's one call:\n%s", len(ids), body)
	}
	if ids[0] != "call_x" {
		t.Errorf("the call reached the client under id %q, want the id the upstream stated (%q)", ids[0], "call_x")
	}
	parts := r58Partials(t, body)
	if len(parts) != 1 || parts[0] != `{"cmd":"ls"}` {
		t.Errorf("the call's input reached the client as %v, want one complete {\"cmd\":\"ls\"}\nthe index named a key the map no longer held after the block was re-keyed to the stated id, so the arguments were split across two blocks and the tail was relayed as text beside a truncated tool_use (2026-09-28 audit, round 58, F58-L3-1)\n%s", parts, body)
	}
	if strings.Contains(body, `"type":"text"`) {
		t.Errorf("the call's own arguments were relayed to the client as prose:\n%s", body)
	}
}

// F58-L3-2: one index stated for two calls is two calls.
func TestASlotStatedForTheTurnsSecondCallIsTheSecondCall(t *testing.T) {
	t.Run("a second id at one index", func(t *testing.T) {
		ids, body := r58FrameCalls(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"b","function":{"name":"Read"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"p\":1}"}}]}}]}`,
		)
		if len(ids) != 2 {
			t.Fatalf("the turn's two calls reached the client as %d block(s) %v:\n%s", len(ids), ids, body)
		}
		if ids[0] != "a" {
			t.Errorf("the first call's id is %q, want the one it stated (%q)", ids[0], "a")
		}
		parts := r58Partials(t, body)
		if len(parts) != 2 || parts[0] != `{"cmd":"ls"}` || parts[1] != `{"p":1}` {
			t.Errorf("the two calls' inputs reached the client as %v, want [{\"cmd\":\"ls\"} {\"p\":1}]\nthe second stated a DIFFERENT id at the slot the first already held, which the keyless arms of this same function split on (round 43's B43-3): folded, the second call's id was discarded and its arguments concatenated onto the first's partial_json (2026-09-28 audit, round 58, F58-L3-2)\n%s", parts, body)
		}
	})

	t.Run("a second name at one index", func(t *testing.T) {
		ids, body := r58FrameCalls(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Read"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"p\":1}"}}]}}]}`,
		)
		if len(ids) != 2 {
			t.Fatalf("the turn's two calls reached the client as %d block(s) %v:\n%s", len(ids), ids, body)
		}
		if !strings.Contains(body, `"name":"Bash"`) || !strings.Contains(body, `"name":"Read"`) {
			t.Errorf("the two names the model wrote did not both reach the client:\n%s", body)
		}
		parts := r58Partials(t, body)
		if len(parts) != 2 || parts[0] != `{"cmd":"ls"}` || parts[1] != `{"p":1}` {
			t.Errorf("the two calls' inputs reached the client as %v, want [{\"cmd\":\"ls\"} {\"p\":1}]\na DIFFERENT name can only be introducing a call, and the keyless arm of this same function splits on exactly that change (round 40's A40-8, round 37's B-F2) (2026-09-28 audit, round 58, F58-L3-2)\n%s", parts, body)
		}
	})
}

// The other side: the same two calls in one document, which every arm must
// agree with. This is the target the fragment cases above are judged by.
func TestOneIndexForTwoCallsIsTwoCallsInTheDocumentArm(t *testing.T) {
	doc := `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[` +
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}},` +
		`{"index":0,"id":"b","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`
	up := round45Upstream(t, "application/json", doc)
	srv, _ := round39Gateway(t, up, nil)
	_, body := round45Ask(t, srv, round45AskStream)
	ids := round46ToolUseIDs(t, body)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("the document arm answered one index stated for two calls with %v, want [a b] — the fragment cases above are judged against this arm", ids)
	}
	parts := r58Partials(t, body)
	if len(parts) != 2 || parts[0] != `{"cmd":"ls"}` || parts[1] != `{"p":1}` {
		t.Fatalf("the document arm's inputs are %v, want [{\"cmd\":\"ls\"} {\"p\":1}]", parts)
	}
}

// The controls: what must NOT split.
func TestTheIndexedRestatementStaysOneCall(t *testing.T) {
	// Round 56's wire: the call whole, then the id, then the name, then the
	// arguments again — all at index 0. The trailing fragments are repeats.
	ids, body := r58FrameCalls(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1"}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]}}]}`,
	)
	if len(ids) != 1 {
		t.Errorf("the call the upstream listed again at its own slot reached the client as %d blocks %v (round 56's F1):\n%s", len(ids), ids, body)
	}
	if parts := r58Partials(t, body); len(parts) != 1 || parts[0] != `{"a":1}` {
		t.Errorf("the restated call's input is %v, want one {\"a\":1}", parts)
	}

	// The ordinary indexed continuation: the name arrives before the arguments.
	ids, body = r58FrameCalls(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
	)
	if len(ids) != 1 || ids[0] != "call_9" {
		t.Errorf("one call stated as name-then-id-and-arguments reached the client as %v:\n%s", ids, body)
	}
	if parts := r58Partials(t, body); len(parts) != 1 || parts[0] != `{"cmd":"ls"}` {
		t.Errorf("its input is %v, want {\"cmd\":\"ls\"}", parts)
	}
}
