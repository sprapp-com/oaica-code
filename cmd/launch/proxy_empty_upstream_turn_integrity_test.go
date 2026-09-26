package launch

// proxy_empty_upstream_turn_integrity_test.go — a 200 that is not a completion
// became a successful empty turn with a fabricated token count (2026-09-26
// audit, thirteenth round).
//
// handleNonStreamResponse recognises an upstream ERROR delivered as a 200 only
// when the body is a top-level `error` object (upstreamErrorMessage). Anything
// else falls through to openAIResponseToChatResponse, whose only choice
// handling is `if len(resp.Choices) > 0` with no else: an empty choices array —
// or a body with no choices key at all, which is what a proxy's HTML-ish
// `{"detail":"Not Found"}`, a bare `{}`, or a health payload looks like —
// becomes a ChatResponse with no message, mapStopReason("") yields "end_turn",
// and the prompt estimate is then written into input_tokens because no usage
// was stated.
//
// The client is told the model finished a turn having said nothing, is billed a
// prompt that never existed, and the leg records a healthy 200 — while the
// STREAMING path answers the same upstream body with 502 and the server-side
// sibling refuses it ("upstream returned no completion choices",
// tools/gateway/messages.go). Same upstream, two verdicts, depending on
// `stream: true`.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestNonStreamNoChoicesIsNotASuccessfulTurn(t *testing.T) {
	bodies := map[string]string{
		"detail-not-found": `{"detail":"Not Found"}`,
		"empty-object":     `{}`,
		"status-ok":        `{"status":"ok"}`,
		"empty-choices":    `{"id":"x","choices":[],"usage":{"prompt_tokens":123,"completion_tokens":0,"total_tokens":123}}`,
	}
	for name, upstreamBody := range bodies {
		t.Run(name, func(t *testing.T) {
			up := jsonUpstream(t, upstreamBody)
			defer up.Close()
			proxy := startCalibProxy(t, up.URL, "sess-empty-turn-"+name)

			resp, err := http.Post(proxy+"/v1/messages", "application/json", strings.NewReader(string(calibMessagesBody(t, 32, 64, false))))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)

			if resp.StatusCode == http.StatusOK {
				var got struct {
					StopReason string `json:"stop_reason"`
					Content    []any  `json:"content"`
					Usage      struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				}
				_ = json.Unmarshal(raw, &got)
				t.Errorf("the upstream body %s was answered %d with stop_reason %q, %d content block(s) and input_tokens %d — a 200 that carries no completion is not a finished turn: the client is told the model answered nothing, is billed a prompt that never existed, and this leg stays green forever. The streaming path and the gateway sibling both refuse this shape\n%s",
					upstreamBody, resp.StatusCode, got.StopReason, len(got.Content), got.Usage.InputTokens, raw)
			} else if resp.StatusCode < 500 {
				t.Errorf("status = %d for %s, want 502 (a bad upstream response, not a client error)\n%s", resp.StatusCode, upstreamBody, raw)
			}
		})
	}
}

// Control: a real completion on the same leg still succeeds, so the refusal is
// about the body's content and not the route.
func TestNonStreamRealCompletionStillSucceeds(t *testing.T) {
	up := jsonUpstream(t, `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`)
	defer up.Close()
	proxy := startCalibProxy(t, up.URL, "sess-real-completion")

	resp, err := http.Post(proxy+"/v1/messages", "application/json", strings.NewReader(string(calibMessagesBody(t, 32, 64, false))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a real completion answered %d\n%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "hello") {
		t.Errorf("the control response carried no content:\n%s", raw)
	}
}
