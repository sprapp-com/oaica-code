package launch

// round81_restatement_split_and_adoption_integrity_test.go — leg 2, the places
// a call boundary the upstream drew mid-stream was answered differently by the
// two arms of one body (2026-09-28 audit, round 81).
//
// One body reaches this leg two ways: as a run of SSE fragments (the streaming
// arm, read delta by delta) and as one whole completion (the non-streaming arm,
// and the message the adoption makes of a frame that carries one). Both arms
// read the SAME entries, in the same order, and they have to answer the same
// calls, in the same order, with the same arguments.
//
//  1. F81-L2-1. A call whose arguments are complete, restated by a fragment that
//     names it again and states no arguments, is the NEXT call: an empty
//     argument list is a COMPLETE one, so there is nothing of that call for the
//     fragment to be more of. The fragment arm folded it and the whole-list arm
//     answered two.
//
//  2. F81-L2-4. The same reading where the slot holds NOTHING either. What the
//     whole-list arm folds, and all it folds, is one call restated under the id
//     the wire states (round 69's rule); two entries that name one call and
//     state no id are two calls, and the fragment arm folded them into one.
//
//  3. F81-L2-5. And the same again over a call whose arguments are still
//     mid-object. The whole-list arm keeps the second entry as a call of its
//     own — the truncated one is wrapped or dropped, never merged with it — while
//     the fragment arm wrote the argument-less call's (nonexistent) arguments
//     into the first one, so the call the wire named reached no client at all.
//
//  4. F81-L2-2. A call this leg adopted a whole completion for is not every call
//     the stream goes on to name: a fragment that names a call the adoption did
//     not write opens a block of its own, and the gate that drops a
//     continuation of an adopted call asked the wrong question of it.
//
//  5. F81-L2-3. A call the adoption DROPPED — the turn ended at the token limit
//     and its arguments never became an object — is not a call the adoption
//     wrote: its slot is not spoken for, so a later fragment completing that
//     call is a call, and the turn reached the client as nothing (a 502) where
//     the same document's other arm answers the call.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r81Arm is one arm's answer: the calls it relayed, in order, and the verdict.
type r81Arm struct {
	Calls []round68Block
	Stop  string
}

func (a r81Arm) String() string {
	s := "stop=" + a.Stop
	for _, c := range a.Calls {
		s += " [" + c.ID + " " + c.Name + " " + c.Args + "]"
	}
	return s
}

// r81FragmentArm reads one SSE script the way this leg's streaming arm delivers
// it: a tool_use block opens where the wire opens it and its input is the
// partial_json deltas joined, in the order the blocks opened.
func r81FragmentArm(t *testing.T, frames []string) r81Arm {
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
		t.Fatalf("fragment arm: status %d\n%s", code, body)
	}
	var out r81Arm
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
			if ev.Delta.StopReason != "" {
				out.Stop = ev.Delta.StopReason
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				out.Stop = ev.Delta.StopReason
			}
		}
	}
	return out
}

// r81WholeArm reads one whole completion the way this leg's non-streaming arm
// delivers it.
func r81WholeArm(t *testing.T, doc string) r81Arm {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, doc)
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", false)
	if code != http.StatusOK {
		t.Fatalf("whole arm: status %d\n%s", code, body)
	}
	var msg struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("whole arm: %v\n%s", err, body)
	}
	out := r81Arm{Stop: msg.StopReason}
	for _, b := range msg.Content {
		if b.Type == "tool_use" {
			out.Calls = append(out.Calls, round68Block{ID: b.ID, Name: b.Name, Args: string(b.Input)})
		}
	}
	return out
}

// r81Doc is one whole completion carrying the given entries.
func r81Doc(entries, fin string) string {
	return `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"",` +
		`"tool_calls":[` + entries + `]},"finish_reason":"` + fin + `"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":3}}`
}

// r81Frame is one SSE frame carrying one entry as a delta.
func r81Frame(entry string) string {
	return `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[` + entry + `]}}]}`
}

// r81WholeFrame is one SSE frame carrying a whole completion, the shape a
// vendor sends instead of deltas (which this leg adopts as the turn).
func r81WholeFrame(calls, fin string) string {
	return `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"",` +
		`"tool_calls":[` + calls + `]},"finish_reason":"` + fin + `"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":3}}`
}

