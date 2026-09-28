package launch

// round82_adoption_refusal_and_restatement_integrity_test.go — leg 2, two
// places where which shape the upstream chose decided what the model's output
// was worth (2026-09-28 audit, round 82).
//
//  1. F82-L2-A. A frame that IS a whole completion and says nothing. Round 46's
//     A46-3 routed such a frame through this leg's adoption so that the verdict
//     on an empty turn could not depend on whether the upstream framed its
//     emptiness as a delta or as a message. It refused the whole STREAM for it:
//     the refusal set the error and broke out of the read loop, so every frame
//     after the empty one was never read. Measured, an empty whole-completion
//     frame followed by `"hi"`, `"hi"` and a stop reached the client as a 502,
//     while the same frames with the empty one deleted answered "hihi" — the
//     model's answer was on the wire and this leg refused to read it, which is
//     the same shape-dependence A46-3 exists to remove, pointing the other way.
//     The frame is dropped now and the loop reads on; a stream that really does
//     say nothing still relays nothing, and the end-of-stream guard refuses it
//     exactly as it did (round 81's control).
//
//  2. F82-L2-B. A fragment that restates an ADOPTED call under that call's own
//     id and name, carrying an object the slot's own arguments cannot take. The
//     wire has said everything the model wanted in the first frame, and the
//     fragment is the NEXT call — the reading this leg's plain fragment arm
//     answers with two calls (the second re-minted) and its whole-list arm with
//     two. Guarded on the adopted slot, the fragment fell through to the
//     adoption's drop instead, so the model's second call reached no client at
//     all — and only when the vendor filed it under an index the stream had
//     already used: the same fragment at a fresh index, or with no index, was
//     answered as two. Round 80's F80-L2-2 removed this guard from the
//     argument-LESS clause below it; this is the same reading one clause up.
//
// Every case below is fail-first: RED against the tree before its round-82 fix.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r82L2Arm drives one SSE script and reads back whatever verdict it produced —
// unlike r81FragmentArm it does not insist on a 200, because the shapes this
// file is about are the ones whose verdict is the question. Its reading of the
// stream is r81FragmentArm's.
func r82L2Arm(t *testing.T, frames []string) (int, r81Arm) {
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
		return code, r81Arm{Stop: "refused"}
	}
	var out r81Arm
	var text strings.Builder
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
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(raw), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				out.Calls = append(out.Calls, round68Block{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name})
				cur = &out.Calls[len(out.Calls)-1]
			} else {
				cur = nil
			}
		case "content_block_delta":
			if cur != nil && ev.Delta.Type == "input_json_delta" {
				cur.Args += ev.Delta.PartialJSON
			}
			if ev.Delta.Type == "text_delta" {
				text.WriteString(ev.Delta.Text)
			}
			if ev.Delta.StopReason != "" {
				out.Stop = ev.Delta.StopReason
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				out.Stop = ev.Delta.StopReason
			}
		}
	}
	out.Stop += ` text "` + text.String() + `"`
	return code, out
}

// r82L2FinStop is the frame that states the turn's verdict with no delta.
const r82L2FinStop = `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

// r82L2Content is one delta frame carrying prose.
func r82L2Content(s string) string {
	return `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + s + `"}}]}`
}

// r82L2Whole is one frame carrying a whole completion whose message holds the
// given calls (the shape this leg adopts as the turn).
func r82L2Whole(calls, fin string) string {
	return r81WholeFrame(calls, fin)
}

// r82L2EmptyWhole is one frame carrying a whole completion that says nothing.
func r82L2EmptyWhole() string {
	return `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":""}}]}`
}

// r82L2Frag is a delta frame carrying one entry at the given index (`""` for a
// frame with no index field at all).
func r82L2Frag(slot, id, name, args string) string {
	entry := `{"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":"` + args + `"}}`
	if slot != "" {
		entry = `{"index":` + slot + `,` + strings.TrimPrefix(entry, "{")
	}
	return `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[` + entry + `]}}]}`
}

// TestAFrameThatSaysNothingDoesNotEndTheStream is F82-L2-A.
func TestAFrameThatSaysNothingDoesNotEndTheStream(t *testing.T) {
	trail := []string{r82L2Content("hi"), r82L2Content("hi"), r82L2FinStop, r81Done}

	code, arm := r82L2Arm(t, append([]string{r82L2EmptyWhole()}, trail...))
	codeCtl, armCtl := r82L2Arm(t, trail)
	if codeCtl != http.StatusOK {
		t.Fatalf("the control (the same frames without the empty one) is itself a %d (%s); the shape is not the one under test", codeCtl, armCtl.Stop)
	}
	if code != http.StatusOK {
		t.Fatalf("an empty whole-completion frame ahead of the answer turned the turn into %d (%s): the frames after it were never read, and the model's answer — which the same frames without it deliver — was thrown away because of the SHAPE the upstream chose (2026-09-28 audit, round 82, F82-L2-A)", code, arm.Stop)
	}
	if arm.String() != armCtl.String() {
		t.Errorf("the empty frame changed the answer:\n   with it: %s\n   without: %s", arm, armCtl)
	}

	// The same reading for a whole completion whose only entry is one the
	// upstream never named: the frame says nothing, and the call the stream
	// goes on to write is the turn.
	called := r82L2Frag("0", "c1", "Bash", `{\"a\":1}`)
	namelessWhole := r82L2Whole(`{"id":"c1","function":{"name":"","arguments":""}}`, "tool_calls")
	code, arm = r82L2Arm(t, []string{namelessWhole, called, r81FinToolCalls, r81Done})
	codeCtl, armCtl = r82L2Arm(t, []string{called, r81FinToolCalls, r81Done})
	if code != http.StatusOK {
		t.Fatalf("a whole completion whose only entry names nothing turned the turn into %d (%s), where the same frames without it answer the call the stream goes on to write (2026-09-28 audit, round 82, F82-L2-A)", code, arm.Stop)
	}
	if arm.String() != armCtl.String() {
		t.Errorf("the entry-less frame changed the call the turn delivered:\n   with it: %s\n   without: %s", arm, armCtl)
	}
}

