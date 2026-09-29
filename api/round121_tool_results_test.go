package api

// Round 121 leg 1 (2026-09-29 audit, F121-L1-1): NameToolResults answers as the nearest-before,
// then nearest-after walk always did, in linear time.

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// nameToolResultsWalk is the walk this function used to be, kept as the reference.
func nameToolResultsWalk(messages []Message) {
	find := func(i int, id string) string {
		for _, tc := range messages[i].ToolCalls {
			if tc.ID == id {
				return tc.Function.Name
			}
		}
		return ""
	}
	for i := range messages {
		m := &messages[i]
		if m.Role != "tool" || m.ToolName != "" || m.ToolCallID == "" {
			continue
		}
		for j := i - 1; j >= 0 && m.ToolName == ""; j-- {
			m.ToolName = find(j, m.ToolCallID)
		}
		for j := i + 1; j < len(messages) && m.ToolName == ""; j++ {
			m.ToolName = find(j, m.ToolCallID)
		}
	}
}

func randomHistory(r *rand.Rand) []Message {
	n := 1 + r.Intn(12)
	msgs := make([]Message, n)
	ids := []string{"a", "b", "c", "call_0", ""}
	names := []string{"", "read", "write", "search"}
	for i := range msgs {
		switch r.Intn(3) {
		case 0:
			msgs[i] = Message{Role: "assistant"}
			for k := r.Intn(4); k > 0; k-- {
				msgs[i].ToolCalls = append(msgs[i].ToolCalls, ToolCall{ID: ids[r.Intn(len(ids))], Function: ToolCallFunction{Name: names[r.Intn(len(names))]}})
			}
		case 1:
			msgs[i] = Message{Role: "tool", ToolCallID: ids[r.Intn(len(ids))]}
			if r.Intn(5) == 0 {
				msgs[i].ToolName = "preset"
			}
			if r.Intn(6) == 0 {
				msgs[i].ToolCalls = []ToolCall{{ID: ids[r.Intn(len(ids))], Function: ToolCallFunction{Name: names[r.Intn(len(names))]}}}
			}
		default:
			msgs[i] = Message{Role: "user"}
		}
	}
	return msgs
}

func TestRound121NameToolResultsMatchesTheWalk(t *testing.T) {
	r := rand.New(rand.NewSource(121))
	for it := 0; it < 20000; it++ {
		a := randomHistory(r)
		b := make([]Message, len(a))
		for i := range a {
			b[i] = a[i]
			b[i].ToolCalls = append([]ToolCall(nil), a[i].ToolCalls...)
		}
		NameToolResults(a)
		nameToolResultsWalk(b)
		for i := range a {
			if a[i].ToolName != b[i].ToolName {
				t.Fatalf("iteration %d message %d: got %q, the walk gives %q\nhistory: %+v", it, i, a[i].ToolName, b[i].ToolName, b)
			}
		}
	}
}

func TestRound121NameToolResultsIsLinear(t *testing.T) {
	const n = 48000
	msgs := make([]Message, 0, 1+n)
	calls := make([]ToolCall, n)
	for i := range calls {
		calls[i] = ToolCall{ID: fmt.Sprintf("c%d", i), Function: ToolCallFunction{Name: "t"}}
	}
	msgs = append(msgs, Message{Role: "assistant", ToolCalls: calls})
	for i := 0; i < n; i++ {
		msgs = append(msgs, Message{Role: "tool", ToolCallID: fmt.Sprintf("orphan%d", i)})
	}
	start := time.Now()
	NameToolResults(msgs)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("%d orphan results took %s: the walk is quadratic", n, d)
	}
}
