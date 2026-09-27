package main

// round52_thinking_only_turn_test.go — round 52's finding on the gateway's
// conversation conversion (the leg-1 audit's F2).
//
// A client turn whose every block is one this wire has no shape for —
// replayed reasoning, most often, which this leg drops deliberately — was
// dropped with it. The conversation then reached the upstream holding nothing,
// and the empty-conversation fallback rewrote it as a USER turn: the client
// asked for an assistant turn and the model was given a user one. Both sibling
// legs keep the turn (the local converter counts a replayed thought as content
// for the turn it arrived in, and the client proxy forwards
// `{"role":"assistant","content":""}`), so the same body was one conversation
// on two legs and another here (2026-09-27 audit, round 52).

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestAThinkingOnlyTurnKeepsItsRoleAndItsPlace: the turn is still a turn of the
// role the client gave it, and it is still the message the model is asked to
// continue.
func TestAThinkingOnlyTurnKeepsItsRoleAndItsPlace(t *testing.T) {
	cap := &round44Capture{}
	up := round44Upstream(t, cap, "application/json", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	srv, _ := round39Gateway(t, up, nil)

	body := `{"model":"kat-awq","max_tokens":64,"stream":false,` +
		`"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"a thought the client echoed back","signature":"sig"}]}]}`
	status, out := round44Raw(t, srv, body)
	if status != http.StatusOK {
		t.Fatalf("status %d for a replayed-thought turn:\n%s", status, out)
	}
	asked := cap.take()
	if asked == "" {
		t.Fatalf("the body never reached the upstream:\n%s", out)
	}
	var sent struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(asked), &sent); err != nil {
		t.Fatalf("the upstream body is not JSON: %v\n%s", err, asked)
	}
	if len(sent.Messages) != 1 {
		t.Fatalf("the conversation reached the upstream as %d messages, want 1 — a turn that carried only reasoning is still a turn:\n%s",
			len(sent.Messages), asked)
	}
	if got, _ := sent.Messages[0]["role"].(string); got != "assistant" {
		t.Errorf("the turn reached the upstream as role %q, want assistant\nthe client asked for an assistant turn and the fallback rewrote it as a user one\n%s",
			got, asked)
	}
}