// TestAStreamThatReallySaysNothingIsStillRefused is F82-L2-A's control, and
// round 46's A46-3 behind it: a stream whose every frame says nothing relays
// nothing, and that verdict does not move.
func TestAStreamThatReallySaysNothingIsStillRefused(t *testing.T) {
	for _, tc := range []struct {
		note string
		wire []string
	}{
		{"one empty whole-completion frame", []string{r82L2EmptyWhole(), r81Done}},
		{"the same with a finish frame", []string{r82L2EmptyWhole(), r82L2FinStop, r81Done}},
		{"an empty whole frame that states tool_calls", []string{r82L2Whole(`{"id":"c1","function":{"name":"","arguments":""}}`, "tool_calls"), r81Done}},
	} {
		code, arm := r82L2Arm(t, tc.wire)
		if code == http.StatusOK {
			t.Errorf("%s: the turn was answered 200 (%s) where every arm of this leg refuses a completion that says nothing — an empty message is not an answer, and a client that waits for one never retries it (round 46's A46-3)", tc.note, arm)
		}
	}
}

// TestARestatementAtAnAdoptedSlotIsTheNextCall is F82-L2-B. The same two entries
// — the adopted call, then the fragment that restates it under its own id and
// name with an object of its own — have to reach the client as two calls
// whichever index the fragment files them under, and as two calls on this leg's
// plain fragment arm too.
func TestARestatementAtAnAdoptedSlotIsTheNextCall(t *testing.T) {
	adopted := r82L2Whole(`{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`, "tool_calls")
	second := `{\"b\":2}`

	// The plain twin of the same two entries, never framed.
	plain := r81One(r82L2Arm2(t, []string{r82L2Frag("0", "c1", "Bash", `{\"a\":1}`), r82L2Frag("0", "c1", "Bash", second), r81FinToolCalls, r81Done}), "c1")

	for _, slot := range []string{"0", "1", ""} {
		code, arm := r82L2Arm(t, []string{adopted, r82L2Frag(slot, "c1", "Bash", second), r81FinToolCalls, r81Done})
		if code != http.StatusOK {
			t.Fatalf("index %q: status %d (%s)", slot, code, arm.Stop)
		}
		got := r81One(arm, "c1")
		if len(arm.Calls) != 2 {
			t.Errorf("index %q: the wire stated two calls and the client was handed %d (%s). A fragment that restates the adopted call under its own id and name with an object the slot's arguments cannot take is not more of that call — it is the next one, which is what this leg's plain arm answers the same entries with (2026-09-28 audit, round 82, F82-L2-B)", slot, len(arm.Calls), got)
			continue
		}
		if arm.Calls[0].Args != `{"a":1}` || arm.Calls[1].Args != `{"b":2}` {
			t.Errorf("index %q: the two calls reached the client as [%s] [%s], want the two objects the wire stated", slot, arm.Calls[0].Args, arm.Calls[1].Args)
		}
		if arm.Calls[0].ID != "c1" || arm.Calls[1].ID == "c1" {
			t.Errorf("index %q: the calls wear %q and %q — the second must not wear the id the first was answered under, one id over two blocks cannot be answered separately", slot, arm.Calls[0].ID, arm.Calls[1].ID)
		}
		if got != plain {
			t.Errorf("index %q: the same two entries reach the client as\n   adopted frame: %s\n   plain deltas : %s", slot, got, plain)
		}
	}
}

// r82L2Arm2 runs one script and returns its arm, requiring a 200 — the shape of
// r81FragmentArm without the text reading this file does not need here.
func r82L2Arm2(t *testing.T, frames []string) r81Arm {
	t.Helper()
	code, arm := r82L2Arm(t, frames)
	if code != http.StatusOK {
		t.Fatalf("status %d (%s)", code, arm.Stop)
	}
	return arm
}

// TestAContinuationOfAnAdoptedCallIsStillDropped is F82-L2-B's control: the
// fragment that names nothing is an argument continuation of the call the
// client already holds, and it is still dropped rather than split.
func TestAContinuationOfAnAdoptedCallIsStillDropped(t *testing.T) {
	adopted := r82L2Whole(`{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`, "tool_calls")
	code, arm := r82L2Arm(t, []string{adopted, r82L2Frag("", "", "", `}`), r81FinToolCalls, r81Done})
	if code != http.StatusOK {
		t.Fatalf("status %d (%s)", code, arm.Stop)
	}
	if len(arm.Calls) != 1 || arm.Calls[0].ID != "c1" || arm.Calls[0].Args != `{"a":1}` {
		t.Errorf("an argument-only fragment after the adopted call reached the client as %s, want the one call the client already holds (c1 Bash, arguments %s): a fragment that names nothing continues the call the stream last named, and that is the reading the adoption's drop keeps", r81One(arm, "c1"), `{"a":1}`)
	}
}
