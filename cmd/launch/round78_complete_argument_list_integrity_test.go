package launch

// round78_complete_argument_list_integrity_test.go — leg 2, the argument list
// that ends a call (2026-09-28 audit, round 78, F78-L3-2's twin on this leg).
//
// `startsANewToolCall` ended an accumulated call when its arguments were a
// COMPLETE JSON object, and treated an EMPTY argument list as complete only for
// the bare repeat (round 39's B-F9: the delta names the call again and states no
// arguments). A delta that named the same call again WITH arguments over a call
// that had accumulated none was read as its continuation, so
// `[{name:"Read"},{name:"Read",arguments:"{\"a\":1}"}]` reached a streaming
// client as ONE tool_use where this leg's own whole-list arm and both arms of
// the gateway deliver two — the first call, which the client runs, did not exist
// on this arm. An empty argument list is a COMPLETE argument list for a call
// that takes no arguments, whatever the next delta carries.
//
// The reading that fix must leave alone is the other direction: an upstream that
// RESTATES the name on every argument fragment of one call is not stating many
// calls, so a partial object stays a continuation.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestACompleteArgumentListEndsTheCall(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
	}{
		{
			"a call that takes no arguments, then the same name with arguments",
			[]string{
				`{"type":"function","function":{"name":"Read"}}`,
				`{"type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}`,
			},
		},
		{
			"two calls that take no arguments",
			[]string{
				`{"type":"function","function":{"name":"Read"}}`,
				`{"type":"function","function":{"name":"Read"}}`,
			},
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			frag, whole, _, _, _, _ := r75Arms(t, tc.entries)
			gotFrag, gotWhole := r76ArmBlocks(t, frag), r76ArmBlocks(t, whole)
			if gotFrag != gotWhole {
				t.Errorf("one body, two arms: the fragment arm answered %s where the whole-list arm answered %s — the call that takes no arguments has all of them the moment the wire names it, so the next delta naming the same call begins another (2026-09-28 audit, round 78, F78-L3-2)",
					gotFrag, gotWhole)
			}
		})
	}
}

// TestARestatedNameOverAPartialObjectIsNotANewCall is the reading the fix must
// leave alone: the accumulated bytes are not yet valid JSON, so they are not a
// complete argument list and the call has not ended.
func TestARestatedNameOverAPartialObjectIsNotANewCall(t *testing.T) {
	frag, _, _, _, _, _ := r75Arms(t, []string{
		`{"type":"function","function":{"name":"Bash","arguments":"{\"cmd\":"}}`,
		`{"type":"function","function":{"name":"Bash","arguments":"\"ls\"}"}}`,
	})
	if got := r76ArmBlocks(t, frag); got != `<<nil> id=call_83cf9330 name=Bash input={"cmd":"ls"}>` {
		t.Errorf("the fragment arm answered %s, want one call — a partial object is not a complete argument list, so a name restated over it continues that call (2026-09-28 audit, round 78, F78-L3-2)", got)
	}
}

// TestANamelessEntryRepeatingACallsBytesIsProseOnBothArms is F78-L2-1: an entry
// that names nothing is not a call however closely it repeats the call beside
// it, and an ID on it does not make it one. The rescue that gives such an entry
// an accumulator of its own — and therefore prose at the flush — required an
// entry with no id AND no name, so a wire that restated the call's own id let
// the entry through to the call's argument line: `[{id c1, name Bash, args
// {"a":1}},{id c1, args {"a":1}}]` handed a streaming client
// `{"_raw":"{\"a\":1}{\"a\":1}"}`, a call no tool can run, where both document
// arms answer the call and the prose beside it.
func TestANamelessEntryRepeatingACallsBytesIsProseOnBothArms(t *testing.T) {
	entries := []string{
		`{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
		`{"index":0,"id":"c1","type":"function","function":{"arguments":"{\"a\":1}"}}`,
	}
	frag, whole, fragText, wholeText, _, _ := r75Arms(t, entries)
	gotFrag, gotWhole := r76ArmBlocks(t, frag), r76ArmBlocks(t, whole)
	if gotFrag != gotWhole {
		t.Errorf("one body, two arms: the fragment arm answered %s where the whole-list arm answered %s (2026-09-28 audit, round 78, F78-L2-1)", gotFrag, gotWhole)
	}
	if fragText != wholeText {
		t.Errorf("one body, two arms: the fragment arm relayed %q as text where the whole-list arm relayed %q (2026-09-28 audit, round 78, F78-L2-1)", fragText, wholeText)
	}
	if fragText == "" {
		t.Errorf("the fragment arm relayed no prose for an entry that names nothing — its bytes are the model's output and reach the client as text (2026-09-28 audit, round 78, F78-L2-1)")
	}
}

// TestATruncatedTurnKeepsTheCallAndTheProse is F78-L2-2: with the same body and
// finish_reason "length" stated to both arms, the bytes joined onto the call no
// longer parsed, so the flush's truncation gate dropped the CALL with them and a
// streaming client was told max_tokens with nothing at all, where the whole-list
// arm kept the call and the text under stop_reason tool_use.
func TestATruncatedTurnKeepsTheCallAndTheProse(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	entries := []string{
		r59L2Frame(`{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`),
		r59L2Frame(`{"index":0,"id":"c1","type":"function","function":{"arguments":"{\"a\":1}"}}`),
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
		`data: [DONE]`,
	}
	script := strings.Join(entries, "\n\n") + "\n\n"
	whole := strings.Replace(r68Doc(
		`{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
			`{"index":0,"id":"c1","type":"function","function":{"arguments":"{\"a\":1}"}}`),
		`"finish_reason":"tool_calls"`, `"finish_reason":"length"`, 1)
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

	_, fragRaw := round54PostMessage(t, proxy, "glm-5.3", true)
	_, wholeRaw := round54PostMessage(t, proxy, "glm-5.3", false)
	if !strings.Contains(fragRaw, `"partial_json":"{\"a\":1}"`) {
		t.Errorf("the fragment arm relayed no call for a turn that truncated after a complete one: %q (2026-09-28 audit, round 78, F78-L2-2)", fragRaw)
	}
	if !strings.Contains(fragRaw, `"stop_reason":"tool_use"`) {
		t.Errorf("the fragment arm reported a stop_reason other than tool_use for a turn whose call it relayed: %q (2026-09-28 audit, round 78, F78-L2-2)", fragRaw)
	}
	var doc struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal([]byte(wholeRaw), &doc); err != nil {
		t.Fatalf("the whole-list arm did not parse: %v\n%s", err, wholeRaw)
	}
	types := ""
	for _, b := range doc.Content {
		types += r76Str(b["type"]) + ","
	}
	if types != "text,tool_use," {
		t.Errorf("the whole-list arm answered blocks %s, want text,tool_use (2026-09-28 audit, round 78, F78-L2-2)\n%s", types, wholeRaw)
	}
	_ = fragRaw
}
