package launch

// round38_stream_usage_and_url_image_integrity_test.go — the client leg's half
// of the round-38 audit: a cache hit the client was never told about because
// each usage chunk was assigned whole, and a url image source the model could
// see (2026-09-27 audit, round 38, B-F1 client leg, A-F3 wire).

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestALaterChunksCacheHitIsReportedToTheClient is B-F1: the streaming reader
// assigned each usage chunk whole and cachedTokens clamps the hit against the
// chunk's OWN prompt_tokens, so a closing usage-only chunk that states the hit
// and omits the prompt zeroed a hit the upstream had stated — the client's
// message_delta read cache_read_input_tokens:0 while the gateway's ledger
// recorded 900 for the same request.
func TestALaterChunksCacheHitIsReportedToTheClient(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		f.Flush()
		// The prompt size in one chunk...
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":1000}}`+"\n\n")
		f.Flush()
		// ...and the cache hit in another that states nothing else.
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_cache_hit_tokens":900,"completion_tokens":3}}`+"\n\n")
		f.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go RunAnthropicOpenAIProxyRoutes(ln, proxyRouteTable{Default: proxyRoute{BaseURL: upstream.URL, UpstreamModel: "kat-awq", Label: "test:kat-awq"}})

	body, _ := json.Marshal(map[string]any{
		"model": "kat-awq", "max_tokens": 16, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	stream := string(out)

	if !strings.Contains(stream, `"cache_read_input_tokens":900`) {
		t.Errorf("the client was told about no cache hit:\n%s\nthe upstream stated 900 cached of a 1000-token prompt; the session's context meter sums both usage fields, so it bills a 900-token miss that never happened", stream)
	}
}

// TestAURLImageSourceReachesTheWireAsAURL is A-F3 at the wire: the converter
// carries the source through as its URL, and the OpenAI content part the proxy
// writes for it is an image_url holding that URL — not a data URL whose base64
// is the URL's own characters.
func TestAURLImageSourceReachesTheWireAsAURL(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const url = "https://example.com/cat.png"
	wire := convertLaunchTurn(t, round34MessagesBody(t, "user", []map[string]any{
		{"type": "text", "text": "what is this?"},
		{"type": "image", "source": map[string]any{"type": "url", "url": url}},
	}, 4096))

	if !strings.Contains(wire, `"url":"`+url+`"`) {
		t.Errorf("a url image source did not reach the wire as a URL: %s\nthe gateway leg forwards the same block as an image_url, so the same body is a picture one leg and a 400 the other", wire)
	}
	if strings.Contains(wire, "data:image/jpeg;base64,aHR0cHM6") {
		t.Errorf("the URL was base64-encoded into a data URL, which sends the upstream a picture of the address: %s", wire)
	}
}
