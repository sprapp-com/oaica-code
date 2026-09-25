package launch

// proxy_stream_invariants_test.go — regression pins for the
// Anthropic<->OpenAI translation proxy: streaming usage accounting, error
// bodies, and tool-call translation. Each test was written as a failing
// reproduction of one defect during the 2026-09-26 audit and now pins the
// fixed behaviour, so a regression fails for the same stated reason it was
// found for. The "Finding N" comments above each test record the defect; the
// assertion messages state the invariant that replaced it.
//
//	rtk proxy go test ./cmd/launch/ -run 'StreamedFullCacheHit|NonStreamError|ParallelToolCalls|SameRemoteSplit' -count=1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// PIN 1: a streamed turn whose prompt was entirely a prefix-cache hit
// reports the prompt TWICE.
//
// Anthropic usage semantics (and the contract this proxy's own comment at
// anthropic_openai_proxy.go:499-502 states): input_tokens is the UNCACHED
// part and cache_read_input_tokens is the cached part, so the client's
// input+cache_read sum is the real prompt length. The non-streaming path
// honours that. The streaming path does not: handleStreamResponse computes
// `cached = finalUsage.cachedTokens()` and
// `doneResp.Metrics.PromptEvalCount = PromptTokens - cached` (line 1784),
// which is 0 for a full hit — and StreamConverter.Process then REFUSES to
// update input_tokens from a zero metric (`if r.Metrics.PromptEvalCount > 0`,
// anthropic/anthropic.go:1025), leaving the seeded whole-prompt ESTIMATE in
// input_tokens while the real cached count is patched in beside it.
// ---------------------------------------------------------------------------

