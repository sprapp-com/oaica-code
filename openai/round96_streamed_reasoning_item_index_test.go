package openai

// round96_streamed_reasoning_item_index_test.go — leg 1, F96-L1-3 (2026-09-29
// audit, round 96).
//
// A streamed turn that thinks, answers, then thinks again reached a streaming
// client as a reasoning item whose summary was the FIRST run alone, while the
// terminal document the same stream ended in stated the whole of it — and the
// resumed run's delta named an output_index no item was ever announced at,
// because the item claimed its index when it was CLOSED rather than when it was
// announced, so a close that happened after another item had been announced
// named that item's index. Measured with the probe that found it:
//
//	chunks [think t1][text hello][think t2]
//	  events   reasoning item added@0, done@0 text=t1, delta@2 t2
//	  document reasoning@0 summary=t1t2, message@1
//
// One turn, one stream, two statements of the same reasoning item. The pin is
// the agreement itself: every index an event names is an index an item was
// announced at, and the summary the events close the item with is the summary
// the terminal document states.
//
// The shape has a live producer: the in-tree parsers emit thinking after
// content inside one turn (gemma4's <|channel>, ministral's [THINK], cogito,
// deepseek3, olmo3-think all answer `think, prose, think` for a turn that
// thinks twice).

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/api"
)

// r96AnnouncedIndexes is every output_index the stream announced an item at.
func r96AnnouncedIndexes(events []ResponsesStreamEvent) map[string]bool {
	out := map[string]bool{}
	for _, ev := range events {
		if ev.Event != "response.output_item.added" {
			continue
		}
		data, _ := ev.Data.(map[string]any)
		out[jsonScalar(data["output_index"])] = true
	}
	return out
}

func jsonScalar(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// r96NamedIndexes is every output_index any event named.
func r96NamedIndexes(events []ResponsesStreamEvent) []string {
	var out []string
	for _, ev := range events {
		data, _ := ev.Data.(map[string]any)
		if v, ok := data["output_index"]; ok {
			out = append(out, jsonScalar(v))
		}
	}
	return out
}

// r96EventReasoningText is the summary text the events last state for the
// reasoning item — the last done/added statement of it, in event order.
func r96EventReasoningText(events []ResponsesStreamEvent) string {
	text := ""
	for _, ev := range events {
		data, _ := ev.Data.(map[string]any)
		switch ev.Event {
		case "response.reasoning_summary_text.done":
			text, _ = data["text"].(string)
		case "response.output_item.done":
			if item, ok := data["item"].(map[string]any); ok && item["type"] == "reasoning" {
				text = r96ItemSummaryText(item) // last word about THIS item wins
			}
		}
	}
	return text
}

// r96ItemSummaryText reads a reasoning item's summary text.
func r96ItemSummaryText(item any) string {
	m, _ := item.(map[string]any)
	if m == nil || m["type"] != "reasoning" {
		return ""
	}
	raw, _ := json.Marshal(m["summary"])
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	out := ""
	for _, p := range parts {
		out += p.Text
	}
	return out
}

// r96DocumentReasoningText is the reasoning item's summary text in the turn's
// terminal document.
func r96DocumentReasoningText(t *testing.T, events []ResponsesStreamEvent) string {
	t.Helper()
	for _, item := range completedOutput(t, events) {
		if text := r96ItemSummaryText(item); text != "" {
			return text
		}
	}
	return ""
}

// r96StreamedTurn drives one streamed turn to its terminal document.
func r96StreamedTurn(t *testing.T, chunks ...api.ChatResponse) []ResponsesStreamEvent {
	t.Helper()
	c := NewResponsesStreamConverter("resp_1", "msg_1", "m", ResponsesRequest{})
	var events []ResponsesStreamEvent
	for _, chunk := range chunks {
		events = append(events, c.Process(chunk)...)
	}
	return append(events, c.Process(api.ChatResponse{Model: "m", Done: true, Message: api.Message{Role: "assistant"}})...)
}

func r96Think(s string) api.ChatResponse {
	return api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Thinking: s}}
}

func r96Text(s string) api.ChatResponse {
	return api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: s}}
}

// One streamed turn's events never name an index no item was announced at, and
// the summary they close the reasoning item with is the one the terminal
// document states — whichever way the turn's thinking was interrupted.
func TestAStreamedTurnsReasoningItemIsTheItemItsDocumentStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []api.ChatResponse
		want   string
	}{
		{"thinking, then the answer, then thinking again", []api.ChatResponse{r96Think("t1"), r96Text("hello"), r96Think("t2")}, "t1t2"},
		{"thinking, a call, then thinking again", []api.ChatResponse{r96Think("t1"), toolCallResponse("Read"), r96Think("t2")}, "t1t2"},
		{"thinking alone", []api.ChatResponse{r96Think("t1")}, "t1"},
		{"the answer, then thinking", []api.ChatResponse{r96Text("hello"), r96Think("t1")}, "t1"},
		{"one uninterrupted run", []api.ChatResponse{r96Think("t1"), r96Text("hello")}, "t1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := r96StreamedTurn(t, tc.chunks...)
			announced := r96AnnouncedIndexes(events)
			for _, idx := range r96NamedIndexes(events) {
				if !announced[idx] {
					t.Errorf("an event names output_index %s and no item was announced there (announced %v) — one turn, one stream, and an index that names nothing (2026-09-29 audit, round 96, F96-L1-3)", idx, announced)
				}
			}
			eventText := r96EventReasoningText(events)
			docText := r96DocumentReasoningText(t, events)
			if eventText != docText {
				t.Errorf("one reasoning item, two summaries (2026-09-29 audit, round 96, F96-L1-3):\n  the events close it with %q\n  the document states     %q", eventText, docText)
			}
			if docText != tc.want {
				t.Errorf("the document states the reasoning summary %q, want %q", docText, tc.want)
			}
		})
	}
}

// The ordinary shapes are unchanged by the index claim moving to the item's
// announcement: one reasoning item, announced at 0, and the next item at 1.
func TestTheOrdinaryReasoningShapesAnnounceTheirIndexesAsBefore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []api.ChatResponse
		want   []string
	}{
		{"thinking then the answer", []api.ChatResponse{r96Think("t1"), r96Text("hello")}, []string{"0", "1"}},
		{"the answer then thinking", []api.ChatResponse{r96Text("hello"), r96Think("t1")}, []string{"0", "1"}},
		{"thinking, a call", []api.ChatResponse{r96Think("t1"), toolCallResponse("Read")}, []string{"0", "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := r96StreamedTurn(t, tc.chunks...)
			var got []string
			for _, ev := range events {
				if ev.Event != "response.output_item.added" {
					continue
				}
				data, _ := ev.Data.(map[string]any)
				got = append(got, jsonScalar(data["output_index"]))
			}
			if len(got) != len(tc.want) {
				t.Fatalf("items announced at %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("items announced at %v, want %v — an ordinary turn's indexes must not move", got, tc.want)
					break
				}
			}
		})
	}
}
