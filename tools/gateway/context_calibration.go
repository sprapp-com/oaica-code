package main

import (
	"fmt"
	"regexp"
	"strconv"
	"sync"
)

// Per-session prompt-size calibration for the context-length-fit clamp.
//
// WHY this exists (real 2026-08-29 incident, client ".46" on oaica 0.4.5):
// the clamp estimated the prompt as len(body)/4 and then added a 30%
// proportional margin. On an ~806 KB Claude Code conversation body that is
// 201,670 estimated tokens; 201,670 x 1.30 = 262,171 > the model's 262,144
// context window, so the CLIENT proxy rejected the request with 400 before
// it ever left the machine. The REAL prompt size (vLLM's
// usage.prompt_tokens for that same session's previous turn) was ~243,000 --
// there were still ~19,000 tokens of room. Worse, the rejected request was
// Claude Code's OWN auto-compaction call, i.e. the one request that would
// have SHRUNK the session: Claude Code logged "Error during compaction: API
// Error: 400 prompt is too large ..." and retried forever. The session could
// not save itself.
//
// chars/4 is wrong in BOTH directions (it under-estimated by 26% in the
// 2026-08-29 gateway recurrence, and over-estimated here once the 30% margin
// was stacked on top), and a single fudge factor cannot fix both. But we do
// not have to guess: every successful upstream response tells us the REAL
// prompt_tokens for a body whose byte size we know. Consecutive turns of one
// conversation share almost all of their content, so tokens-per-byte for a
// given session is extremely stable -- calibrating on the previous turn
// turns the estimate from a heuristic into near-measurement.
//
// WHY the calibrated margin is a fraction of the DELTA, not of the total
// (real 2026-08-30 16:13-16:17 UTC incident, client ".46" on oaica 0.4.7):
// a session sat at 253,958 real prompt tokens (vLLM usage) of a 262,144
// window, prefix-cached. Claude Code's auto-compaction call was 714,483
// body bytes, only ~2.5 KB more than the previous turn, and the calibrated
// estimate was ~254.8k -- correct. But the margin was then 3% of that WHOLE
// estimate, ~7.6k tokens, so 262,144 - 254.8k - 7.6k came out under the
// 16-token minimum and the clamp rejected the request with "prompt is too
// long" eleven times running. The session died with "Context limit reached"
// while holding ~7k tokens of genuine headroom -- again on the one request
// that would have shrunk it.
//
// The fix is to size the margin to what is actually uncertain. When a
// calibration exists, the shared prefix of the two turns is MEASURED, not
// estimated: the only part carrying tokenizer-model error is the
// turn-to-turn delta (a new user message plus tool results). So the margin
// is uncalibratedMarginRatio (30% -- the same fraction we trust to bound a
// fully unmeasured prompt) applied to the delta alone, floored at
// calibratedMarginFloor. On the incident above that is 30% of ~900 delta
// tokens = ~265, floored to 512, leaving a ~6.8k budget: the compaction
// call (which needs ~2k of output) would have been forwarded with
// max_tokens clamped to that budget, and would have succeeded.
//
// Falling back to the old chars/4 + 30% behaviour is deliberate: the first
// request of a session has nothing to calibrate against, and being coarse
// but safe there is exactly the old (well-tested) trade-off.

// calibratedMarginFloor is the smallest margin the calibrated path will
// use, so that a delta of zero bytes (a retry of the identical body) still
// leaves a little slack for anything the byte ratio cannot see.
const calibratedMarginFloor = 512

// uncalibratedMarginRatio / uncalibratedMarginFloor are the ORIGINAL
// (pre-calibration) values, kept verbatim -- see the clamp's own comment in
// main.go for the 2026-08-29 recurrence that set 30%.
// They now apply only to the first request of a session.
const uncalibratedMarginRatio = 0.30
const uncalibratedMarginFloor = 4096

// Sanity bounds on a calibrated ratio, in tokens per request byte. Real
// values sit around 0.25-0.35 for JSON-wrapped chat traffic. Anything
// outside this band means the pairing was bogus (a mismatched response, a
// body that was not the prompt, a zero) -- ignore it and fall back rather
// than trust a number that could clamp every future request to nothing.
const calibMinRatio = 0.1
const calibMaxRatio = 1.0

// maxCalibratedSessions bounds the map so a long-lived proxy (or the
// gateway, which sees every client) cannot grow it without limit. Eviction
// is oldest-last-seen-first.
const maxCalibratedSessions = 4096

type calibSample struct {
	bodyBytes    int
	promptTokens int
	seq          uint64 // monotonic last-seen stamp, for eviction
}

// promptCalibrator maps a session key to the most recent
// (request body bytes, real upstream prompt_tokens) pair for that session.
type promptCalibrator struct {
	mu      sync.Mutex
	seq     uint64
	max     int
	samples map[string]calibSample
}

func newPromptCalibrator(max int) *promptCalibrator {
	if max <= 0 {
		max = maxCalibratedSessions
	}
	return &promptCalibrator{max: max, samples: map[string]calibSample{}}
}

