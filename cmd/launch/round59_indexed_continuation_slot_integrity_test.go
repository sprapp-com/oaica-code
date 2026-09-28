package launch

// round59_indexed_continuation_slot_integrity_test.go — round 59's finding on
// the client proxy's fragment arm (F59-L2-1).
//
// The index is the upstream's SLOT identity, and round 58 taught this
// accumulator to split a call the upstream states one slot for twice — round
// 36's B-F2 for the vendor that writes index 0 for every call of the turn. What
// the split did not do is re-point the slot at the call it minted: the index
// went on naming the call that was split FROM, so the wire that states the index
// on every fragment — the same three fragments round 58 pinned as two calls, one
// field apart — sent the second call's own arguments back into the FIRST call's
// accumulator. The client was handed a Bash whose input is `{"_raw":…}` with
// both calls' arguments concatenated (JSON no tool can parse) and a Read with an
// EMPTY input, under a stop_reason of tool_use.
//
// Every case below asks the same body of both arms of this leg — the fragments
// the upstream streamed and the whole completion the same upstream writes — and
// requires the same two calls with the same inputs. Each is fail-first: RED
// against the tree before this round's fix, where the whole-list arm (and the
// gateway leg, and this leg's own adoption path) already answer two calls.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r59L2Arms serves the SSE script to a streaming request and the whole
// completion to a non-streaming one, and returns both arms' tool_use blocks.
func r59L2Arms(t *testing.T, frames []string, whole string) (fragments, wholeBlocks []map[string]any) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	script := strings.Join(frames, "\n\n") + "\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, script)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, whole)
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})

	code, body := round54PostMessage(t, proxy, "glm-5.3", true)
	if code != http.StatusOK {
		t.Fatalf("stream arm status %d\n%s", code, body)
	}
	fragments = sseToolUseBlocks(t, body)

	code, wholeBody := round54PostMessage(t, proxy, "glm-5.3", false)
	if code != http.StatusOK {
		t.Fatalf("whole arm status %d\n%s", code, wholeBody)
	}
	var resp struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal([]byte(wholeBody), &resp); err != nil {
		t.Fatalf("whole arm did not parse: %v\n%s", err, wholeBody)
	}
	for _, b := range resp.Content {
		if b["type"] == "tool_use" {
			wholeBlocks = append(wholeBlocks, map[string]any{"id": b["id"], "name": b["name"], "input": b["input"]})
		}
	}
	return fragments, wholeBlocks
}

// r59L2WholeTwoCalls is the whole-list spelling of the two-call turn every case
// below streams: Bash{"cmd":"ls"} then Read{"p":1}, in that order.
const r59L2WholeTwoCalls = `{"id":"c","object":"chat.completion","model":"glm-5.3",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[` +
	`{"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}},` +
	`{"id":"b","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}]},` +
	`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`

const r59L2Fin = `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`

// r59L2Frame spells one streamed tool-call fragment.
func r59L2Frame(body string) string {
	return `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[` + body + `]}}]}`
}

// r59L2SameAsWhole requires the fragment arm to hand the client the same calls
// the whole-list arm does — id, name and input, in order.
func r59L2SameAsWhole(t *testing.T, fragments, whole []map[string]any) {
	t.Helper()
	if len(fragments) != len(whole) {
		t.Fatalf("the same turn is %d call(s) as fragments and %d whole:\nfragments: %v\nwhole:     %v\n%s",
			len(fragments), len(whole), fragments, whole, r59L2Note)
	}
	for i := range whole {
		fj, _ := json.Marshal(fragments[i])
		wj, _ := json.Marshal(whole[i])
		if string(fj) != string(wj) {
			t.Errorf("call %d is %s as fragments and %s whole\n%s", i, fj, wj, r59L2Note)
		}
	}
}

const r59L2Note = "the index names the slot the upstream is writing, and a call it states that slot for twice begins the next one — including the fragments that FOLLOW the split, which state the same index the vendor writes for every call. Routed back into the call that was split from, they were concatenated onto its input (JSON no tool can parse) and the split call reached the client with an empty input (2026-09-28 audit, round 59, F59-L2-1)"

