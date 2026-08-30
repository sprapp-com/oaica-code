package main

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleTiers is the shape the production config uses: $0.05/M up to 32k,
// $0.06/M up to 128k, $0.10/M above (expressed per token). The steep top
// bracket is the point -- >128k prompts are what monopolize chunked prefill.
func sampleTiers() []gwPricingTier {
	return []gwPricingTier{
		{UpToPromptTokens: 32000, Prompt: "0.00000005"},
		{UpToPromptTokens: 128000, Prompt: "0.00000006"},
		{Prompt: "0.0000001"},
	}
}

func closeEnough(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestSelectPricingTier_BracketSelection(t *testing.T) {
	tiers := sampleTiers()
	for _, tc := range []struct {
		name      string
		prompt    int
		wantRate  string
		wantBound int
	}{
		{"zero tokens", 0, "0.00000005", 32000},
		{"well inside first", 1000, "0.00000005", 32000},
		{"exactly at first bound is still first", 32000, "0.00000005", 32000},
		{"one past first bound falls to second", 32001, "0.00000006", 128000},
		{"exactly at second bound is still second", 128000, "0.00000006", 128000},
		{"one past second bound falls to catch-all", 128001, "0.0000001", 0},
		{"real-world 253958 hits catch-all", 253958, "0.0000001", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, bound, ok := selectPricingTier(tiers, tc.prompt)
			if !ok {
				t.Fatalf("no bracket matched %d prompt tokens", tc.prompt)
			}
			if got.Prompt != tc.wantRate {
				t.Errorf("rate = %q, want %q", got.Prompt, tc.wantRate)
			}
			if bound != tc.wantBound {
				t.Errorf("price_tier = %d, want %d", bound, tc.wantBound)
			}
		})
	}
}

func TestComputeCostUSDTiered_BracketRateAppliesToUncachedOnly(t *testing.T) {
	p := gwPricing{Prompt: "0.00000006", Completion: "0.00000028", CachedPrompt: "0.000000008"}
	tiers := sampleTiers()
	for _, tc := range []struct {
		name                       string
		prompt, cached, completion int
		wantCost                   float64
		wantTier                   int
	}{
		{
			// Cached tokens never take the bracket rate: a cache hit skips
			// prefill, so context size doesn't change what it costs us.
			name: "first bracket, half cached", prompt: 20000, cached: 10000, completion: 100,
			wantCost: 10000*0.00000005 + 10000*0.000000008 + 100*0.00000028, wantTier: 32000,
		},
		{
			name: "boundary 32000 bills at first bracket", prompt: 32000, cached: 0, completion: 10,
			wantCost: 32000*0.00000005 + 10*0.00000028, wantTier: 32000,
		},
		{
			name: "boundary 32001 bills at second bracket", prompt: 32001, cached: 0, completion: 10,
			wantCost: 32001*0.00000006 + 10*0.00000028, wantTier: 128000,
		},
		{
			// Bracket is chosen by TOTAL prompt tokens including cached ones
			// (context length is what makes it expensive to schedule), even
			// though only the uncached ones bill at that rate.
			name: "catch-all chosen on total, mostly cached", prompt: 200000, cached: 199000, completion: 5,
			wantCost: 1000*0.0000001 + 199000*0.000000008 + 5*0.00000028, wantTier: 0,
		},
		{
			name: "no tiers falls back to flat prompt rate", prompt: 200000, cached: 0, completion: 5,
			wantCost: 200000*0.00000006 + 5*0.00000028, wantTier: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := tiers
			if strings.HasPrefix(tc.name, "no tiers") {
				ts = nil
			}
			cost, tier := computeCostUSDTiered(p, ts, tc.prompt, tc.cached, tc.completion)
			if !closeEnough(cost, tc.wantCost) {
				t.Errorf("cost = %.12f, want %.12f", cost, tc.wantCost)
			}
			if tier != tc.wantTier {
				t.Errorf("price_tier = %d, want %d", tier, tc.wantTier)
			}
		})
	}
}

// TestComputeCostUSDTiered_GoldenRealRequest pins one real production-shaped
// request end to end: 253,958 prompt tokens of which 251,920 were a prefix
// cache hit, 543 completion. Catch-all bracket (253958 > 128000), so the
// 2,038 uncached tokens bill at $0.10/M while the cache hit stays at the
// flat cached rate.
func TestComputeCostUSDTiered_GoldenRealRequest(t *testing.T) {
	p := gwPricing{Prompt: "0.00000006", Completion: "0.00000028", CachedPrompt: "0.000000008"}
	cost, tier := computeCostUSDTiered(p, sampleTiers(), 253958, 251920, 543)
	want := (253958-251920)*0.0000001 + 251920*0.000000008 + 543*0.00000028
	if !closeEnough(cost, want) {
		t.Fatalf("cost = %.12f, want %.12f", cost, want)
	}
	if tier != 0 {
		t.Fatalf("price_tier = %d, want 0 (catch-all)", tier)
	}
}

