package middleware

// round111_leg1_usage_counts_record_test.go — leg 1, round 111 (2026-09-29 audit),
// F111-L1-2. RECORDED, not changed.
//
// The response converters relay the runner's token counts as they are: a runner that
// reports a negative count reaches the chat body as a negative prompt/completion count
// and a negative total, and two counts near 2^62 make the total wrap to the most
// negative int64; /v1/messages substitutes an estimate for a non-positive INPUT count
// only and relays a negative output count unchanged. Recorded rather than fixed, at
// rank c: the only producer is a runner or a relayed peer reporting nonsense, and
// none in this tree does. A fix is one shared clamp-and-saturate helper used by every
// usage builder (chat, completions, responses, anthropic); the pin states today's
// reading for the chat door and goes red when the helper lands.

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ollama/ollama/api"
)

func TestMine111TheChatDoorRelaysANegativeUsageCount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/chat/completions", ChatMiddleware(), func(c *gin.Context) {
		b, _ := json.Marshal(api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: "hi"},
			Done: true, DoneReason: "stop",
			Metrics: api.Metrics{PromptEvalCount: -5, EvalCount: -9}})
		c.Writer.Write(b)
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `"total_tokens":-14`) {
		t.Errorf("the chat door answered %q, this record states a negative total of -14 relayed as reported — if it is clamped the shared helper has landed and this record is spent (2026-09-29 audit, round 111, F111-L1-2)", w.Body.String())
	}
}
