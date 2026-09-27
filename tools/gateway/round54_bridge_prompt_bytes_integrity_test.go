package main

// round54_bridge_prompt_bytes_integrity_test.go — round 54, finding B1.
//
// The prompt this gateway writes for a message of text blocks is the sibling
// converter's prompt, down to the byte. Two things broke that for a block that
// states no text:
//
//   - an absent or `null` "text" was kept as an empty text PART, where the local
//     converter writes nothing at all for such a block (`block.Text != nil` gates
//     its text arm), so a body of ["aa", {"type":"text"}] reached the model as
//     "aa\n\n" here and "aa" there;
//   - the parts of such a message were joined with the blank line BETWEEN EVERY
//     PAIR, where the local converter writes the separator behind a
//     `text.Len() > 0` test — so a run that opens with an empty block took one
//     locally and two here: ["","aa"] is "aa" there and was "\n\naa" here, and a
//     message of nothing but empty text blocks is no message there and "\n\n"
//     here.
//
// Both are prompt bytes the client never sent, and the estimate bills them. The
// expectations below are the local converter's own output for the same content
// array, measured with a probe in the anthropic package (round 54).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// round54PromptOf runs one Anthropic body through the gateway and returns the
// prompt the backend was actually sent — the content of the first message, or
// "" when the gateway sent no message with content at all.
func round54PromptOf(t *testing.T, content string) string {
	t.Helper()
	var prompt string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(b, &req)
		if len(req.Messages) > 0 {
			prompt, _ = req.Messages[0].Content.(string)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":9,"completion_tokens":1}}`)
	}))
	defer up.Close()

	srv, _ := round39Gateway(t, up, nil)
	body := `{"model":"kat-awq","max_tokens":16,"stream":false,"messages":[{"role":"user","content":` + content + `}]}`
	if status, resp := round45Ask(t, srv, body); status != http.StatusOK {
		t.Fatalf("status %d for %s\n%s", status, content, resp)
	}
	return prompt
}

// TestThePromptForAnEmptyTextBlockIsTheOneTheLocalLegWrites pins the two rules
// above against the local converter's output for the same body.
func TestThePromptForAnEmptyTextBlockIsTheOneTheLocalLegWrites(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string // what anthropic.convertMessage writes for this content
	}{
		// A block with no text states nothing and takes no separator: the local
		// text arm never runs for it.
		{"nil text opening", `[{"type":"text"},{"type":"text","text":"aa"}]`, "aa"},
		{"nil text closing", `[{"type":"text","text":"aa"},{"type":"text"}]`, "aa"},
		{"nil text between", `[{"type":"text","text":"aa"},{"type":"text"},{"type":"text","text":"bb"}]`, "aa\n\nbb"},
		{"null text opening", `[{"type":"text","text":null},{"type":"text","text":"aa"}]`, "aa"},
		{"nothing but nil text blocks", `[{"type":"text"},{"type":"text"}]`, ""},

		// A STATED empty text is a block: it takes the separator when the run
		// already carries bytes, and a stated empty run takes none.
		{"stated empty opening", `[{"type":"text","text":""},{"type":"text","text":"aa"}]`, "aa"},
		{"stated empty closing", `[{"type":"text","text":"aa"},{"type":"text","text":""}]`, "aa\n\n"},
		{"stated empty between", `[{"type":"text","text":"aa"},{"type":"text","text":""},{"type":"text","text":"bb"}]`, "aa\n\n\n\nbb"},
		{"two stated empties then text", `[{"type":"text","text":""},{"type":"text","text":""},{"type":"text","text":"aa"}]`, "aa"},

		// The control: two blocks that state text are joined by the blank line
		// on both legs and always were.
		{"two stated texts", `[{"type":"text","text":"aa"},{"type":"text","text":"bb"}]`, "aa\n\nbb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := round54PromptOf(t, tc.content); got != tc.want {
				t.Errorf("the prompt sent for %s was %q, want %q — the local converter writes this body that way (its text arm is gated on block.Text != nil and writes the separator behind a text.Len() > 0 test), and the estimate bills what this leg writes, so a phantom blank line is prompt bytes the client never sent and the model is charged for",
					tc.content, got, tc.want)
			}
		})
	}
}

// The control for the first rule: the gateway must still keep a turn whose
// blocks all state nothing — the fix above drops the empty PARTS, not the turn,
// and the round-52 rule (a content array whose blocks all drop out is still a
// turn) is what the sibling legs' treatment of a dropped block rests on. The
// prompt for it is empty, never a phantom blank line.
func TestATurnOfBlocksThatStateNothingIsStillATurn(t *testing.T) {
	if got := round54PromptOf(t, `[{"type":"text"},{"type":"text"}]`); got != "" {
		t.Errorf("a message of two text blocks that state nothing was sent as %q, want the empty content the client's own blocks carry — the sibling converter writes no message for it at all, and a phantom blank line here is prompt the client never sent", got)
	}
}
