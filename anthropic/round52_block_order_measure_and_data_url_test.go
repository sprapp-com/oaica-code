package anthropic

// round52_block_order_measure_and_data_url_test.go — round 52's findings on the
// local leg.
//
// A1: a turn's own blocks and its tool results were written back in two groups
// — every own block before or after every result — so the client's own text was
// hoisted past the result it followed. `[text, result, text]` reached the model
// as `[text, result]` with the second text merged into the first, and
// `[result, text, result]` deferred the text past both results. One client body
// was two different conversations, and the metered gateway leg writes the same
// blocks in the order they arrived.
//
// A2: a tool was charged for the length of its NAME, its DESCRIPTION and the
// raw `input_schema` bytes, while every other measure here is the bytes of the
// object that is sent (clampedJSONBytes, escapes subtracted). The object the
// converter forwards is longer than those three fields — it carries the
// structural keys of the tool and of its parameters — and a pretty-printed
// schema is longer than a compact one, so the same tool was charged two
// different numbers by the client's JSON formatting alone. That number seeds
// the client-visible input_tokens and the session's auto-compaction.
//
// A3: a `source` of type "url" whose url IS a `data:` URL was carried as url
// TEXT. The OpenAI door both other legs hand it to decodes exactly these forms
// (data:;base64,… and data:image/{jpeg,jpg,png,webp};base64,…), so the same
// block was an image on the gateway leg and, on this one, was either forwarded
// as an unfetchable string or refused in words (2026-09-27 audit, round 52).

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func round52Text(s string) *string { return &s }

// TestATurnsBlocksKeepTheOrderTheClientWroteThem is A1.
func TestATurnsBlocksKeepTheOrderTheClientWroteThem(t *testing.T) {
	for _, tc := range []struct {
		name    string
		role    string
		content []ContentBlock
		// wantRoles is the role of each converted message, in order; wantText
		// the content of the message at the same position.
		wantRoles []string
		wantText  []string
	}{
		{
			name: "text result text",
			role: "user",
			content: []ContentBlock{
				{Type: "text", Text: round52Text("first")},
				{Type: "tool_result", ToolUseID: "c1", Content: "one"},
				{Type: "text", Text: round52Text("second")},
			},
			wantRoles: []string{"user", "tool", "user"},
			wantText:  []string{"first", "one", "second"},
		},
		{
			name: "result text result",
			role: "user",
			content: []ContentBlock{
				{Type: "tool_result", ToolUseID: "c1", Content: "one"},
				{Type: "text", Text: round52Text("between")},
				{Type: "tool_result", ToolUseID: "c2", Content: "two"},
			},
			wantRoles: []string{"tool", "user", "tool"},
			wantText:  []string{"one", "between", "two"},
		},
		{
			name: "an assistant run before its result stays before it",
			role: "assistant",
			content: []ContentBlock{
				{Type: "thinking", Thinking: round52Text("why")},
				{Type: "text", Text: round52Text("say")},
				{Type: "tool_result", ToolUseID: "c1", Content: "one"},
			},
			wantRoles: []string{"assistant", "tool"},
			wantText:  []string{"say", "one"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs, err := convertMessage(MessageParam{Role: tc.role, Content: tc.content})
			if err != nil {
				t.Fatalf("convertMessage: %v", err)
			}
			if len(msgs) != len(tc.wantRoles) {
				t.Fatalf("a turn of %d blocks converted to %d messages, want %d (%v)\nthe blocks of one turn are written back in the order the client wrote them, and the gateway leg writes that same order",
					len(tc.content), len(msgs), len(tc.wantRoles), msgs)
			}
			for i := range msgs {
				if msgs[i].Role != tc.wantRoles[i] {
					t.Errorf("message %d has role %q, want %q", i, msgs[i].Role, tc.wantRoles[i])
				}
				if msgs[i].Content != tc.wantText[i] {
					t.Errorf("message %d carries %q, want %q", i, msgs[i].Content, tc.wantText[i])
				}
			}
		})
	}
}

