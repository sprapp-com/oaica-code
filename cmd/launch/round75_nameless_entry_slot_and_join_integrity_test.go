package launch

// round75_nameless_entry_slot_and_join_integrity_test.go — leg 2, the two ways
// an entry the upstream never NAMED was read (2026-09-28 audit, round 75,
// F75-L2-1 and F75-L2-2).
//
// An entry whose `function.name` is empty is not a call on this path: its
// arguments reach the client as TEXT and no block is built from it. That is
// every arm's answer, and the two findings here are the two places this leg
// stopped keeping it.
//
//  1. The slot it occupies. The whole-list arm numbers slots the way the
//     fragment arm does (round 74), but it numbered only the entries it EMITS —
//     the numbering sat below the nameless skip — while the fragment arm
//     advances its counter for every indexed fragment it sees, nameless ones
//     included. Behind any nameless entry the list arm's numbering lagged the
//     wire's slots, and a later id-less entry was folded onto a slot the wire
//     had given the stated call: `[{nameless@0},{id c1,name Bash},{idless@0}]`
//     answered 2 calls as fragments and 1 as one list — legs 1 and 3 answering
//     2 — and the same wire with the id-less tail at index 1 answered 1 as
//     fragments and 2 as one list. The numbering now runs for every entry,
//     before the skip.
//  2. The call a later fragment names. The fragment arm's `startsANewToolCall`
//     answered "not a new call" for an accumulator with neither id nor name,
//     so a following fragment that NAMED a call was written into the nameless
//     one. Both entries of `[{nameless@0,args {"a":1}},{id c1,name Bash,args
//     {"a":1}}]` carry the same object and the client was handed ONE tool_use
//     whose input was `{"_raw":"{\"a\":1}{\"a\":1}"}` — a call no tool can run,
//     under an id of its own — where this leg's whole-list arm, the local
//     server's two arms and both arms of the gateway leg answer the same bytes
//     as one call with its own arguments plus the nameless entry's object as
//     text. A nameless accumulator is never the call a later fragment names.
//
// Recorded, not fixed (same round): the same wire with the arguments SPLIT
// across the two entries — the nameless prefix `{"a":` then the named entry
// carrying `1}` — still joins here and answers one call whose input is
// `{"a":1}`, where the list arm, both gateway arms and the local server's
// document arm answer the prefix as text and the named call as
// `{"_raw":"1}"}`. The joined answer is the working call and the others are
// not, so closing it moves this leg TOWARD an unrunnable answer; the direction
// is a decision about the header-late fragment wire (a fragment that states no
// name continued by one that does), not one line. Round 73's F73-L3-2 — a
// restatement folds only onto a twin the upstream named an id for — is the rule
// the other four arms read this wire by, and applying it to this leg's indexed
// routing path is what closing it would take.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	r75NamelessIdx0 = `{"index":0,"type":"function","function":{"name":"","arguments":"{\"a\":1}"}}`
	r75NamelessNoIx = `{"id":"x","type":"function","function":{"name":"","arguments":"{\"a\":1}"}}`
	r75StatedNoIx   = `{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	r75IdlessAt0    = `{"index":0,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	r75IdlessAt1    = `{"index":1,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
)

// r75Arms serves the SSE script to a streaming request and the whole
// completion to a non-streaming one and returns both arms' blocks, the text
// each relayed, and the raw bodies.
func r75Arms(t *testing.T, entries []string) (frag, whole []map[string]any, fragText, wholeText, fragRaw, wholeRaw string) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	frames := make([]string, 0, len(entries)+2)
	for _, e := range entries {
		frames = append(frames, r59L2Frame(e))
	}
	frames = append(frames, r59L2Fin, `data: [DONE]`)
	script := strings.Join(frames, "\n\n") + "\n\n"
	wholeBody := r68Doc(strings.Join(entries, ","))

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
		_, _ = io.WriteString(w, wholeBody)
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})

	code, fragRaw := round54PostMessage(t, proxy, "glm-5.3", true)
	if code != http.StatusOK {
		t.Fatalf("stream arm status %d\n%s", code, fragRaw)
	}
	frag = sseToolUseBlocks(t, fragRaw)
	fragText = r75StreamText(t, fragRaw)

	code, wholeRaw = round54PostMessage(t, proxy, "glm-5.3", false)
	if code != http.StatusOK {
		t.Fatalf("whole arm status %d\n%s", code, wholeRaw)
	}
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(wholeRaw), &resp); err != nil {
		t.Fatalf("whole arm did not parse: %v\n%s", err, wholeRaw)
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(wholeRaw), &struct {
		Content *[]map[string]any `json:"content"`
	}{&blocks}); err == nil {
		for _, b := range blocks {
			if b["type"] == "tool_use" {
				whole = append(whole, map[string]any{"id": b["id"], "name": b["name"], "input": b["input"]})
			}
		}
	}
	for _, b := range resp.Content {
		if b.Type == "text" {
			wholeText += b.Text
		}
	}
	return frag, whole, fragText, wholeText, fragRaw, wholeRaw
}

// r75StreamText collects the text deltas the streaming arm relayed.
func r75StreamText(t *testing.T, body string) string {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		if ev["type"] != "content_block_delta" {
			continue
		}
		d, _ := ev["delta"].(map[string]any)
		if d == nil || d["type"] != "text_delta" {
			continue
		}
		s, _ := d["text"].(string)
		sb.WriteString(s)
	}
	return sb.String()
}

func r75Input(t *testing.T, b map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(b["input"])
	if err != nil {
		t.Fatalf("block input did not marshal: %v", err)
	}
	return string(raw)
}

// TestTheListArmNumbersANamelessEntrysSlot is the F75-L2-1 pin: the numbering
// runs for every entry of the list, so an entry this path does not emit still
// consumes the slot the fragment arm gave it.
func TestTheListArmNumbersANamelessEntrysSlot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		want    int
	}{
		{"a nameless entry at index 0, then a stated call, then an id-less one at index 0",
			[]string{r75NamelessIdx0, r75StatedNoIx, r75IdlessAt0}, 2},
		{"the same with the id-less tail at index 1",
			[]string{r75NamelessIdx0, r75StatedNoIx, r75IdlessAt1}, 1},
		{"a nameless entry with no index, then a stated call, then an id-less one at index 0",
			[]string{r75NamelessNoIx, r75StatedNoIx, r75IdlessAt0}, 2},
		{"the same with the id-less tail at index 1",
			[]string{r75NamelessNoIx, r75StatedNoIx, r75IdlessAt1}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frag, whole, _, _, fragRaw, wholeRaw := r75Arms(t, tc.entries)
			if len(frag) != len(whole) {
				t.Errorf("this leg answered %d call(s) as fragments and %d as one list for the SAME body; a nameless entry is not emitted but it is still a slot on the wire, and the numbering has to run for it (2026-09-28 audit, round 75, F75-L2-1)\nfrag: %v\nwhole: %v",
					len(frag), len(whole), frag, whole)
			}
			if len(frag) != tc.want {
				t.Errorf("the fragment arm answered %d call(s), want %d — legs 1 and 3 answer %d for this body (2026-09-28 audit, round 75, F75-L2-1)\n%v\n%s",
					len(frag), tc.want, tc.want, frag, fragRaw)
			}
			if len(whole) != tc.want {
				t.Errorf("the whole-list arm answered %d call(s), want %d — legs 1 and 3 answer %d for this body (2026-09-28 audit, round 75, F75-L2-1)\n%v\n%s",
					len(whole), tc.want, tc.want, whole, wholeRaw)
			}
		})
	}
}

// TestANamelessFragmentIsNotTheCallALaterFragmentNames is the F75-L2-2 pin: a
// fragment that names a call begins one, and the arguments of the nameless
// fragment before it are relayed as text — never folded into that call.
func TestANamelessFragmentIsNotTheCallALaterFragmentNames(t *testing.T) {
	entries := []string{r75NamelessIdx0, r75StatedNoIx}
	frag, whole, fragText, wholeText, fragRaw, wholeRaw := r75Arms(t, entries)

	if len(frag) != 1 || len(whole) != 1 {
		t.Fatalf("the two arms answered %d and %d call(s), want 1 each (2026-09-28 audit, round 75, F75-L2-2)\nfrag: %v\nwhole: %v",
			len(frag), len(whole), frag, whole)
	}
	if got := r75Input(t, frag[0]); got != r75Input(t, whole[0]) {
		t.Errorf("the fragment arm delivered the call with input %s and the whole-list arm with %s — the named call keeps its own arguments, and the nameless fragment's object is text (2026-09-28 audit, round 75, F75-L2-2)\n%s",
			got, r75Input(t, whole[0]), fragRaw)
	}
	if !strings.Contains(r75Input(t, frag[0]), `"a"`) || strings.Contains(r75Input(t, frag[0]), "_raw") {
		t.Errorf("the fragment arm delivered the call with input %s — the nameless fragment's arguments were folded into the call a later fragment named, and no tool can run that input (2026-09-28 audit, round 75, F75-L2-2)\n%s",
			r75Input(t, frag[0]), fragRaw)
	}
	if fragText != wholeText {
		t.Errorf("the fragment arm relayed %q as text and the whole-list arm %q — a nameless entry's arguments reach the client as text on every arm (2026-09-28 audit, round 75, F75-L2-2)\n%s\n%s",
			fragText, wholeText, fragRaw, wholeRaw)
	}
	if !strings.Contains(fragText, `"a"`) {
		t.Errorf("the fragment arm relayed no text for the nameless entry (text %q); its arguments reach the client as text rather than as a block (2026-09-28 audit, round 75, F75-L2-2)", fragText)
	}
}
