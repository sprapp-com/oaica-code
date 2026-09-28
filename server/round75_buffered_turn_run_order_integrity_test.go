package server

// round75_buffered_turn_run_order_integrity_test.go — the buffered lane keeps
// where the text arrived relative to the turn's tool calls (2026-09-28 audit,
// round 75, F75-L1-1).
//
// The non-streaming arm of this handler merges the pieces the streaming arm
// relays one at a time, so a turn the model wrote as text, call, text reached a
// client that asked for no stream as ONE text string with the call after it —
// prose the model wrote after the call sitting in the block before it. The lane
// now records the run boundaries on the Anthropic surface, which is the only
// surface that reads them back; the native wire has one text field by
// definition and is left exactly as it was.

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
)

// r75BufferedTurn drives writeChatResponse with the given chunks on the
// Anthropic surface (or the native one) and returns the response it wrote.
func r75BufferedTurn(t *testing.T, anthropicSurface bool, chunks ...api.ChatResponse) api.ChatResponse {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	if anthropicSurface {
		c.Set("anthropic_messages", true)
	}
	ch := make(chan any, len(chunks))
	for _, chunk := range chunks {
		ch <- chunk
	}
	close(ch)
	streaming := false
	writeChatResponse(c, api.ChatRequest{Stream: &streaming}, ch)

	var resp api.ChatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("the buffered lane wrote no response: %v\n%s", err, w.Body.String())
	}
	return resp
}

func r75OneCall() api.ToolCall {
	return api.ToolCall{
		ID: "call_1",
		Function: api.ToolCallFunction{
			Name: "Bash",
			Arguments: func() api.ToolCallFunctionArguments {
				a := api.NewToolCallFunctionArguments()
				a.Set("cmd", "ls")
				return a
			}(),
		},
	}
}

// TestABufferedTurnKeepsItsRunBoundaries pins the runs a text/call/text turn
// hands to the Anthropic surface.
func TestABufferedTurnKeepsItsRunBoundaries(t *testing.T) {
	resp := r75BufferedTurn(t, true,
		api.ChatResponse{Message: api.Message{Content: "before"}},
		api.ChatResponse{Message: api.Message{ToolCalls: []api.ToolCall{r75OneCall()}}},
		api.ChatResponse{Message: api.Message{Content: "after"}},
	)
	if resp.Message.Content != "beforeafter" {
		t.Fatalf("the merged content is %q, want beforeafter", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("the buffered lane kept %d call(s), want 1", len(resp.Message.ToolCalls))
	}
	want := []string{"before", "after"}
	if len(resp.Message.ContentRuns) != len(want) {
		t.Fatalf("the turn's runs are %q, want %q — one run before each call and one after the last, or the order the model wrote is gone and a non-streaming client is told the prose all preceded the call (2026-09-28 audit, round 75, F75-L1-1)",
			resp.Message.ContentRuns, want)
	}
	for i := range want {
		if resp.Message.ContentRuns[i] != want[i] {
			t.Errorf("run %d is %q, want %q (2026-09-28 audit, round 75, F75-L1-1)", i, resp.Message.ContentRuns[i], want[i])
		}
	}
}

// TestTwoCallsInOneChunkGetTheirOwnRuns pins the alignment: the runs must end
// up one more than the calls, however the calls were packed.
func TestTwoCallsInOneChunkGetTheirOwnRuns(t *testing.T) {
	second := r75OneCall()
	second.ID = "call_2"
	resp := r75BufferedTurn(t, true,
		api.ChatResponse{Message: api.Message{Content: "before"}},
		api.ChatResponse{Message: api.Message{ToolCalls: []api.ToolCall{r75OneCall(), second}}},
	)
	if len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("the buffered lane kept %d call(s), want 2", len(resp.Message.ToolCalls))
	}
	if len(resp.Message.ContentRuns) != 3 {
		t.Fatalf("two calls in one chunk left %q — the runs must stay aligned with the calls, one per call plus one for the text after the last (2026-09-28 audit, round 75, F75-L1-1)", resp.Message.ContentRuns)
	}
	if resp.Message.ContentRuns[0] != "before" {
		t.Errorf("run 0 is %q, want before", resp.Message.ContentRuns[0])
	}
}

// TestTheNativeWireKeepsItsShape: no runs on the OpenAI-compatible wire.
func TestTheNativeWireKeepsItsShape(t *testing.T) {
	resp := r75BufferedTurn(t, false,
		api.ChatResponse{Message: api.Message{Content: "before"}},
		api.ChatResponse{Message: api.Message{ToolCalls: []api.ToolCall{r75OneCall()}}},
		api.ChatResponse{Message: api.Message{Content: "after"}},
	)
	if len(resp.Message.ContentRuns) != 0 {
		t.Errorf("the native wire carries runs %q; it has one text field by definition and only the Anthropic surface reads them (2026-09-28 audit, round 75, F75-L1-1)", resp.Message.ContentRuns)
	}
	if resp.Message.Content != "beforeafter" {
		t.Errorf("the native wire's merged content is %q, want beforeafter", resp.Message.Content)
	}
}

// TestATurnWithNoCallsHasNoRuns: the shape is about the calls, and a turn with
// none of them is the merged text it always was.
func TestATurnWithNoCallsHasNoRuns(t *testing.T) {
	resp := r75BufferedTurn(t, true,
		api.ChatResponse{Message: api.Message{Content: "just prose"}},
	)
	if len(resp.Message.ContentRuns) != 0 {
		t.Errorf("a call-free turn carries runs %q, want none (2026-09-28 audit, round 75, F75-L1-1)", resp.Message.ContentRuns)
	}
	if resp.Message.Content != "just prose" {
		t.Errorf("the turn's content is %q, want just prose", resp.Message.Content)
	}
}