// record stores a ground-truth pair for key. Callers must only pass counts
// that came from a real, successful upstream usage report (a non-stream
// usage object, a stream's final usage chunk, or an upstream context-overflow
// error that states the real message-token count) -- never a guess, and
// never a value read off an error response that had no usage.
func (c *promptCalibrator) record(key string, bodyBytes, promptTokens int) {
	if c == nil || key == "" || bodyBytes <= 0 || promptTokens <= 0 {
		return
	}
	ratio := float64(promptTokens) / float64(bodyBytes)
	if ratio < calibMinRatio || ratio > calibMaxRatio {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	if _, exists := c.samples[key]; !exists && len(c.samples) >= c.max {
		c.evictOldestLocked()
	}
	c.samples[key] = calibSample{bodyBytes: bodyBytes, promptTokens: promptTokens, seq: c.seq}
}

func (c *promptCalibrator) evictOldestLocked() {
	oldestKey := ""
	var oldestSeq uint64
	for k, s := range c.samples {
		if oldestKey == "" || s.seq < oldestSeq {
			oldestKey, oldestSeq = k, s.seq
		}
	}
	if oldestKey != "" {
		delete(c.samples, oldestKey)
	}
}

// estimate returns the calibrated prompt-token estimate for a body of
// bodyBytes on this session, the byte size of the SAMPLE that estimate was
// derived from, and whether a calibration was available. Callers need the
// sample size to size their safety margin: see contextFitPlan.
func (c *promptCalibrator) estimate(key string, bodyBytes int) (est, sampleBytes int, ok bool) {
	if c == nil || key == "" || bodyBytes <= 0 {
		return 0, 0, false
	}
	c.mu.Lock()
	s, ok := c.samples[key]
	c.mu.Unlock()
	if !ok || s.bodyBytes <= 0 || s.promptTokens <= 0 {
		return 0, 0, false
	}
	// Integer math (widened to int64 so the product cannot overflow on a
	// ~1 MB body): float rounding would make the common "same body size as
	// last turn" case land one token off its own measured count, which is
	// confusing in the error message for no benefit.
	est = int(int64(bodyBytes) * int64(s.promptTokens) / int64(s.bodyBytes))
	if est <= 0 {
		return 0, 0, false
	}
	return est, s.bodyBytes, true
}

// contextFitPlan returns the prompt-token estimate and the safety margin to
// use for one request: measured-and-tight when the session has a
// calibration, coarse-and-generous (the pre-2026-08-30 behaviour, byte for
// byte) when it does not.
func contextFitPlan(c *promptCalibrator, key string, bodyBytes int) (est, margin int, calibrated bool) {
	if e, sampleBytes, ok := c.estimate(key, bodyBytes); ok {
		// Only the turn-to-turn delta is unmeasured, so only the delta gets
		// a tokenizer-error margin -- see the 2026-08-30 incident at the top
		// of this file. deltaTokens uses the session's own measured ratio
		// (e/bodyBytes is exactly sample.promptTokens/sample.bodyBytes).
		deltaBytes := bodyBytes - sampleBytes
		if deltaBytes < 0 {
			deltaBytes = -deltaBytes
		}
		deltaTokens := int(int64(deltaBytes) * int64(e) / int64(bodyBytes))
		margin = int(float64(deltaTokens) * uncalibratedMarginRatio)
		if margin < calibratedMarginFloor {
			margin = calibratedMarginFloor
		}
		return e, margin, true
	}
	est = bodyBytes / 4
	margin = int(float64(est) * uncalibratedMarginRatio)
	if margin < uncalibratedMarginFloor {
		margin = uncalibratedMarginFloor
	}
	return est, margin, false
}

// promptTooLongMessage renders the rejection in Anthropic's OWN wording.
// This matters for behaviour, not cosmetics: Claude Code pattern-matches
// "prompt is too long: N tokens > M maximum" to trigger its context-recovery
// path (drop old turns, retry smaller). The 2026-08-29 incident's rejection
// said "prompt is too large to fit this model's ...-token context window",
// which Claude Code did not recognise -- so it treated a structural,
// permanently-fatal condition as a transient API error and retried the
// identical compaction call in a loop. The tail after the semicolon is ours
// and is free-form; the leading sentence is a contract.
func promptTooLongMessage(estTokens, maxTokens int) string {
	return fmt.Sprintf("prompt is too long: %d tokens > %d maximum; reduce the prompt or compact the conversation",
		estTokens, maxTokens)
}

// upstreamContextOverflowRE matches vLLM's context-overflow 400, e.g.
// "This model's maximum context length is 262144 tokens. However, you
// requested 262145 tokens (230145 in the messages, 32000 in the completion).
// Please reduce the length of the messages or completion."
// The parenthesised message-token count is the GROUND TRUTH prompt size --
// better than any estimate we can compute, and free.
var upstreamContextOverflowRE = regexp.MustCompile(
	`(?s)maximum context length is (\d+) tokens.*?requested (\d+) tokens(?:.*?(\d+) in the messages)?`)

// parseUpstreamContextOverflow extracts (promptTokens, maxTokens) from an
// upstream context-overflow error body. promptTokens is the reported
// in-the-messages count when present, else the total requested count.
func parseUpstreamContextOverflow(body string) (promptTokens, maxTokens int, ok bool) {
	m := upstreamContextOverflowRE.FindStringSubmatch(body)
	if m == nil {
		return 0, 0, false
	}
	maxTokens, _ = strconv.Atoi(m[1])
	promptTokens, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		if inMessages, err := strconv.Atoi(m[3]); err == nil && inMessages > 0 {
			promptTokens = inMessages
		}
	}
	if maxTokens <= 0 || promptTokens <= 0 {
		return 0, 0, false
	}
	return promptTokens, maxTokens, true
}
