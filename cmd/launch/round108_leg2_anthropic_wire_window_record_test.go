package launch

// round108_leg2_anthropic_wire_window_record_test.go — leg 2, round 108
// (2026-09-29 audit), F108-L2-1. RECORDED, not changed.
//
// An openai-wire leg with a probed ContextWindow gets the fit plan: `max_tokens`
// clamped to what fits, a local "prompt is too long" 400, and the --oversize
// crossover. An anthropic-wire REMOTE leg (a plan row like zai-coding-plan)
// returns from the handler before that block and gets none of it: the request goes
// to the vendor with `max_tokens` as sent, an oversize prompt reaches the vendor
// and its own 400 is relayed instead of crossing over.
//
// Recorded, not fixed, because the fix is a decision and not a patch. The plan is
// calibrated from the response's real `usage`, and the anthropic-wire passthrough
// relays bytes and parses none, so it would run on the UNCALIBRATED margin (30% of
// chars/4) forever. On this leg that heuristic would reject, or cross over to a
// dearer leg, requests the vendor would have served — a loss of service and a
// billing change on legs that work today, for a protection whose failure (a vendor
// 400) is loud and recoverable. What a later round should know: a fix needs either
// usage parsing on the passthrough (so the plan can calibrate) or an explicit
// decision that this leg runs the uncalibrated plan. The pins state both readings
// as they stand and go red if either moves.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func r108Serve(t *testing.T, table proxyRouteTable) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = RunAnthropicOpenAIProxyRoutes(ln, table) }()
	return "http://" + ln.Addr().String()
}

func r108Message(t *testing.T, url string, maxTokens int) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": maxTokens,
		"messages": []map[string]any{{"role": "user", "content": strings.Repeat("word ", 8000)}}})
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", "k")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestMine108AnAnthropicWireLegSkipsTheOversizeCrossover(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	hits := map[string][2]int32{}
	for _, wire := range []string{"openai", "anthropic"} {
		var primaryHits, overHits int32
		prim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&primaryHits, 1)
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"too long"}}`))
		}))
		t.Cleanup(prim.Close)
		over := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&overHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
		}))
		t.Cleanup(over.Close)
		p := round54Route(prim.URL, "m", wire)
		p.ContextWindow = 2000
		o := round54Route(over.URL, "big", "openai")
		o.ContextWindow = 1000000
		url := r108Serve(t, proxyRouteTable{Default: p, ByModel: map[string]proxyRoute{"m": p}, Oversize: o})
		r108Message(t, url, 16)
		hits[wire] = [2]int32{atomic.LoadInt32(&primaryHits), atomic.LoadInt32(&overHits)}
	}
	if hits["openai"] != [2]int32{0, 1} {
		t.Fatalf("premise: the openai-wire leg crosses over (primary,over hits) = %v, want [0 1]", hits["openai"])
	}
	if hits["anthropic"] != [2]int32{1, 0} {
		t.Errorf("the anthropic-wire leg's (primary,over) hits are %v, this record states [1 0] — the request went to the vendor and did not cross over; if it now crosses over the fix is made and this record is spent (2026-09-29 audit, round 108, F108-L2-1)", hits["anthropic"])
	}
}

func TestMine108AnAnthropicWireLegKeepsTheClientsMaxTokens(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	sent := map[string]float64{}
	for _, wire := range []string{"openai", "anthropic"} {
		wire := wire
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			sent[wire], _ = m["max_tokens"].(float64)
			w.Header().Set("Content-Type", "application/json")
			if wire == "anthropic" {
				_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
		}))
		t.Cleanup(up.Close)
		p := round54Route(up.URL, "m", wire)
		p.ContextWindow = 20000
		url := r108Serve(t, proxyRouteTable{Default: p, ByModel: map[string]proxyRoute{"m": p}})
		r108Message(t, url, 32000)
	}
	if sent["openai"] <= 0 || sent["openai"] >= 20000 {
		t.Fatalf("premise: the openai-wire leg clamps max_tokens to what fits, sent %v", sent["openai"])
	}
	if sent["anthropic"] != 32000 {
		t.Errorf("the anthropic-wire leg sent max_tokens=%v, this record states 32000 as the client sent it; if it is now clamped the fix is made and this record is spent (2026-09-29 audit, round 108, F108-L2-1)", sent["anthropic"])
	}
}
