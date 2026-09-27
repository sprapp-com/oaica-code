package launch

// round49_proxy_request_integrity_test.go — round 49's findings on the
// client-side proxy leg.
//
// B-F1: this handler was the one translation site with NO check on the
// request's own shape. The local server refuses a body with no max_tokens, a
// non-positive one, or no messages at all (middleware/anthropic.go) and the
// metered gateway refuses the same three, so one body was answered one way by
// two legs and another way here: max_tokens 0 (a body asking for a refusal) had
// the field dropped so the backend applied its own cap, a negative one was
// forwarded as written, and a body with no messages was converted into an empty
// conversation. A missing model is deliberately NOT one of the three — this
// proxy falls back to the upstream model it was started with, which is pinned
// behaviour of this entry point.
//
// B-F6: the OpenAI message the proxy builds for a contentless turn marshalled
// with no content key at all, so the upstream saw `{"role":"user"}` — the same
// `"input":{}` class round 45 fixed on the gateway's tool blocks, where a
// required field is absent rather than empty.
//
// A-F2: the slot a streaming delta names is how the client's block is keyed.
// A whole completion adopted mid-stream wrote the slots it carried, and the
// delta loop then wrote a NEW call into a slot the adopted text had already
// filled because the accumulated entry was looked up without asking whether
// that slot had been written — one call lost, another doubled (2026-09-27
// audit, round 49, A49-2).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// round49CapturingUpstream records the upstream request body and answers one
// complete non-streaming completion.
func round49CapturingUpstream(t *testing.T, captured *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*captured = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","model":"kat-awq","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// round49Post sends one raw Anthropic body through the proxy.
func round49Post(t *testing.T, proxyURL, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read proxy response: %v", err)
	}
	return resp.StatusCode, string(b)
}

// round49Refusal reads the error text of a refusal body.
func round49Refusal(t *testing.T, body string) (kind, message string) {
	t.Helper()
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("decode refusal %s: %v", body, err)
	}
	return e.Error.Type, e.Error.Message
}

// TestARequestBodyWithoutMaxTokensOrMessagesIsRefused is B-F1. Both other legs
// refuse these bodies; this one served them.
func TestARequestBodyWithoutMaxTokensOrMessagesIsRefused(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"no_max_tokens", `{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`, "max_tokens is required and must be positive"},
		{"max_tokens_zero", `{"model":"kat-awq","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`, "max_tokens is required and must be positive"},
		{"max_tokens_negative", `{"model":"kat-awq","max_tokens":-3,"messages":[{"role":"user","content":"hi"}]}`, "max_tokens is required and must be positive"},
		{"messages_empty", `{"model":"kat-awq","max_tokens":16,"messages":[]}`, "messages is required"},
		{"no_messages", `{"model":"kat-awq","max_tokens":16}`, "messages is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var captured string
			up := round49CapturingUpstream(t, &captured)
			proxy := startCalibProxy(t, up.URL, "r49-"+c.name)
			status, body := round49Post(t, proxy, c.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 — the body was served where both other legs refuse it\n%s", status, body)
			}
			kind, message := round49Refusal(t, body)
			if kind != "invalid_request_error" {
				t.Errorf("the refusal's error type is %q, want invalid_request_error", kind)
			}
			if message != c.want {
				t.Errorf("the refusal reads %q, want %q — the three legs must refuse one body with one message", message, c.want)
			}
			if captured != "" {
				t.Errorf("the refused body still reached the backend: %s", captured)
			}
		})
	}

	// The control: a body the local server cannot serve at all is servable
	// here, because this proxy was started with an upstream model and its
	// documented fallback supplies it.
	t.Run("model_may_be_omitted", func(t *testing.T) {
		var captured string
		up := round49CapturingUpstream(t, &captured)
		proxy := startCalibProxy(t, up.URL, "r49-no-model")
		status, body := round49Post(t, proxy, `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		if status != http.StatusOK {
			t.Fatalf("status %d, want 200 — the proxy falls back to the model it was started with\n%s", status, body)
		}
		var req map[string]any
		if err := json.Unmarshal([]byte(captured), &req); err != nil {
			t.Fatalf("decode upstream body %s: %v", captured, err)
		}
		if req["model"] != "kat-awq" {
			t.Errorf("the upstream model is %v, want the route's own kat-awq", req["model"])
		}
	})
}

// TestAnEmptyContentPartStillSendsAContentKey is B-F6. A contentless turn is a
// message with no content, not a message with no content FIELD.
func TestAnEmptyContentPartStillSendsAContentKey(t *testing.T) {
	var captured string
	up := round49CapturingUpstream(t, &captured)
	proxy := startCalibProxy(t, up.URL, "r49-content-key")
	status, body := round49Post(t, proxy, `{"model":"kat-awq","max_tokens":16,"messages":[{"role":"user","content":[]},{"role":"user","content":"go"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	var req struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(captured), &req); err != nil {
		t.Fatalf("decode upstream body %s: %v", captured, err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("the backend was handed %d messages, want 2:\n%s", len(req.Messages), captured)
	}
	if _, ok := req.Messages[0]["content"]; !ok {
		t.Errorf("the contentless turn reached the backend as %v — the message has no content key\n%s", req.Messages[0], captured)
	}
	if req.Messages[0]["content"] != "" {
		t.Errorf("the contentless turn's content is %v, want the empty string", req.Messages[0]["content"])
	}
}

// TestAWholeCompletionForOneSlotIsNotRewritten is A-F2. The adopted completion
// wrote slot 0, and the delta that follows names a different call in slot 1;
// both calls reach the client, each once.
func TestAWholeCompletionForOneSlotIsNotRewritten(t *testing.T) {
	up := streamUpstream(t,
		"data: {\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"\",\"tool_calls\":[{\"index\":0,\"id\":\"call_A\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_B\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"{\\\"b\\\":2}\"}}]}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", true)
	proxy := startCalibProxy(t, up.URL, "r49-adopt")
	body, status := postMessagesStream(t, proxy)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	blocks := r45ToolUseBlocks(t, body)
	if len(blocks) != 2 {
		t.Fatalf("the client was handed %d tool_use block(s), want 2 — the adopted call and the streamed one:\n%s", len(blocks), body)
	}
	ids := map[string]string{}
	for _, b := range blocks {
		id, _ := b["id"].(string)
		name, _ := b["name"].(string)
		if prev, dup := ids[name]; dup {
			t.Fatalf("the call %q was handed to the client twice (as %s and %s):\n%s", name, prev, id, body)
		}
		ids[name] = id
	}
	if _, ok := ids["Bash"]; !ok {
		t.Errorf("the adopted call is missing from the client's blocks: %v\n%s", ids, body)
	}
	if _, ok := ids["Read"]; !ok {
		t.Errorf("the streamed call is missing from the client's blocks: %v\n%s", ids, body)
	}
}
