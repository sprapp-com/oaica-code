package launch

// round67_multi_choice_and_cache_measurement_integrity_test.go — round 67's two
// findings on the client proxy, both of them a reading that was right on one of
// this leg's arms and different on the other.
//
// F67-L2-1. This proxy never states `n`, so one chunk is one choice — but the
// fragment arm folded the deltas of EVERY choice it found into the one message
// it is streaming, while the arm that read the same turn as a whole document
// reads Choices[0] (openAIResponseToChatResponse). A chunk carrying "A" at index
// 0 and "B" at index 1 therefore reached the client as "AB" streamed and "A"
// whole: text the model never wrote, spliced into the conversation, and only on
// the path production uses.
//
// F67-L2-2. cache_read_input_tokens is present exactly when the upstream stated
// a cache reading — a stated 0 is a reading, and absence means no measurement.
// The whole arm has asked that question of the object for several rounds
// (cacheReadPtr); the fragment arm asked `cached > 0` instead, so a stated zero
// travelled as a present field one way and as silence the other.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r67Texts serves the SSE script and the whole document and returns (a) the
// text deltas the streaming arm relayed, concatenated, and (b) the text blocks
// the non-streaming arm answered.
func r67Texts(t *testing.T, frames []string, whole string) (streamed, wholeText string) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	script := strings.Join(frames, "\n\n") + "\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, script)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, whole)
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})

	code, body := round54PostMessage(t, proxy, "glm-5.3", true)
	if code != http.StatusOK {
		t.Fatalf("stream arm status %d\n%s", code, body)
	}
	var streamedText strings.Builder
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(raw), &ev) != nil {
			continue
		}
		if ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" {
			streamedText.WriteString(ev.Delta.Text)
		}
	}

	code, wholeBody := round54PostMessage(t, proxy, "glm-5.3", false)
	if code != http.StatusOK {
		t.Fatalf("whole arm status %d\n%s", code, wholeBody)
	}
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(wholeBody), &resp); err != nil {
		t.Fatalf("whole arm did not parse: %v\n%s", err, wholeBody)
	}
	for _, c := range resp.Content {
		if c.Type == "text" {
			wholeText += c.Text
		}
	}
	return streamedText.String(), wholeText
}

// r67TwoChoiceChunk is one chunk carrying two alternatives, the shape a
// multiplexing proxy or a stray `n` in front of the backend produces.
const r67TwoChoiceChunk = `data: {"id":"c","object":"chat.completion.chunk","choices":[` +
	`{"index":0,"delta":{"content":"A"}},{"index":1,"delta":{"content":"B"}}]}`

// r67TwoChoiceWhole is the same turn as a whole document.
const r67TwoChoiceWhole = `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[` +
	`{"index":0,"message":{"role":"assistant","content":"A"},"finish_reason":"stop"},` +
	`{"index":1,"message":{"role":"assistant","content":"B"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":10,"completion_tokens":1}}`

