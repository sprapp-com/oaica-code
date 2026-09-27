package launch

// round40_cache_read_total_and_call_split_integrity_test.go — round 40's
// findings on this leg:
//
//   - A40-2: the closing message_delta partitioned the prompt by clamping the
//     stated hit DOWN into a total that this gateway had estimated for itself.
//     A hit larger than the estimate is the upstream telling us the estimate is
//     too small, and the clamp reported a 900-token hit as an 8-token one.
//   - A40-8: a tool-call delta naming a DIFFERENT call over one whose arguments
//     were still incomplete was read as an argument continuation, so the two
//     calls collapsed into one block named after the last of them.

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

// TestAHitLargerThanTheEstimateRaisesTheTotal is A40-2 at the wire. The client
// only ever sees what the closing message_delta states, and the total it
// partitions is this proxy's own reading of the body whenever the upstream
// states no prompt size — so a stated hit LARGER than that reading is evidence
// the reading is short, not a number to clamp into it.
func TestAHitLargerThanTheEstimateRaisesTheTotal(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		f.Flush()
		// The hit, and nothing about the prompt — the shape a build narrating
		// only its cache reading sends.
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

	// A body far smaller than the stated hit: the estimate is a few tokens and
	// the hit is 900. Clamping the hit into the estimate reported the hit as
	// the estimate, and the client's cache read-out read a served-from-cache
	// turn as a miss while the gateway's row for the same request recorded 900.
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

	usage := deltaUsage(t, string(out))
	if usage == nil {
		t.Fatalf("no message_delta reached the client:\n%s", out)
	}
	input, cached := usageInt(usage, "input_tokens"), usageInt(usage, "cache_read_input_tokens")
	if cached != 900 {
		t.Errorf("the client was told cache_read_input_tokens=%d for a turn whose upstream stated a 900-token hit: the hit was clamped into this proxy's own estimate of the prompt instead of raising it, and a session's cache-efficiency read-out shows a hit as a miss (input_tokens=%d)\n%s", cached, input, out)
	}
	if input+cached != 900 {
		t.Errorf("the two usage fields sum to %d (input %d, hit %d), want 900: they partition the turn's prompt, so the stated hit is carved out of the total rather than added to it or reported beside an estimate of a prompt it already accounts for", input+cached, input, cached)
	}
	if input < 0 {
		t.Errorf("input_tokens=%d: a negative count is not a count\n%s", input, out)
	}
}

// TestADifferentNameOverIncompleteArgumentsSplitsTheCall is A40-8. An
// index-less delta that names a different call cannot be continuing the one in
// progress — an argument continuation carries arguments alone — and reading it
// as a continuation built one tool_use named after the LAST call with both
// calls' arguments concatenated: a well-formed-looking block with the wrong
// name, which nothing about the client's behaviour will signal.
func TestADifferentNameOverIncompleteArgumentsSplitsTheCall(t *testing.T) {
	cases := []struct {
		name                        string
		accID, accName, accArgs     string
		deltaID, deltaName, deltaAr string
		want                        bool
	}{
		{
			name:      "a different name over an unfinished object begins the next call",
			accID:     "c1",
			accName:   "Read",
			accArgs:   `{"path":`,
			deltaName: "Write",
			deltaAr:   `{"path":"b.txt"}`,
			want:      true,
		},
		{
			name:      "a different name over no arguments at all begins the next call",
			accID:     "c1",
			accName:   "Read",
			deltaName: "Write",
			want:      true,
		},
		{
			name:      "the SAME name over an unfinished object continues it",
			accID:     "c1",
			accName:   "Read",
			accArgs:   `{"path":`,
			deltaName: "Read",
			deltaAr:   `"a.txt"}`,
			want:      false,
		},
		{
			name:      "a bare repeat of the same name over no arguments is the NEXT call",
			accID:     "c1",
			accName:   "Read",
			deltaName: "Read",
			want:      true,
		},
		{
			name:      "a bare name fragment over an unfinished object continues it",
			accID:     "c1",
			accName:   "Read",
			accArgs:   `{"path":`,
			deltaName: "Read",
			want:      false,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := startsANewToolCall(tt.accID, tt.accName, tt.accArgs, tt.deltaID, tt.deltaName, tt.deltaAr)
			if got != tt.want {
				t.Errorf("startsANewToolCall(acc %q/%q/%q, delta %q/%q/%q) = %v, want %v", tt.accID, tt.accName, tt.accArgs, tt.deltaID, tt.deltaName, tt.deltaAr, got, tt.want)
			}
		})
	}
}

// TestTheTwoCallsOfANameChangeReachTheClientAsTwoBlocks is A40-8's consequence,
// measured on the wire rather than in the predicate: two calls in, two blocks
// out, each named after its own call.
func TestTheTwoCallsOfANameChangeReachTheClientAsTwoBlocks(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":"}}]}}]}`+"\n\n")
		f.Flush()
		// No index, a different name, and arguments: the second call.
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"type":"function","function":{"name":"Write","arguments":"{\"path\":\"b.txt\"}"}}]}}]}`+"\n\n")
		f.Flush()
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
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
		"model": "kat-awq", "max_tokens": 64, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "go"}},
	})
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	stream := string(out)

	if got := strings.Count(stream, `"type":"tool_use"`); got != 2 {
		t.Fatalf("the client was handed %d tool_use blocks for two calls:\n%s\nthe second call was read as an argument continuation of the first, so one block carries both calls' arguments under the last call's name", got, stream)
	}
	for _, name := range []string{`"name":"Read"`, `"name":"Write"`} {
		if !strings.Contains(stream, name) {
			t.Errorf("no block named %s reached the client:\n%s", name, stream)
		}
	}
}
