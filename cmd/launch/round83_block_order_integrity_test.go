package launch

// round83_block_order_integrity_test.go — leg 2, F83-L2-1, and the reversal of
// F83-L2-2 (2026-09-28 audit, rounds 83 and 84).
//
// One turn, two spellings. When the vendor writes the turn as a whole
// completion the gateway adopts it and writes the message in the frame's own
// order; when it writes the same turn as deltas this arm accumulates the calls
// and flushes them at the turn's end, writing the prose as it arrives. Two
// ways the same body reached the client differently:
//
//   - A whole completion arriving after a fragment: the early gate that opened
//     the adoption closed at the first emitted event, and a fragment that
//     relayed anything — even bytes the turn had nowhere else to put — emitted
//     one. The whole completion behind it was refused and its content, the
//     model's answer, was never written (F83-L2-1, still pinned below).
//   - The block order: round 83 read a finished call followed by prose as "the
//     wire finished the call first, so the call block goes first" and flushed
//     settled calls when prose arrived. Round 84 measured the premise false —
//     the document arm writes the prose first — and the flush moved this arm
//     off the document arm for that very turn, so the flush is reverted and the
//     pin below reads the other way (R84-L2-1).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r83OrderArm drives one SSE script and reports the verdict plus the block
// sequence the client was handed (a block per content_block_start, in order)
// with each tool block's joined input.
func r83OrderArm(t *testing.T, frames []string) (int, string, string) {
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
	code, body := round54PostMessage(t, proxy, "glm-5.3", true)
	if code != http.StatusOK {
		return code, "<refused>", ""
	}
	var order []string
	var text string
	var args string
	var cur *round68Block
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(raw), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			order = append(order, ev.ContentBlock.Type)
			if ev.ContentBlock.Type == "tool_use" {
				cur = &round68Block{Name: ev.ContentBlock.Name}
			} else {
				cur = nil
			}
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" {
				text += ev.Delta.Text
			}
			if cur != nil && ev.Delta.Type == "input_json_delta" {
				cur.Args += ev.Delta.PartialJSON
				args = cur.Args
			}
		}
	}
	return code, "blocks=" + strings.Join(order, ",") + ` text="` + text + `"`, args
}

// TestAWholeCompletionAfterAFragmentIsStillAdopted is F83-L2-1. The fragment
// states a nameless entry whose bytes the turn has nowhere else to put, so the
// arm relays them as prose — that is an emitted event, and it used to close the
// window in which the whole completion behind it could be adopted. The answer
// that frame carried was lost: the same wire answered `hello{"a":1}` one way
// round and no `hello` at all the other.
func TestAWholeCompletionAfterAFragmentIsStillAdopted(t *testing.T) {
	frag := `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"","arguments":"{\"a\":1}"}}]}}]}`
	whole := `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`

	_, first, _ := r83OrderArm(t, []string{frag, whole, r81Done})
	_, second, _ := r83OrderArm(t, []string{whole, frag, r81Done})
	if first != second {
		t.Errorf("one wire, two frame orders, two answers: %s then %s gives %s, %s then %s gives %s — the whole completion is the model's answer and the fragment before it, whose bytes the turn has nowhere else to put, must not close the window it is adopted in (2026-09-28 audit, round 83, F83-L2-1)", frag, whole, first, whole, frag, second)
	}
	if !strings.Contains(first, `text="hello`) {
		t.Errorf("fragment then whole completion = %s: the content the whole frame carried was never written (2026-09-28 audit, round 83, F83-L2-1)", first)
	}
}

// TestAFinishedCallStandsWhereTheDocumentArmPutsIt REVERSES F83-L2-2. Round 83
// read this turn as "the wire finished the call before it wrote the prose, so
// the call block goes first", and flushed settled calls when prose arrived. The
// premise was that the document arm writes calls before prose; measured, it
// writes the prose first (the converter's fixed thinking→text→tool_use order,
// F68-L1-1), so the flush moved the delta arm OFF the document arm for this
// very turn — streamed [tool_use,text] where the same body as a non-stream
// request, as one whole frame carrying both, and as a fragment-then-whole-frame
// run all answer [text,tool_use] (2026-09-28 audit, round 84, R84-L2-1). The
// call is held to the turn's end, as it was before round 83.
func TestAFinishedCallStandsWhereTheDocumentArmPutsIt(t *testing.T) {
	callDelta := `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`
	textDelta := `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"a"}}]}`
	fin := `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`

	_, deltas, args := r83OrderArm(t, []string{callDelta, textDelta, fin, r81Done})
	want := "text,tool_use"
	if got := strings.TrimPrefix(deltas, `blocks=`); !strings.HasPrefix(got, want) {
		t.Errorf("a finished call followed by prose streamed as %s, want %s — the prose block comes first because that is where this leg's own non-stream arm and the converter's whole-message arm put it, and the two spellings of one turn may not reach the client in different orders (2026-09-28 audit, round 84, R84-L2-1; reverses round 83's F83-L2-2)", deltas, want)
	}
	if args != `{"a":1}` {
		t.Errorf("the emitted call's input is %q, want the wire's own `{\"a\":1}` (2026-09-28 audit, round 83)", args)
	}
}

// TestACallStillBeingWrittenIsNotEmittedEarly guards the boundary the round-83
// flush drew (and round 84's reversal keeps for a different reason). The wire
// has not finished this call's arguments when the prose arrives, so the call
// stays where it was and is written when its own bytes are done: emitting it
// early would hand the client half an argument string as a complete,
// executable call (rounds 16, 17).
func TestACallStillBeingWrittenIsNotEmittedEarly(t *testing.T) {
	open := `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"Bash","arguments":"{\"a\":"}}]}}]}`
	textDelta := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"a"}}]}`
	close := `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`
	fin := `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`

	_, got, args := r83OrderArm(t, []string{open, textDelta, close, fin, r81Done})
	if !strings.HasPrefix(strings.TrimPrefix(got, `blocks=`), "text,tool_use") {
		t.Errorf("mid-object call then prose = %s, want the prose first and the call written once its bytes are done: a call the wire was still writing is not a call this arm may hand over early (2026-09-28 audit, round 83)", got)
	}
	if args != `{"a":1}` {
		t.Errorf("the call's input is %q, want the whole `{\"a\":1}` the wire finished writing (2026-09-28 audit, round 83)", args)
	}
}

// TestAFreeformCallIsNotEmittedEarly is the same guard for the other argument
// shape: a freeform line is not a complete JSON object, so it is not a call the
// wire has moved past either (round 51's G1 — the model's whole command,
// delivered once).
func TestAFreeformCallIsNotEmittedEarly(t *testing.T) {
	freeform := `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"Bash","arguments":"ls -la"}}]}}]}`
	textDelta := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"a"}}]}`
	fin := `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`

	_, got, args := r83OrderArm(t, []string{freeform, textDelta, fin, r81Done})
	if !strings.HasPrefix(strings.TrimPrefix(got, `blocks=`), "text,tool_use") {
		t.Errorf("freeform call then prose = %s, want the prose first: an argument text that is not a finished object is not a call the wire has moved past (2026-09-28 audit, round 83)", got)
	}
	if args != `{"_raw":"ls -la"}` {
		t.Errorf("the call's input is %q, want the freeform line the wire wrote, wrapped the way this leg wraps argument text that is not an object (2026-09-28 audit, round 83)", args)
	}
}
