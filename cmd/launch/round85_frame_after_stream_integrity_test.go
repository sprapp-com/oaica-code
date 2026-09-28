package launch

// round85_frame_after_stream_integrity_test.go — leg 2, R85-L2-1 … R85-L2-4
// (2026-09-28 audit, round 85).
//
// The client proxy reads an upstream turn off one of two spellings: delta
// frames, which it folds into accumulators, and a whole-completion document
// carried in a single frame, which it adopts. Round 84 closed the case where an
// adopted document REPEATS what the deltas already wrote. Round 85 closes the
// case where the document arrives while the deltas have already written
// something — prose, a finished call, a usage statement — and the two spellings
// were not answering the same turn:
//
//   - the frame was adopted and its calls were emitted BEFORE the accumulators
//     flushed, so `[c1 delta][frame with c2]` answered [c1 c2] one way and
//     [c2 c1] the other (R85-L2-3);
//   - a frame arriving after the stream had started was adopted with the
//     accumulator state left behind, so a second, whole statement of the turn
//     was dropped (R85-L2-1);
//   - a usage statement the stream had already made was overwritten by the
//     frame's own (nil) usage rather than merged with it, so a body whose usage
//     arrived in its own chunk lost it (R85-L2-2);
//   - an entry the upstream never named was matched against the accumulators by
//     its (empty) name, so a nameless entry the stream had already relayed as
//     text was held back as a repeat and its bytes were lost (R85-L2-4).
//
// The rule the pins below hold to is the leg's own: one upstream body, whatever
// the two spellings are called, answers one turn — same block order, same call
// order, same ids, same usage, same relayed bytes.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// r85Body relays one SSE script through the proxy and returns the client body.
func r85Body(t *testing.T, frames []string) string {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	script := strings.Join(frames, "\n\n") + "\n\n"
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
	_, body := round54PostMessage(t, proxy, "glm-5.3", true)
	return body
}

type r85Event struct {
	Type         string `json:"type"`
	ContentBlock struct {
		Type string `json:"type"`
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"content_block"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
	Message struct {
		Usage map[string]any `json:"usage"`
	} `json:"message"`
	Usage map[string]any `json:"usage"`
}

func r85Events(t *testing.T, body string) []r85Event {
	t.Helper()
	var evs []r85Event
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev r85Event
		if json.Unmarshal([]byte(raw), &ev) != nil {
			continue
		}
		evs = append(evs, ev)
	}
	return evs
}

// r85Blocks is the turn's block order, and its calls as name/id in that order.
func r85Blocks(t *testing.T, body string) (string, []string) {
	t.Helper()
	var order []string
	var calls []string
	for _, ev := range r85Events(t, body) {
		if ev.Type != "content_block_start" {
			continue
		}
		order = append(order, ev.ContentBlock.Type)
		if ev.ContentBlock.Type == "tool_use" {
			calls = append(calls, ev.ContentBlock.Name+"/"+ev.ContentBlock.ID)
		}
	}
	return strings.Join(order, ","), calls
}

// r85Text is every text_delta byte the client was handed.
func r85Text(t *testing.T, body string) string {
	t.Helper()
	var text string
	for _, ev := range r85Events(t, body) {
		if ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" {
			text += ev.Delta.Text
		}
	}
	return text
}

// r85Usage is the usage the client ends with, later statements winning, as a
// sorted "key=value" string so two turns are comparable.
func r85Usage(t *testing.T, body string) string {
	t.Helper()
	usage := map[string]any{}
	for _, ev := range r85Events(t, body) {
		m := ev.Usage
		if ev.Type == "message_start" {
			m = ev.Message.Usage
		}
		for k, v := range m {
			usage[k] = v
		}
	}
	keys := make([]string, 0, len(usage))
	for k := range usage {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		b, _ := json.Marshal(usage[k])
		parts = append(parts, k+"="+string(b))
	}
	return strings.Join(parts, ",")
}

func r85WholeFrame(content string) string {
	return `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"` + content +
		`","tool_calls":[{"id":"c2","type":"function","function":{"name":"Read","arguments":"{\"f\":2}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`
}

var (
	r85TextDelta = `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`
	r85WhyDelta  = `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"why"}}]}`
	r85CallDelta = `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c2","type":"function","function":{"name":"Read","arguments":"{\"f\":2}"}}]}}]}`
	r85BashDelta = `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`
	r85CallsFin  = `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	r85StopFin   = `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
)

// TestAWholeFrameAfterProseAnswersTheSameTurnAsItsDeltas is R85-L2-1: the same
// turn spelled as deltas and spelled as prose-then-document must reach the
// client as the same blocks, in the same order, with the same calls.
func TestAWholeFrameAfterProseAnswersTheSameTurnAsItsDeltas(t *testing.T) {
	deltasOrder, deltasCalls := r85Blocks(t, r85Body(t, []string{r85TextDelta, r85CallDelta, r85CallsFin, r81Done}))
	for _, tc := range []struct {
		name    string
		frames  []string
		wantPre string
	}{
		{"the document alone", []string{r85WholeFrame("hi there"), r85CallsFin, r81Done}, ""},
		{"the document after prose", []string{r85TextDelta, r85WholeFrame("hi there"), r85CallsFin, r81Done}, ""},
		{"the document after reasoning", []string{r85WhyDelta, r85WholeFrame("hi there"), r85CallsFin, r81Done}, "thinking"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order, calls := r85Blocks(t, r85Body(t, tc.frames))
			want := deltasOrder
			if tc.wantPre != "" {
				want = tc.wantPre + "," + deltasOrder
			}
			if order != want {
				t.Errorf("the turn relayed %q, want %q — a whole completion that arrives after the stream has written must answer the same turn, in the same block order, as the deltas spelling of it (2026-09-28 audit, round 85, R85-L2-1)", order, want)
			}
			if len(calls) != len(deltasCalls) || (len(calls) > 0 && calls[len(calls)-1] != deltasCalls[len(deltasCalls)-1]) {
				t.Errorf("the turn's calls are %v, want the deltas spelling's %v — the document's calls are the turn's calls (2026-09-28 audit, round 85, R85-L2-1)", calls, deltasCalls)
			}
		})
	}
}

