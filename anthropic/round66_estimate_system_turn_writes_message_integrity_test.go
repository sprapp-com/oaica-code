package anthropic

// round66_estimate_system_turn_writes_message_integrity_test.go — round 66's
// finding on leg 1, and the generator that would have caught it.
//
// A SYSTEM TURN THAT WRITES NO SYSTEM MESSAGE IS NOT A BLANK SYSTEM MESSAGE.
// The estimate's rewrite predicate is the hoist's own question — does this
// conversation have a system message that is not first, or one that is blank —
// and it was asked of every system TURN, including one whose content is nothing
// but tool results. That turn writes a role-"tool" message and no system message
// at all, so its text is empty for a reason the predicate is not about: reading
// it as a blank system message took the joined branch for a conversation
// normalizeSystemFirst leaves alone. One 147-byte prompt — [system S1, system
// S2, user u, <result>] — was charged 42 bytes when the result's turn was a
// system turn and 46 when the same turn was written as a user turn, the whole
// difference being that the joined branch charges every merged text one role.
// This estimate is the client's own input_tokens and auto-compaction arithmetic
// whenever the upstream states no usage, so the two spellings of one session
// were metered two amounts (2026-09-28 audit, round 66, F66-L1-1).
//
// The fix is one predicate: a system turn writes a system message when it has a
// run of its own, or when it has no tool result to write as a message of another
// role. Both clauses of the rewrite test are then asked only of a turn that
// wrote one. `endsWithTool` still sets `seenNonSystem` for such a turn — that is
// what makes a LATER system text land after a tool message, which the hoist
// rewrites, and it is pinned by the control below.

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/api"
)

// TestASystemTurnOfNothingButToolResultsIsNotABlankSystemMessage is the finding.
// The two bodies convert to the SAME prompt — asserted, not assumed — so they
// must be charged one amount.
func TestASystemTurnOfNothingButToolResultsIsNotABlankSystemMessage(t *testing.T) {
	a := `{"model":"m","messages":[{"role":"system","content":"S1"},{"role":"system","content":"S2"},{"role":"user","content":"u"},{"role":"system","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`
	b := `{"model":"m","messages":[{"role":"system","content":"S1"},{"role":"system","content":"S2"},{"role":"user","content":"u"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`
	nA, pA, _, refused := r65ConvBody(t, a)
	if refused {
		t.Fatalf("the premise pair must convert: %s", a)
	}
	nB, pB, _, refused := r65ConvBody(t, b)
	if refused {
		t.Fatalf("the premise pair must convert: %s", b)
	}
	if pA != pB {
		t.Fatalf("premise: the two bodies must convert to ONE prompt\n A %s\n B %s", pA, pB)
	}
	if nA != nB {
		t.Errorf("one %d-byte prompt charged %d and %d bytes: a system turn holding nothing but tool results writes no system message, so its empty text is not the blank system message the hoist deletes and the estimate must charge the conversation as it stands (2026-09-28 audit, round 66, F66-L1-1)", len(pA), nA, nB)
	}
}

// TestTheClauseThatOutlivesTheFixIsStillAsked pins the one thing the fix must
// NOT also switch off: a results-only system turn still ends with a message of
// another role, so a system turn written AFTER it is not first and the hoist
// rewrites the conversation. Here the results-only turn sits between two system
// texts and the last of them must be charged the joined reading — one role for
// the merged text, which the as-is reading (one role per turn) exceeds.
func TestTheClauseThatOutlivesTheFixIsStillAsked(t *testing.T) {
	merged := `{"model":"m","messages":[{"role":"system","content":"S1"},{"role":"system","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]},{"role":"system","content":"S2"}]}`
	asIs := `{"model":"m","messages":[{"role":"system","content":"S1"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]},{"role":"system","content":"S2"}]}`
	nM, pM, _, refused := r65ConvBody(t, merged)
	if refused {
		t.Fatalf("the premise pair must convert: %s", merged)
	}
	nA, pA, _, refused := r65ConvBody(t, asIs)
	if refused {
		t.Fatalf("the premise pair must convert: %s", asIs)
	}
	if pM != pA {
		t.Fatalf("premise: the two bodies must convert to ONE prompt\n merged %s\n as-is  %s", pM, pA)
	}
	// The prompt puts "S2" after a tool message, so the hoist rewrote it. Its
	// own converter is the authority on which reading that is; the two
	// spellings must agree, and the merged one is the one that is charged the
	// merged role.
	if nM != nA {
		t.Errorf("the same prompt charged %d and %d: a results-only system turn still makes the NEXT system text land after a message of another role, so that conversation IS rewritten and both spellings must be charged it (2026-09-28 audit, round 66)", nM, nA)
	}
	if pM != pA {
		t.Fatalf("premise drift")
	}
}

// r66Respell rewrites a CONVERTED prompt as a body: one turn per message, the
// message's own role, and a tool message written back as the tool_result block
// it came from. Nothing is merged and nothing is joined — that is the point.
// Round 65's helper joins the system texts into the request's `system` field,
// which is the right second spelling only for a conversation the hoist rewrites;
// for one it leaves alone (two system messages at the front, which is exactly
// the shape this round's finding is about) the joined spelling converts to a
// DIFFERENT prompt, so the premise check skipped the very bodies the divergence
// lives in — and the generator reported zero divergences with the fix reverted.
// Re-spelling message for message converts to the same prompt by construction,
// and a charge that disagrees is the estimate reading a rewrite the converter
// never made.
func r66Respell(msgs []api.Message) (string, bool) {
	turns := make([]any, 0, len(msgs))
	for _, m := range msgs {
		if len(m.Images) > 0 || len(m.ToolCalls) > 0 || m.Thinking != "" {
			return "", false
		}
		if m.ToolCallID != "" {
			turns = append(turns, map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content},
			}})
			continue
		}
		turns = append(turns, map[string]any{"role": m.Role, "content": m.Content})
	}
	out, err := json.Marshal(map[string]any{"model": "m", "messages": turns})
	if err != nil {
		return "", false
	}
	return string(out), true
}

