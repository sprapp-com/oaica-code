package middleware

// round41_local_leg_url_image_integrity_test.go — C41-13. This leg's backend
// takes image BYTES. A source declared as a url travelled through
// api.Message.Images as its own characters, llm.NewMediaData sniffed those
// characters as text/plain, forced them to image/jpeg and base64'd them: the
// model was shown a picture of the address while the client had pointed at a
// screenshot it never sent, and the turn was answered 200. The client leg and
// the gateway both hand a url source to their backend as a URL — this leg has
// no fetcher, so it refuses in words.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// round41LocalLeg posts one body through the middleware with a handler that
// records whether it was reached, and returns the client's status.
func round41LocalLeg(t *testing.T, body string) (int, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	reached := false
	router := gin.New()
	router.Use(AnthropicMessagesMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		reached = true
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[],"model":"test-model","usage":{"input_tokens":1,"output_tokens":1}}`))
	})

	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp.Code, reached
}

// TestAURLSourcedImageIsRefusedOnTheByteBackedLeg is C41-13. The refusal is the
// verdict the converter gives any source this wire cannot express, and it is
// the difference between a client learning its local model never saw the
// picture and a client believing it did.
func TestAURLSourcedImageIsRefusedOnTheByteBackedLeg(t *testing.T) {
	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"url","url":"https://e.test/shot.png"}}]}]}`
	status, reached := round41LocalLeg(t, body)
	if status != http.StatusBadRequest {
		t.Errorf("a url-sourced image was answered with %d (handler reached: %v): this leg's backend takes bytes, so the address text would be sniffed as text/plain, forced to image/jpeg and base64'd — the model is shown a picture of the URL while the client pointed at a screenshot it never sent", status, reached)
	}
	if reached {
		t.Errorf("the request reached the backend handler despite a url source this leg cannot carry")
	}
}

// TestAByteBackedImageStillReachesTheBackend is the other side of the same
// rule: the refusal must name one source type, not images as a class.
func TestAByteBackedImageStillReachesTheBackend(t *testing.T) {
	// A one-pixel PNG, carried the way this leg can carry it.
	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="}}]}]}`
	status, reached := round41LocalLeg(t, body)
	if status == http.StatusBadRequest {
		t.Errorf("a base64-sourced image was refused with 400: only the url source is unrepresentable on this leg")
	}
	if !reached {
		t.Errorf("a base64-sourced image did not reach the backend handler (status %d)", status)
	}
}