// TestAWholeFrameJoinsTheCallsTheStreamAlreadyHolds is R85-L2-3: a call the
// deltas finished, followed by a document stating the turn again with a second
// call, must reach the client in the list's own order — the order the deltas
// spelling gives — and not swept behind the document's calls. Only the calls are
// compared: the document carries prose of its own, and a text block for it is
// the document's content, not an ordering of the turn's calls.
func TestAWholeFrameJoinsTheCallsTheStreamAlreadyHolds(t *testing.T) {
	_, wantCalls := r85Blocks(t, r85Body(t, []string{r85BashDelta, r85CallDelta, r85CallsFin, r81Done}))
	order, calls := r85Blocks(t, r85Body(t, []string{r85BashDelta, r85WholeFrame("hi"), r85CallsFin, r81Done}))
	if strings.Join(calls, " ") != strings.Join(wantCalls, " ") {
		t.Errorf("the turn relayed blocks %q calls %v; its deltas spelling gives calls %v — one body, one call order, whichever spelling carries it (2026-09-28 audit, round 85, R85-L2-3)",
			order, calls, wantCalls)
	}
}

// TestUsageTheStreamStatedSurvivesAWholeFrame is R85-L2-2: usage the stream
// stated in its own chunk is not overwritten by a document that states none.
func TestUsageTheStreamStatedSurvivesAWholeFrame(t *testing.T) {
	usageOnly := `data: {"id":"c","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":4242,"completion_tokens":7}}`
	usageCached := `data: {"id":"c","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":242,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4000}}}`
	frameNoUsage := `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`

	before := r85Usage(t, r85Body(t, []string{usageOnly, frameNoUsage, r85StopFin, r81Done}))
	after := r85Usage(t, r85Body(t, []string{frameNoUsage, usageOnly, r85StopFin, r81Done}))
	if before != after {
		t.Errorf("the same usage statement reached the client as %q when it preceded the document and %q when it followed it — one body, one turn (2026-09-28 audit, round 85, R85-L2-2)", before, after)
	}
	if !strings.Contains(before, "input_tokens=4242") || !strings.Contains(before, "output_tokens=7") {
		t.Errorf("the client was handed usage %q, want the prompt_tokens=4242 and completion_tokens=7 the stream stated — a document that states no usage does not unstate the stream's (2026-09-28 audit, round 85, R85-L2-2)", before)
	}

	cachedBefore := r85Usage(t, r85Body(t, []string{usageCached, frameNoUsage, r85StopFin, r81Done}))
	cachedAfter := r85Usage(t, r85Body(t, []string{frameNoUsage, usageCached, r85StopFin, r81Done}))
	if cachedBefore != cachedAfter || !strings.Contains(cachedBefore, "cache_read_input_tokens=4000") {
		t.Errorf("a cached-token statement reached the client as %q before the document and %q after it, want the same reading carrying cache_read_input_tokens=4000 on both — prompt caching is what makes the field worth relaying (2026-09-28 audit, round 85, R85-L2-2)", cachedBefore, cachedAfter)
	}
}

// TestANamelessEntryIsRelayedAsTextWhateverSpellingCarriesIt is R85-L2-4: an
// entry the upstream never named is prose, not a call, so it is relayed as text
// once — whether the stream spelled it as a fragment, as a document, or both.
func TestANamelessEntryIsRelayedAsTextWhateverSpellingCarriesIt(t *testing.T) {
	frag := `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"","arguments":"{\"a\":1}"}}]}}]}`
	frame := `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"type":"function","function":{"name":"","arguments":"{\"a\":1}"}}]},"finish_reason":"stop"}]}`

	twice := r85Text(t, r85Body(t, []string{frag, frag, r85StopFin, r81Done}))
	for _, tc := range []struct {
		name   string
		frames []string
	}{
		{"a fragment then a document stating the same entry", []string{frag, frame, r85StopFin, r81Done}},
		{"a document then the same fragment", []string{frame, frag, r85StopFin, r81Done}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := r85Text(t, r85Body(t, tc.frames))
			if got != twice {
				t.Errorf("the turn relayed the text %q; the same two statements spelled as two fragments relay %q — an entry the upstream never named is prose, relayed once, and a document stating it again is not a repeat of a call (2026-09-28 audit, round 85, R85-L2-4)", got, twice)
			}
			if strings.Contains(got, "tool_use") || got == "" {
				t.Errorf("the relayed text is %q — the nameless entry's argument bytes are the text the client gets (2026-09-28 audit, round 85, R85-L2-4)", got)
			}
		})
	}
}