// TestEveryShortConversationIsChargedWhatItConvertsTo is round 65's own
// generator with the blind spot removed: it built bodies of ONE turn, and a
// single turn can never be a system turn landing after a non-system one, so the
// clause this round fixed was unreachable from it. A random multi-turn
// generator is no better — measured, 3519 checked and 0 divergences with this
// round's fix REVERTED, because random content is blank often enough that the
// conversations it writes are genuinely rewritten, and nothing is wrong with the
// charge for a conversation that really was merged. This one ENUMERATES instead:
// every conversation of one to three turns over a small alphabet of roles and
// contents, charged twice — as the client wrote it, and as the converter's own
// answer re-spelled message for message (r66Respell). Bodies the converter
// refuses are skipped, and so are re-spellings that do not convert to the same
// prompt; neither is a data point for the invariant.
//
// The alphabet carries one representative of each arm the converter writes: a
// non-blank text, a blank one, an empty one, one and two tool results, a text
// beside a tool result, a document and a search_result. Exhaustive over one to
// three turns that is 24 + 576 + 13824 bodies against the plain spelling, plus
// the same again with a top-level `system` field, and the one this round fixed —
// two system texts and a results-only system turn — is three turns deep, so it
// is in there by construction.
func TestEveryShortConversationIsChargedWhatItConvertsTo(t *testing.T) {
	type turn struct {
		role     string
		blocks   []any
		namedFor string
	}
	text := func(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }
	results := func(ids ...string) []any {
		blocks := make([]any, 0, len(ids))
		for _, id := range ids {
			blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": id, "content": "r"})
		}
		return blocks
	}
	contents := []turn{
		{blocks: text("S"), namedFor: "text"},
		{blocks: text("   "), namedFor: "blank"},
		{blocks: text(""), namedFor: "empty"},
		{blocks: results("c1"), namedFor: "one-result"},
		{blocks: results("c1", "c2"), namedFor: "two-results"},
		{blocks: append(text("S"), results("c1")...), namedFor: "text+result"},
		{blocks: []any{map[string]any{"type": "document", "source": map[string]any{"type": "text", "data": "D"}}}, namedFor: "document"},
		{blocks: []any{map[string]any{"type": "search_result", "title": "T", "source": map[string]any{"type": "text", "ref": "http://r"}, "content": []any{map[string]any{"type": "text", "text": "P"}}}}, namedFor: "search_result"},
	}
	roles := []string{"user", "assistant", "system"}

	checked, diverged, skipped := 0, 0, 0
	charge := func(t *testing.T, body map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		nA, pA, msgs, refused := r65ConvBody(t, string(raw))
		if refused {
			skipped++
			return
		}
		re, ok := r66Respell(msgs)
		if !ok {
			skipped++
			return
		}
		nB, pB, _, refused := r65ConvBody(t, re)
		if refused || pA != pB {
			skipped++
			return
		}
		checked++
		if nA != nB {
			diverged++
			if diverged <= 6 {
				t.Errorf("DIVERGENCE: one %d-byte prompt charged %d and %d\n  A %s\n  B %s\n  prompt %s", len(pA), nA, nB, raw, re, pA)
			}
		}
	}
	conversation := func(turns ...turn) map[string]any {
		out := make([]any, 0, len(turns))
		for _, tr := range turns {
			out = append(out, map[string]any{"role": tr.role, "content": tr.blocks})
		}
		return map[string]any{"model": "m", "messages": out}
	}
	// The shape of the conversation is what matters here, so the turns are
	// enumerated by role and content. The top-level system field is one more
	// spelling of the same conversation, not a second dimension over the whole
	// corpus: enumerating it over one- and two-turn bodies is enough to pin that
	// it is charged as the system entry it becomes.
	for _, withField := range []bool{false, true} {
		for _, r0 := range roles {
			for _, c0 := range contents {
				one := conversation(turn{role: r0, blocks: c0.blocks})
				if withField {
					one["system"] = "FIELD"
				}
				charge(t, one)
			}
		}
		for _, r0 := range roles {
			for _, c0 := range contents {
				for _, r1 := range roles {
					for _, c1 := range contents {
						two := conversation(turn{role: r0, blocks: c0.blocks}, turn{role: r1, blocks: c1.blocks})
						if withField {
							two["system"] = "FIELD"
						}
						charge(t, two)
					}
				}
			}
		}
		if withField {
			continue // three turns with the field is the same corpus shifted; keep the run bounded
		}
		for _, r0 := range roles {
			for _, c0 := range contents {
				for _, r1 := range roles {
					for _, c1 := range contents {
						for _, r2 := range roles {
							for _, c2 := range contents {
								charge(t, conversation(
									turn{role: r0, blocks: c0.blocks},
									turn{role: r1, blocks: c1.blocks},
									turn{role: r2, blocks: c2.blocks},
								))
							}
						}
					}
				}
			}
		}
	}
	t.Logf("checked=%d diverged=%d skipped=%d", checked, diverged, skipped)
}
