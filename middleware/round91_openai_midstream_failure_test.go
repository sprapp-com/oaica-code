package middleware

// round91_openai_midstream_failure_test.go — leg 1, F91-L1-1 (2026-09-29 audit,
// round 91).
//
// The chat path states a mid-turn failure by writing an error frame INTO the
// 200 ndjson stream it was already streaming (`server/routes.go`'s
// `streamResponse`, the already-written branch: `{"error": …, "status": …}`).
// The Anthropic writer has read that frame since round 86 (`upstreamErrorFrame`
// → `failTurn`), so `/v1/messages` answers the runner's own sentence. The two
// OpenAI writers did not read it at all: `api.ChatResponse` has no `error`
// field, so the frame unmarshalled to a ZERO chunk and was relayed as an empty
// delta — 200, no sentence, no `[DONE]` — while the same body on the buffered
// arm answered 500 with the sentence. A client of `/v1/chat/completions` or
// `/v1/completions` that asked for a stream was therefore told nothing at all
// about a turn its model never finished, and had nothing to retry on.
//
// The buffered arm needs no change (an unwritten writer is answered by the chat
// lane's `c.JSON(status, …)`, which reaches `writeError`), so these tests pin the
// two arms agreeing on ONE sentence and ONE envelope, differing only in the
// transport the client asked for.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/openai"
)

const r91Sentence = "upstream stream ended before the response was complete"

// r91ErrorFrame is the frame the chat path writes into an already-streaming 200.
func r91ErrorFrame() []byte {
	d, _ := json.Marshal(gin.H{"error": r91Sentence, "status": http.StatusInternalServerError})
	return d
}

// r91BufferedFrame is what the SAME failure reaches an unwritten writer as: the
// chat lane answers it with `c.JSON(status, gin.H{"error": …})`, so the body
// carries the sentence alone and the status rides the header.
func r91BufferedFrame() []byte {
	d, _ := json.Marshal(gin.H{"error": r91Sentence})
	return d
}

// r91Frames returns the `data:` payloads of an SSE body, in order.
func r91Frames(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			out = append(out, strings.TrimSpace(payload))
		}
	}
	return out
}

// r91ErrorOf reads the error envelope out of either medium.
func r91ErrorOf(t *testing.T, body string) openai.ErrorResponse {
	t.Helper()
	candidate := strings.TrimSpace(body)
	if frames := r91Frames(body); len(frames) > 0 {
		candidate = frames[len(frames)-1]
	}
	var env openai.ErrorResponse
	if err := json.Unmarshal([]byte(candidate), &env); err != nil {
		t.Fatalf("the client was handed %q, which carries no error envelope: %v", body, err)
	}
	if env.Error.Message == "" {
		t.Fatalf("the client was handed %q, which states no sentence", body)
	}
	return env
}

func TestTheOpenAIChatStreamStatesTheRunnersFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// The streamed arm: content first (so the writer has put its 200 and its
	// event-stream header on the wire), then the runner's failure.
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	w := &ChatWriter{stream: true, id: "chatcmpl-1", BaseWriter: BaseWriter{ResponseWriter: ctx.Writer}}
	mid, _ := json.Marshal(api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: "the model wrote this much"}})
	if _, err := w.Write(mid); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(r91ErrorFrame()); err != nil {
		t.Fatal(err)
	}
	streamed := rec.Body.String()
	if ctx.Writer.Status() != http.StatusOK {
		t.Fatalf("premise: the streamed arm answered %d", ctx.Writer.Status())
	}
	if strings.Contains(streamed, "[DONE]") {
		t.Errorf("the streamed arm wrote the [DONE] sentinel for a turn that never finished: %s", streamed)
	}
	if !strings.Contains(streamed, r91Sentence) {
		t.Fatalf("the streamed client of /v1/chat/completions was told NOTHING about the runner's failure (2026-09-29 audit, round 91, F91-L1-1):\n%s", streamed)
	}
	streamedErr := r91ErrorOf(t, streamed)

	// The frames after the failure are nothing: the turn is over and the client
	// has been told why (the Anthropic writer's `failed` door, round 72).
	before := len(r91Frames(streamed))
	if _, err := w.Write(mid); err != nil {
		t.Fatal(err)
	}
	if after := len(r91Frames(streamed)); after != before {
		t.Errorf("a frame written after the failure was relayed anyway (%d frames before, %d after): %s", before, after, streamed)
	}

	// The buffered arm answers the same body with the same envelope; only the
	// transport differs.
	rec2 := httptest.NewRecorder()
	ctx2, _ := gin.CreateTestContext(rec2)
	ctx2.Writer.WriteHeader(http.StatusInternalServerError)
	w2 := &ChatWriter{stream: false, id: "chatcmpl-1", BaseWriter: BaseWriter{ResponseWriter: ctx2.Writer}}
	if _, err := w2.Write(r91BufferedFrame()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec2.Body.String(), r91Sentence) {
		t.Fatalf("the buffered client of /v1/chat/completions was handed %q, which states no sentence", rec2.Body.String())
	}
	bufferedErr := r91ErrorOf(t, rec2.Body.String())
	if bufferedErr != streamedErr {
		t.Errorf("one upstream body answered two ways:\n  streamed %+v\n  buffered %+v", streamedErr, bufferedErr)
	}
}

func TestTheOpenAICompletionStreamStatesTheRunnersFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	w := &CompleteWriter{stream: true, id: "cmpl-1", BaseWriter: BaseWriter{ResponseWriter: ctx.Writer}}
	mid, _ := json.Marshal(api.GenerateResponse{Model: "m", Response: "the model wrote this much"})
	if _, err := w.Write(mid); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(r91ErrorFrame()); err != nil {
		t.Fatal(err)
	}
	streamed := rec.Body.String()
	if strings.Contains(streamed, "[DONE]") {
		t.Errorf("the completion stream wrote the [DONE] sentinel for a turn that never finished: %s", streamed)
	}
	if !strings.Contains(streamed, r91Sentence) {
		t.Fatalf("the streamed client of /v1/completions was told NOTHING about the runner's failure (2026-09-29 audit, round 91, F91-L1-1):\n%s", streamed)
	}
	streamedErr := r91ErrorOf(t, streamed)

	rec2 := httptest.NewRecorder()
	ctx2, _ := gin.CreateTestContext(rec2)
	ctx2.Writer.WriteHeader(http.StatusInternalServerError)
	w2 := &CompleteWriter{stream: false, id: "cmpl-1", BaseWriter: BaseWriter{ResponseWriter: ctx2.Writer}}
	if _, err := w2.Write(r91BufferedFrame()); err != nil {
		t.Fatal(err)
	}
	if bufferedErr := r91ErrorOf(t, rec2.Body.String()); bufferedErr != streamedErr {
		t.Errorf("one upstream body answered two ways:\n  streamed %+v\n  buffered %+v", streamedErr, bufferedErr)
	}
}
