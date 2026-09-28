package main

// round97_leg3_noncall_repeat_test.go — leg 3, round 97 (2026-09-29 audit),
// R97-L3-3.
//
// `restatesCarriedCall` asked `tb.name == ""` where the rest of the bridge asks
// `namesItself` (TrimSpace). A block opened by a name that is WHITESPACE is no
// call on any arm — namesItself is the bridge's own test for that, asked at
// every other site (round 66/67's rule) — so a byte-identical repeat of such an
// entry is not "the call already carried, listed again". Read raw, the
// blank-named block declared itself the carried call and the repeat was DROPPED
// at the fragment write (messages.go, the restatesCarriedCall drop): the model's
// bytes reached no client at all, where both document spellings of the same body
// relay every copy as prose. Measured on the frozen tree:
//
//	plain/adopt (whole document)  text "{\"h\":8}{\"h\":8}"   14 bytes
//	framed   (one entry per delta) text "{\"h\":8}"           7 bytes
//	three copies: 21 bytes against 7.
//
// The same bytes are prose on every arm now. The control keeps the drop that the
// gate exists for: a call the upstream really NAMED, listed again complete, is
// still dropped rather than appended (round 53's F2 — appended, the client
// accumulated `{"a":1}{"a":1}`).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r97ProseOf returns the prose and the tool-call arguments a client is handed,
// in wire order, for either an SSE stream or a whole document: the two carry the
// same JSON shapes, so one walk reads both.
func r97ProseOf(t *testing.T, body string) (texts, args []string) {
	t.Helper()
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				if s, ok := val.(string); ok {
					switch k {
					case "text":
						texts = append(texts, s)
					case "partial_json":
						args = append(args, s)
					}
					continue
				}
				if k == "input" {
					// A whole document states a tool_use block's input as an
					// object; a stream states it as partial_json deltas. One
					// reading either way.
					if m, err := json.Marshal(val); err == nil {
						args = append(args, string(m))
					}
					continue
				}
				walk(val)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "data: [DONE]" {
			continue
		}
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			payload = line
		}
		var v any
		if json.Unmarshal([]byte(payload), &v) == nil {
			walk(v)
		}
	}
	return texts, args
}

// round97Spellings runs one upstream turn three ways: the whole document under
// stream:false, the whole document under stream:true, and the same entries as
// one fragment per delta. It returns the prose and the argument bytes each
// spelling handed the client.
func round97Spellings(t *testing.T, entries, frames []string) (texts [][]string, args [][]string) {
	t.Helper()
	doc := `{"id":"chatcmpl-r97","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[` +
		strings.Join(entries, ",") + `]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`
	var writes []string
	for _, f := range frames {
		writes = append(writes, f+"\n\n")
	}
	writes = append(writes,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`+"\n\n",
		"data: [DONE]\n\n")

	plain := round45Upstream(t, "application/json", doc)
	adopted := round45Upstream(t, "text/event-stream", doc)
	framed := round45Frames(t, writes...)
	for _, up := range []*httptest.Server{plain, adopted, framed} {
		srv, _ := round39Gateway(t, up, nil)
		ask := round45AskPlain
		if up != plain {
			ask = round45AskStream
		}
		status, body := round45Ask(t, srv, ask)
		if status != http.StatusOK {
			t.Fatalf("status %d\n%s", status, body)
		}
		tx, ar := r97ProseOf(t, body)
		texts, args = append(texts, tx), append(args, ar)
	}
	return texts, args
}

// TestAByteIdenticalRepeatOfANoNameEntryIsNotDropped is R97-L3-3's pin.
func TestAByteIdenticalRepeatOfANoNameEntryIsNotDropped(t *testing.T) {
	const fragment = `{"h":8}`
	ws := `{"index":0,"id":"c1","type":"function","function":{"name":"  ","arguments":"{\"h\":8}"}}`
	wsf := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"  ","arguments":"{\"h\":8}"}}]}}]}`

	for _, tc := range []struct {
		name string
		n    int
	}{{"two copies", 2}, {"three copies", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			entries := make([]string, tc.n)
			frames := make([]string, tc.n)
			for i := range entries {
				entries[i], frames[i] = ws, wsf
			}
			texts, args := round97Spellings(t, entries, frames)
			names := []string{"plain", "adopted", "framed"}
			for i, tx := range texts {
				if len(args[i]) != 0 {
					t.Errorf("%s: the arm handed the client tool-call arguments for an entry that names no call: %q",
						names[i], args[i])
				}
				joined := strings.Join(tx, "")
				if strings.Count(joined, fragment) != tc.n {
					t.Errorf("%s: the client read %q, want %d copies of %s (2026-09-29 audit, round 97, R97-L3-3 — a repeat of an entry no arm delivers as a call is prose, not a repeat of a call)\n  entries: %d",
						names[i], joined, tc.n, fragment, tc.n)
				}
			}
			if strings.Join(texts[0], "") != strings.Join(texts[1], "") || strings.Join(texts[0], "") != strings.Join(texts[2], "") {
				t.Errorf("one upstream body, three client readings: plain %q, adopted %q, framed %q",
					texts[0], texts[1], texts[2])
			}
			t.Logf("%d copies: plain %q | adopted %q | framed %q", tc.n, texts[0], texts[1], texts[2])
		})
	}
}

// TestAFinishedCallListedAgainIsStillDropped is R97-L3-3's control (round 53's
// F2): the gate asks namesItself, and a call the upstream really named is named
// by it — the repeat is still dropped rather than appended.
func TestAFinishedCallListedAgainIsStillDropped(t *testing.T) {
	repeat := `{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	_, args := round97Spellings(t,
		[]string{repeat, repeat},
		[]string{
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		})
	for i, name := range []string{"plain", "adopted", "framed"} {
		got := strings.Join(args[i], "")
		if strings.Count(got, `{"a":1}`) != 1 {
			t.Errorf("%s: the call's arguments reached the client %d times (%q), want once — appended, the client accumulates JSON no tool parses (round 53's F2)",
				name, strings.Count(got, `{"a":1}`), got)
		}
	}
	t.Logf("framed args %q", args[2])
}
