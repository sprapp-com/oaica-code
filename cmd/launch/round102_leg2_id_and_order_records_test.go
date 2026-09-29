package launch

// round102_leg2_id_and_order_records_test.go — leg 2, round 102 (2026-09-29
// audit). Five readings RECORDED, none changed: F102-L2-2, F102-L2-3, F102-L2-4,
// F102-L2-5, F102-L2-6.
//
// All five are one family — an entry's routing on the fragment arm diverging
// from the document arms — and each needs a DECISION this leg has not made,
// which is why they are written down rather than patched.
//
// F102-L2-2. An entry that names nothing and states no argument bytes claims the
// call's id on the fragment arm; the whole-list reader skips it and mints
// (round 65's F65-L2-2: "because no block is built, the id it states is not this
// call's to claim"). A `tool_result` written against one arm's id names an id the
// other never issued. A content-based narrowing was written, measured, and
// DROPPED: requiring the fragment to carry the call's name or its arguments
// reddens round 60's late-id pin
// (`TestTheLateIdFragmentNamesTheCallOnThisLeg`), whose wire splits ONE entry
// across three frames — name, then arguments, then the id ALONE — and whose
// whole-list spelling reads that id as the call's. The two bodies' fragment
// spellings differ only in whether the opening frame stated an index, so no rule
// keyed on the fragment's own contents can separate them; deciding it needs the
// turn, not the fragment.
//
// F102-L2-3. Round 81 decided that an argument-less call restated argless, stating
// no id, is TWO calls, and pinned the shape with the index STRIPPED. Keep the
// index the wire wrote and the document arm folds while the fragment arm still
// splits — the same body, one field apart, answered as one call or two by which
// reader looked at it.
//
// F102-L2-4. A repeated id-less call mints a different id when the FIRST call
// arrived inside the adopted whole-completion frame than when both arrived as
// deltas, because the adoption mints and reserves the head call's id but not its
// mint KEY, so the flush's mint for the repeat takes the collision bump rather
// than the "#n" rule. Round 71 closed this defect for a STATED id
// (F71-L2-1); this is the minted one.
//
// F102-L2-5. A dropped fragment's prose stands AFTER the call on the
// adopted-tail path and BEFORE it on every other arm.
//
// F102-L2-6. The mirror of F102-L2-3: a second entry that NAMES the same call,
// states a different id and states no arguments folds onto the finished call on
// the fragment arm and opens its own block on the document arm.
//
// Every reading below was reproduced fail-first on the tree this round opened at
// (fe7ab5769) before any of this round's fixes were written.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r102L2Stream spells one SSE script and answers the whole turn, so the mixed
// spellings (a whole completion this leg adopts, then deltas) can be read the
// same way as the pure ones.
func r102L2Stream(t *testing.T, frames []string) r102L2Arm {
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
		t.Fatalf("status %d\n%s", code, body)
	}
	return r102L2ParseStream(t, body)
}

// r102L2DeltaFrame spells one delta-chunk tool-call entry the way this wire
// writes it.
func r102L2DeltaFrame(entry string) string {
	return `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[` + entry + `]}}]}`
}

// TestMine102AnIdStatedByANamelessEntryStillNamesTheCall is F102-L2-2's record.
func TestMine102AnIdStatedByANamelessEntryStillNamesTheCall(t *testing.T) {
	call := `{"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	for _, c := range []struct{ name, twin string }{
		{"the entry states the call's index too", `{"index":0,"id":"c1"}`},
		{"the entry states no index", `{"id":"c1"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			streamed, buffered := r102L2Run(t,
				[]string{r59L2Frame(call), r59L2Frame(c.twin), r59L2Fin},
				r102L2Whole(call+","+c.twin, "tool_calls"))
			if got := streamed.String(); got != `stop=tool_use blocks=[call:c1|Bash|{"a":1}]` {
				t.Errorf("the streamed arm reads %s, want the claimed id this record states; if the fragment arm has been taught to leave the id to the mint, this record is spent and F102-L2-2 is fixed (2026-09-29 audit, round 102, F102-L2-2)", got)
			}
			if got := buffered.String(); got != `stop=tool_use blocks=[call:call_7ff51383|Bash|{"a":1}]` {
				t.Errorf("the buffered arm reads %s, want the minted id this record states (2026-09-29 audit, round 102, F102-L2-2)", got)
			}
		})
	}
}

// TestMine102TheLateIdWireIsWhyTheClaimCouldNotBeNarrowed is the counterexample
// that made F102-L2-2 a record: ONE entry split across three frames, whose
// whole-list spelling reads the id-only frame as the call's. Any narrowing keyed
// on what the frame carries reddens round 60's pin — this states the reading the
// narrowing must not move.
func TestMine102TheLateIdWireIsWhyTheClaimCouldNotBeNarrowed(t *testing.T) {
	streamed, buffered := r102L2Run(t,
		[]string{
			r59L2Frame(`{"index":0,"function":{"name":"Bash"}}`),
			r59L2Frame(`{"index":0,"function":{"arguments":"{\"a\":1}"}}`),
			r59L2Frame(`{"index":0,"id":"z"}`),
			r59L2Fin,
		},
		r102L2Whole(`{"id":"z","function":{"name":"Bash","arguments":"{\"a\":1}"}}`, "tool_calls"))
	if got := streamed.String(); got != `stop=tool_use blocks=[call:z|Bash|{"a":1}]` {
		t.Errorf("the streamed arm reads %s, want the stated id — the id-only fragment names the call the preceding frames built, and a rule that made it claim nothing instead would redden round 60's pin (2026-09-29 audit, round 102, F102-L2-2)", got)
	}
	if got := buffered.String(); got != streamed.String() {
		t.Errorf("the two arms read %s and %s — they agree on this wire, and that is what fixes the shape of the record above (2026-09-29 audit, round 102, F102-L2-2)", streamed, buffered)
	}
}

