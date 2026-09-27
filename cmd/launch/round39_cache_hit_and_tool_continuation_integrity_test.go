package launch

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAStatedCacheHitSurvivesAnUnstatedPrompt is A-F4. cachedTokens clamps the
// hit to the prompt size, and an upstream that narrates the hit while never
// stating prompt_tokens (or stating it negative, which is no statement either)
// clamped it to 0: the client believed the whole prompt was fresh input while
// the ledger row for the same turn recorded the hit — the round-38 B-F1
// disagreement, one chunk ordering away from the case that was fixed.
func TestAStatedCacheHitSurvivesAnUnstatedPrompt(t *testing.T) {
	cases := []struct {
		name  string
		usage openAIUsage
		want  int
	}{
		{
			name:  "prompt unstated, hit in the sibling field",
			usage: openAIUsage{PromptCacheHitTokens: 900},
			want:  900,
		},
		{
			name:  "prompt stated negative is not a measurement",
			usage: openAIUsage{PromptTokens: -5, PromptCacheHitTokens: 900},
			want:  900,
		},
		{
			name:  "hit above a stated prompt is clamped to it",
			usage: openAIUsage{PromptTokens: 1000, PromptCacheHitTokens: 4096},
			want:  1000,
		},
		{
			name:  "nothing stated is nothing",
			usage: openAIUsage{},
			want:  0,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			u := tt.usage
			if got := u.cachedTokens(); got != tt.want {
				t.Errorf("cachedTokens() = %d, want %d\nreturning 0 threw a stated hit away: the client reads a full-prompt fresh input for a turn the cache served", got, tt.want)
			}
		})
	}
}

// TestABareRepeatOfTheNameBeginsTheNextCall is B-F9, the client leg's half of
// the gateway's toolKey rule. An empty argument string is a COMPLETE argument
// list for a call that takes none, so a fragment that names the call again over
// a call that has accumulated none is the second of two argument-less calls:
// read as a continuation, the client is handed one tool_use where the model
// asked for two.
func TestABareRepeatOfTheNameBeginsTheNextCall(t *testing.T) {
	cases := []struct {
		name                        string
		accID, accName, accArgs     string
		deltaID, deltaName, deltaAr string
		want                        bool
	}{
		{
			name: "no call accumulated yet",
			want: false,
		},
		{
			name:      "bare repeat of the same name over no arguments",
			accID:     "c1",
			accName:   "Bash",
			deltaName: "Bash",
			want:      true,
		},
		{
			name:    "repeat of the same name over COMPLETE arguments",
			accID:   "c1",
			accName: "Bash",
			accArgs: `{"cmd":"ls"}`,
			deltaID: "c2",
			want:    true,
		},
		{
			name:      "arguments continuing an unfinished object",
			accID:     "c1",
			accName:   "Bash",
			accArgs:   `{"cmd":`,
			deltaName: "Bash",
			deltaAr:   `"ls"}`,
			want:      false,
		},
		{
			name:      "arguments arriving after a name-only fragment",
			accID:     "c1",
			accName:   "Bash",
			deltaName: "Bash",
			deltaAr:   `{"cmd":"ls"}`,
			want:      false,
		},
		{
			name:    "a different id is the next call",
			accID:   "c1",
			accName: "Bash",
			deltaID: "c2",
			want:    true,
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

// TestAStatedCacheHitOverAnUnstatedPromptReachesTheClient is A-F4 at the wire,
// which is where it was felt. cachedTokens now returns a stated hit an upstream
// never paired with a prompt size, but the client only ever sees what
// UsageFromMetrics derives from api.Metrics — and it derives input_tokens as
// total minus the hit, clamped to the total. Reported with a zero total, the hit
// is clamped away and the client is told a cache-served turn was a full-prompt
// fresh input read: the "usage through oaica looks much higher than native"
// complaint, and the exact disagreement with the gateway's ledger that B-F4
// fixed on the other leg.
//
// The invariant is the sum: a turn reporting a 900-token hit must report the
// same real prompt length as the identical turn reporting no usage at all, with
// the 900 carved out of it rather than added to it or thrown away.
func TestAStatedCacheHitOverAnUnstatedPromptReachesTheClient(t *testing.T) {
	hit := jsonUpstream(t, `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_cache_hit_tokens":900}}`)
	defer hit.Close()
	silent := jsonUpstream(t, `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	defer silent.Close()

	type usage struct {
		InputTokens          int `json:"input_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
	}
	ask := func(up *httptest.Server, session string) usage {
		t.Helper()
		proxy := startCalibProxy(t, up.URL, session)
		resp, err := http.Post(proxy+"/v1/messages", "application/json",
			strings.NewReader(string(calibMessagesBody(t, 20000, 64, false))))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200\n%s", resp.StatusCode, b)
		}
		var got struct {
			Usage usage `json:"usage"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("decode: %v\n%s", err, b)
		}
		return got.Usage
	}

	withHit := ask(hit, "sess-round39-unstated-hit")
	without := ask(silent, "sess-round39-unstated-none")

	if withHit.CacheReadInputTokens != 900 {
		t.Errorf("cache_read_input_tokens = %d, want the stated 900: the number the upstream narrated over a prompt it never sized is a measurement, and the session's cache read-out is wrong without it\n%+v", withHit.CacheReadInputTokens, withHit)
	}
	if withHit.InputTokens <= 0 {
		t.Errorf("input_tokens = %d for a turn whose prompt was real: the hit was clamped against a total that stated nothing, so the client reads a cache-served prompt as an empty one\n%+v", withHit.InputTokens, withHit)
	}
	if sum, want := withHit.InputTokens+withHit.CacheReadInputTokens, without.InputTokens; sum != want {
		t.Errorf("input+cache_read = %d (input %d, hit %d), want the %d the same turn reports with no usage at all: the hit must be carved out of the prompt, not added to it or discarded\n%+v", sum, withHit.InputTokens, withHit.CacheReadInputTokens, want, withHit)
	}
}
