package tools

// round52_argument_order_test.go — round 52's finding on the tool-call parser.
//
// The arguments of a parsed call were set from the decoded map in Go's map
// iteration order, which is randomized: one model output rendered as many
// different argument JSONs across runs. The parsed call becomes the assistant
// turn of the conversation, and for a renderer-backed model that turn is part
// of the next prompt — whose bytes then differed run to run, so every prefix
// cache keyed on the rendered prompt was recomputed and the model was asked the
// same question in different bytes (2026-09-27 audit, round 52).

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/api"
)

// TestAParsedCallsArgumentsRenderInOneOrder runs the parser over one output
// repeatedly: whatever order it picks, it has to be the same order every time.
func TestAParsedCallsArgumentsRenderInOneOrder(t *testing.T) {
	toolset := []api.Tool{{Function: api.ToolFunction{Name: "get_weather"}}}
	const output = `{"name":"get_weather","arguments":{"zeta":1,"alpha":2,"mu":3,"beta":4}}`
	// Sorted, which is the order the template leg normalizes to: the decoded
	// map kept none of the order the model wrote, and what the rule needs is
	// that one output renders one prompt.
	const want = `{"alpha":2,"beta":4,"mu":3,"zeta":1}`

	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		p := NewParserWithTag(toolset, "{")
		calls, _ := p.Add(output)
		if len(calls) != 1 {
			t.Fatalf("run %d: parsed %d calls, want 1", i, len(calls))
		}
		got, err := json.Marshal(calls[0].Function.Arguments)
		if err != nil {
			t.Fatalf("run %d: marshal arguments: %v", i, err)
		}
		if string(got) != want {
			t.Fatalf("run %d: the call's arguments render as %s, want %s\nthe parsed call is the assistant turn of the next prompt, so a map's iteration order asks the model the same question in different bytes and every prefix cache keyed on them is recomputed",
				i, got, want)
		}
		seen[string(got)] = true
	}
	if len(seen) != 1 {
		t.Errorf("one output rendered %d different argument JSONs", len(seen))
	}
}