// TestComputeCostUSD_UntieredUnchanged guards backward compatibility: the
// old 4-arg helper must keep behaving exactly as before.
func TestComputeCostUSD_UntieredUnchanged(t *testing.T) {
	p := gwPricing{Prompt: "0.00000006", Completion: "0.00000028", CachedPrompt: "0.000000008"}
	got := computeCostUSD(p, 1000, 400, 50)
	want := 600*0.00000006 + 400*0.000000008 + 50*0.00000028
	if !closeEnough(got, want) {
		t.Fatalf("cost = %.12f, want %.12f", got, want)
	}
}

func TestValidatePricingTiers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tiers   []gwPricingTier
		wantErr string
	}{
		{"absent is fine", nil, ""},
		{"valid three-bracket", sampleTiers(), ""},
		{"single catch-all", []gwPricingTier{{Prompt: "0.0000001"}}, ""},
		{
			"non-ascending bounds",
			[]gwPricingTier{{UpToPromptTokens: 128000, Prompt: "0.00000005"}, {UpToPromptTokens: 32000, Prompt: "0.00000006"}, {Prompt: "0.0000001"}},
			"strictly greater",
		},
		{
			"equal bounds",
			[]gwPricingTier{{UpToPromptTokens: 32000, Prompt: "0.00000005"}, {UpToPromptTokens: 32000, Prompt: "0.00000006"}, {Prompt: "0.0000001"}},
			"strictly greater",
		},
		{
			"no catch-all",
			[]gwPricingTier{{UpToPromptTokens: 32000, Prompt: "0.00000005"}, {UpToPromptTokens: 128000, Prompt: "0.00000006"}},
			"must omit up_to_prompt_tokens",
		},
		{
			"two catch-alls",
			[]gwPricingTier{{Prompt: "0.00000005"}, {Prompt: "0.0000001"}},
			"only the LAST entry",
		},
		{
			"bad decimal",
			[]gwPricingTier{{UpToPromptTokens: 32000, Prompt: "cheap"}, {Prompt: "0.0000001"}},
			"not a decimal number",
		},
		{
			"zero rate",
			[]gwPricingTier{{UpToPromptTokens: 32000, Prompt: "0"}, {Prompt: "0.0000001"}},
			"must be positive",
		},
		{
			"negative rate",
			[]gwPricingTier{{UpToPromptTokens: 32000, Prompt: "-0.001"}, {Prompt: "0.0000001"}},
			"must be positive",
		},
		{
			"negative bound",
			[]gwPricingTier{{UpToPromptTokens: -5, Prompt: "0.00000005"}, {Prompt: "0.0000001"}},
			"must be positive",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePricingTiers(tc.tiers)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected rejection containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoadConfig_RejectsBadPricingTiers proves the validation actually runs
// at config load (a reload with a broken tier table must be refused, not
// applied and then mispriced silently).
func TestLoadConfig_RejectsBadPricingTiers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tiers   string
		wantErr bool
	}{
		{"valid", `[{"up_to_prompt_tokens":32000,"prompt":"0.00000005"},{"prompt":"0.0000001"}]`, false},
		{"non-ascending", `[{"up_to_prompt_tokens":128000,"prompt":"0.00000005"},{"up_to_prompt_tokens":32000,"prompt":"0.00000006"},{"prompt":"0.0000001"}]`, true},
		{"no catch-all", `[{"up_to_prompt_tokens":32000,"prompt":"0.00000005"}]`, true},
		{"two catch-alls", `[{"prompt":"0.00000005"},{"prompt":"0.0000001"}]`, true},
		{"bad decimal", `[{"up_to_prompt_tokens":32000,"prompt":"free"},{"prompt":"0.0000001"}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "cfg.json")
			js := `{"upstream_addr":"http://127.0.0.1:30098","ledger_path":"/tmp/l.jsonl",
			 "api_keys":[{"sha256":"` + keyHash("sk") + `","label":"x"}],
			 "models":[{"id":"a","owned_by":"oaica","pricing":{"prompt":"0.00000006","completion":"0.00000028"},
			  "pricing_tiers":` + tc.tiers + `}]}`
			if err := os.WriteFile(p, []byte(js), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig(p)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected loadConfig to reject tiers %s", tc.tiers)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if len(cfg.Models[0].PricingTiers) != 2 {
				t.Fatalf("tiers = %d, want 2", len(cfg.Models[0].PricingTiers))
			}
		})
	}
}

// ---- priority keys ----------------------------------------------------

// newTestGatewayWithKeys mirrors newTestGatewayWithAdmission but lets the
// test supply the key set (so a priority key and a normal key can race the
// same saturated pool).
func newTestGatewayWithKeys(t *testing.T, upstream string, threshold, maxConcurrent int, keys []gwKey) *gateway {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg := gwConfig{
		UpstreamAddr: upstream, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: keys,
		Models: []gwModel{{
			ID: "kat-awq", UpstreamID: "kat-awq-served", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
		LargeContextTokenThreshold: threshold,
		MaxConcurrentLargeContext:  maxConcurrent,
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return g
}

// TestAdmission_PriorityKeyBypassesSaturatedPool: with the only pool slot
// held, a normal key gets the 429 while a priority key sails through. This
// is the whole product: priority buys never-waiting.
func TestAdmission_PriorityKeyBypassesSaturatedPool(t *testing.T) {
	release := make(chan struct{})
	holding := make(chan struct{}, 10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		holding <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"4"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer upstream.Close()
	g := newTestGatewayWithKeys(t, upstream.URL, 1000, 1, []gwKey{
		{SHA256: keyHash("sk-normal"), Label: "openrouter"},
		{SHA256: keyHash("sk-vip"), Label: "vip", Priority: true},
	})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postCompletionWithMessages(t, g, "sk-normal", largeMessages(5000)) }()
	<-holding // the single pool slot is now held

	// Normal key: rejected, pool is full (unchanged behavior).
	if w := postCompletionWithMessages(t, g, "sk-normal", largeMessages(5000)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("normal key with pool full: status = %d, want 429", w.Code)
	}

	// Priority key: served concurrently despite the saturated pool. It runs
	// in a goroutine because the fake upstream holds every request until
	// release; <-holding proves it actually reached the backend rather than
	// being queued at the gate.
	vip := make(chan *httptest.ResponseRecorder, 1)
	go func() { vip <- postCompletionWithMessages(t, g, "sk-vip", largeMessages(5000)) }()
	<-holding // priority request reached upstream while the pool was full

	close(release)
	if w := <-vip; w.Code != http.StatusOK {
		t.Fatalf("priority key: status = %d, want 200", w.Code)
	}
	if w := <-done; w.Code != http.StatusOK {
		t.Fatalf("first held request: status = %d, want 200", w.Code)
	}
}

func TestPriorityKeyCount(t *testing.T) {
	cfg := gwConfig{APIKeys: []gwKey{
		{Label: "a"}, {Label: "b", Priority: true}, {Label: "c", Priority: true},
	}}
	if got := priorityKeyCount(cfg); got != 2 {
		t.Fatalf("priorityKeyCount = %d, want 2", got)
	}
}

// ---- per-key output cap ------------------------------------------------

// TestPerKeyMaxCompletionTokens drives a real request through the proxy and
// inspects what the upstream actually received, so it covers the interaction
// with the model-level clamp rather than just the arithmetic.
func TestPerKeyMaxCompletionTokens(t *testing.T) {
	for _, tc := range []struct {
		name      string
		keyCap    int
		requested int
		want      float64
	}{
		// Model clamp is 4096 here (non-stream nonStreamMaxTokens).
		{"key cap lower than request wins", 512, 2000, 512},
		{"key cap absent leaves request untouched", 0, 2000, 2000},
		{"key cap higher than request never raises it", 8192, 100, 100},
		{"key cap never raises above model clamp", 32768, 30000, 4096},
		{"key cap below model clamp beats it", 1024, 30000, 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			upstream := fakeUpstream(t, &got)
			g := newTestGatewayWithKeys(t, upstream.URL, -1, 0, []gwKey{
				{SHA256: keyHash("sk-capped"), Label: "capped", MaxCompletionTokens: tc.keyCap},
			})
			body, _ := json.Marshal(map[string]any{
				"model": "kat-awq", "max_tokens": tc.requested,
				"messages": []map[string]any{{"role": "user", "content": "hi"}},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
			req.Header.Set("Authorization", "Bearer sk-capped")
			w := httptest.NewRecorder()
			mux(g).ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if v, _ := got["max_tokens"].(float64); v != tc.want {
				t.Fatalf("upstream saw max_tokens = %v, want %v", got["max_tokens"], tc.want)
			}
		})
	}
}
