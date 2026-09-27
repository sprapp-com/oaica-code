package anthropic

// round58_contentless_turn_charge_integrity_test.go — round 58's three findings
// on the local leg's estimate (F58-L1-1, F58-L1-2, F58-L1-3).
//
// All three are the same rule one step further than round 57 took it: the
// estimate must charge the conversation the CONVERTER writes, and the walk that
// models it (messageShapeBytes) knew what convertMessage writes but not what
// FromMessagesRequest does AROUND it — the role-only turn its fallback writes
// for a message that converts to nothing, and the single user turn its last
// guard writes for a conversation no turn carries anything in. Nor did the walk
// know which blocks the converter drops whole.
//
//   - F58-L1-1: a message that converts to NOTHING is not absent from the
//     prompt. FromMessagesRequest writes one message carrying the client's role
//     and no content, so an empty text block, an empty content array, a null
//     content and a replayed redacted_thinking turn are all a turn the model
//     reads — and all were charged nothing. A conversation in which EVERY turn
//     is one of those is replaced outright by one user turn, which is a
//     different charge again, not the sum of the roles it replaced.
//   - F58-L1-2: redacted_thinking never reaches a run (convertMessage drops it
//     deliberately) and a textless thinking block leaves its run empty, so
//     neither is a message — but both marked the run stated and charged the
//     client's role for a prompt that does not contain that message.
//   - F58-L1-3: normalizeSystemFirst deletes a system message the converter
//     wrote nothing for, and the rewrite it deletes it with was suppressed by
//     asking the CLIENT's block types whether the message was text-only. A
//     system message holding only redacted_thinking or a textless thinking
//     block was charged a role the prompt does not contain.
//
// Every case below is fail-first: each one is RED against the tree before this
// round's fix and names the finding it pins. The estimate seeds the
// client-visible input_tokens whenever the upstream states no usage, so a wrong
// charge here is the session's meter and its auto-compaction threshold reading
// a prompt the model was never sent.

import (
	"encoding/json"
	"testing"
)

// r58Charge converts one body and returns the estimate and the prompt it
// converts to — the prompt is the premise every case here rests on.
func r58Charge(t *testing.T, body string) (int, string) {
	t.Helper()
	var req MessagesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	conv, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("convert %s: %v", body, err)
	}
	raw, err := json.Marshal(conv.Messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	return EstimateInputTokens(req), string(raw)
}

// r58SamePrompt asserts two bodies are one conversation before judging their
// charges: a parity case whose premise fails proves nothing.
func r58SamePrompt(t *testing.T, first, second string) (int, int) {
	t.Helper()
	firstEst, firstPrompt := r58Charge(t, first)
	secondEst, secondPrompt := r58Charge(t, second)
	if firstPrompt != secondPrompt {
		t.Fatalf("the two spellings are not one prompt — this case cannot judge the charge:\n %s\n %s", firstPrompt, secondPrompt)
	}
	return firstEst, secondEst
}

// F58-L1-1: every spelling of a turn that converts to nothing is the same
// role-only turn on the wire.
func TestATurnTheConverterWritesNothingForIsStillATurn(t *testing.T) {
	real := `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[]},{"role":"user","content":"x"}]}`
	withRealTurn, realPrompt := r58Charge(t, real)
	// The one role-only turn plus the user turn: FromMessagesRequest's fallback
	// writes "assistant" and the user turn writes "user" + "x".
	if want := (len("assistant") + len("user") + 1) / 4; withRealTurn != want {
		t.Errorf("a contentless assistant turn beside a real one was charged %d, want %d\nthe fallback writes one message carrying the client's role, so the prompt holds a turn the model reads — charging nothing under-bills the session's meter (2026-09-28 audit, round 58, F58-L1-1)\nprompt: %s", withRealTurn, want, realPrompt)
	}

	// Every spelling of that turn is the same prompt and the same charge.
	for _, spelling := range []string{
		`{"role":"assistant","content":[]}`,
		`{"role":"assistant","content":null}`,
		`{"role":"assistant","content":""}`,
		`{"role":"assistant","content":[{"type":"text","text":""}]}`,
		`{"role":"assistant","content":[{"type":"redacted_thinking"}]}`,
		`{"role":"assistant","content":[{"type":"thinking"}]}`,
		`{"role":"assistant","content":[{"type":"thinking","thinking":""}]}`,
	} {
		body := `{"model":"m","max_tokens":16,"messages":[` + spelling + `,{"role":"user","content":"x"}]}`
		est, _ := r58SamePrompt(t, body, real)
		if est != withRealTurn {
			t.Errorf("%s was charged %d against the %d of the same turn written as an empty array — one prompt, two charges (2026-09-28 audit, round 58, F58-L1-1/F58-L1-2)", spelling, est, withRealTurn)
		}
	}
}

