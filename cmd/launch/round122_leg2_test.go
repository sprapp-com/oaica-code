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

// F123-L2-1 (2026-09-29 audit, round 123): one large call streamed in small fragments, and many calls
// each continued by id, are handled in linear time.
func TestRound123StreamedSingleLargeCallIsLinear(t *testing.T) {
	var b strings.Builder
	b.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c0\",\"type\":\"function\",\"function\":{\"name\":\"t\",\"arguments\":\"{\\\"a\\\":\\\"\"}}]}}]}\n\n")
	for i := 0; i < 24000; i++ {
		b.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"abcdefghijklmnop\"}}]}}]}\n\n")
	}
	b.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"}\"}}]}}]}\n\n")
	b.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	w := httptest.NewRecorder()
	start := time.Now()
	handleStreamResponse(w, strings.NewReader(b.String()), "m", func(int) {}, 0, "")
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("one call in 24000 fragments took %s: the finished-object check re-reads the whole text each time", d)
	}
	if got := strings.Count(w.Body.String(), `"type":"tool_use"`); got != 1 {
		t.Errorf("%d tool_use blocks, want 1", got)
	}
}

func TestRound123ManyCallsContinuedByIDAreLinear(t *testing.T) {
	const n = 30000
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":%d,\"id\":\"c%d\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n", i, i)
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":%d,\"id\":\"c%d\",\"function\":{\"arguments\":\"1}\"}}]}}]}\n\n", i, i)
	}
	b.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	w := httptest.NewRecorder()
	start := time.Now()
	handleStreamResponse(w, strings.NewReader(b.String()), "m", func(int) {}, 0, "")
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("%d calls continued by id took %s: the slot lookup walks every call on every fragment", n, d)
	}
	if got := strings.Count(w.Body.String(), `"type":"tool_use"`); got != n {
		t.Errorf("%d tool_use blocks, want %d", got, n)
	}
}
