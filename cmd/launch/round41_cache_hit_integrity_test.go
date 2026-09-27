package launch

// round41_cache_hit_integrity_test.go — round 41's finding on this leg, A41-1
// (with C41-1 behind it):
//
// The non-stream leg derived input_tokens from its own estimate of the body
// and then let cachedTokens() clamp the upstream's stated hit down into that
// estimate. An upstream that narrated "900 of this prompt came from cache" and
// nothing about the prompt size was reported to the client as a handful of
// cached tokens out of a handful-token prompt — while the streaming leg, handed
// the identical usage object, reported 900. One turn, two answers, and which one
// a session saw was decided by whether it had asked for a stream.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// round41NonStream posts one non-streaming /v1/messages body through a real
// proxy route and returns the client's parsed usage object.
func round41NonStream(t *testing.T, upstream *httptest.Server, body []byte) map[string]any {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go RunAnthropicOpenAIProxyRoutes(ln, proxyRouteTable{Default: proxyRoute{BaseURL: upstream.URL, UpstreamModel: "kat-awq", Label: "test:kat-awq"}})

	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("premise: status %d:\n%s", resp.StatusCode, out)
	}
	var doc struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal the non-stream answer: %v\n%s", err, out)
	}
	if doc.Usage == nil {
		t.Fatalf("the non-stream answer carries no usage object:\n%s", out)
	}
	return doc.Usage
}

// TestAStatedHitWithoutAStatedPromptRaisesTheNonStreamTotal is A41-1. The two
// fields partition the turn's prompt, so a hit the upstream stated has to be
// carved out of the total — and a total derived from this proxy's own reading
// of the body is a reading that the stated hit has just contradicted.
func TestAStatedHitWithoutAStatedPromptRaisesTheNonStreamTotal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		// A hit, and nothing about the prompt — the shape a build narrating
		// only its cache reading sends.
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_cache_hit_tokens":900,"completion_tokens":3}}`)
	}))
	defer upstream.Close()

	body, _ := json.Marshal(map[string]any{
		"model": "kat-awq", "max_tokens": 16,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	usage := round41NonStream(t, upstream, body)
	input, cached := usageInt(usage, "input_tokens"), usageInt(usage, "cache_read_input_tokens")

	if cached != 900 {
		t.Errorf("the client was told cache_read_input_tokens=%d for a turn whose upstream stated a 900-token hit: the hit is a measurement of the prompt, so the prompt total is raised to it rather than the hit being clamped into this proxy's own shorter estimate", cached)
	}
	if input+cached != 900 {
		t.Errorf("the two usage fields sum to %d (input %d, hit %d), want 900: they partition the turn's prompt (usage: %v)", input+cached, input, cached, usage)
	}
	if input < 0 {
		t.Errorf("input_tokens=%d: a negative count is not a count", input)
	}
}

// TestBothLegsReportOneTurnTheSameWay is A41-1's real harm: the SAME upstream
// usage object, once streamed and once answered whole, told a session two
// different things about its own cache efficiency. Whatever the shape of the
// answer, the same usage object reports the same two numbers.
func TestBothLegsReportOneTurnTheSameWay(t *testing.T) {
	usage := `{"prompt_cache_hit_tokens":900,"completion_tokens":3}`
	asked, _ := json.Marshal(map[string]any{
		"model": "kat-awq", "max_tokens": 16,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})

	// The whole-document shape.
	whole := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":`+usage+`}`)
	}))
	defer whole.Close()
	doc := round41NonStream(t, whole, asked)
	docInput, docCached := usageInt(doc, "input_tokens"), usageInt(doc, "cache_read_input_tokens")

	// The frame-by-frame shape, same usage object.
	streams := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		f.Flush()
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":`+usage+`}`+"\n\n")
		f.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer streams.Close()

	setLaunchTestHome(t, t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go RunAnthropicOpenAIProxyRoutes(ln, proxyRouteTable{Default: proxyRoute{BaseURL: streams.URL, UpstreamModel: "kat-awq", Label: "test:kat-awq"}})
	streamBody, _ := json.Marshal(map[string]any{
		"model": "kat-awq", "max_tokens": 16, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(streamBody))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	d := deltaUsage(t, string(out))
	if d == nil {
		t.Fatalf("no message_delta reached the client:\n%s", out)
	}
	stInput, stCached := usageInt(d, "input_tokens"), usageInt(d, "cache_read_input_tokens")

	if docInput != stInput || docCached != stCached {
		t.Errorf("one upstream usage object reported as input=%d hit=%d when streamed and input=%d hit=%d when answered whole: a session's cache read-out must not depend on whether it asked for a stream", stInput, stCached, docInput, docCached)
	}
}
