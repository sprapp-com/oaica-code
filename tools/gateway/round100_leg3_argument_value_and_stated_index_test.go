package main

// round100_leg3_argument_value_and_stated_index_test.go — leg 3, round 100
// (2026-09-29 audit), F100-L3-1.
//
// One upstream turn is spelled three ways on this leg: the whole document under
// stream:false (plain), the same document answering a client that asked for a
// stream (adopted), and the same entries one fragment per delta (framed). One
// body, one call, on every arm.
//
// `arguments` is spelled as a STRING holding the argument object's text — what
// every arm of this leg relays — but a server that hands its own parse of the
// model's object rather than the text of it states the same arguments as a JSON
// VALUE, and `oaToolCall`'s typed string fields refused the frame (and the
// document) for it:
//
//	"arguments":{"cmd":"ls"}
//	  plain    502 {"error":{"message":"unparseable upstream response",...}}
//	  adopted  502 same
//	  framed   200 text "hi", stop_reason end_turn — NO call at all
//
// The framed arm did not merely disagree: the frame the decoder refused was
// dropped silently, the call reached the client on no arm, and a streaming client
// was told the turn ended normally, so an agent loop stops without running the
// tool. The document arms instead refused the whole body. The same shape with the
// introducing fragment carrying a string and the continuation the value reads
// 200 on the framed arm with `input:{}` — a call the client runs with no
// arguments — where the document arms are 502.
//
// The same rule covers the slot: a server that stringifies its integers states
// `"index":"0"`, which the typed pointer refused the same way. Both are read as
// what they spell — a value as its own compact JSON, a numeric string as the
// number — so the turn above is `call:call_1|Read|{"cmd":"ls"}` on all three arms
// (app/tools/tools.go reads the same field as json.RawMessage).

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// r100Arms runs one upstream body three ways and answers the ordered blocks each
// arm handed its client, and the body it answered with. Every arm must serve the
// body: one upstream body, one verdict.
func r100Arms(t *testing.T, doc string, frames []string) ([3][]string, [3]string) {
	t.Helper()
	blocks, bodies := r100RunArms(t, doc, frames)
	for i, body := range bodies {
		if !strings.Contains(body, `"error"`) {
			continue
		}
		t.Errorf("the %s arm answered %s for a turn its sibling arms serve — one upstream body, one verdict (2026-09-29 audit, round 100, F100-L3-1)",
			[3]string{"plain", "adopted", "framed"}[i], strings.TrimSpace(body))
	}
	return blocks, bodies
}

// r100RunArms runs the three arms and answers what each said, judging nothing.
func r100RunArms(t *testing.T, doc string, frames []string) ([3][]string, [3]string) {
	t.Helper()
	var writes []string
	for _, f := range frames {
		writes = append(writes, f+"\n\n")
	}
	ups := []*httptest.Server{
		round45Upstream(t, "application/json", doc),
		round45Upstream(t, "text/event-stream", doc),
		round45Frames(t, writes...),
	}
	var out [3][]string
	var bodies [3]string
	for i, up := range ups {
		srv, _ := round39Gateway(t, up, nil)
		ask := round45AskPlain
		if i > 0 {
			ask = round45AskStream
		}
		_, body := round45Ask(t, srv, ask)
		bodies[i] = body
		out[i] = round98Blocks(t, body)
	}
	return out, bodies
}

func TestAnArgumentsValueIsTheArgumentTextEveryArmReads(t *testing.T) {
	for _, c := range []struct {
		name   string
		entry  string
		frames []string
	}{
		{
			"the value spelling in one document",
			`{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":{"cmd":"ls"}}}`,
			[]string{
				`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				round98Frame(`{"index":0,"id":"call_1","function":{"name":"Read","arguments":{"cmd":"ls"}}}`),
				round98Tail("tool_calls"),
			},
		},
		{
			"the value spelling on a continuation fragment",
			`{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":{"cmd":"ls"}}}`,
			[]string{
				`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				round98Frame(`{"index":0,"id":"call_1","function":{"name":"Read","arguments":""}}`),
				round98Frame(`{"index":0,"function":{"arguments":{"cmd":"ls"}}}`),
				round98Tail("tool_calls"),
			},
		},
		{
			"the slot stated as a numeric string",
			`{"index":"0","id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"cmd\":\"ls\"}"}}`,
			[]string{
				`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				round98Frame(`{"index":"0","id":"call_1","function":{"name":"Read","arguments":"{\"cmd\":\"ls\"}"}}`),
				round98Tail("tool_calls"),
			},
		},
		{
			"the string spelling every arm has always read",
			`{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"cmd\":\"ls\"}"}}`,
			[]string{
				`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				round98Frame(`{"index":0,"id":"call_1","function":{"name":"Read","arguments":"{\"cmd\":\"ls\"}"}}`),
				round98Tail("tool_calls"),
			},
		},
		{
			"a call that states no arguments at all",
			`{"index":0,"id":"call_1","type":"function","function":{"name":"Read"}}`,
			[]string{
				`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				round98Frame(`{"index":0,"id":"call_1","function":{"name":"Read"}}`),
				round98Tail("tool_calls"),
			},
		},
		{
			"a call whose arguments field is null",
			`{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":null}}`,
			[]string{
				`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				round98Frame(`{"index":0,"id":"call_1","function":{"name":"Read","arguments":null}}`),
				round98Tail("tool_calls"),
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := round98Doc("tool_calls", `"hi"`, c.entry)
			arms, _ := r100Arms(t, doc, c.frames)
			want := "text:hi call:call_1|Read|{\"cmd\":\"ls\"}"
			if strings.Contains(c.name, "no arguments") || strings.Contains(c.name, "null") {
				want = "text:hi call:call_1|Read|{}"
			}
			for i, name := range [3]string{"plain", "adopted", "framed"} {
				if got := strings.Join(arms[i], " "); got != want {
					t.Errorf("%s arm reads %q, want %q — an `arguments` value is the argument text its sibling spelling would have carried, and a slot spelled as a numeric string is that slot, so one upstream body is one call on every arm (2026-09-29 audit, round 100, F100-L3-1)",
						name, got, want)
				}
			}
		})
	}
}
