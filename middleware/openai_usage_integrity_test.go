package middleware

// openai_usage_integrity_test.go — usage belongs on the final chunk only
// (2026-09-26 audit, fourth round). The writer emitted
// `"usage":{"prompt_tokens":0,"completion_tokens":0}` on EVERY intermediate
// chunk when the client asked for include_usage. Clients that read usage off
// each chunk — and OpenAI's own documented shape is that intermediate chunks
// carry `"usage": null` — record a zero for the turn and under-count the
// conversation.

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/openai"
)

func TestUsageAppearsOnlyOnTheFinalChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	w := &CompleteWriter{
		stream: true, streamOptions: &openai.StreamOptions{IncludeUsage: true},
		id: "cmpl-1", BaseWriter: BaseWriter{ResponseWriter: ctx.Writer},
	}

	mid, _ := json.Marshal(api.GenerateResponse{Model: "m", Response: "hello"})
	if _, err := w.Write(mid); err != nil {
		t.Fatal(err)
	}
	last, _ := json.Marshal(api.GenerateResponse{Model: "m", Done: true, DoneReason: "stop",
		Metrics: api.Metrics{PromptEvalCount: 7, EvalCount: 3}})
	if _, err := w.Write(last); err != nil {
		t.Fatal(err)
	}

	// frames includes the terminating `data: [DONE]` marker; only the JSON
	// chunks carry a usage field.
	var chunks []string
	for _, f := range sseDataFrames(rec.Body.String()) {
		if strings.TrimSpace(f) == "[DONE]" {
			continue
		}
		chunks = append(chunks, f)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 JSON chunks, got %d: %s", len(chunks), rec.Body.String())
	}
	var sawFinalUsage bool
	for i, f := range chunks {
		var m map[string]any
		if err := json.Unmarshal([]byte(f), &m); err != nil {
			t.Fatalf("chunk %d is not JSON: %v\n%s", i, err, f)
		}
		usage, has := m["usage"]
		isFinal := i == len(chunks)-1
		if !isFinal {
			if has && usage != nil {
				t.Errorf("chunk %d of %d carries usage %v — an intermediate chunk must report usage:null (or omit it), or a client records a zero for a turn it will be billed for", i+1, len(chunks), usage)
			}
			continue
		}
		u, ok := usage.(map[string]any)
		if !ok {
			t.Fatalf("the final chunk carries no usage object: %s", f)
		}
		if u["prompt_tokens"] != float64(7) || u["completion_tokens"] != float64(3) {
			t.Errorf("final usage = %v, want prompt 7 / completion 3", u)
		}
		sawFinalUsage = true
	}
	if !sawFinalUsage {
		t.Error("no final chunk with usage was emitted at all")
	}
}

// TestTimingsRideTheUsageTrailerOnly pins upstream 16b4376ae at the fork's
// writer: an OpenAI-API client reads timings beside the usage trailer, since
// that is the only chunk the fork's writer emits with usage at all — and an
// intermediate chunk that carried timings would report the run's performance
// before the run finished.
func TestTimingsRideTheUsageTrailerOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	w := &ChatWriter{
		stream: true, streamOptions: &openai.StreamOptions{IncludeUsage: true},
		id: "chatcmpl-1", BaseWriter: BaseWriter{ResponseWriter: ctx.Writer},
	}

	mid, _ := json.Marshal(api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: "hi"}})
	if _, err := w.Write(mid); err != nil {
		t.Fatal(err)
	}
	last, _ := json.Marshal(api.ChatResponse{
		Model: "m", Done: true, DoneReason: "stop",
		Metrics: api.Metrics{
			PromptEvalCount:    10,
			PromptEvalDuration: 20 * time.Millisecond,
			EvalCount:          4,
			EvalDuration:       8 * time.Millisecond,
		},
	})
	if _, err := w.Write(last); err != nil {
		t.Fatal(err)
	}

	var chunks []string
	for _, f := range sseDataFrames(rec.Body.String()) {
		if strings.TrimSpace(f) != "[DONE]" {
			chunks = append(chunks, f)
		}
	}
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 JSON chunks, got %d: %s", len(chunks), rec.Body.String())
	}
	for i, f := range chunks {
		var m map[string]any
		if err := json.Unmarshal([]byte(f), &m); err != nil {
			t.Fatalf("chunk %d is not JSON: %v\n%s", i, err, f)
		}
		timings, has := m["timings"]
		if i != len(chunks)-1 {
			if has {
				t.Errorf("intermediate chunk %d carries timings %v — the run has not finished, so the numbers describe a partial run", i+1, timings)
			}
			continue
		}
		tm, ok := timings.(map[string]any)
		if !ok {
			t.Fatalf("the usage trailer carries no timings object: %s", f)
		}
		if tm["prompt_n"] != float64(10) || tm["predicted_n"] != float64(4) {
			t.Errorf("timings = %v, want prompt_n 10 / predicted_n 4", tm)
		}
		if tm["predicted_per_second"] != float64(500) {
			t.Errorf("predicted_per_second = %v, want 500 (4 tokens over 8ms)", tm["predicted_per_second"])
		}
	}
}
