package main

// round73_second_choice_integrity_test.go — leg 3, one body whose completion
// states a SECOND choice (2026-09-28 audit, round 73, F73-L3-1).
//
// The gateway never states `n`, so a second choice is the upstream's own doing —
// and one client request still produced one message here. The non-stream
// document path and the whole-message arm read `Choices[0]`; the fragment arm
// relayed EVERY choice in every chunk, so the alternatives were spliced into one
// turn ("AB" where the same body written as one list, and the client leg under
// either spelling, answer "A"), and a call stated only by the second choice was
// handed to the client as an executable tool_use under a tool_use verdict. Leg 2
// closed the same hole positionally (round 67, F67-L2-1).

import (
	"encoding/json"
	"strings"
	"testing"
)

// r73Read reads a relayed turn into: the prose, the tool_use ids in block order,
// the assembled input per id, and the stop_reason.
func r73Read(t *testing.T, body string) (text string, ids []string, inputs map[string]string, stop string) {
	t.Helper()
	inputs = map[string]string{}
	parts := map[int]*strings.Builder{}
	stated := map[int]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				ID    string          `json:"id"`
				Type  string          `json:"type"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				ids = append(ids, ev.ContentBlock.ID)
				parts[ev.Index] = &strings.Builder{}
				stated[ev.Index] = string(ev.ContentBlock.Input)
			}
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" {
				text += ev.Delta.Text
			}
			if ev.Delta.Type == "input_json_delta" {
				if b := parts[ev.Index]; b != nil {
					b.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "message_delta":
			stop = ev.Delta.StopReason
		}
	}
	for i, id := range ids {
		if b := parts[i]; b != nil {
			if s := b.String(); s != "" {
				inputs[id] = s
				continue
			}
			inputs[id] = stated[i]
		}
	}
	return text, ids, inputs, stop
}

// r73DocTurn relays one whole body to a stream request.
func r73DocTurn(t *testing.T, doc string) string {
	t.Helper()
	_, body := r73DocAsk(t, doc)
	return body
}

// r73DocAsk relays one whole body to a stream request and hands back the status
// and the turn.
func r73DocAsk(t *testing.T, doc string) (int, string) {
	t.Helper()
	up := round45Upstream(t, "application/json", doc)
	srv, _ := round39Gateway(t, up, nil)
	return round45Ask(t, srv, round45AskStream)
}

// r73FrameAsk relays frames and hands back the status and the raw turn.
// round45Frames does NOT append a finish frame of its own (r58FrameCalls does),
// so the frames are exactly what is stated here.
func r73FrameAsk(t *testing.T, frames ...string) (int, string) {
	t.Helper()
	lines := make([]string, 0, len(frames))
	for _, f := range frames {
		lines = append(lines, "data: "+f)
	}
	lines = append(lines, "data: [DONE]")
	srv, _ := round39Gateway(t, round45Frames(t, lines...), nil)
	return round45Ask(t, srv, round45AskStream)
}

// r73FrameTurn is r73FrameAsk's turn alone.
func r73FrameTurn(t *testing.T, frames ...string) string {
	t.Helper()
	_, body := r73FrameAsk(t, frames...)
	return body
}

// TestATurnThatOnlyTheSecondChoiceStatesIsRefusedTheSameWay is the other half of
// F73-L3-1: a frame whose only content sits in a later choice relays nothing, so
// it commits nothing — the turn the one-list spelling refuses as empty must not
// be answered 200 because the upstream delivered the alternatives as deltas.
func TestATurnThatOnlyTheSecondChoiceStatesIsRefusedTheSameWay(t *testing.T) {
	doc := `{"id":"x","model":"m","choices":[` +
		`{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"},` +
		`{"index":1,"message":{"role":"assistant","content":"B"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
	docStatus, docBody := r73DocAsk(t, doc)
	frameStatus, frameBody := r73FrameAsk(t,
		`{"id":"x","model":"m","choices":[{"index":0,"delta":{}},{"index":1,"delta":{"content":"B"}}]}`,
		`{"id":"x","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)

	if docStatus != 502 {
		t.Fatalf("PREMISE: the one-list spelling refuses this turn %d, want 502 (%s)", docStatus, docBody)
	}
	if frameStatus != docStatus {
		t.Errorf("a turn only the second choice states is answered %d as fragments and %d as one list: a choice that is not the turn committed it anyway (2026-09-28 audit, round 73, F73-L3-1)\n%s",
			frameStatus, docStatus, frameBody)
	}
}

func TestASecondChoiceIsNotPartOfTheTurn(t *testing.T) {
	bodies := []struct {
		name   string
		doc    string
		frames []string
	}{
		{
			// One upstream body, two spellings: the whole completion (which the
			// document arm reads as Choices[0]) and the same content delivered as
			// deltas, one entry per choice, in one chunk.
			name: "the second choice states text",
			doc: `{"id":"x","model":"m","choices":[` +
				`{"index":0,"message":{"role":"assistant","content":"A"},"finish_reason":"stop"},` +
				`{"index":1,"message":{"role":"assistant","content":"B"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
			frames: []string{
				`{"id":"x","model":"m","choices":[{"index":0,"delta":{"content":"A"}},{"index":1,"delta":{"content":"B"}}]}`,
				`{"id":"x","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
		},
		{
			name: "the second choice states the call",
			doc: `{"id":"x","model":"m","choices":[` +
				`{"index":0,"message":{"role":"assistant","content":"A"},"finish_reason":"tool_calls"},` +
				`{"index":1,"message":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_9",` +
				`"type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],` +
				`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
			frames: []string{
				`{"id":"x","model":"m","choices":[{"index":0,"delta":{"content":"A"}}]}`,
				// The call is stated by the SECOND POSITION of one chunk — the
				// same shape as the whole completion above, where it is the
				// second choice's message.
				`{"id":"x","model":"m","choices":[{"index":0,"delta":{}},` +
					`{"index":1,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}]}}]}`,
				`{"id":"x","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			},
		},
		{
			// The same two-choice content delivered as a whole completion INSIDE
			// one frame: the adoption path, which reads Choices[0].
			name: "the second choice arrives in one whole-completion frame",
			doc: `{"id":"x","model":"m","choices":[` +
				`{"index":0,"message":{"role":"assistant","content":"A"},"finish_reason":"stop"},` +
				`{"index":1,"message":{"role":"assistant","content":"B"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
			frames: []string{`{"id":"x","model":"m","choices":[` +
				`{"index":0,"message":{"role":"assistant","content":"A"},"finish_reason":"stop"},` +
				`{"index":1,"message":{"role":"assistant","content":"B"},"finish_reason":"stop"}]}`},
		},
	}
	for _, tc := range bodies {
		t.Run(tc.name, func(t *testing.T) {
			docBody := r73DocTurn(t, tc.doc)
			frameBody := r73FrameTurn(t, tc.frames...)

			docText, docIDs, docInputs, docStop := r73Read(t, docBody)
			frameText, frameIDs, frameInputs, frameStop := r73Read(t, frameBody)

			// Premise: the one-list spelling reads the first choice alone.
			if len(docIDs) != 0 {
				t.Fatalf("PREMISE: the one-list spelling answers %v, want no call", docIDs)
			}
			if docText != "A" {
				t.Fatalf("PREMISE: the one-list spelling relays %q, want %q", docText, "A")
			}

			if frameText != docText {
				t.Errorf("the same body relays %q as fragments and %q as one list: the second choice's text was spliced into the turn (2026-09-28 audit, round 73, F73-L3-1)\n%s\n%s",
					frameText, docText, frameBody, tc.doc)
			}
			if len(frameIDs) != len(docIDs) {
				t.Errorf("the same body answers %v as fragments and %v as one list: a call only the second choice stated was handed to the client (2026-09-28 audit, round 73, F73-L3-1)\n%s",
					frameIDs, docIDs, frameBody)
			}
			for i := range frameIDs {
				if i < len(docIDs) && (frameIDs[i] != docIDs[i] || frameInputs[frameIDs[i]] != docInputs[docIDs[i]]) {
					t.Errorf("call %d is %s %s as fragments and %s %s as one list\n%s",
						i, frameIDs[i], frameInputs[frameIDs[i]], docIDs[i], docInputs[docIDs[i]], frameBody)
				}
			}
			if frameStop != docStop {
				t.Errorf("the same body ends the turn with stop_reason %q as fragments and %q as one list (2026-09-28 audit, round 73, F73-L3-1)", frameStop, docStop)
			}
		})
	}
}