const (
	r81FinToolCalls = `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	r81FinLength    = `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`
	r81Done         = `data: [DONE]`
)

// r81One is a whole-list arm's answer with its minted ids normalised. A call the
// wire never stated an id for is given one by the mint, and the two arms of one
// body mint different ids for it; the ids the wire DID state are kept, because
// those are the client's to answer by.
func r81One(arm r81Arm, stated ...string) string {
	keep := map[string]bool{}
	for _, id := range stated {
		keep[id] = true
	}
	out := "stop=" + arm.Stop
	for _, c := range arm.Calls {
		id := c.ID
		if !keep[id] {
			id = "<minted>"
		}
		out += " [" + id + " " + c.Name + " " + c.Args + "]"
	}
	return out
}

// r81Both runs one entry list down the fragment arm and the whole arm and
// answers both readings normalised the same way.
func r81Both(t *testing.T, frames []string, doc string, stated ...string) (string, string) {
	t.Helper()
	return r81One(r81FragmentArm(t, frames), stated...), r81One(r81WholeArm(t, doc), stated...)
}

// TestTheArmsAgreeOnWhichEntriesAreCalls is F81-L2-1, F81-L2-4 and F81-L2-5. One
// list of entries, written as fragments and as one document, answers the same
// calls in the same order: an entry naming a call is a call unless it is the
// SAME call restated under the id the wire states.
func TestTheArmsAgreeOnWhichEntriesAreCalls(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
		fin     string
		want    string
	}{
		{
			"an argument-less call, restated argless stating no id",
			[]string{
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash"}}`,
				`{"index":0,"type":"function","function":{"name":"Bash"}}`,
			},
			"tool_calls",
			"stop=tool_use [c1 Bash {}] [<minted> Bash {}]",
		},
		{
			"an argument-less call, restated argless under its own id (one call)",
			[]string{
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash"}}`,
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash"}}`,
			},
			"tool_calls",
			"stop=tool_use [c1 Bash {}]",
		},
		{
			"a mid-object call, then an argument-less call naming it",
			[]string{
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":"}}`,
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash"}}`,
			},
			"tool_calls",
			"stop=tool_use [c1 Bash {\"_raw\":\"{\\\"a\\\":\"}] [<minted> Bash {}]",
		},
		{
			"a mid-object call, then an argument-less call naming it, turn cut",
			[]string{
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":"}}`,
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash"}}`,
			},
			"length",
			"stop=tool_use [c1 Bash {}]",
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			frames := make([]string, 0, len(tc.entries)+2)
			doc := make([]string, 0, len(tc.entries))
			for _, e := range tc.entries {
				frames = append(frames, r81Frame(e))
				doc = append(doc, r81EntryWithoutIndex(e))
			}
			frames = append(frames, r81Fin(tc.fin), r81Done)
			frag, whole := r81Both(t, frames, r81Doc(strings.Join(doc, ","), tc.fin), "c1")
			for _, arm := range []struct {
				name string
				got  string
			}{{"fragment", frag}, {"whole", whole}} {
				if arm.got != tc.want {
					t.Errorf("the %s arm answered %s, want %s — an entry that names a call is a call, and the whole-list arm of this leg is the reading both arms keep: what it folds is one call restated under the id the wire states (2026-09-28 audit, round 81, L2-1/L2-4/L2-5)",
						arm.name, arm.got, tc.want)
				}
			}
		})
	}
}

// TestAReListingOfAWholeCallStaysOneCallOnTheFragmentArm is the shape this
// round did NOT change, recorded with its reason. A call whose arguments are
// whole, restated by a fragment that states no arguments, is read by this arm as
// the vendor re-listing that call — which is what round 56 pinned this arm
// against, on the wire that states the call again with its id and its name on
// chunks of their own, and what round 69's restatement rule says for the
// whole-list arm's identical-arguments fold. The whole-list arm of this leg
// answers the same two entries as a document with TWO calls, so this one shape
// is read two ways inside one leg; the reading is recorded here rather than
// decided, because the bytes are the same either way and the arm that folds is
// the one the vendor's own re-listing wire was pinned against
// (2026-09-28 audit, round 81, L2-1).
func TestAReListingOfAWholeCallStaysOneCallOnTheFragmentArm(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
		want    string
	}{
		{"the repeat states its id",
			[]string{
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash"}}`,
			},
			`stop=tool_use [c1 Bash {"a":1}]`},
		{"the repeat states no id",
			[]string{
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
				`{"index":0,"type":"function","function":{"name":"Bash"}}`,
			},
			`stop=tool_use [c1 Bash {"a":1}]`},
		{"the call's arguments are freeform and whole",
			[]string{
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"ls"}}`,
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash"}}`,
			},
			`stop=tool_use [c1 Bash {"_raw":"ls"}]`},
	} {
		t.Run(tc.note, func(t *testing.T) {
			frames := make([]string, 0, len(tc.entries)+2)
			for _, e := range tc.entries {
				frames = append(frames, r81Frame(e))
			}
			frames = append(frames, r81FinToolCalls, r81Done)
			got := r81One(r81FragmentArm(t, frames), "c1")
			if got != tc.want {
				t.Errorf("the fragment arm answered %s, want %s — a whole call restated by a fragment that states nothing is the RE-LISTING round 56 pinned as one call, and this arm keeps that reading (2026-09-28 audit, round 81, L2-1, recorded)\n%s",
					got, tc.want, got)
			}
		})
	}
}

