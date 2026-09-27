package launch

// round47_proxy_wire_test.go — round 47's findings on the client proxy leg.
//
// Every finding here was reproduced on the unmodified tree before it was fixed.
//
// A-F1: an upstream stream whose only frames were an empty delta with a
// finish_reason, and the [DONE] sentinel, was relayed as a complete turn — 200
// with an empty answer — where the byte-identical outcome with no frames at all
// is refused. The client leg is the third translation site: the local leg's
// converter and the gateway's noAnswer arm both refuse it.
//
// C-F3: a whole completion arriving inside a frame was adopted and then the
// stream was ABANDONED — every later delta was dropped, so a turn that carried
// text after its whole-completion frame reached the client truncated to the
// frame. The local leg's converter continues, and so does the gateway.
//
// A-F2: the prompt-size unit counted the request ENVELOPE — the model id,
// `stream`, `stream_options` — which the upstream's chat template never renders
// and its prompt_tokens never count. The gateway leg measures messages plus tool
// schemas, so the two legs' calibrated ratios disagreed by a constant, and the
// same prompt measured 12 tokens more with `stream: true` (the flag Claude Code
// always sets).
//
// C-F8: the output estimate counted only the NAMED calls' arguments, so the raw
// arguments of a call the upstream never named — relayed to the client as text
// by flushToolCalls — reached it with output_tokens=0. The gateway leg's own
// byte tally counts those arguments (its relayDelta books them), so the same
// turn's output count depended on the leg that answered it.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAnEmptyCompletionIsNotATurn is A-F1: the upstream said stop over an empty
// delta and sent [DONE], which is not an answer.
func TestAnEmptyCompletionIsNotATurn(t *testing.T) {
	t.Run("finish_reason_only", func(t *testing.T) {
		up := streamUpstream(t, "data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true)
		proxy := startCalibProxy(t, up.URL, "r47-af1a")
		body, status := postMessagesStream(t, proxy)
		if status == 200 {
			t.Errorf("an upstream turn that carried nothing was relayed as a complete one:\n%s", body)
		}
		if !strings.Contains(body, "empty completion") {
			t.Errorf("the refusal does not name the upstream's empty answer:\n%s", body)
		}
	})

	t.Run("sentinel_only", func(t *testing.T) {
		up := streamUpstream(t, "data: [DONE]\n\n", true)
		proxy := startCalibProxy(t, up.URL, "r47-af1b")
		body, status := postMessagesStream(t, proxy)
		if status == 200 {
			t.Errorf("[DONE] over an empty stream was relayed as a complete turn:\n%s", body)
		}
	})
}

// TestATurnKeepsStreamingAfterAWholeCompletion is C-F3: the frames that follow
// an adopted whole completion are the rest of the same turn.
func TestATurnKeepsStreamingAfterAWholeCompletion(t *testing.T) {
	up := streamUpstream(t,
		"data: {\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"hi there\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\" and more\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true)
	proxy := startCalibProxy(t, up.URL, "r47-cf3")
	body, status := postMessagesStream(t, proxy)
	if status != 200 {
		t.Fatalf("status %d\n%s", status, body)
	}
	for _, want := range []string{"hi there", " and more"} {
		if !strings.Contains(body, want) {
			t.Errorf("the turn stops at its whole-completion frame: %q never reached the client\n%s", want, body)
		}
	}
	// The estimate is the adopted text PLUS the deltas that followed it, so the
	// count is evidence of the same thing the text is: 17 characters is 5.
	if got := usageInt(deltaUsage(t, body), "output_tokens"); got != 5 {
		t.Errorf("output_tokens=%d, want 5 (\"hi there and more\" = 17 characters) — the deltas after the adopted frame are not counted as the turn's output\n%s", got, body)
	}
}

// TestTheNamelessCallTextIsCountedAsOutput is C-F8: a fragment that names no
// call still writes arguments, and finishStream relays them to the client as
// text (round 39's B-F8). Text the client reads is output.
func TestTheNamelessCallTextIsCountedAsOutput(t *testing.T) {
	up := streamUpstream(t,
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"\",\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true)
	proxy := startCalibProxy(t, up.URL, "r47-cf8")
	body, status := postMessagesStream(t, proxy)
	if status != 200 {
		t.Fatalf("status %d\n%s", status, body)
	}
	if !strings.Contains(body, `{\"a\":1}`) {
		t.Fatalf("the nameless call's arguments never reached the client as text:\n%s", body)
	}
	if got := usageInt(deltaUsage(t, body), "output_tokens"); got <= 0 {
		t.Errorf("output_tokens=%d for a turn whose text the client just read — the nameless call's arguments are relayed but not counted\n%s", got, body)
	}
}

// TestTheMeasuredPromptDoesNotDependOnStream is A-F2. Nothing about the prompt
// changes when the caller asks for a stream, so the unit both legs measure must
// not either.
func TestTheMeasuredPromptDoesNotDependOnStream(t *testing.T) {
	build := func(stream bool) []byte {
		b, err := json.Marshal(map[string]any{
			"model":      "kat-awq",
			"max_tokens": 256,
			"stream":     stream,
			"messages": []map[string]any{
				{"role": "user", "content": "what does this file do?"},
				{"role": "assistant", "content": "It parses the header."},
				{"role": "user", "content": "and the trailer?"},
			},
			"tools": []map[string]any{{
				"name": "Read", "description": "read a file",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	plain := clientPromptBytes(build(false))
	streamed := clientPromptBytes(build(true))
	if plain != streamed {
		t.Errorf("the same prompt measures %d bytes with stream:false and %d with stream:true — the unit counts the request envelope, not the prompt, and the gateway leg measures the prompt alone (its messagesBytes charges \"messages\" plus \"tools\"/\"functions\")", plain, streamed)
	}
}
