package server

// round75_tool_parser_chunk_carry_integrity_test.go — the tools.Parser lane of
// /api/chat read the parser's two products as ALTERNATIVES (2026-09-28 audit,
// round 75, F75-L1-2).
//
// `toolCalls, content := toolParser.Add(chunk)` then `if len(content) > 0 { … }
// else if len(toolCalls) > 0 { … }` kept the prose and dropped the call whenever
// one step released both — the model's output `Sure, let me look that up.
// <tool_call>…</tool_call>` reaching the client as text with stop_reason
// end_turn and no tool_use at all, so an agent client ends the turn and never
// runs the tool. The built-in parser lane of the same handler has always
// written both.
//
// The probe is the REAL tools.Parser, fed the whole output in one step so both
// products co-occur, and the REAL branch helper — the shape a runner that
// batches its callbacks produces.

import (
	"strings"
	"testing"
	"text/template"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/tools"
)

func r75ParserTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl, err := template.New("r75").Parse(`{{if .ToolCalls}}<tool_call>{{range .ToolCalls}}{"name": "{{.Function.Name}}", "arguments": {{.Function.Arguments}}}{{end}}</tool_call>{{end}}`)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	return tmpl
}

func r75ParserTools() []api.Tool {
	return []api.Tool{{
		Type: "function",
		Function: api.ToolFunction{
			Name:       "get_temperature",
			Parameters: api.ToolFunctionParameters{Type: "object"},
		},
	}}
}

const r75ProseAndCall = "Sure, let me look that up.\n" +
	"<tool_call>{\"name\": \"get_temperature\", \"arguments\": {\"city\": \"Tokyo\"}}</tool_call>"

func TestACallReturnedBesideProseIsStillACall(t *testing.T) {
	p := tools.NewParser(r75ParserTemplate(t), r75ParserTools())
	calls, content := p.Add(r75ProseAndCall)
	if len(calls) == 0 || strings.TrimSpace(content) == "" {
		t.Fatalf("this probe needs one parser step releasing prose AND a call; got calls=%v content=%q", calls, content)
	}

	var res api.ChatResponse
	if !carryToolParserChunk(&res, content, calls) {
		t.Fatalf("the branch reported an empty chunk for prose %q and %d call(s)", content, len(calls))
	}
	if strings.TrimSpace(res.Message.Content) == "" {
		t.Errorf("the prose the parser released is missing from the chunk; a turn that ran a tool still relays what the model said before it (2026-09-28 audit, round 75, F75-L1-2)")
	}
	if len(res.Message.ToolCalls) != 1 {
		t.Fatalf("the chunk carries %d tool call(s), want 1 — a call returned in the same step as prose is still a call, and dropping it answers the turn with stop_reason end_turn and no tool_use (2026-09-28 audit, round 75, F75-L1-2)", len(res.Message.ToolCalls))
	}
	if got := res.Message.ToolCalls[0].Function.Name; got != "get_temperature" {
		t.Errorf("the chunk carries a call named %q, want get_temperature", got)
	}
	if strings.TrimSpace(res.Message.ToolCalls[0].ID) == "" {
		t.Errorf("the call was written without an id; the server mints one for every call it emits (2026-09-28 audit, round 75, F75-L1-2)")
	}
}

func TestACallAloneIsUnchanged(t *testing.T) {
	p := tools.NewParser(r75ParserTemplate(t), r75ParserTools())
	calls, content := p.Add("<tool_call>{\"name\": \"get_temperature\", \"arguments\": {\"city\": \"Tokyo\"}}</tool_call>")
	var res api.ChatResponse
	if !carryToolParserChunk(&res, content, calls) {
		t.Fatal("a call alone is a chunk")
	}
	if res.Message.Content != "" {
		t.Errorf("a call-only chunk carries content %q, want empty", res.Message.Content)
	}
	if len(res.Message.ToolCalls) != 1 {
		t.Fatalf("a call-only chunk carries %d call(s), want 1", len(res.Message.ToolCalls))
	}
}

func TestProseAloneIsUnchanged(t *testing.T) {
	p := tools.NewParser(r75ParserTemplate(t), r75ParserTools())
	calls, content := p.Add("just prose, no tool call at all")
	var res api.ChatResponse
	if !carryToolParserChunk(&res, content, calls) {
		t.Fatal("released prose is a chunk")
	}
	if res.Message.Content != content {
		t.Errorf("the chunk carries %q, want the released prose %q", res.Message.Content, content)
	}
	if len(res.Message.ToolCalls) != 0 {
		t.Errorf("a prose-only chunk carries %d call(s), want none", len(res.Message.ToolCalls))
	}
}

func TestNeitherIsNotAChunk(t *testing.T) {
	var res api.ChatResponse
	if carryToolParserChunk(&res, "", nil) {
		t.Error("a step that released nothing is not a chunk; the caller falls through to the buffered path (2026-09-28 audit, round 75, F75-L1-2)")
	}
	if res.Message.Content != "" || len(res.Message.ToolCalls) != 0 {
		t.Errorf("an empty step wrote %q / %d call(s) onto the response", res.Message.Content, len(res.Message.ToolCalls))
	}
}