// TestStreamedFullCacheHitDoesNotReportThePromptTwice
//
// Two identical requests. The first records the session's calibration (real
// prompt_tokens = 50000 for a 200 KB body); the second is the same body
// again — a client retry, or the proxy's own retry — so the upstream reports
// the whole prompt as a prefix-cache hit. The client must be told
// input_tokens = 0 and cache_read = 50000.
func TestStreamedFullCacheHitDoesNotReportThePromptTwice(t *testing.T) {
	var reqs int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&reqs, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		usage := `{"choices":[],"usage":{"prompt_tokens":50000,"completion_tokens":37}}`
		if n > 1 {
			usage = `{"choices":[],"usage":{"prompt_tokens":50000,"completion_tokens":37,"prompt_tokens_details":{"cached_tokens":50000}}}`
		}
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"hello"}}]}`,
			"",
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			"",
			"data: " + usage,
			"",
			"data: [DONE]",
			"",
		}, "\n"))
	}))
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-r3a-fullcache")
	body := calibMessagesBody(t, 200000, 64, true)
	post := func() string {
		t.Helper()
		resp, err := http.Post(proxy+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST /v1/messages: %v", err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read proxy response: %v", err)
		}
		return string(b)
	}

	first := post()
	if got := usageInt(deltaUsage(t, first), "input_tokens"); got <= 0 {
		t.Fatalf("first turn input_tokens = %d, want the real 50000 — the fixture upstream must report usage\n%s", got, first)
	}

	second := post()
	u := deltaUsage(t, second)
	in, cr := usageInt(u, "input_tokens"), usageInt(u, "cache_read_input_tokens")
	if cr != 50000 {
		t.Fatalf("cache_read_input_tokens = %d, want 50000 — the fixture upstream said the whole prompt was cached\n%s", cr, second)
	}
	if in+cr != 50000 {
		t.Errorf("input_tokens+cache_read_input_tokens = %d for a 50000-token prompt: the client's context accounting reads both fields, so a fully cache-hit turn must report input_tokens=0 (input_tokens=%d, the pre-turn ESTIMATE, was kept because StreamConverter.Process only overwrites input_tokens when the metric is >0 — anthropic/anthropic.go:1025)\n%s",
			in+cr, in, second)
	}

	// Contrast, and the reason this is a defect rather than a convention: the
	// non-streaming path reports the very same turn correctly.
	up2 := jsonUpstream(t, `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50000,"completion_tokens":37,"prompt_tokens_details":{"cached_tokens":50000}}}`)
	defer up2.Close()
	proxy2 := startCalibProxy(t, up2.URL, "sess-r3a-fullcache-nonstream")
	resp, err := http.Post(proxy2+"/v1/messages", "application/json",
		bytes.NewReader(calibMessagesBody(t, 200000, 64, false)))
	if err != nil {
		t.Fatalf("POST /v1/messages (non-stream): %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var got struct {
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode non-stream response: %v\n%s", err, raw)
	}
	if got.Usage.InputTokens != 0 || got.Usage.CacheReadInputTokens != 50000 {
		t.Fatalf("non-stream control: input_tokens=%d cache_read=%d, want 0/50000 — if this fails the premise is wrong, not the invariant\n%s",
			got.Usage.InputTokens, got.Usage.CacheReadInputTokens, raw)
	}
}

// ---------------------------------------------------------------------------
// Finding 2: the same-remote tier split's secondary window is never probed.
//
// withContextWindows only probes the secondary when its BaseURL DIFFERS from
// the primary's (context_window_remote.go:147-151). But the documented split
// `--model box/small --sonnet-model box/big` puts both legs on ONE remote
// with two different models (tier_routing.go:464-469, sameRemote, keeps the
// BaseURL and swaps the model id) — and the file's own comment ~20 lines
// below (applyContextWindowsToRoutes, lines 252-259) says exactly that: two
// legs on one remote share a base URL and matching on the URL alone stamps
// the sibling's window where it does not belong. So the guard skips the
// probe, SecondaryContext stays 0, and two things follow:
//
//   - the proxy's context-fit clamp is gated on route.ContextWindow > 0, so
//     the sonnet leg is served with NO ceiling at all;
//   - envVars folds in only the probed leg windows, so every sonnet/subagent
//     session is advertised the PRIMARY's usable window — a quarter of the
//     window its own model serves when the secondary is larger (early
//     auto-compaction), and none of it when the secondary is smaller.
// ---------------------------------------------------------------------------

// TestSameRemoteSplitSecondaryWindowNeverProbed builds the real plan
// through buildTierPlan for `--model box/small --sonnet-model box/big` on one
// remote that serves both (32768 / 262144), then probes for real.
func TestSameRemoteSplitSecondaryWindowNeverProbed(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	stubBareIndex(t, map[string][]string{})

	const smallWindow, bigWindow = 32768, 262144
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":[{"id":"small","max_model_len":%d},{"id":"big","max_model_len":%d}]}`,
			smallWindow, bigWindow)
	}))
	defer srv.Close()

	writeRemotes(t, fmt.Sprintf(
		`{"remotes":[{"name":"box","base_url":%q,"version":"none","api_key":"k"}]}`,
		srv.URL))

	plan, err := buildTierPlan("box/small", "box/big", "", true)
	if err != nil {
		t.Fatalf("buildTierPlan: %v", err)
	}
	if plan.SecondaryName != "box/big" || plan.Secondary.UpstreamModel != "big" {
		t.Fatalf("fixture is wrong: SecondaryName=%q Secondary.UpstreamModel=%q — the split must be two models on one remote",
			plan.SecondaryName, plan.Secondary.UpstreamModel)
	}
	if plan.Routes.Default.BaseURL != plan.Routes.ByModel["box/big"].BaseURL {
		t.Fatalf("fixture is wrong: the two legs must share a base URL (%q vs %q)",
			plan.Routes.Default.BaseURL, plan.Routes.ByModel["box/big"].BaseURL)
	}

	plan.withContextWindows()
	if plan.SecondaryContext != bigWindow {
		t.Errorf("SecondaryContext = %d, want %d: withContextWindows only probes the secondary when its BaseURL differs from the primary's (context_window_remote.go:147-151), so a same-remote split's real window is never asked for — the primary's %d came back instead (%d)",
			plan.SecondaryContext, bigWindow, smallWindow, plan.PrimaryContext)
	}
	plan.applyContextWindowsToRoutes()
	if got := plan.Routes.ByModel["box/big"].ContextWindow; got != bigWindow {
		t.Errorf("box/big route ContextWindow = %d, want %d: the context-fit clamp is gated on ContextWindow > 0, so the sonnet leg runs with no ceiling",
			got, bigWindow)
	}

	// Second consequence: the advisory env pair is the ONLY window Claude Code
	// gets for its sonnet/subagent sessions, and envVars folds in the probed
	// leg windows (tier_routing.go:981-1004) — which is 0 here.
	want := usableContextWindow("box/big", bigWindow)
	got := ""
	for _, kv := range plan.envVars("http://127.0.0.1:1", "tok") {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CODE_MAX_CONTEXT_TOKENS="); ok {
			got = v
		}
	}
	if got != strconv.Itoa(want) {
		t.Errorf("CLAUDE_CODE_MAX_CONTEXT_TOKENS=%s for a split whose sonnet leg serves a %d-token window (want %d); envVars only sees the probed SecondaryContext, so every sonnet/subagent session is advertised the PRIMARY's usable window (%d) instead",
			got, bigWindow, want, usableContextWindow("box/small", smallWindow))
	}
}