// r81Fin spells the frame that states the turn's verdict.
func r81Fin(fin string) string {
	if fin == "length" {
		return r81FinLength
	}
	return r81FinToolCalls
}

// r81EntryWithoutIndex strips the index from one entry, the way a document
// states its entries (an index is how a DELTA names its slot).
func r81EntryWithoutIndex(entry string) string {
	var e map[string]json.RawMessage
	if err := json.Unmarshal([]byte(entry), &e); err != nil {
		return entry
	}
	delete(e, "index")
	out, err := json.Marshal(e)
	if err != nil {
		return entry
	}
	return string(out)
}

// TestAnIndexLessCallAfterAnAdoptedFrameIsItsOwnCall is F81-L2-2. A whole
// completion arrives inside a frame and is adopted as the turn; a fragment after
// it that NAMES a call the adoption did not write is a call the model made, and
// the gate the adoption keeps for continuations of ITS calls may not swallow it.
func TestAnIndexLessCallAfterAnAdoptedFrameIsItsOwnCall(t *testing.T) {
	frames := []string{
		round68WholeFrame(round68CallA),
		r81Frame(`{"id":"c2","type":"function","function":{"name":"Read","arguments":"{\"f\":2}"}}`),
		r81FinToolCalls,
		r81Done,
	}
	doc := r81Doc(round68CallA+`,`+`{"id":"c2","type":"function","function":{"name":"Read","arguments":"{\"f\":2}"}}`, "tool_calls")
	frag, whole := r81Both(t, frames, doc, "call_a", "c2")
	want := `stop=tool_use [call_a Bash {"a":1}] [c2 Read {"f":2}]`
	for _, arm := range []struct {
		name string
		got  string
	}{{"fragment", frag}, {"whole", whole}} {
		if arm.got != want {
			t.Errorf("the %s arm answered %s, want %s — a fragment that names a call the adoption did not write opens a block of its own; only a CONTINUATION of an adopted call is dropped (2026-09-28 audit, round 81, L2-2)",
				arm.name, arm.got, want)
		}
	}
}

// TestACallTheAdoptionDroppedIsStillFilledByItsLaterDelta is F81-L2-3. The frame
// this leg adopted carries a call the turn was cut off in the middle of: its
// arguments never became an object, so the adoption wrote no call and spoke for
// no slot — and the fragment that goes on to finish that call is a call.
func TestACallTheAdoptionDroppedIsStillFilledByItsLaterDelta(t *testing.T) {
	truncated := `{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":"}}`
	for _, tc := range []struct {
		note string
		rest string
	}{
		{"the fragment states the index and the finished arguments",
			`{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`},
		{"the fragment states them without an index",
			`{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`},
	} {
		t.Run(tc.note, func(t *testing.T) {
			frag := r81FragmentArm(t, []string{
				r81WholeFrame(truncated, "length"),
				r81Frame(tc.rest),
				r81FinLength,
				r81Done,
			})
			// The whole-document twin of this turn is the call its fragments
			// add up to, under the same verdict the upstream stated.
			whole := r81WholeArm(t, r81Doc(`{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`, "length"))
			want := `stop=tool_use [c1 Bash {"a":1}]`
			got, twin := r81One(frag, "c1"), r81One(whole, "c1")
			if got != want {
				t.Errorf("the fragment arm answered %s, want %s — a call the adoption DROPPED spoke for no slot, so the fragment that finishes it is a call the model made (2026-09-28 audit, round 81, L2-3)",
					got, want)
			}
			if twin != want {
				t.Fatalf("the whole arm answered %s for the same turn, want %s — control drifted", twin, want)
			}
		})
	}
}

// TestTheAdoptionStillRefusesADocumentThatSaysNothing is the control for
// F81-L2-3: the reading that lets a dropped call's slot go free must not turn a
// document that says nothing at all into an answer.
func TestTheAdoptionStillRefusesADocumentThatSaysNothing(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		script := strings.Join([]string{
			`data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":10,"completion_tokens":0}}`,
			r81Done,
		}, "\n\n") + "\n\n"
		_, _ = io.WriteString(w, script)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", true)
	if code != http.StatusBadGateway {
		t.Fatalf("a whole completion that names no call and carries no text answered %d, want %d (2026-09-28 audit, round 81, L2-3 control)\n%s",
			code, http.StatusBadGateway, body)
	}
}
