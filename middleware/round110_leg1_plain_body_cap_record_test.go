package middleware

// round110_leg1_plain_body_cap_record_test.go — leg 1, round 110 (2026-09-29
// audit), F110-L1-1. RECORDED, not changed.
//
// The 20 MiB maxDecompressedBodySize cap bounds a zstd-encoded request body on the
// JSON doors and is applied nowhere to a plain one: the identical /v1/responses
// body is refused as zstd and accepted as plain JSON, and plain /v1/chat/completions
// and /v1/messages bodies of the same size are accepted. So the size limit is
// bypassed by not compressing, and any client that can reach the port can make the
// server buffer and decode an arbitrarily large plain body.
//
// Recorded rather than fixed because the fix needs a NUMBER, and the number is an
// operator policy that a test cannot choose. Capping plain bodies at the zstd
// limit would refuse multi-image vision requests (base64 inflates by a third, and
// several full-size screenshots pass 20 MiB) that work today, on every door and for
// every client, in a process whose default bind is loopback. What a later round
// should know: the change is one MaxBytesReader at the start of the four JSON doors
// (or one shared pre-middleware) with the limit taken from an environment setting
// whose default is decided on purpose; the pin below states today's reading and goes
// red when a cap is added, so it is met by a decision and not by accident.

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func r110door(path string, mw gin.HandlerFunc, body []byte) int {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST(path, mw, func(c *gin.Context) { c.Status(200) })
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestMine110APlainBodyPastTheZstdCapIsAccepted(t *testing.T) {
	pad := strings.Repeat("a", 21<<20)
	chat := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"x":"` + pad + `"}`)
	msgs := []byte(`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}],"x":"` + pad + `"}`)
	resp := []byte(`{"model":"m","input":"hi","metadata":{"pad":"` + pad + `"}}`)
	for name, code := range map[string]int{
		"chat":      r110door("/v1/chat/completions", ChatMiddleware(), chat),
		"messages":  r110door("/v1/messages", AnthropicMessagesMiddleware(), msgs),
		"responses": r110door("/v1/responses", ResponsesMiddleware(), resp),
	} {
		if code != 200 {
			t.Errorf("%s: a %d MiB plain body answered %d, this record states it is accepted — if a cap now refuses it the decision has been made and this record is spent (2026-09-29 audit, round 110, F110-L1-1)", name, len(pad)>>20, code)
		}
	}
}
