package server

// round75_interleaved_turn_end_to_end_integrity_test.go — one upstream turn,
// one block list, whichever arm the client asked for (2026-09-28 audit, round
// 75, F75-L1-1).
//
// This is the whole path with a REAL producer: the qwen3-coder builtin parser,
// fed the model's output one byte at a time — the granularity both of this
// leg's runners deliver (llama-server emits one token per SSE event and the MLX
// pipeline one response per token) — producing a call in its own step between
// two runs of text. The streaming arm of the chat handler relays those steps
// and the Anthropic streaming converter writes text, call, text. The buffered
// arm used to write ONE text block with the call after it, prose the model
// wrote after the call included in the block before it, so `stream:false` and
// `stream:true` answered one body with two different block lists.

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/model/parsers"
)

// r75Producer is one model output: prose, one call, more prose.
const r75Producer = "Let me check the tree.\n" +
	"<tool_call>\n<function=Bash>\n<parameter=cmd>\nls\n</parameter>\n</function>\n</tool_call>\n" +
	"Now I wait for the result."

// r75ParserChunks feeds r75Producer to the real builtin parser in pieces of the
// given size and returns the chunks the run loop puts on the channel, with the
// ids the builtin parser lane assigns.
func r75ParserChunks(t *testing.T, size int) []api.ChatResponse {
	t.Helper()
	p := parsers.ParserForName("qwen3-coder")
	if p == nil {
		t.Fatal("no qwen3-coder parser")
	}
	var out []api.ChatResponse
	emit := func(content, thinking string, calls []api.ToolCall) {
		if content == "" && thinking == "" && len(calls) == 0 {
			return
		}
		for i := range calls {
			calls[i].ID = toolCallId()
		}
		out = append(out, api.ChatResponse{Message: api.Message{Content: content, Thinking: thinking, ToolCalls: calls}})
	}
	for i := 0; i < len(r75Producer); i += size {
		j := i + size
		if j > len(r75Producer) {
			j = len(r75Producer)
		}
		content, thinking, calls, err := p.Add(r75Producer[i:j], false)
		if err != nil {
			t.Fatalf("parser step %d: %v", i, err)
		}
		emit(content, thinking, calls)
	}
	content, thinking, calls, err := p.Add("", true)
	if err != nil {
		t.Fatalf("parser done: %v", err)
	}
	emit(content, thinking, calls)
	return out
}

func r75StreamedTypes(t *testing.T, chunks []api.ChatResponse) []string {
	t.Helper()
	conv := anthropic.NewStreamConverter("msg_r75", "m", 0)
	var out []string
	for _, chunk := range chunks {
		for _, ev := range conv.Process(chunk) {
			if b, ok := ev.Data.(anthropic.ContentBlockStartEvent); ok {
				out = append(out, b.ContentBlock.Type)
			}
		}
	}
	return out
}

// TestAnInterleavedTurnAnswersOneBlockListOnBothArms is the cross-arm pin.
func TestAnInterleavedTurnAnswersOneBlockListOnBothArms(t *testing.T) {
	chunks := r75ParserChunks(t, 1)
	streamed := r75StreamedTypes(t, chunks)
	if len(streamed) != 3 {
		t.Fatalf("this probe needs the parser to release the call between two runs of text; the streaming arm wrote %v", streamed)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("anthropic_messages", true)
	ch := make(chan any, len(chunks))
	for _, chunk := range chunks {
		ch <- chunk
	}
	close(ch)
	streaming := false
	writeChatResponse(c, api.ChatRequest{Stream: &streaming}, ch)

	var resp api.ChatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("the buffered arm wrote no response: %v\n%s", err, w.Body.String())
	}
	msg := anthropic.ToMessagesResponse("msg_r75", resp)
	var buffered []string
	for _, b := range msg.Content {
		buffered = append(buffered, b.Type)
	}
	if len(buffered) != len(streamed) {
		t.Fatalf("one upstream turn: the buffered arm wrote %v and the streaming arm %v — a client that asked for no stream is told the prose all preceded a call that came before some of it (2026-09-28 audit, round 75, F75-L1-1)\nbuffered content: %q",
			buffered, streamed, resp.Message.Content)
	}
	for i := range streamed {
		if buffered[i] != streamed[i] {
			t.Fatalf("one upstream turn: block %d is %q buffered and %q streamed (2026-09-28 audit, round 75, F75-L1-1)", i, buffered[i], streamed[i])
		}
	}
}

// TestTheSameTurnWholeOutputIsUnchanged: fed in one piece — the shape where the
// wire itself stated no interleaving — both arms still answer text then call.
func TestTheSameTurnWholeOutputIsUnchanged(t *testing.T) {
	chunks := r75ParserChunks(t, len(r75Producer))
	if len(chunks) == 0 {
		t.Fatal("the whole-output feed produced no chunk")
	}
	streamed := r75StreamedTypes(t, chunks)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("anthropic_messages", true)
	ch := make(chan any, len(chunks))
	for _, chunk := range chunks {
		ch <- chunk
	}
	close(ch)
	streaming := false
	writeChatResponse(c, api.ChatRequest{Stream: &streaming}, ch)
	var resp api.ChatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("the buffered arm wrote no response: %v\n%s", err, w.Body.String())
	}
	msg := anthropic.ToMessagesResponse("msg_r75", resp)
	var buffered []string
	for _, b := range msg.Content {
		buffered = append(buffered, b.Type)
	}
	if len(buffered) != len(streamed) {
		t.Fatalf("the two arms answered %v and %v", buffered, streamed)
	}
}
