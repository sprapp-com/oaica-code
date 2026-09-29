package api

import "testing"

func r107call(id, name string) ToolCall {
	return ToolCall{ID: id, Function: ToolCallFunction{Name: name}}
}

// TestNameToolResultsNearestNamedCallBeforeThenAfter is F107-L1-2's pin: the one
// walk every surface shares — nearest call before with a name, then after.
func TestNameToolResultsNearestNamedCallBeforeThenAfter(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{r107call("c1", "Bash")}},
		{Role: "tool", ToolCallID: "c1"},                               // named after the call before it
		{Role: "assistant", ToolCalls: []ToolCall{r107call("c1", "")}}, // nearer, but nameless
		{Role: "tool", ToolCallID: "c1"},                               // walks past the nameless one
		{Role: "tool", ToolCallID: "late"},                             // result listed before its call
		{Role: "assistant", ToolCalls: []ToolCall{r107call("late", "Read")}},
		{Role: "tool", ToolCallID: "c1", ToolName: "Stated"}, // a stated name is never replaced
	}
	NameToolResults(msgs)
	for i, want := range map[int]string{1: "Bash", 3: "Bash", 4: "Read", 6: "Stated"} {
		if msgs[i].ToolName != want {
			t.Errorf("message %d is named %q, want %q (2026-09-29 audit, round 107, F107-L1-2)", i, msgs[i].ToolName, want)
		}
	}
}
