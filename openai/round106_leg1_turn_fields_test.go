package openai

// round106_leg1_turn_fields_test.go — leg 1, round 106 (2026-09-29 audit),
// F106-L1-1 .. F106-L1-4: fields a turn states that one spelling of the same body
// kept and another dropped.

import (
	"encoding/json"
	"testing"
)

func r106Convert(t *testing.T, msgs string) []Message {
	t.Helper()
	var ms []Message
	if err := json.Unmarshal([]byte(msgs), &ms); err != nil {
		t.Fatal(err)
	}
	return ms
}

// TestMine106AnArrayTurnKeepsItsReasoning is F106-L1-1's pin.
func TestMine106AnArrayTurnKeepsItsReasoning(t *testing.T) {
	str := r104Convert(t, r106Convert(t, `[{"role":"assistant","content":"hi","reasoning":"think"}]`))
	arr := r104Convert(t, r106Convert(t, `[{"role":"assistant","content":[{"type":"text","text":"hi"}],"reasoning":"think"}]`))
	if str[0].Thinking != "think" {
		t.Fatalf("premise: the string spelling reads thinking %q", str[0].Thinking)
	}
	if arr[0].Thinking != str[0].Thinking {
		t.Errorf("the array spelling reads thinking %q, want %q (2026-09-29 audit, round 106, F106-L1-1)", arr[0].Thinking, str[0].Thinking)
	}
}

// TestMine106AnArrayToolResultKeepsItsName is F106-L1-2's pin: `name` with no id.
func TestMine106AnArrayToolResultKeepsItsName(t *testing.T) {
	str := r104Convert(t, r106Convert(t, `[{"role":"tool","name":"Bash","content":"ok"}]`))
	arr := r104Convert(t, r106Convert(t, `[{"role":"tool","name":"Bash","content":[{"type":"text","text":"ok"}]}]`))
	if str[0].ToolName != "Bash" {
		t.Fatalf("premise: the string spelling reads name %q", str[0].ToolName)
	}
	if arr[0].ToolName != "Bash" {
		t.Errorf("the array spelling reads name %q, want Bash (2026-09-29 audit, round 106, F106-L1-2)", arr[0].ToolName)
	}
}

// TestMine106ResponsesNamesItsToolResults is F106-L1-3's pin for Responses.
func TestMine106ResponsesNamesItsToolResults(t *testing.T) {
	var req ResponsesRequest
	body := `{"model":"m","input":[{"type":"message","role":"user","content":"go"},` +
		`{"type":"function_call","call_id":"call_1","name":"Bash","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"a.txt"}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	out, err := FromResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	last := out.Messages[len(out.Messages)-1]
	if last.Role != "tool" || last.ToolName != "Bash" {
		t.Errorf("the result reads role=%s name=%q, want tool/Bash — chat names a result after the call it answers (2026-09-29 audit, round 106, F106-L1-3)", last.Role, last.ToolName)
	}
}

// TestMine106ResponsesJSONObjectIsJSONMode is F106-L1-4's pin.
func TestMine106ResponsesJSONObjectIsJSONMode(t *testing.T) {
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(`{"model":"m","input":"hi","text":{"format":{"type":"json_object"}}}`), &req); err != nil {
		t.Fatal(err)
	}
	out, err := FromResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Format) != `"json"` {
		t.Errorf("format = %q, want \"json\" — chat's json_object sends it (2026-09-29 audit, round 106, F106-L1-4)", out.Format)
	}
}

// TestMine107ResponsesReadsTheFormatTypeLikeChat is F107-L1-3's pin: chat folds
// case and space on response_format.type, and Responses compared it exactly.
func TestMine107ResponsesReadsTheFormatTypeLikeChat(t *testing.T) {
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(`{"model":"m","input":"hi","text":{"format":{"type":" JSON_OBJECT "}}}`), &req); err != nil {
		t.Fatal(err)
	}
	out, err := FromResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Format) != `"json"` {
		t.Errorf("format = %q, want \"json\" (2026-09-29 audit, round 107, F107-L1-3)", out.Format)
	}
}
