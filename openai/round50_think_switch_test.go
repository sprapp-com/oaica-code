package openai

// round50_think_switch_test.go — round 50's finding on the OpenAI wire this
// fork reads: the `think` key.
//
// The Anthropic request states the thinking control twice over —
// `thinking:{type:"enabled"|"disabled"}` (a switch) and `output_config.effort`
// (a level) — and FromMessagesRequest decodes both into api.ChatRequest.Think.
// The client proxy leg rebuilds the request through ChatCompletionRequest, and
// that struct had no field for the value: the switch was dropped between the
// two, the daemon applied its own default, and a client that had asked for
// thinking OFF paid for reasoning it ruled out.
//
// The OpenAI spellings alone cannot carry that switch: `reasoning_effort` can
// say "none" or a level, but there is no spelling of the plain "yes, think"
// that `thinking:{type:"enabled"}` means. So the key is carried, and being the
// more specific spelling of the same field, it wins over the effort when both
// are stated.

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/api"
)

// TestTheThinkKeyIsDecodedOffTheWire pins the spelling: what a body says under
// `think` is the control, and its absence is no control at all.
func TestTheThinkKeyIsDecodedOffTheWire(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want any // nil = no control stated
	}{
		{"absent", `{}`, nil},
		{"true", `{"think":true}`, true},
		{"false", `{"think":false}`, false},
		{"level", `{"think":"high"}`, "high"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var req ChatCompletionRequest
			if err := json.Unmarshal([]byte(c.body), &req); err != nil {
				t.Fatalf("unmarshal %s: %v", c.body, err)
			}
			if c.want == nil {
				if req.Think != nil {
					t.Fatalf("the body states no thinking control but decoded %+v", req.Think)
				}
				return
			}
			if req.Think == nil {
				t.Fatalf("the body states think, and this struct dropped it — the switch never reaches the daemon")
			}
			if req.Think.Value != c.want {
				t.Errorf("decoded think=%v, want %v", req.Think.Value, c.want)
			}
		})
	}
}

// TestTheThinkKeyWinsOverTheEffort is the precedence. A request that states
// both is stating the same field twice, and the explicit spelling is the more
// specific one — a client that said "do not think" and also named a level
// meant the switch.
func TestTheThinkKeyWinsOverTheEffort(t *testing.T) {
	raw := func(s string) *string { return &s }
	think := func(v any) *api.ThinkValue { return &api.ThinkValue{Value: v} }

	for _, c := range []struct {
		name   string
		think  *api.ThinkValue
		effort *string
		want   any
	}{
		{"think alone true", think(true), nil, true},
		{"think alone false", think(false), nil, false},
		{"think alone level", think("high"), nil, "high"},
		{"effort alone", nil, raw("high"), "high"},
		{"effort none alone", nil, raw("none"), false},
		{"false beats effort", think(false), raw("high"), false},
		{"true beats effort none", think(true), raw("none"), true},
		{"level beats effort", think("low"), raw("high"), "low"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var req ChatCompletionRequest
			req.Model = "test-model"
			req.Messages = []Message{{Role: "user", Content: "hi"}}
			req.Think = c.think
			req.ReasoningEffort = c.effort

			out, err := FromChatRequest(req)
			if err != nil {
				t.Fatalf("FromChatRequest: %v", err)
			}
			if out.Think == nil {
				t.Fatalf("the request reached the daemon with no thinking control; want %v", c.want)
			}
			if out.Think.Value != c.want {
				t.Errorf("the daemon was handed think=%v, want %v", out.Think.Value, c.want)
			}
		})
	}
}

// TestAnUnstatedThinkLeavesTheEffortToSayIt keeps the two apart: adding the key
// changed nothing for a body that only ever used the OpenAI spellings.
func TestAnUnstatedThinkLeavesTheEffortToSayIt(t *testing.T) {
	effort := "medium"
	out, err := FromChatRequest(ChatCompletionRequest{
		Model:           "test-model",
		Messages:        []Message{{Role: "user", Content: "hi"}},
		ReasoningEffort: &effort,
	})
	if err != nil {
		t.Fatalf("FromChatRequest: %v", err)
	}
	if out.Think == nil || out.Think.Value != "medium" {
		t.Fatalf("the daemon was handed %+v, want the medium effort the body stated", out.Think)
	}
}