// The split wire's continuation, spelled the four ways an upstream writes it.
func TestTheContinuationOfTheSplitCallIsTheSplitCallOnThisLeg(t *testing.T) {
	first := r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`)
	second := r59L2Frame(`{"index":0,"id":"b","function":{"name":"Read"}}`)
	for _, tc := range []struct {
		name    string
		third   string
		indexed bool
	}{
		{
			"the continuation states the index alone",
			r59L2Frame(`{"index":0,"function":{"arguments":"{\"p\":1}"}}`),
			true,
		},
		{
			"the continuation states the index and the id",
			r59L2Frame(`{"index":0,"id":"b","function":{"arguments":"{\"p\":1}"}}`),
			true,
		},
		{
			"the continuation states the index and the name",
			r59L2Frame(`{"index":0,"function":{"name":"Read","arguments":"{\"p\":1}"}}`),
			true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "the continuation states the index and the name" {
				// REVERSED by round 79 (F79-L2-A). This row held that the
				// fragment naming the call again BESIDE its arguments continues
				// the argument-less call introduced at that slot, and compared
				// the stream against a hand-written two-call document. It is the
				// reading round 78 reversed for the index-LESS spelling of the
				// same wire (F78-L3-2), with the same reason: an empty argument
				// list is a COMPLETE one, so a call introduced with no arguments
				// is whole the moment it is named, and a fragment naming it
				// again WITH arguments is the NEXT call. The document to compare
				// against is this leg's own whole-list arm for the same entries,
				// which answers THREE calls here — measured 2026-09-28: the
				// hand-written two-call document agreed with the fragment arm
				// only while the fragment arm was folding the two Reads into
				// one, and the whole-list arm for these entries never did.
				entries := []string{
					`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`,
					`{"index":0,"id":"b","type":"function","function":{"name":"Read"}}`,
					`{"index":0,"type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}`,
				}
				frag, whole, _, _, _, _ := r75Arms(t, entries)
				r59L2SameAsWhole(t, frag, whole)
				return
			}
			frag, whole := r59L2Arms(t, []string{first, second, tc.third, r59L2Fin, `data: [DONE]`}, r59L2WholeTwoCalls)
			r59L2SameAsWhole(t, frag, whole)
		})
	}

	// The control round 58 pinned: the same three fragments with the index
	// OMITTED on the continuation stay two calls — the wire the round-58 cure
	// was written for, which this round's re-pointing must not disturb.
	t.Run("the continuation omits the index", func(t *testing.T) {
		frag, whole := r59L2Arms(t, []string{
			first, second,
			r59L2Frame(`{"function":{"arguments":"{\"p\":1}"}}`),
			r59L2Fin, `data: [DONE]`,
		}, r59L2WholeTwoCalls)
		r59L2SameAsWhole(t, frag, whole)
	})
}

// TestTheContinuationAtAnEarlierIndexIsTheOpenCallOnThisLeg is the same rule for
// the vendor that states the call's own index on its introduction and the
// default index 0 on the continuation: an argument-only fragment whose slot
// already holds a FINISHED argument list is not more of that call, and it
// belongs to the call the stream last wrote to while that one is still open.
func TestTheContinuationAtAnEarlierIndexIsTheOpenCallOnThisLeg(t *testing.T) {
	frag, whole := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`),
		r59L2Frame(`{"index":1,"id":"b","function":{"name":"Read"}}`),
		r59L2Frame(`{"index":0,"function":{"arguments":"{\"p\":1}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, r59L2WholeTwoCalls)
	r59L2SameAsWhole(t, frag, whole)
}

// The controls: the ordinary interleaved wire (both calls open at once, each
// continuation at its own index) keeps each call's own arguments, and the
// restatement round 56 pinned — a call listed again at its own slot — stays one
// call.
func TestTheIndexedWireKeepsEachCallOnThisLeg(t *testing.T) {
	t.Run("two open calls, each continued at its own index", func(t *testing.T) {
		frag, whole := r59L2Arms(t, []string{
			r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":"}}`),
			r59L2Frame(`{"index":1,"id":"b","function":{"name":"Read","arguments":"{\"p\":"}}`),
			r59L2Frame(`{"index":0,"function":{"arguments":"\"ls\"}"}}`),
			r59L2Frame(`{"index":1,"function":{"arguments":"1}"}}`),
			r59L2Fin, `data: [DONE]`,
		}, r59L2WholeTwoCalls)
		r59L2SameAsWhole(t, frag, whole)
	})

	t.Run("the whole call listed again at its own slot", func(t *testing.T) {
		frag, whole := r59L2Arms(t, []string{
			r59L2Frame(`{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}`),
			r59L2Frame(`{"index":0,"id":"call_1"}`),
			r59L2Frame(`{"index":0,"function":{"name":"Bash"}}`),
			r59L2Frame(`{"index":0,"function":{"arguments":"{\"a\":1}"}}`),
			r59L2Fin, `data: [DONE]`,
		}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
			`{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
		r59L2SameAsWhole(t, frag, whole)
	})
}