// ---------------------------------------------------------------------------
// Finding 3: a JSON error object delivered over HTTP 200 on the NON-stream
// path is still reported to the client as a successful empty turn.
//
// This is the asymmetric sibling of the 2026-09-26 streaming fix: the
// streaming path recognises a bare JSON error body over 200
// (upstreamErrorMessage, anthropic_openai_proxy.go:1646-1653 and 1817) and
// fails the request so the client SDK retries. handleNonStreamResponse has
// no such check — it unmarshals the error object into an empty
// openAIChatResponse and writes HTTP 200 with content [] and stop_reason "".
// Worse, it then fills usage.input_tokens with the prompt ESTIMATE
// (line 1499-1504), so the failure is not even inert: the client is told the
// turn consumed tokens and produced nothing.
// ---------------------------------------------------------------------------

// TestNonStreamErrorObjectOver200IsNotASilentSuccess
func TestNonStreamErrorObjectOver200IsNotASilentSuccess(t *testing.T) {
	up := jsonUpstream(t, `{"error":{"type":"invalid_request_error","message":"boom: the decode crashed"}}`)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-r3a-err200")
	resp, err := http.Post(proxy+"/v1/messages", "application/json",
		bytes.NewReader(calibMessagesBody(t, 4096, 64, false)))
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var body struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
		Error      *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)

	if resp.StatusCode == http.StatusOK {
		if body.Error == nil && body.StopReason == "" {
			t.Errorf("HTTP 200 with stop_reason %q, no error, content [] for an upstream that answered an ERROR OBJECT over 200: handleNonStreamResponse never calls upstreamErrorMessage, so a dead turn reaches the client as an empty success (and usage.input_tokens is set to the prompt estimate, line 1499-1504)\n%s",
				body.StopReason, raw)
		}
	}
}

// ---------------------------------------------------------------------------
// Finding 4: parallel tool calls are dropped when the upstream does not put an
// id on them.
//
// StreamConverter keeps a `toolCallsSent map[string]bool` so one tool call is
// not emitted twice across Process calls (anthropic/anthropic.go:926/994).
// Keyed by tc.ID, it collides on the empty string: with two id-less parallel
// calls the first marks toolCallsSent[""] = true and the second is skipped
// entirely. In this proxy the guard has no other reachable effect — the
// stream handler flushes accumulated tool calls exactly once, in index order
// (anthropic_openai_proxy.go:1770), so nothing else can be double-emitted.
// The control half of the test shows both calls survive when ids ARE present,
// i.e. the dedup, not the parsing, is what drops the call.
// ---------------------------------------------------------------------------

// TestParallelToolCallsWithoutIDsAreDropped
func TestParallelToolCallsWithoutIDsAreDropped(t *testing.T) {
	script := func(withIDs bool) string {
		id := func(n int, name string) string {
			if !withIDs {
				return ""
			}
			return fmt.Sprintf(`"id":"call_%d",`, n)
		}
		return strings.Join([]string{
			fmt.Sprintf(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"type":"function",%s"function":{"name":"Read","arguments":"{}"}}]}}]}`, id(1, "Read")),
			"",
			fmt.Sprintf(`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"type":"function",%s"function":{"name":"Bash","arguments":"{}"}}]}}]}`, id(2, "Bash")),
			"",
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			"",
			"data: [DONE]",
			"",
		}, "\n")
	}

	run := func(withIDs bool) string {
		t.Helper()
		up := streamUpstream(t, script(withIDs), true)
		defer up.Close()
		proxy := startCalibProxy(t, up.URL, fmt.Sprintf("sess-r3a-toolids-%v", withIDs))
		body, status := postMessagesStream(t, proxy)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (withIDs=%v)\n%s", status, withIDs, body)
		}
		return body
	}

	if got := run(true); !strings.Contains(got, `"name":"Bash"`) {
		t.Fatalf("control: the second tool call vanished even WITH ids — the premise of this finding is wrong\n%s", got)
	}
	if got := run(false); !strings.Contains(got, `"name":"Bash"`) {
		t.Errorf("the second of two parallel tool calls never reached the client: with no id on either, StreamConverter's dedup (anthropic/anthropic.go:926, keyed by tc.ID) sees both as the same empty-string call and skips the second — the agent runs one call and silently loses the other\n%s", got)
	}
}
