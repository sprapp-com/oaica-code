package launch

// F122-L2-1 (2026-09-29 audit, round 122): a streamed turn of many tool calls is handled in
// linear time and keeps arrival order, as the document arm does.

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func round122StreamBody(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":%d,\"id\":\"c%d\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\n", i, i)
	}
	b.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	return b.String()
}

func TestRound122StreamedToolCallsAreLinearAndInOrder(t *testing.T) {
	const n = 30000
	w := httptest.NewRecorder()
	start := time.Now()
	handleStreamResponse(w, strings.NewReader(round122StreamBody(n)), "m", func(int) {}, 0, "")
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("%d streamed tool calls took %s: the flush is quadratic", n, d)
	}
	out := w.Body.String()
	if got := strings.Count(out, `"type":"tool_use"`); got != n {
		t.Fatalf("%d tool_use blocks, want %d", got, n)
	}
	last := -1
	for _, part := range strings.Split(out, `"id":"c`)[1:] {
		var id int
		fmt.Sscanf(part, "%d", &id)
		if id != last+1 {
			t.Fatalf("call c%d came after c%d: arrival order lost", id, last)
		}
		last = id
	}
}
