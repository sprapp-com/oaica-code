package launch

// round37_nested_block_and_source_integrity_test.go — the round-36 fixes for
// nested blocks and search sources, probed one type over (2026-09-27 audit,
// round 37, A-F2/A-F3/A-F6/A-F7).

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ollama/ollama/anthropic"
)

// convertLaunchTurn runs one Anthropic body through this leg's converter and
// returns the OpenAI wire the upstream would receive.
func convertLaunchTurn(t *testing.T, body []byte) string {
	t.Helper()
	var anthReq anthropic.MessagesRequest
	if err := json.Unmarshal(body, &anthReq); err != nil {
		t.Fatalf("the request did not parse: %v", err)
	}
	chatReq, err := anthropic.FromMessagesRequest(anthReq)
	if err != nil {
		t.Fatalf("the request did not convert: %v", err)
	}
	wire, err := json.Marshal(chatRequestToOpenAI(chatReq, anthReq, anthReq.Model))
	if err != nil {
		t.Fatal(err)
	}
	return string(wire)
}

// TestANestedSearchResultIsNotDroppedByTheToolResultConverter is A-F2: round 36
// gave `convertToolResultContent` a case for a nested document and left a
// `default` out, so every other nested type was still skipped in silence —
// including a search_result, whose passages the gateway leg puts in the same
// prompt. The model was asked about text it never received, on this leg only.
func TestANestedSearchResultIsNotDroppedByTheToolResultConverter(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const passage = "NESTED-KEEP: the passage the search returned"
	wire := convertLaunchTurn(t, round34MessagesBody(t, "user", []map[string]any{
		{"type": "tool_result", "tool_use_id": "toolu_1", "content": []map[string]any{
			{"type": "text", "text": "the search ran"},
			{"type": "search_result", "title": "Result", "source": "https://example.com/r",
				"content": []map[string]any{{"type": "text", "text": passage}}},
		}},
	}, 4096))

	if !bytes.Contains([]byte(wire), []byte("NESTED-KEEP")) {
		t.Errorf("a search_result nested in a tool result left no trace in the prompt: %s\nthe client put the passages in the body and the gateway leg forwards the same block, so the same turn is answered from a prompt holding evidence on one leg and nothing on the other", wire)
	}
}

// TestSearchResultTextDoesNotReadAStrangersTextKey is A-F3: searchResultText
// pasted `text` from a block of ANY type and dropped a block whose passage sat
// under another key, because it never looked at the type. A passage that
// arrives in a described form must be recognisable as a description, not
// silently pasted as though it were a text block.
func TestSearchResultTextDoesNotReadAStrangersTextKey(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	wire := convertLaunchTurn(t, round34MessagesBody(t, "user", []map[string]any{
		{"type": "search_result", "title": "T", "source": "https://example.com/s",
			"content": []map[string]any{
				{"type": "text", "text": "PASSAGE"},
				{"type": "future_block", "text": "FUTURE"},
				{"type": "search_result", "content": []map[string]any{{"type": "text", "text": "INNER"}}},
			}},
	}, 4096))

	if !strings.Contains(wire, "PASSAGE") {
		t.Errorf("the passage the search returned is missing from the prompt: %s", wire)
	}
	if strings.Contains(wire, `PASSAGE\nFUTURE`) {
		t.Errorf("a block of an unknown type was pasted as though its `text` key were a passage rather than described: %s\nthe type is what says whether a block's text is a passage, and reading the key regardless put a stranger's field into the prompt as if the search had returned it", wire)
	}
	if !strings.Contains(wire, "future_block") {
		t.Errorf("the block of an unknown type left no trace at all: %s", wire)
	}
	if !strings.Contains(wire, "INNER") {
		t.Errorf("a nested search_result's passage was dropped: %s", wire)
	}
}

// TestASearchResultSourceObjectKeepsItsURL is A-F6: the block's source is a
// bare URL string in the documented spelling and an object in the other, and
// the converter read only the bare one — so a URL the body stated was accepted
// by UnmarshalJSON and then discarded, and the model was shown passages with
// their origin stripped.
func TestASearchResultSourceObjectKeepsItsURL(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	wire := convertLaunchTurn(t, round34MessagesBody(t, "user", []map[string]any{
		{"type": "search_result", "title": "T",
			"source":  map[string]any{"type": "url", "url": "https://obj.example/x"},
			"content": []map[string]any{{"type": "text", "text": "PASSAGE-D"}}},
	}, 4096))

	if !bytes.Contains([]byte(wire), []byte("https://obj.example/x")) {
		t.Errorf("the URL the block's source states is not in the prompt: %s\nthe object spelling parses without error, so the turn is answered 200 with the origin of every passage silently removed", wire)
	}
}

// TestTheToolResultJoinMatchesTheGatewayLeg is A-F7: the two legs assemble the
// same blocks with different separators, so the prompt for one body is not the
// same bytes depending on which leg serves it. This is the separator the
// gateway's toolResultText uses.
func TestTheToolResultJoinMatchesTheGatewayLeg(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var anthReq anthropic.MessagesRequest
	if err := json.Unmarshal(round34MessagesBody(t, "user", []map[string]any{
		{"type": "tool_result", "tool_use_id": "toolu_1", "content": []map[string]any{
			{"type": "text", "text": "A"},
			{"type": "document", "source": map[string]any{
				"type": "text", "media_type": "text/plain", "data": "FILETEXT"}},
		}},
	}, 4096), &anthReq); err != nil {
		t.Fatal(err)
	}
	chatReq, err := anthropic.FromMessagesRequest(anthReq)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, m := range chatReq.Messages {
		if m.Role == "tool" {
			got = m.Content
		}
	}
	if got != "A\nFILETEXT" {
		t.Errorf("the assembled tool result is %q: the gateway leg joins the same blocks with a single newline, so a blank line here makes the prompt a different string on each leg — and the two legs must measure and send the same prompt", got)
	}
}