// The other side of F58-L1-1: a conversation in which EVERY turn is one of those
// is replaced whole, so it is not charged the sum of the roles it replaced.
func TestAConversationNothingCarriesIsChargedAsTheOneTurnItBecomes(t *testing.T) {
	est, prompt := r58Charge(t, `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[]},{"role":"assistant","content":[]}]}`)
	if prompt != `[{"role":"user","content":""}]` {
		t.Fatalf("the premise moved — the writer no longer replaces the list: %s", prompt)
	}
	// The one user turn it becomes: four bytes of role and no content.
	oneUserTurn, _ := r58Charge(t, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":""}]}`)
	if est != len("user")/4 || oneUserTurn != est {
		t.Errorf("a conversation no turn carries anything in was charged %d, want %d (%d for the same one-word prompt written by hand)\nthe writer replaces the whole list with ONE user turn, so charging each replaced role bills turns the model never received (2026-09-28 audit, round 58, F58-L1-1)\nprompt: %s", est, len("user")/4, oneUserTurn, prompt)
	}

	// A blank system message is deleted by the rewrite, so a conversation of one
	// of those plus contentless turns is the same one-word prompt.
	blank, _ := r58Charge(t, `{"model":"m","max_tokens":16,"messages":[{"role":"system","content":[{"type":"text","text":"   "}]},{"role":"assistant","content":[]}]}`)
	if blank != est {
		t.Errorf("a blank system message beside a contentless turn was charged %d against the %d of the same one-word prompt written without it (2026-09-28 audit, round 58, F58-L1-1/F58-L1-3)", blank, est)
	}
}

// F58-L1-2: a block the converter drops whole is not a message beside a tool
// result either — the result's tool message is the whole turn.
func TestABlockTheConverterDropsIsNotAMessageBesideAResult(t *testing.T) {
	for _, dropped := range []string{
		`{"type":"redacted_thinking"}`,
		`{"type":"thinking"}`,
		`{"type":"thinking","thinking":""}`,
	} {
		withIt := `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[` + dropped + `,{"type":"tool_result","tool_use_id":"t","content":"out"}]},{"role":"user","content":"x"}]}`
		withoutIt := `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"t","content":"out"}]},{"role":"user","content":"x"}]}`
		est, want := r58SamePrompt(t, withIt, withoutIt)
		if est != want {
			t.Errorf("%s beside a tool_result was charged %d against the %d of the same tool message alone: the converter writes no run for it, so the turn is one tool message and the client's role is a message the prompt does not contain (2026-09-28 audit, round 58, F58-L1-2)", dropped, est, want)
		}
	}
}

// F58-L1-3: a system message the converter writes nothing for is deleted by the
// rewrite, so its role is not in the prompt and must not be charged.
func TestASystemMessageTheConverterWritesNothingForIsNotCharged(t *testing.T) {
	for _, content := range []string{
		`[{"type":"redacted_thinking"}]`,
		`[{"type":"thinking"}]`,
		`[{"type":"thinking","thinking":""}]`,
		`[{"type":"text","text":""},{"type":"redacted_thinking"}]`,
	} {
		withIt := `{"model":"m","max_tokens":16,"messages":[{"role":"system","content":` + content + `},{"role":"user","content":"x y"}]}`
		withoutIt := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"x y"}]}`
		est, want := r58SamePrompt(t, withIt, withoutIt)
		if est != want {
			t.Errorf("a system message holding only %s was charged %d against the %d of the same prompt written without it: the rewrite deletes it, so the prompt holds no system role (2026-09-28 audit, round 58, F58-L1-3)", content, est, want)
		}
	}
}

// The controls: the fixes must not fold a block that DOES write, nor a system
// message that carries something the merge would drop.
func TestBlocksThatWriteAreStillCharged(t *testing.T) {
	// A thinking block with text is a run of its own: beside a tool result it
	// is a message the converter writes.
	withThinking := `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reasoning"},{"type":"tool_result","tool_use_id":"t","content":"out"}]},{"role":"user","content":"x"}]}`
	withoutThinking := `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"t","content":"out"}]},{"role":"user","content":"x"}]}`
	est, _ := r58Charge(t, withThinking)
	want, _ := r58Charge(t, withoutThinking)
	if est == want {
		t.Errorf("a thinking block WITH text was charged the same as a turn without it (%d): the converter writes that run, and it is a message of the client's own role", est)
	}

	// A system message carrying an image cannot be merged, so it is charged as
	// it arrives — the prompts genuinely differ.
	_, imagePrompt := r58Charge(t, `{"model":"m","max_tokens":16,"messages":[{"role":"system","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]},{"role":"user","content":"x y"}]}`)
	if imagePrompt == `[{"role":"user","content":"x y"}]` {
		t.Errorf("the image-system premise moved: %s", imagePrompt)
	}
}
