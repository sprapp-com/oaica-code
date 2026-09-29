package api

import "sort"

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
	// One pass builds, per call id, the messages that carry a NAMED call with it (in order; the
	// first call with the id inside a message decides, as the walk always did). Each result then
	// finds its nearest one by binary search. The walk it replaces was quadratic in the number of
	// calls and results: a hostile history of ~64k of each held a CPU core for tens of seconds per
	// call, twice per request on the translated leg (2026-09-29 audit, round 121, F121-L1-1).
	type named struct {
		msg  int
		name string
	}
	var byID map[string][]named
	for j := range messages {
		var seen map[string]bool
		for _, tc := range messages[j].ToolCalls {
			if tc.ID == "" {
				continue
			}
			if seen == nil {
				seen = map[string]bool{}
			}
			if seen[tc.ID] {
				continue
			}
			seen[tc.ID] = true
			if tc.Function.Name == "" {
				continue
			}
			if byID == nil {
				byID = map[string][]named{}
			}
			byID[tc.ID] = append(byID[tc.ID], named{j, tc.Function.Name})
		}
	}
	for i := range messages {
		m := &messages[i]
		if m.Role != "tool" || m.ToolName != "" || m.ToolCallID == "" {
			continue
		}
		list := byID[m.ToolCallID]
		// first entry at or after i; the entry before it (if any) is the nearest one before i
		k := sort.Search(len(list), func(x int) bool { return list[x].msg >= i })
		if k > 0 {
			m.ToolName = list[k-1].name
			continue
		}
		for ; k < len(list); k++ {
			if list[k].msg > i {
				m.ToolName = list[k].name
				break
			}
		}
	}
}
