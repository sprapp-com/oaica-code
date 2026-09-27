package renderers

// round51_schema_render_order_test.go — round 51's finding on the renderer.
//
// D1: the extra keys of a tool schema were rendered straight out of a
// map[string]any, and Go randomizes map iteration. The same tool schema
// therefore rendered two different prompts on two turns — a schema carrying
// `required` beside `additionalProperties` (the strict-mode shape), or a
// property carrying `enum` beside `default`, flipped a coin on their order. The
// tools block sits at the TOP of this prompt, so either order invalidates the
// whole cached prefix: llama.cpp and every provider that keys a cache on the
// rendered prompt recomputed the conversation on every turn, and the model was
// asked the same question in different bytes.
//
// The keys are rendered in the order the schema STATES them, which is the
// order a decoded schema marshals back (api.ToolFunctionParameters carries the
// client's bytes) (2026-09-27 audit, round 51).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

// round51RenderedSchema renders one schema as the model is shown it.
func round51RenderedSchema(t *testing.T, schema string) string {
	t.Helper()
	var params api.ToolFunctionParameters
	if err := json.Unmarshal([]byte(schema), &params); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	tools := []api.Tool{{
		Type:     "function",
		Function: api.ToolFunction{Name: "Read", Description: "read a file", Parameters: params},
	}}
	prompt, err := (&Qwen3CoderRenderer{}).Render([]api.Message{{Role: "user", Content: "go"}}, tools, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return prompt
}

// TestASchemaRendersTheSamePromptEveryTurn is D1: two keys on one schema are
// two orders out of a map, and only one of them can be the cached prefix.
func TestASchemaRendersTheSamePromptEveryTurn(t *testing.T) {
	// The client states required BEFORE additionalProperties, and enum before
	// default on the property: the strict-mode shape.
	const schema = `{"type":"object","properties":{"path":{"type":"string","description":"the path","enum":["a","b"],"default":"a"}},"required":["path"],"additionalProperties":false}`

	first := round51RenderedSchema(t, schema)
	for i := 0; i < 200; i++ {
		if got := round51RenderedSchema(t, schema); got != first {
			t.Fatalf("the same tool schema rendered two different prompts; run %d:\n%s\nfirst:\n%s\nthe tools block is the top of this prompt, so either order invalidates the whole cached prefix", i, got, first)
		}
	}

	// And the order is the client's, not sorted, not random: `required` is
	// stated before `additionalProperties` and rendered before it.
	required := strings.Index(first, "<required>")
	additional := strings.Index(first, "<additionalProperties>")
	if required < 0 || additional < 0 {
		t.Fatalf("the schema's own keys never reached the prompt:\n%s", first)
	}
	if required > additional {
		t.Errorf("the schema states `required` before `additionalProperties` and the model was shown them the other way:\n%s", first)
	}
	if enum, def := strings.Index(first, "<enum>"), strings.Index(first, "<default>"); enum < 0 || def < 0 || enum > def {
		t.Errorf("the property states `enum` before `default` and the model was shown them in another order:\n%s", first)
	}
}

// TestASchemaStatesItsKeysAsTheClientWroteThem is the same rule read as bytes:
// the rendered extras are in the schema's own order, whichever that is.
func TestASchemaStatesItsKeysAsTheClientWroteThem(t *testing.T) {
	const schema = `{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false,"required":["path"]}`
	prompt := round51RenderedSchema(t, schema)
	additional := strings.Index(prompt, "<additionalProperties>")
	required := strings.Index(prompt, "<required>")
	if additional < 0 || required < 0 {
		t.Fatalf("the schema's own keys never reached the prompt:\n%s", prompt)
	}
	if additional > required {
		t.Errorf("this schema states `additionalProperties` before `required` and the model was shown them the other way:\n%s", prompt)
	}
}

// TestASchemaTheModelIsShownIsTheSchemaTheCallerWrote keeps the round-50
// contract in view: nothing about this fix rewrites the keys themselves.
func TestASchemaTheModelIsShownIsTheSchemaTheCallerWrote(t *testing.T) {
	const schema = `{"type":"object","properties":{"path":{"type":"string","format":"uri"}},"required":["path"],"additionalProperties":false}`
	prompt := round51RenderedSchema(t, schema)
	for _, want := range []string{"<additionalProperties>false</additionalProperties>", `<required>["path"]</required>`, `<format>uri</format>`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt is missing %s:\n%s", want, prompt)
		}
	}
}
