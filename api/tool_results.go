package api

// NameToolResults states, on every tool result that carries only the id of the
// call it answers, the name of that call.
//
// The call is the nearest one BEFORE the result that carries the id and a name
// (a call with no name yields none, so the walk goes on past it):
// a result answers the call ahead of it, and clients and local proxies that
// repeat an id like `call_0` every turn must not have an earlier result named
// after a later call. A call with no name is not an answer to "what tool was
// this" and is walked past. Calls AFTER the result are the fallback, nearest
// first, so a history that lists the result first still finds its call.
//
// One rule for every surface: the chat converter derives the name with the same
// walk, and the Anthropic and Responses converters call this, so the same
// history reaches the renderers (which read ToolName) named alike
// (2026-09-29 audit, rounds 105-107, F105-L1-3, F106-L1-3, F107-L1-2).
func NameToolResults(messages []Message) {
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
