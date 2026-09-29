package openai

// round105_leg1_request_turns_test.go — leg 1, round 105 (2026-09-29 audit),
// F105-L1-1, F105-L1-2, F105-L1-3 (fixed) and F105-L1-4 (recorded).

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

// TestMine105AnEmptyArrayTurnIsStillATurn is F105-L1-1's pin: the array spelling
// of a contentless turn keeps the turn, as `content: ""`, Anthropic and Responses do.
func TestMine105AnEmptyArrayTurnIsStillATurn(t *testing.T) {
	got := r104Convert(t, []Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: []any{}},
	})
	if len(got) != 2 || got[1].Role != "user" {
		t.Errorf("the turns are %+v — a user turn whose array held no part was deleted, where content:\"\" keeps it as an empty message (2026-09-29 audit, round 105, F105-L1-1)", got)
	}
}

// TestMine105ToolChoiceNoneSendsNoToolsOnResponses is F105-L1-2's pin.
func TestMine105ToolChoiceNoneSendsNoToolsOnResponses(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}}}],"tool_choice":"none"}`
	for _, tc := range []struct{ choice, want string }{{`"none"`, "0"}, {`" None "`, "0"}, {`"auto"`, "1"}} {
		var req ResponsesRequest
		if err := jsonUnmarshal(strings.Replace(body, `"none"`, tc.choice, 1), &req); err != nil {
			t.Fatal(err)
		}
		out, err := FromResponsesRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := itoa(len(out.Tools)); got != tc.want {
			t.Errorf("tool_choice %s left %s tools, want %s — chat and Anthropic send none for \"none\" (2026-09-29 audit, round 105, F105-L1-2)", tc.choice, got, tc.want)
		}
	}
}

// TestMine105AResultIsNamedAfterTheCallBeforeIt is F105-L1-3's pin: the same id
// reused by a later turn names the LATER result after the later call only.
func TestMine105AResultIsNamedAfterTheCallBeforeIt(t *testing.T) {
	call := func(name string) Message {
		c := r104Call()
		c.ID = "call_0"
		c.Function.Name = name
		return Message{Role: "assistant", Content: "", ToolCalls: []ToolCall{c}}
	}
	got := r104Convert(t, []Message{
		{Role: "user", Content: "go"},
		call("read_file"), {Role: "tool", Content: "file text", ToolCallID: "call_0"},
		call("run_tests"), {Role: "tool", Content: "ok", ToolCallID: "call_0"},
	})
	var names []string
	for _, m := range got {
		if m.Role == "tool" {
			names = append(names, m.ToolName)
		}
	}
	if strings.Join(names, ",") != "read_file,run_tests" {
		t.Errorf("results are named %v, want [read_file run_tests] — a result answers the call before it, nearest first (2026-09-29 audit, round 105, F105-L1-3)", names)
	}
}

// TestMine105AVisionTurnIsSplitIntoTwoMessages RECORDS F105-L1-4: a chat turn
// `[text, image_url]` becomes two consecutive user messages where Responses and
// Anthropic keep one. Inherited from upstream ollama's FromChatRequest (one
// message per part), and merging changes what every chat vision client sends the
// runner — recorded, not changed. If this now reads one message the record is
// spent and the merge has been made.
func TestMine105AVisionTurnIsSplitIntoTwoMessages(t *testing.T) {
	got := r104Convert(t, []Message{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "what is this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
	}}})
	var users []api.Message
	for _, m := range got {
		if m.Role == "user" {
			users = append(users, m)
		}
	}
	if len(users) != 2 {
		t.Errorf("the vision turn is %d user messages, this record states 2 (2026-09-29 audit, round 105, F105-L1-4)", len(users))
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
func itoa(n int) string                   { return strconv.Itoa(n) }
