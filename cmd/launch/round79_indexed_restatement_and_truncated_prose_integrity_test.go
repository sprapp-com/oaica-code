package launch

// round79_indexed_restatement_and_truncated_prose_integrity_test.go — leg 2,
// the two places the fragment arm still answered one upstream body differently
// from this leg's whole-list arm (2026-09-28 audit, round 79).
//
//  1. F79-L2-A. Round 78 taught this leg that an EMPTY accumulated argument
//     list is a COMPLETE one, so a fragment naming the same call again WITH
//     arguments begins the next call (startsANewToolCall's tail). That
//     predicate has exactly one call site, inside the index-LESS branch of the
//     router. The index-STATED branch — the spelling vendors actually write —
//     split on a differing id, a differing name, an unnamed occupant and
//     bytes the accumulated arguments cannot take, and on nothing else, so a
//     call introduced with no arguments and then restated with them was folded
//     into one: `[{index 0,id c1,name Read},{index 0,id c1,name Read,
//     arguments {"a":1}}]` reached the client as ONE call holding `{"a":1}`,
//     while the whole-list arm answers the argument-less call AND the
//     completed one, the second under a minted id. The same body with the
//     index stated and no ids, and with the ids but no index, showed the field
//     that decided it was the index alone.
//
//  2. F79-L2-B. A nameless entry's bytes are the model's prose (round 77).
//     While the call they were folded into is KEPT they are that call's
//     arguments — the freeform trade-off round 77 records. When the turn
//     truncates, the flush's gate drops that call, and the bytes went with it:
//     `[{index 0,id c1,name Bash,arguments "ls "},{index 0,arguments "-la"}]`
//     under finish_reason "length" reached the client as NO content at all,
//     where this leg's whole-list arm answers the model's prose `-la` under
//     max_tokens. The bytes were recorded only for entries landing on an
//     accumulator that still named nothing, so a slot that already carried a
//     call recorded none of them.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r79L2ArmsRaw runs one upstream body down both arms of this leg and returns
// the raw client bodies, which is what the two findings below need: F79-L2-B's
// answer is a TEXT block and a stop_reason, and r59L2Arms keeps only the
// tool_use blocks.
func r79L2ArmsRaw(t *testing.T, frames []string, whole string) (fragRaw, wholeRaw string) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	script := strings.Join(frames, "\n\n") + "\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, script)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, whole)
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	_, fragRaw = round54PostMessage(t, proxy, "glm-5.3", true)
	_, wholeRaw = round54PostMessage(t, proxy, "glm-5.3", false)
	return fragRaw, wholeRaw
}

// r79L2Text reads back the text the client was handed, in the order it arrived.
func r79L2Text(raw string) string {
	var out strings.Builder
	for _, part := range strings.Split(raw, `"type":"text_delta","text":"`)[1:] {
		if i := strings.Index(part, `"`); i >= 0 {
			out.WriteString(part[:i])
		}
	}
	return out.String()
}

// TestARestatedIndexedCallWithArgumentsIsTheNextCall is F79-L2-A: the reading
// round 78 gave the index-less spelling is the reading of the index-stated one,
// because the entry list is the same entry list and this leg's whole-list arm
// answers it the same way.
func TestARestatedIndexedCallWithArgumentsIsTheNextCall(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
	}{
		{
			"the index and the id are stated",
			[]string{
				`{"index":0,"id":"c1","type":"function","function":{"name":"Read"}}`,
				`{"index":0,"id":"c1","type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}`,
			},
		},
		{
			"the index alone is stated",
			[]string{
				`{"index":0,"type":"function","function":{"name":"Read"}}`,
				`{"index":0,"type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}`,
			},
		},
		{
			"the second call arrives at the index the vendor writes for both",
			[]string{
				`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`,
				`{"index":0,"id":"b","type":"function","function":{"name":"Read"}}`,
				`{"index":0,"type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}`,
			},
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			frag, whole, fragText, wholeText, _, _ := r75Arms(t, tc.entries)
			if len(frag) != len(whole) {
				t.Fatalf("the same entries are %d call(s) as fragments and %d whole — an empty argument list is a complete one, so a call restated with arguments is the next call (2026-09-28 audit, round 79, F79-L2-A)\nfragments: %s\nwhole:     %s",
					len(frag), len(whole), r76ArmBlocks(t, frag), r76ArmBlocks(t, whole))
			}
			if fragText != wholeText {
				t.Errorf("one body, two arms: the fragment arm relayed %q as text where the whole-list arm relayed %q (2026-09-28 audit, round 79, F79-L2-A)", fragText, wholeText)
			}
		})
	}
}

// TestATruncatedCallKeepsTheNamelessEntriesProse is F79-L2-B: the bytes an
// entry that names nothing contributed are prose, and the call they were folded
// into being dropped at the token limit does not take them with it.
func TestATruncatedCallKeepsTheNamelessEntriesProse(t *testing.T) {
	for _, fin := range []string{"tool_calls", "length"} {
		t.Run(fin, func(t *testing.T) {
			frames := []string{
				r59L2Frame(`{"index":0,"id":"c1","function":{"name":"Bash","arguments":"ls "}}`),
				r59L2Frame(`{"index":0,"function":{"arguments":"-la"}}`),
				`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"` + fin + `"}]}`,
				`data: [DONE]`,
			}
			whole := `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"Bash","arguments":"ls "}},` +
				`{"type":"function","function":{"arguments":"-la"}}]},"finish_reason":"` + fin + `"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`
			fragRaw, wholeRaw := r79L2ArmsRaw(t, frames, whole)
			// The property that does not depend on where the bytes were filed:
			// under finish_reason "length" the whole-list arm answers the
			// nameless entry's prose and drops the truncated call, and the
			// fragment arm must not answer LESS than that. It answered nothing
			// at all before this round.
			if fin == "length" {
				if got := r79L2Text(fragRaw); got != "-la" {
					t.Errorf("the fragment arm relayed %q for a truncated turn whose prose every other arm answers — the bytes of an entry that names nothing are the model's output, and the call they were folded into being dropped does not take them with it (2026-09-28 audit, round 79, F79-L2-B)\n%s",
						got, fragRaw)
				}
				if want := `"stop_reason":"max_tokens"`; !strings.Contains(fragRaw, want) {
					t.Errorf("the fragment arm reported a stop_reason other than max_tokens: want %s in\n%s", want, fragRaw)
				}
				if want := `"text":"-la"`; !strings.Contains(wholeRaw, want) {
					t.Fatalf("the whole-list arm no longer answers %s — this pin's control has drifted:\n%s", want, wholeRaw)
				}
			}
			// The control: the same body NOT truncated keeps the call, and the
			// nameless bytes are part of it (the freeform reading round 77
			// records as deliberate), on both arms' own terms.
			if fin == "tool_calls" && !strings.Contains(fragRaw, `ls -la`) {
				t.Errorf("the untruncated freeform reading has drifted: the fragment arm no longer keeps the call whole:\n%s", fragRaw)
			}
		})
	}
}
