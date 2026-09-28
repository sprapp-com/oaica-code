package server

// round96_statementless_cause_test.go — leg 1, F96-L1-4, RECORDED (2026-09-29
// audit, round 96).
//
// One statementless refusal — the runner lane's own frame for a failure that
// states no message, `{"error":"","status":500}` — reads as two causes on the
// buffered arm, depending on the surface. Measured:
//
//	native chat ""            anthropic ""            (the frame's own message)
//	openai      "something went wrong, please see the ollama server logs for details"
//	responses   "something went wrong, please see the ollama server logs for details"
//
// where all four streaming arms state nothing, as the frame does. The two
// sentences come from api.StatusError.Error()'s `default:` branch, reached by
// the OpenAI and Responses writers, which read an api.StatusError back out of
// the written body — `middleware/openai.go` (`openai.NewError(w.Status(),
// serr.Error())`) — while the Anthropic writer reads only the `error` field and
// so stays empty.
//
// It is recorded, not fixed:
//
//   - the fallback is a DECISION, pinned on purpose by round 93
//     (round93_relayed_refusal_body_test.go: "The fallback for a peer that
//     stated no message is pinned too: its status line is all the cause there
//     is, and an empty string reaches the middleware's generic 'something went
//     wrong' sentence instead"). A later round does not revert a pinned
//     decision — see round 95's F95-L1-4, the same family on the relay lane.
//   - no live producer reaches this frame on the direct lane: a message-less
//     status needs llama-server to answer >= 400 with an EMPTY body and no
//     captured error line (llm/llama_server.go statusErrorMessage returns
//     TrimSpace(body), and lastErrMsg must be empty too). Neither the round-96
//     auditor nor this pin could exhibit one; this pin states the frame by
//     hand, as the runner lane's own pin does.
//
// The pin is the reading itself: if a later round decides the four surfaces must
// agree here, this test fails and the decision must be taken deliberately.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/middleware"
)

// r96StatementlessChunks is the turn: one prose chunk, then the statementless
// refusal the runner lane pushes for a failure with no message.
func r96StatementlessChunks() []any {
	return []any{
		r96RelayText("half "),
		gin.H{"error": "", "status": 500},
	}
}

// r96StatedCause is the cause a surface handed the client, in any of the four
// envelopes and on either arm.
func r96StatedCause(body string) string {
	if !strings.Contains(body, "\n") {
		var doc map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(body)), &doc) == nil {
			if msg, ok := doc["error"].(string); ok {
				return msg
			}
			if e, ok := doc["error"].(map[string]any); ok {
				return causeFromMap(e)
			}
		}
	}
	out := ""
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			line = strings.TrimPrefix(line, "data: ")
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if msg, ok := m["error"].(string); ok {
			out = msg
		}
		if e, ok := m["error"].(map[string]any); ok {
			out = causeFromMap(e)
		}
	}
	return out
}

// causeFromMap reads the cause out of an error object.
func causeFromMap(e map[string]any) string {
	if msg, ok := e["message"].(string); ok {
		return msg
	}
	if t, ok := e["type"].(string); ok {
		return t
	}
	return ""
}

// One statementless refusal, four surfaces, both arms — the readings this
// record stands on.
func TestAStatementlessFailureStatesItsCausePerSurface(t *testing.T) {
	// The sentence the two OpenAI writers fabricate, kept here verbatim so a
	// change to it is a change to this record.
	const fabricated = "something went wrong, please see the ollama server logs for details"

	for _, tc := range []struct {
		name string
		mw   gin.HandlerFunc
		want string
	}{
		{"native chat", nil, ""},
		{"openai chat", middleware.ChatMiddleware(), fabricated},
		{"responses", middleware.ResponsesMiddleware(), fabricated},
		{"anthropic", middleware.AnthropicMessagesMiddleware(), ""},
	} {
		for _, stream := range []bool{false, true} {
			srv := r96RelayRouter(t, r96RelayLane(r96StatementlessChunks()), tc.mw)
			_, body := r96RelayPost(t, srv, r96StatementlessBody(tc.mw, stream))
			want := tc.want
			if stream {
				// Every streaming arm states the frame's own message: nothing.
				want = ""
			}
			if got := r96StatedCause(body); got != want {
				t.Errorf("%s stream=%v states %q, want %q (2026-09-29 audit, round 96, F96-L1-4 — recorded, see this file's header):\n%s",
					tc.name, stream, got, want, body)
			}
			t.Logf("%-12s stream=%-5v states %q", tc.name, stream, r96StatedCause(body))
		}
	}
}

// r96StatementlessBody is the client body one surface accepts.
func r96StatementlessBody(mw gin.HandlerFunc, stream bool) string {
	if mw == nil {
		return `{"model":"m","stream":` + r96Lit(stream) + `,"messages":[{"role":"user","content":"hi"}]}`
	}
	return `{"model":"m","max_tokens":64,"stream":` + r96Lit(stream) + `,"messages":[{"role":"user","content":"hi"}]}`
}

// r96Lit is a JSON boolean.
func r96Lit(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
