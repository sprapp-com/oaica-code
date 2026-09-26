package launch

// request_log_system_prompt_chars_integrity_test.go — the logged CHARS missed
// Claude Code's entire system prompt (2026-09-26 audit, ninth round, auditor B).
//
// extractLastAndTotalMessageLen read messages[] only, but Anthropic carries
// the system prompt in a top-level `system` field — and for Claude Code that
// field is the largest single part of every request. CHARS (and therefore
// would_be_hard_by_len, which the flashplan classifier is evaluated against)
// measured the conversation while ignoring the prompt, so a turn with a ~30k
// character system prompt and a one-word user message logged a few hundred
// characters and read as an easy turn. The router's own classifier never had
// this blind spot: the translation puts that prompt at messages[0] first.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The top-level system prompt counts toward the total, in both of the shapes
// Anthropic accepts for it: a string, and an array of content blocks.
func TestSystemPromptCountsTowardLoggedChars(t *testing.T) {
	prompt := strings.Repeat("S", 30000)
	cases := map[string]string{
		"string system":  `{"system":"` + prompt + `","messages":[{"role":"user","content":"hi"}]}`,
		"block system":   `{"system":[{"type":"text","text":"` + prompt + `"}],"messages":[{"role":"user","content":"hi"}]}`,
		"openai control": `{"messages":[{"role":"system","content":"` + prompt + `"},{"role":"user","content":"hi"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			last, total := extractLastAndTotalMessageLen([]byte(body))
			if total < len(prompt) {
				t.Errorf("total = %d chars for a request carrying a %d-character system prompt — the prompt is the largest part of every Claude Code request and it was not measured, so CHARS and would_be_hard_by_len classify the turn as easy", total, len(prompt))
			}
			if last != len("hi") {
				t.Errorf("last = %d, want %d — lastLen is defined as the last MESSAGE's length and must not absorb the system prompt", last, len("hi"))
			}
		})
	}
}

// The end of the chain: a body like Claude Code's must make the logged row say
// the turn would have been classified hard by length.
func TestAHugeSystemPromptLogsAsLongByLen(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	body, err := json.Marshal(map[string]any{
		"model":  "m1",
		"system": strings.Repeat("S", 30000),
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	upstream := jsonUpstream(t, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	defer upstream.Close()

	proxy := startLocalLoggingProxy(t, upstream.URL)
	req, err := http.NewRequest(http.MethodPost, proxy+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// The row is written from the handler's own goroutine, which can land just
	// after the client has read the last byte.
	rows := waitForRequestLogRows(t, 1)
	if len(rows) != 1 {
		t.Fatalf("got %d rows for one turn, want 1", len(rows))
	}
	if rows[0].TotalMessagesLen < 30000 {
		t.Errorf("row total_messages_len = %d for a body carrying a 30000-character system prompt", rows[0].TotalMessagesLen)
	}
	if !rows[0].WouldBeHardByLen {
		t.Errorf("row %+v says the turn would NOT be hard by length — the system prompt was invisible, so `oaica usage` reports the classifier's view of a request it never saw", rows[0])
	}
}
