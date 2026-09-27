package anthropic

// round49_system_and_tool_integrity_test.go — round 49's findings on the local
// leg.
//
// B-F5: a system array's text blocks were joined with a blank line between
// them, so a block that carried no text at all still contributed its separator
// — a body whose system array ended with an empty block reached the backend as
// "alpha\n\n" where the metered gateway (and the client-side proxy, which share
// this conversion) send "alpha". An empty text block says nothing and is
// skipped, which is what both other legs do.
//
// B-F3 (the tool surface): a tool that states no input_schema reaches the
// backend as the zero ToolFunctionParameters, and a schema of the wrong KIND is
// refused; the built-in web_search's schema lives in its TYPE, and a client tool
// claiming the name the built-in owns is dropped. Round 49 mirrored all three
// on the gateway leg, where they were missing — the pins here are what the
// mirror is checked against.

import (
	"encoding/json"
	"testing"
)

// TestAnEmptySystemTextBlockContributesNothing is B-F5. The wire that adds an
// empty text block to a system array is the same turn as the wire without it.
func TestAnEmptySystemTextBlockContributesNothing(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"empty_last", `{"model":"m","messages":[{"role":"user","content":"go"}],"system":[{"type":"text","text":"alpha"},{"type":"text","text":""}]}`},
		{"empty_first", `{"model":"m","messages":[{"role":"user","content":"go"}],"system":[{"type":"text","text":""},{"type":"text","text":"alpha"}]}`},
		{"empty_only", `{"model":"m","messages":[{"role":"user","content":"go"}],"system":[{"type":"text","text":""}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := round42Convert(t, c.body)
			want := []string{"system=alpha", "user=go"}
			if c.name == "empty_only" {
				want = []string{"user=go"}
			}
			if len(got) != len(want) {
				t.Fatalf("the backend was handed %v, want %v — an empty text block contributed to the prompt", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("the backend was handed %v, want %v", got, want)
				}
			}
		})
	}

	// Text that is not empty is text, trailing newlines and all: only the block
	// that carries nothing is skipped.
	got := round42Convert(t, `{"model":"m","messages":[{"role":"user","content":"go"}],"system":[{"type":"text","text":"alpha\n\n"}]}`)
	if len(got) != 2 || got[0] != "system=alpha\n\n" {
		t.Errorf("the backend was handed %v, want system=alpha\\n\\n preserved", got)
	}
}

// TestAToolWithoutAnInputSchemaCarriesAParametersObject is B-F3's sibling on
// this leg: the zero parameters object is what the gateway now mirrors, and a
// schema of the wrong kind is refused rather than unmarshalled into it.
func TestAToolWithoutAnInputSchemaCarriesAParametersObject(t *testing.T) {
	var req MessagesRequest
	body := `{"model":"m","messages":[{"role":"user","content":"go"}],"tools":[{"name":"Read"}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("conversion refused a tool that states no schema: %v", err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("the backend was handed %d tool(s), want 1", len(out.Tools))
	}
	encoded, err := json.Marshal(out.Tools[0].Function.Parameters)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != `{"type":"","properties":null}` {
		t.Errorf("the parameters are %s, want the zero object {\"type\":\"\",\"properties\":null} the gateway mirrors", encoded)
	}

	for _, bad := range []string{`"nope"`, `[1,2]`} {
		var badReq MessagesRequest
		if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"go"}],"tools":[{"name":"Read","input_schema":`+bad+`}]}`), &badReq); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, err := FromMessagesRequest(badReq); err == nil {
			t.Errorf("input_schema %s was accepted; the gateway refuses it and both legs must answer one body the same way", bad)
		}
	}
}

// TestTheBuiltinsNameCannotBeRedefinedOnThisLeg is B-F3: the built-in's schema
// comes from its TYPE, and a client tool that claims the name the built-in owns
// is dropped, so the backend is never handed two tools with one name. The
// gateway's round-49 fix mirrors this pre-scan.
func TestTheBuiltinsNameCannotBeRedefinedOnThisLeg(t *testing.T) {
	var req MessagesRequest
	body := `{"model":"m","messages":[{"role":"user","content":"go"}],"tools":[` +
		`{"type":"web_search_20250305","name":"web_search"},` +
		`{"name":"web_search","input_schema":{"type":"object"}}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("conversion refused the body: %v", err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("the backend was handed %d tool(s), want 1 — the built-in's name cannot be redefined", len(out.Tools))
	}
	params := out.Tools[0].Function.Parameters
	if params.Type != "object" || len(params.Required) != 1 || params.Required[0] != "query" {
		t.Errorf("the surviving tool is not the built-in's: %+v", params)
	}
}
