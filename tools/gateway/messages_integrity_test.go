package main

// messages_integrity_test.go — the Anthropic bridge's translation of the
// shapes a real backend actually sends. Four defects were reproduced against
// this file's own conversion (2026-09-26 audit, fourth round):
//
//   - a tool_result carrying an image (or any non-text block) reached the
//     model as content:"", indistinguishable from "the tool returned
//     nothing";
//   - text between two tool_use blocks made the second call be written onto
//     the text message as a copy of the whole list, so the client saw the
//     first call twice, once on each message;
//   - `reasoning_content` — the spelling DeepSeek and several vLLM builds
//     use — was not modelled at all, so a backend that puts its whole answer
//     there returned an empty reply;
//   - and the same field, when it DID parse, was surfaced as ordinary text.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %#v", v)
	}
	return m
}

// G1: text between two tool_use blocks must not duplicate a call, and no
// tool_calls may land on the text message.
func TestToolCallsAreNotDuplicatedByInterleavedText(t *testing.T) {
	out, _ := contentBlocksToOpenAI("assistant", []any{
		map[string]any{"type": "tool_use", "id": "call_1", "name": "a", "input": map[string]any{"x": 1}},
		map[string]any{"type": "text", "text": "thinking out loud"},
		map[string]any{"type": "tool_use", "id": "call_2", "name": "b", "input": map[string]any{"y": 2}},
	}, false)

	var calls []string
	var textMsgs []map[string]any
	for _, m := range out {
		if tcs, ok := m["tool_calls"].([]map[string]any); ok {
			for _, tc := range tcs {
				id, _ := tc["id"].(string)
				calls = append(calls, id)
			}
		}
		if m["role"] == "assistant" && m["content"] == "thinking out loud" {
			textMsgs = append(textMsgs, m)
		}
	}

	if len(calls) != 2 || calls[0] != "call_1" || calls[1] != "call_2" {
		t.Errorf("tool_calls across the converted messages = %v, want exactly [call_1 call_2] — an interleaved text block made the second call be appended to a copy of the list on the text message", calls)
	}
	if len(textMsgs) != 1 {
		t.Fatalf("found %d text messages, want 1: %#v", len(textMsgs), out)
	}
	tc := textMsgs[0]["tool_calls"]
	if tcs, ok := tc.([]map[string]any); ok && len(tcs) > 0 {
		t.Errorf("the text message carries %d tool_calls (%#v) — the upstream would replay that call twice", len(tcs), tcs)
	}
}

// G2: a tool result that is an image is described, not emptied.
func TestToolResultWithAnImageIsNotEmptied(t *testing.T) {
	img := map[string]any{"type": "image", "source": map[string]any{
		"type": "base64", "media_type": "image/png", "data": strings.Repeat("A", 400),
	}}

	onlyBlocks, onlyErr := contentBlocksToOpenAI("user", []any{
		map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": []any{img}},
	}, false)
	if onlyErr != "" {
		t.Fatalf("a tool_result image must still be representable as text: %s", onlyErr)
	}
	only := asMap(t, onlyBlocks[0])
	if s, _ := only["content"].(string); s == "" {
		t.Error("an image-only tool_result converted to content:\"\" — the model cannot tell that from a tool that returned nothing")
	} else if !strings.Contains(s, "image/png") {
		t.Errorf("image-only tool_result content = %q, want it to name the media type", s)
	} else if strings.Contains(s, strings.Repeat("A", 400)) {
		t.Errorf("the base64 payload was inlined into the text tool result (%d bytes) — it is charged against the context window and tokenizes as noise", len(s))
	}

	mixedBlocks, mixedErr := contentBlocksToOpenAI("user", []any{
		map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": []any{
			map[string]any{"type": "text", "text": "screenshot attached"},
			img,
		}},
	}, false)
	if mixedErr != "" {
		t.Fatalf("a mixed tool_result must still be representable: %s", mixedErr)
	}
	mixed := asMap(t, mixedBlocks[0])
	s, _ := mixed["content"].(string)
	if !strings.Contains(s, "screenshot attached") {
		t.Errorf("the text block was lost: %q", s)
	}
	if !strings.Contains(s, "image/png") {
		t.Errorf("the image block was lost: %q", s)
	}

	// A plain string result is still passed through untouched.
	strBlocks, strErr := contentBlocksToOpenAI("user", []any{
		map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "42"},
	}, false)
	if strErr != "" {
		t.Fatalf("a plain string tool_result must convert: %s", strErr)
	}
	str := asMap(t, strBlocks[0])
	if s, _ := str["content"].(string); s != "42" {
		t.Errorf("string tool_result = %q, want 42", s)
	}
}

// G3: `reasoning_content` is a reasoning field too. A backend that answers
// wholly in it must produce the answer, not an empty reply.
func TestReasoningContentIsNotSilentlyDropped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream func(w http.ResponseWriter, r *http.Request)
		stream   bool
	}{
		{
			name: "non-stream",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"c1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":null,"reasoning_content":"The answer is 42."}}],"usage":{"prompt_tokens":5,"completion_tokens":4}}`)
			},
		},
		{
			name:   "stream",
			stream: true,
			upstream: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				f := w.(http.Flusher)
				for _, p := range []string{
					`data: {"choices":[{"delta":{"reasoning_content":"The answer "}}]}`,
					`data: {"choices":[{"delta":{"reasoning_content":"is 42."},"finish_reason":null}]}`,
					`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
					`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":4}}`,
					`data: [DONE]`,
				} {
					io.WriteString(w, p+"\n\n")
					f.Flush()
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(tc.upstream))
			t.Cleanup(srv.Close)

			g := &gateway{}
			if err := g.apply(gwConfig{
				UpstreamAddr: srv.URL, ListenAddr: ":0",
				LedgerPath: t.TempDir() + "/ledger.jsonl",
				APIKeys:    []gwKey{{SHA256: keyHash("sk-test"), Label: "test"}},
				Models: []gwModel{{
					ID: "oaica-35b-a3b-vision", UpstreamID: "oaica-35b-a3b-vision", OwnedBy: "oaica",
					ContextLength: 262144, MaxCompletionTokens: 32768,
				}},
			}); err != nil {
				t.Fatalf("apply: %v", err)
			}

			body := map[string]any{
				"model": "oaica-35b-a3b-vision", "max_tokens": 100,
				"messages": []map[string]any{{"role": "user", "content": "hi"}},
			}
			if tc.stream {
				body["stream"] = true
			}
			w := postMessages(t, g, "sk-test", body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}

			raw := w.Body.String()
			if tc.stream {
				// The text rides the SSE deltas; the whole body is the event
				// stream, so an emptiness check has to look for the text
				// itself.
				if !strings.Contains(raw, "The answer ") && !strings.Contains(raw, "is 42.") {
					t.Errorf("the streamed answer never reached the client:\n%s — a backend that answers in reasoning_content produced an empty reply", raw)
				}
				return
			}
			var resp struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("not Anthropic JSON: %v\n%s", err, raw)
			}
			if len(resp.Content) != 1 || resp.Content[0].Text != "The answer is 42." {
				t.Errorf("content = %+v, want the reasoning_content as one text block — an empty reply is what this test exists to prevent", resp.Content)
			}
		})
	}
}