// TestAToolIsChargedAsTheToolTheConverterForwards is A2: the same tool, written
// compactly and pretty-printed, is one tool on the wire and must be one number
// in the estimate — and that number is the converted object's bytes, in the
// unit every other charge in this function uses.
func TestAToolIsChargedAsTheToolTheConverterForwards(t *testing.T) {
	compact := json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`)
	pretty := json.RawMessage("{\n  \"type\": \"object\",\n  \"properties\": {\n    \"a\": {\"type\": \"string\"}\n  },\n  \"required\": [\"a\"]\n}")
	if len(compact) == len(pretty) {
		t.Fatal("the two spellings are the same length; this test cannot tell them apart")
	}

	charge := func(schema json.RawMessage) int {
		t.Helper()
		tool := Tool{Name: "read_file", Description: "Read a file", InputSchema: schema}
		converted, _, err := convertTool(tool)
		if err != nil {
			t.Fatalf("convertTool: %v", err)
		}
		want := clampedJSONBytes(converted) / 4
		got := estimateTokens(CountTokensRequest{Tools: []Tool{tool}})
		if got != want {
			t.Errorf("a tool whose converted form is %d bytes was charged %d tokens, want %d\nthe charge is the bytes of the object that is sent, the unit every other measure in this function uses",
				clampedJSONBytes(converted), got, want)
		}
		return got
	}

	if got, want := charge(compact), charge(pretty); got != want {
		t.Errorf("the same tool was charged %d vs %d tokens by its JSON formatting alone; the schema travels to the wire as one object either way", got, want)
	}
}

// TestADataURLSourceIsServedAsItsImage is A3: the forms the OpenAI door
// decodes are image bytes here too, whichever leg serves the block.
func TestADataURLSourceIsServedAsItsImage(t *testing.T) {
	payload := []byte("\x89PNG\r\n\x1a\nround52")
	for _, prefix := range []string{"data:;base64,", "data:image/jpeg;base64,", "data:image/jpg;base64,", "data:image/png;base64,", "data:image/webp;base64,"} {
		t.Run(prefix, func(t *testing.T) {
			url := prefix + base64.StdEncoding.EncodeToString(payload)
			got, err := convertMessage(MessageParam{Role: "user", Content: []ContentBlock{
				{Type: "image", Source: &ImageSource{Type: "url", URL: url}},
			}})
			if err != nil {
				t.Fatalf("a %s url was refused: %v", prefix, err)
			}
			if len(got) != 1 || len(got[0].Images) != 1 {
				t.Fatalf("converted to %v, want one message carrying one image", got)
			}
			if string(got[0].Images[0]) != string(payload) {
				t.Errorf("the image carried %q, want the decoded bytes %q — a data URL IS the image, not an address to fetch",
					got[0].Images[0], payload)
			}
		})
	}
}

// TestADataURLThatIsNotImageBytesIsRefused is the other side of A3: a form the
// door refuses must be refused here, not carried as a fetchable address.
func TestADataURLThatIsNotImageBytesIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"not base64", "data:;base64,!!!not base64!!!"},
		{"decodes to a url", "data:;base64," + base64.StdEncoding.EncodeToString([]byte("https://example.com/a.png"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveImageSource(&ImageSource{Type: "url", URL: tc.url})
			if err == nil {
				t.Fatalf("%q was served as an image; the door refuses these bytes in words and one body is answered one way on both legs", tc.url)
			}
		})
	}
}

// TestAnOrdinaryURLSourceIsStillItsText keeps A3's other side honest: a real
// address is still carried as its own text, because that is what the wire
// fetches.
func TestAnOrdinaryURLSourceIsStillItsText(t *testing.T) {
	got, err := resolveImageSource(&ImageSource{Type: "url", URL: "https://example.com/a.png"})
	if err != nil {
		t.Fatalf("a fetchable address was refused: %v", err)
	}
	if string(got) != "https://example.com/a.png" {
		t.Errorf("a url source carried %q, want its own text", got)
	}
}
