package server

// round111_leg1_bad_option_status_test.go — leg 1, round 111 (2026-09-29 audit),
// F111-L1-1.
//
// A wrong-typed sampling parameter is the caller's mistake. The OpenAI and Anthropic
// doors refuse it at decode with a 400; the native doors (/api/chat, /api/generate,
// /api/embed, /api/embeddings) sent the same mistake through Options.FromMap, whose
// error reached handleScheduleError's default branch and became a 500 — a server
// fault as far as any client's retry policy can tell. The request's own options now
// fail as a client error on every door; a bad option in the MODEL's own config stays
// a 500, because that one is the server's.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func r111Status(t *testing.T, err error) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/chat", nil)
	handleScheduleError(c, "m", err)
	return w.Code
}

func TestMine111AWrongTypedRequestOptionIsA400(t *testing.T) {
	s := &Server{}
	for name, opts := range map[string]map[string]any{
		"temperature as a string": {"temperature": "hot"},
		"num_predict as a string": {"num_predict": "many"},
		"num_ctx as a string":     {"num_ctx": "big"},
		"seed as a string":        {"seed": "x"},
		"stop as a number":        {"stop": 7},
	} {
		_, err := s.modelOptions(nil, opts)
		if err == nil {
			t.Fatalf("%s: premise: the option was accepted", name)
		}
		if got := r111Status(t, err); got != http.StatusBadRequest {
			t.Errorf("%s: answered %d %q, want 400 — a wrong-typed request option is the caller's mistake, as on the OpenAI and Anthropic doors (2026-09-29 audit, round 111, F111-L1-1)", name, got, err)
		}
	}
}

// TestMine111ABadOptionInTheModelsOwnConfigStaysA500 is the control: that one is the
// server's fault, not the caller's.
func TestMine111ABadOptionInTheModelsOwnConfigStaysA500(t *testing.T) {
	s := &Server{}
	_, err := s.modelOptions(&Model{Options: map[string]any{"temperature": "hot"}}, nil)
	if err == nil {
		t.Fatal("premise: the model's bad option was accepted")
	}
	if got := r111Status(t, err); got != http.StatusInternalServerError {
		t.Errorf("a bad option in the model's own config answered %d, want 500 (2026-09-29 audit, round 111, F111-L1-1)", got)
	}
}
