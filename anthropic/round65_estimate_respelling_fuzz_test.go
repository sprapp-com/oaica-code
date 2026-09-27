package anthropic

// round65_estimate_respelling_fuzz_test.go — the search that found round 65's
// four leg-1 findings, kept as a permanent guard.
//
// The invariant the estimate owes the meter is one amount per prompt, whatever
// a client's spelling of that prompt was. Four hand-written pairs pin the
// spellings rounds 63-65 fixed; this test generates bodies instead — a random
// turn of random content blocks, with or without a top-level system field —
// charges each one, and then asks a SECOND spelling of the same conversation
// what it is charged: the system text the converter wrote, written as the
// request's own `system` field, and each tool result written as a turn of its
// own, which is exactly the shape the hoist merges to. Bodies the converter
// refuses, and re-renderings that do not convert to the same prompt, are
// skipped — they are not data points for the invariant.
//
// The seed is fixed, so the run is deterministic: at HEAD (0cf50c1ca) it
// reports 1330 of 3628 checked pairs charged differently; with round 65's fix
// it reports none. A future reading that feeds the joined branch something
// other than the runs the merge keeps shows up here as a count, not as a
// surprise in a user's context meter (2026-09-28 audit, round 65).

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

// r65ConvBody charges a body and reports the prompt the converter writes for
// it. A body the converter REFUSES is not a data point for the invariant — the
// random generator can write documents and search results the converter has its
// own opinions about — so the refusal is reported and the caller skips it,
// never a t.Fatalf.
func r65ConvBody(t *testing.T, body string) (charge int, prompt string, msgs []api.Message, refused bool) {
	t.Helper()
	var req MessagesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	conv, err := FromMessagesRequest(req)
	if err != nil {
		return 0, "", nil, true
	}
	p, err := json.Marshal(conv.Messages)
	if err != nil {
		t.Fatalf("marshal prompt: %v", err)
	}
	return conversationBytes(req.Messages, req.System), string(p), conv.Messages, false
}

// r65Rerender writes an equivalent body for a converted prompt: the merged
// system text as the top-level `system` field, and each non-system message as a
// turn of its own role carrying either its text or the tool_result it came from.
// Bodies with images or calls are skipped (nil, false).
func r65Rerender(msgs []api.Message) (string, bool) {
	var sys []string
	type turn struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	var turns []turn
	for _, m := range msgs {
		if len(m.Images) > 0 || len(m.ToolCalls) > 0 || m.Thinking != "" {
			return "", false
		}
		if m.Role == "system" {
			sys = append(sys, m.Content)
			continue
		}
		if m.ToolCallID != "" {
			turns = append(turns, turn{Role: "user", Content: []map[string]any{
				{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content},
			}})
			continue
		}
		turns = append(turns, turn{Role: m.Role, Content: m.Content})
	}
	body := map[string]any{"model": "m", "messages": turns}
	if len(sys) > 0 {
		body["system"] = strings.Join(sys, "\n\n")
	}
	out, err := json.Marshal(body)
	if err != nil {
		return "", false
	}
	return string(out), true
}

func TestTwoSpellingsOfOnePromptAreChargedOneAmount(t *testing.T) {
	rng := rand.New(rand.NewSource(20260928))
	words := []string{"", " ", "\n", "\n\n", "a", "line.", "  x  ", "\t", "long text here"}
	pick := func() string { return words[rng.Intn(len(words))] }
	// One content block, at random.
	block := func() map[string]any {
		switch rng.Intn(6) {
		case 0, 1:
			return map[string]any{"type": "text", "text": pick()}
		case 2:
			return map[string]any{"type": "document", "source": map[string]any{"type": "text", "data": pick()}}
		case 3:
			return map[string]any{"type": "search_result", "title": pick(), "source": map[string]any{"type": "text", "ref": "http://r"}, "content": []any{map[string]any{"type": "text", "text": pick()}}}
		case 4:
			return map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("c%d", rng.Intn(3)), "content": pick()}
		default:
			return map[string]any{"type": "web_search_tool_result", "tool_use_id": fmt.Sprintf("c%d", rng.Intn(3)), "content": []any{map[string]any{"type": "web_search_result", "title": pick(), "url": "http://u"}}}
		}
	}
	checked, diverged, skipped := 0, 0, 0
	for i := 0; i < 4000; i++ {
		role := "user"
		if rng.Intn(2) == 0 {
			role = "system"
		}
		n := 1 + rng.Intn(4)
		content := make([]any, 0, n)
		for j := 0; j < n; j++ {
			content = append(content, block())
		}
		msg := map[string]any{"role": role, "content": content}
		body := map[string]any{"model": "m", "messages": []any{msg}}
		if rng.Intn(2) == 0 {
			body["system"] = pick()
		}
		raw, _ := json.Marshal(body)
		nA, pA, msgs, refused := r65ConvBody(t, string(raw))
		if refused {
			skipped++
			continue
		}
		if len(msgs) == 0 {
			skipped++
			continue
		}
		re, ok := r65Rerender(msgs)
		if !ok {
			skipped++
			continue
		}
		nB, pB, _, refused := r65ConvBody(t, re)
		if refused {
			skipped++
			continue
		}
		if pA != pB {
			// not an equivalent spelling; skip (premise)
			skipped++
			continue
		}
		checked++
		if nA != nB {
			diverged++
			if diverged <= 6 {
				t.Errorf("DIVERGENCE: one %d-byte prompt charged %d and %d\n  A %s\n  B %s\n  prompt %s", len(pA), nA, nB, raw, re, pA)
			}
		}
	}
	t.Logf("checked=%d diverged=%d skipped=%d", checked, diverged, skipped)
}