// TestMine102AnArgumentlessRestatementWithAnIndexSplitsOnlyOnTheFragmentArm is
// F102-L2-3's record: round 81's body, index kept.
func TestMine102AnArgumentlessRestatementWithAnIndexSplitsOnlyOnTheFragmentArm(t *testing.T) {
	first := `{"index":0,"id":"c1","type":"function","function":{"name":"Bash"}}`
	twin := `{"index":0,"type":"function","function":{"name":"Bash"}}`
	streamed, buffered := r102L2Run(t,
		[]string{r59L2Frame(first), r59L2Frame(twin), r59L2Fin},
		r102L2Whole(first+","+twin, "tool_calls"))
	if got := streamed.String(); got != `stop=tool_use blocks=[call:c1|Bash|{} call:call_781541ff|Bash|{}]` {
		t.Errorf("the streamed arm reads %s, want the two calls round 81 decided this body states (2026-09-29 audit, round 102, F102-L2-3)", got)
	}
	if got := buffered.String(); got != `stop=tool_use blocks=[call:c1|Bash|{}]` {
		t.Errorf("the buffered arm reads %s, want the ONE call this record states — if the document arm has been taught to read the index here as round 81's rule reads it, the record is spent (2026-09-29 audit, round 102, F102-L2-3)", got)
	}
}

// TestMine102ANamedRestatementWithADifferentIdFoldsOnlyOnTheFragmentArm is
// F102-L2-6's record — F102-L2-3's mirror.
func TestMine102ANamedRestatementWithADifferentIdFoldsOnlyOnTheFragmentArm(t *testing.T) {
	call := `{"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	named := `{"index":0,"id":"c9","type":"function","function":{"name":"Bash"}}`
	streamed, buffered := r102L2Run(t,
		[]string{r59L2Frame(call), r59L2Frame(named), r59L2Fin},
		r102L2Whole(call+","+named, "tool_calls"))
	if got := streamed.String(); got != `stop=tool_use blocks=[call:c9|Bash|{"a":1}]` {
		t.Errorf("the streamed arm reads %s, want the folded reading this record states (2026-09-29 audit, round 102, F102-L2-6)", got)
	}
	if got := buffered.String(); got != `stop=tool_use blocks=[call:call_7ff51383|Bash|{"a":1} call:c9|Bash|{}]` {
		t.Errorf("the buffered arm reads %s, want the two-call reading this record states; if the two arms now agree here, the record is spent (2026-09-29 audit, round 102, F102-L2-6)", got)
	}
}

// TestMine102TheMintOfARepeatedIdlessCallFollowsTheAdoptedHead is F102-L2-4's
// record.
func TestMine102TheMintOfARepeatedIdlessCallFollowsTheAdoptedHead(t *testing.T) {
	call := `{"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	mixed := r102L2Stream(t, []string{
		round68WholeFrame(call), r102L2DeltaFrame(call),
		r102L2Fin("tool_calls"), `data: [DONE]`,
	})
	pure := r102L2Stream(t, []string{
		r59L2Frame(call), r59L2Frame(call),
		r102L2Fin("tool_calls"), `data: [DONE]`,
	})
	if got := pure.String(); got != `stop=tool_use blocks=[call:call_7ff51383|Bash|{"a":1} call:call_1f1f3503|Bash|{"a":1}]` {
		t.Fatalf("premise: the pure-delta arm reads %s, want the #n pair this record is measured against (2026-09-29 audit, round 102, F102-L2-4)", got)
	}
	if got := mixed.String(); got != `stop=tool_use blocks=[call:call_7ff51383|Bash|{"a":1} call:call_70d6647a|Bash|{"a":1}]` {
		t.Errorf("the mixed arm reads %s, want the bumped mint this record states; if the two spellings now mint the repeat alike, the record is spent (2026-09-29 audit, round 102, F102-L2-4)", got)
	}
}

// TestMine102ADroppedFragmentsProseStandsAfterTheCallOnTheAdoptedTail is
// F102-L2-5's record.
func TestMine102ADroppedFragmentsProseStandsAfterTheCallOnTheAdoptedTail(t *testing.T) {
	head := `{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	tail := `{"index":0,"function":{"arguments":"{\"a\":1}"}}`
	mixed := r102L2Stream(t, []string{
		round68WholeFrame(head), r102L2DeltaFrame(tail),
		r102L2Fin("tool_calls"), `data: [DONE]`,
	})
	pure := r102L2Stream(t, []string{
		r59L2Frame(head), r59L2Frame(tail),
		r102L2Fin("tool_calls"), `data: [DONE]`,
	})
	if got := pure.String(); got != `stop=tool_use blocks=[text:"{\"a\":1}" call:c1|Bash|{"a":1}]` {
		t.Fatalf("premise: the pure-delta arm reads %s, want the prose-before-call reading this record is measured against (2026-09-29 audit, round 102, F102-L2-5)", got)
	}
	if got := mixed.String(); got != `stop=tool_use blocks=[call:c1|Bash|{"a":1} text:"{\"a\":1}"]` {
		t.Errorf("the mixed arm reads %s, want the prose-AFTER-call order this record states; if the adopted-tail path now writes the prose where every other arm writes it, the record is spent (2026-09-29 audit, round 102, F102-L2-5)", got)
	}
}
