package anthropic

// F122-L1-5 (2026-09-29 audit, round 122): joining the thinking blocks of one turn is linear.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRound122ThinkingBlocksJoinInLinearTime(t *testing.T) {
	const n = 100000
	var sb strings.Builder
	sb.WriteString(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"type":"thinking","thinking":"aaaaaaaa","signature":"s"}`)
	}
	sb.WriteString(`]},{"role":"user","content":"go"}]}`)
	var req MessagesRequest
	if err := json.Unmarshal([]byte(sb.String()), &req); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	cr, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("%d thinking blocks took %s to convert: the join is quadratic", n, d)
	}
	joined := 0
	for _, m := range cr.Messages {
		joined += len(m.Thinking)
	}
	if want := n*8 + (n-1)*2; joined != want {
		t.Errorf("joined thinking is %d bytes, want %d", joined, want)
	}
}
