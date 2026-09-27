package anthropic

// round57_estimate_spelling_integrity_test.go — round 57's three findings on
// the local leg's estimate (F57-L1-1, F57-L1-2, F57-L1-3).
//
// The estimate seeds the client-visible input_tokens whenever the upstream
// states no usage, so the session's meter and its auto-compaction threshold are
// read off it: two bodies that convert to the SAME prompt must be charged the
// same. Each case below is a spelling the converter folds, charged differently
// because the charge was taken from the body rather than from what the
// converter writes:
//
//   - the top-level `system` field becomes a system MESSAGE and was charged no
//     role bytes, while the message-level spelling of the same prompt was
//     charged six (F57-L1-1);
//   - a call stating "input":{} flattens to the same "arguments":{} an absent
//     input flattens to, and was charged 11 bytes for the key (F57-L1-2);
//   - a tool_result inside an assistant message becomes a message with role
//     "tool" and was charged the CLIENT's role — 9 bytes for "assistant" against
//     the 4 the converter writes (F57-L1-3).
//
// The cases that must NOT fold are pinned beside them: "input":null flattens to
// "arguments":null, a different prompt with a different charge, and a message
// split into two own runs around a result is two messages, not one.

import (
	"encoding/json"
	"strings"
	"testing"
)

// r57Prompt converts one body and returns the estimate and the prompt it
// converts to, so a test can show the two spellings are the same conversation.
func r57Prompt(t *testing.T, body string) (int, string) {
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

func TestSpellingsOfOnePromptAreChargedTheSame(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  string
		second string
	}{
		{
			"the top-level system field is the system message it writes",
			`{"model":"m","max_tokens":16,"system":"hi","messages":[{"role":"user","content":"there"}]}`,
			`{"model":"m","max_tokens":16,"messages":[{"role":"system","content":"hi"},{"role":"user","content":"there"}]}`,
		},
		{
			"an array system is the one joined system message",
			`{"model":"m","max_tokens":16,"system":[{"type":"text","text":"hi"},{"type":"text","text":"j"}],"messages":[{"role":"user","content":"x"}]}`,
			`{"model":"m","max_tokens":16,"messages":[{"role":"system","content":"hi\n\nj"},{"role":"user","content":"x"}]}`,
		},
		{
			"a call stating the empty object states no input",
			`{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{}}]},{"role":"user","content":"x"}]}`,
			`{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f"}]},{"role":"user","content":"x"}]}`,
		},
		{
			"a server_tool_use stating the empty object states no input",
			`{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"t","name":"web_search","input":{}}]},{"role":"user","content":"x"}]}`,
			`{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"t","name":"web_search"}]},{"role":"user","content":"x"}]}`,
		},
		{
			"a tool_result is the tool message it writes, in either role",
			`{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"t1","content":"out"}]},{"role":"user","content":"x"}]}`,
			`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"out"}]},{"role":"user","content":"x"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			firstEst, firstPrompt := r57Prompt(t, tc.first)
			secondEst, secondPrompt := r57Prompt(t, tc.second)
			if firstPrompt != secondPrompt {
				t.Fatalf("the two spellings are not one prompt — this test cannot judge the charge:\n %s\n %s", firstPrompt, secondPrompt)
			}
			if firstEst != secondEst {
				t.Errorf("one prompt, two charges: %d against %d\nThe estimate seeds the client-visible input_tokens and the auto-compaction threshold, so the session's meter reads a prompt the model was never sent (2026-09-28 audit, round 57, F57-L1-1/F57-L1-2/F57-L1-3).\nprompt: %s", firstEst, secondEst, firstPrompt)
			}
		})
	}
}

// The cases the fixes must NOT fold: each is a genuinely different prompt, and
// the charge has to keep telling them apart.
func TestSpellingsThatAreDifferentPromptsKeepDifferentCharges(t *testing.T) {
	nullInput, _ := r57Prompt(t, `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":null}]},{"role":"user","content":"x"}]}`)
	absentInput, absentPrompt := r57Prompt(t, `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f"}]},{"role":"user","content":"x"}]}`)
	if nullInput == absentInput {
		t.Errorf("a call stating input:null was charged the same as one stating no input (%d): null flattens to \"arguments\":null, so it is a different prompt and its charge is its own (2026-09-28 audit, round 57, F57-L1-2 — the fix must not fold this case)", nullInput)
	}
	if !strings.Contains(absentPrompt, `"arguments":{}`) {
		t.Errorf("the absent-input prompt moved: %s", absentPrompt)
	}

	// [text, result, text] is THREE messages: two runs of the client's role
	// around one tool message, and each role is charged.
	split, splitPrompt := r57Prompt(t, `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"text","text":"read it."},{"type":"tool_result","tool_use_id":"t1","content":"out"},{"type":"text","text":"now run."}]}]}`)
	if !strings.Contains(splitPrompt, `"role":"tool"`) || !strings.Contains(splitPrompt, `"role":"assistant"`) {
		t.Fatalf("the split prompt is not the one this test is about: %s", splitPrompt)
	}
	// The same prompt built by hand, charged with the roles the converter
	// writes: two assistant messages and one tool message.
	built := `{"model":"m","max_tokens":16,"messages":[` +
		`{"role":"assistant","content":[{"type":"text","text":"read it."}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"out"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"now run."}]}]}`
	builtEst, builtPrompt := r57Prompt(t, built)
	if builtPrompt != splitPrompt {
		t.Fatalf("the hand-built twin is not the same prompt:\n %s\n %s", builtPrompt, splitPrompt)
	}
	if split != builtEst {
		t.Errorf("a message split into two runs around a result was charged %d against the %d of the same three messages written out: the run count is what the charge follows (2026-09-28 audit, round 57, F57-L1-3)", split, builtEst)
	}
}