// TestASecondChoiceInAChunkDoesNotJoinTheFirstOnThisLeg is F67-L2-1.
func TestASecondChoiceInAChunkDoesNotJoinTheFirstOnThisLeg(t *testing.T) {
	streamed, whole := r67Texts(t, []string{
		r67TwoChoiceChunk,
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, r67TwoChoiceWhole)

	if whole != "A" {
		t.Fatalf("PREMISE: the whole-document arm answers %q, want the first choice — it reads Choices[0], and this proxy never states `n`", whole)
	}
	if streamed != whole {
		t.Errorf("the same turn is %q streamed and %q as a whole document: the fragment arm folded every choice of the chunk into the one message it streams, so the client's conversation gained text the model wrote only as an alternative (2026-09-28 audit, round 67, F67-L2-1)", streamed, whole)
	}
}

// TestAFinishReasonOnALaterChoiceStillEndsTheTurn is the control for F67-L2-1:
// which choice carried the finish_reason is not this arm's to decide, so the
// turn must still end when it is not the first one that states it.
func TestAFinishReasonOnALaterChoiceStillEndsTheTurn(t *testing.T) {
	_, body := func() (int, string) {
		setLaunchTestHome(t, t.TempDir())
		script := strings.Join([]string{
			`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"A"}},{"index":1,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, "\n\n") + "\n\n"
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, script)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}))
		t.Cleanup(up.Close)
		route := round54Route(up.URL, "glm-5.3", "openai")
		proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
		return round54PostMessage(t, proxy, "glm-5.3", true)
	}()

	if !strings.Contains(body, `"message_delta"`) || !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Errorf("a finish_reason stated on the chunk's SECOND choice did not end the turn: the scan for it must cover every entry, not only the first\n%s", body)
	}
	if !strings.Contains(body, `"text":"A"`) || strings.Contains(body, `"text":"B"`) {
		t.Errorf("the relayed text is not the first choice's alone\n%s", body)
	}
}

// r67UsageScript is r67Texts' two-arm harness for one usage object: it returns
// the streaming arm's message_delta usage object and the whole arm's usage
// object, both as maps, so the presence of a field can be compared.
func r67UsageObjects(t *testing.T, usage string) (streamed, whole map[string]any) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	script := strings.Join([]string{
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"yo"}}]}`,
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":` + usage + `}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	wholeDoc := `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"yo"},"finish_reason":"stop"}],"usage":` + usage + `}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, script)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, wholeDoc)
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})

	_, body := round54PostMessage(t, proxy, "glm-5.3", true)
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string         `json:"type"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(raw), &ev) != nil || ev.Type != "message_delta" {
			continue
		}
		streamed = ev.Usage
	}
	_, wbody := round54PostMessage(t, proxy, "glm-5.3", false)
	var resp struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal([]byte(wbody), &resp); err != nil {
		t.Fatalf("whole arm did not parse: %v\n%s", err, wbody)
	}
	return streamed, resp.Usage
}

// TestAStatedZeroCacheHitReachesTheClientTheSameWayOnBothPaths is F67-L2-2: a
// stated reading of zero is a reading, and the field that carries it must not
// depend on whether the client asked for `stream`.
func TestAStatedZeroCacheHitReachesTheClientTheSameWayOnBothPaths(t *testing.T) {
	streamed, whole := r67UsageObjects(t, `{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":0}}`)
	if _, ok := whole["cache_read_input_tokens"]; !ok {
		t.Fatalf("PREMISE: the whole-document arm omits cache_read_input_tokens for a stated 0, so this is not the shape the finding is about: %v", whole)
	}
	s, ok := streamed["cache_read_input_tokens"]
	if !ok {
		t.Errorf("the whole-document arm relays cache_read_input_tokens=%v for a usage object that states a cache reading of zero, and the streaming arm omits the field entirely: presence means 'a reading was stated', so the same object cannot answer two ways (2026-09-28 audit, round 67, F67-L2-2)\nstreamed: %v\nwhole:    %v", whole["cache_read_input_tokens"], streamed, whole)
	} else if s != whole["cache_read_input_tokens"] {
		t.Errorf("the two arms relay different cache readings for one usage object: %v streamed, %v whole", s, whole["cache_read_input_tokens"])
	}
}

// TestAnUnmeasuredCacheStaysAbsentOnBothPaths is the control: an object that
// states no cache reading at all keeps the field off, on both arms — the fix
// must not turn "no measurement" into "zero".
func TestAnUnmeasuredCacheStaysAbsentOnBothPaths(t *testing.T) {
	streamed, whole := r67UsageObjects(t, `{"prompt_tokens":100,"completion_tokens":7}`)
	for arm, u := range map[string]map[string]any{"streamed": streamed, "whole": whole} {
		if v, ok := u["cache_read_input_tokens"]; ok {
			t.Errorf("the %s arm reports cache_read_input_tokens=%v for a usage object that states no cache measurement, so a client cannot tell 'nothing cached' from 'nothing measured'", arm, v)
		}
	}
}
