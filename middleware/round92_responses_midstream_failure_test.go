package middleware

// round92_responses_midstream_failure_test.go — leg 1, F92-L1-1 (2026-09-29
// audit, round 92).
//
// The chat path states a mid-turn failure by writing an error frame INTO the 200
// ndjson stream it was already streaming (`server/routes.go`'s `streamResponse`,
// the already-written branch: `{"error": …, "status": …}`). Round 91 taught the
// two OpenAI writers to read that frame (F91-L1-1) and round 86 taught the
// Anthropic one (F86-L1-1). The Responses writer still had no reading of it at
// all: `api.ChatResponse` has no `error` field, so the frame unmarshalled to a
// ZERO chunk, the converter was handed a chunk that said nothing, and the client
// got its partial text, then nothing — no `response.failed`, no `response.
// completed`, HTTP 200. Measured on the streamed arm before this change: 200 and
// the runner's sentence nowhere in the body, while the same body on the buffered
// arm answered the failure. A Responses client that waits for the terminal event
// of the turn waits forever, and one that reads what it has reads a truncated
// answer as the whole one.
//
// The buffered arm needs no change here — an unwritten writer is answered by the
// chat lane's `c.JSON(status, …)`, which reaches `writeError` — so these tests
// pin the streamed arm stating the cause, and the two arms naming it the same
// way.

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

const r92ResponsesSentence = "upstream stream ended before the response was complete"

// r92ResponsesEvents returns the (event, data) pairs of an SSE body, in order.
func r92ResponsesEvents(t *testing.T, body string) [][2]string {
	t.Helper()
	var out [][2]string
	var event string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(name)
			continue
		}
		if payload, ok := strings.CutPrefix(line, "data:"); ok {
			out = append(out, [2]string{event, strings.TrimSpace(payload)})
			event = ""
		}
	}
	return out
}

// r92ResponsesWriter is the writer a streamed or buffered /v1/responses request
// is answered by, over a recorder.
func r92ResponsesWriter(rec *httptest.ResponseRecorder, stream bool) (*ResponsesWriter, *gin.Context) {
	ctx, _ := gin.CreateTestContext(rec)
	return &ResponsesWriter{
		BaseWriter: BaseWriter{ResponseWriter: ctx.Writer},
		converter:  openai.NewResponsesStreamConverter("resp_1", "msg_1", "m", openai.ResponsesRequest{}),
		model:      "m",
		stream:     stream,
		responseID: "resp_1",
		itemID:     "msg_1",
	}, ctx
}

// The failure the producer states, as the frame it is written in.
func r92ResponsesFrame() []byte {
	d, _ := json.Marshal(gin.H{"error": r92ResponsesSentence, "status": http.StatusBadGateway})
	return d
}

func TestTheResponsesStreamStatesTheRunnersFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	w, ctx := r92ResponsesWriter(rec, true)

	// Content first, so the writer has put its 200 and its event-stream header
	// on the wire, then the failure the runner stated.
	mid, _ := json.Marshal(api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: "the model wrote this much"}})
	if _, err := w.Write(mid); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(r92ResponsesFrame()); err != nil {
		t.Fatal(err)
	}
	streamed := rec.Body.String()
	if ctx.Writer.Status() != http.StatusOK {
		t.Fatalf("premise: the streamed arm answered %d instead of leaving the already-written 200 up", ctx.Writer.Status())
	}
	if !strings.Contains(streamed, "the model wrote this much") {
		t.Fatalf("premise: the partial answer never reached the client: %s", streamed)
	}

	// The terminal event of a turn that did not finish is `response.failed`.
	events := r92ResponsesEvents(t, streamed)
	var failed string
	for _, e := range events {
		if e[0] == "response.failed" {
			failed = e[1]
		}
		if e[0] == "response.completed" {
			t.Errorf("the streamed arm closed a turn that never finished with response.completed: %s", e[1])
		}
	}
	if failed == "" {
		t.Fatalf("the streamed client of /v1/responses was told NOTHING about the runner's failure (2026-09-29 audit, round 92, F92-L1-1):\n%s", streamed)
	}

	var envelope struct {
		Response struct {
			Status string `json:"status"`
			Error  *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(failed), &envelope); err != nil {
		t.Fatalf("the terminal event is not a response object: %v (%s)", err, failed)
	}
	if envelope.Response.Status != "failed" {
		t.Errorf("the terminal event states status %q, not failed: %s", envelope.Response.Status, failed)
	}
	if envelope.Response.Error == nil || envelope.Response.Error.Message != r92ResponsesSentence {
		t.Errorf("the terminal event does not carry the producer's own sentence: %s", failed)
	}

	// And the frames after the failure are nothing: the turn is over and the
	// client has been told why.
	before := len(r92ResponsesEvents(t, streamed))
	if _, err := w.Write(mid); err != nil {
		t.Fatal(err)
	}
	if after := len(r92ResponsesEvents(t, rec.Body.String())); after != before {
		t.Errorf("an event written after the failure was relayed anyway (%d events before, %d after)", before, after)
	}

	// The buffered arm names the same cause, in the envelope this wire uses for
	// a failure it can still put a status on.
	rec2 := httptest.NewRecorder()
	w2, ctx2 := r92ResponsesWriter(rec2, false)
	ctx2.Writer.WriteHeader(http.StatusBadGateway)
	buffered, _ := json.Marshal(gin.H{"error": r92ResponsesSentence})
	if _, err := w2.Write(buffered); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec2.Body.String(), r92ResponsesSentence) {
		t.Fatalf("the buffered client of /v1/responses was handed %q, which states no sentence", rec2.Body.String())
	}
	var bufferedEnv struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &bufferedEnv); err != nil {
		t.Fatalf("the buffered arm states no envelope: %v (%s)", err, rec2.Body.String())
	}
	if bufferedEnv.Error.Message != envelope.Response.Error.Message {
		t.Errorf("one upstream body answered two ways:\n  streamed %q\n  buffered %q",
			envelope.Response.Error.Message, bufferedEnv.Error.Message)
	}
	// The name of the cause is the same string on both arms: the buffered arm's
	// `error.type`, which is what a client of this wire switches on, and the
	// streaming arm's `error.code`.
	if bufferedEnv.Error.Type != envelope.Response.Error.Code {
		t.Errorf("one upstream body named the cause two ways:\n  streamed code %q\n  buffered type %q",
			envelope.Response.Error.Code, bufferedEnv.Error.Type)
	}
}

// A failed writer says nothing more, whichever arm it is on: the buffered arm
// already put its status and envelope out.
func TestAFailedResponsesWriterRelaysNothingFurther(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	w, _ := r92ResponsesWriter(rec, true)
	if _, err := w.Write(r92ResponsesFrame()); err != nil {
		t.Fatal(err)
	}
	first := rec.Body.String()
	if !strings.Contains(first, "response.failed") {
		t.Fatalf("premise: the failure was not stated: %s", first)
	}
	mid, _ := json.Marshal(api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: "text for a turn that is over"}})
	if _, err := w.Write(mid); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != first {
		t.Errorf("text arrived after the failure:\n  before %s\n  after  %s", first, rec.Body.String())
	}
}
