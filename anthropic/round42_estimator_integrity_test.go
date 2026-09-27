package anthropic

// round42_estimator_integrity_test.go — round 42's estimator findings on this
// leg (A42-2, A42-3, A42-4, A42-5, A42-6, C42-1):
//
// The estimate seeds the client-visible input_tokens whenever the upstream
// states no usage, so every arm of it has to charge what the CONVERTER writes
// into the prompt — not what the block carries, and not what the transport
// spends carrying it. Round 42 closed four arms that charged something else,
// all four measured against the wire they describe:
//
//   - tools the converter DROPS (tool_choice "none", and a user tool that
//     collides with the built-in web_search) were charged their full schema;
//   - a system ARRAY was charged through the general content reader, billing
//     the blocks the converter discards;
//   - a tool_result was charged its own serialized JSON rather than the
//     content the converter converts out of it;
//   - a decoded server_tool_use was charged raw JSON beside a tool_use twin
//     charged through clampedJSONBytes, so the same call cost two prices.
//
// C42-1 is the nested carrier: an image reached through a tool_result is
// CARRIED (the allowance the product's other two measures charge it), and one
// reached through a search_result's passages is DESCRIBED — and this walk has
// to answer the same way the converter does for each.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// round42Estimate estimates the prompt for one /v1/messages body.
func round42Estimate(t *testing.T, body string) int {
	t.Helper()
	var req MessagesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return EstimateInputTokens(req)
}

// round42BigSchema is a tool definition whose input_schema is `n` bytes of
// prose: the shape whose charge the two tool fixes below are about.
func round42BigSchema(n int) string {
	return fmt.Sprintf(`{"type":"object","description":%q}`, strings.Repeat("x", n))
}

// TestAToolSurfaceTheConverterDropsIsNotCharged is A42-3. tool_choice "none"
// makes the converter send NO tools, so a body carrying a 400 KB schema it had
// just asked not to be given was charged ~100 000 tokens against a converted
// prompt of 97 bytes — and since the estimate seeds the client-visible
// input_tokens, the session's meter and auto-compaction read a tool surface the
// model never saw.
func TestAToolSurfaceTheConverterDropsIsNotCharged(t *testing.T) {
	withTools := func(choice string) string {
		return fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"go"}],"tools":[{"name":"big","input_schema":%s}]%s}`,
			round42BigSchema(400000), choice)
	}
	dropped := round42Estimate(t, withTools(`,"tool_choice":{"type":"none"}`))
	none := round42Estimate(t, `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"go"}]}`)
	offered := round42Estimate(t, withTools(""))

	if d := dropped - none; d > 2 || d < -2 {
		t.Errorf("a body refusing every tool estimated %d against %d for the same body with no tools at all: tool_choice \"none\" drops the whole surface, so the 400 KB schema may not be charged (%d tokens would be the schema alone)", dropped, none, 400000/4)
	}
	if offered <= none {
		t.Errorf("a body OFFERING the same 400 KB schema estimated %d against %d with no tools: the schema is sent, so it must be charged", offered, none)
	}
}

// TestAUserToolTheConverterDropsIsNotCharged is A42-3's second predicate: the
// converter drops a user-defined tool NAMED web_search when a built-in search
// tool is present, because the built-in is the one the model is given. Charging
// both billed a tool the model never saw.
func TestAUserToolTheConverterDropsIsNotCharged(t *testing.T) {
	builtin := `{"type":"web_search_20250305","name":"web_search"}`
	both := fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"go"}],"tools":[%s,{"name":"web_search","input_schema":%s}]}`,
		builtin, round42BigSchema(400000))
	alone := fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"go"}],"tools":[%s]}`, builtin)

	if a, b := round42Estimate(t, both), round42Estimate(t, alone); a-b > 2 || b-a > 2 {
		t.Errorf("a body carrying the built-in search tool beside a user tool of the same name estimated %d against %d for the built-in alone: the converter drops the user tool, so its 400 KB schema may not be charged", a, b)
	}
}

// TestASystemArrayChargesOnlyTheTextBlocksTheConverterReads is A42-4. The
// converter reads a system array for its "text" blocks, joined with a blank
// line, and drops every other block and every non-string, non-array value
// entirely — so charging the input through the general content reader billed
// the blocks the converter discards.
func TestASystemArrayChargesOnlyTheTextBlocksTheConverterReads(t *testing.T) {
	withDoc := func(data string) string {
		src, _ := json.Marshal(map[string]any{"type": "base64", "media_type": "application/pdf", "data": data})
		return fmt.Sprintf(`{"model":"m","max_tokens":64,"system":[{"type":"text","text":"be brief"},{"type":"document","source":%s}],"messages":[{"role":"user","content":"go"}]}`, src)
	}
	small := round42Estimate(t, withDoc("AAAA"))
	big := round42Estimate(t, withDoc(strings.Repeat("A", 400000)))
	if d := big - small; d > 3 || d < 0 {
		t.Errorf("a 400 KB document in the system array measured %d against %d for the same block with four bytes of base64: the converter reads the array's TEXT blocks alone, so the payload's size may not appear in the estimate (%d charged)", big, small, d)
	}

	// Blocks joined with a blank line: the array spelling and the equivalent
	// string are one converted prompt and must be one estimate.
	array := round42Estimate(t, `{"model":"m","max_tokens":64,"system":[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}],"messages":[{"role":"user","content":"go"}]}`)
	str := round42Estimate(t, `{"model":"m","max_tokens":64,"system":"alpha\n\nbeta","messages":[{"role":"user","content":"go"}]}`)
	if array != str {
		t.Errorf("a two-block system array estimated %d and the string it converts to estimated %d: the converter joins the blocks with a blank line, so the estimate must charge the same bytes (%d vs %d)", array, str, array, str)
	}

	// A system value the converter drops is charged nothing.
	object := round42Estimate(t, fmt.Sprintf(`{"model":"m","max_tokens":64,"system":{"note":%q},"messages":[{"role":"user","content":"go"}]}`, strings.Repeat("x", 400000)))
	if object > 100 {
		t.Errorf("a system field holding an OBJECT estimated %d tokens: the converter reads a string or an array of text blocks and writes nothing for anything else, so a 400 KB value it drops may not be charged (%d tokens is the payload alone)", object, 400000/4)
	}
}

// TestAToolResultIsChargedWhatTheConverterWrites is A42-2. A tool_result is
// CONVERTED, not pasted: a 400 KB PDF inside one reaches the prompt as a
// seventy-byte notice, so serializing the block billed the base64 of a file the
// model is told about in one line.
func TestAToolResultIsChargedWhatTheConverterWrites(t *testing.T) {
	body := func(data string) string {
		src, _ := json.Marshal(map[string]any{"type": "base64", "media_type": "application/pdf", "data": data})
		return fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"document","source":%s}]}]}]}`, src)
	}
	small := round42Estimate(t, body("AAAA"))
	big := round42Estimate(t, body(strings.Repeat("A", 400000)))
	if d := big - small; d > 3 || d < 0 {
		t.Errorf("a 400 KB PDF in a tool_result measured %d against %d for the same block with four bytes of base64: the converter writes one notice for both, so the only difference the estimate may charge is the digits of the size it names (%d charged)", big, small, d)
	}
	if big > 100 {
		t.Errorf("a 400 KB PDF in a tool_result was charged %d estimated tokens (%d tokens would be the payload alone)", big, 400000/4)
	}
}

// TestABareToolResultObjectIsChargedItsJSONInFull is A42-6. A bare object as a
// tool_result's content is DESCRIBED — the describer's fallback is the block's
// own JSON, and that JSON is TEXT in the prompt, so its escapes are prompt
// bytes. Subtracting them (as clampedJSONBytes does for a JSON *string field*,
// where the receiver decodes the escapes away) charged half the wire.
//
// Two objects differing only in characters that marshal with an escape
// therefore differ by exactly those bytes, divided by the characters-per-token
// the estimate uses.
func TestABareToolResultObjectIsChargedItsJSONInFull(t *testing.T) {
	body := func(content string) string {
		return `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":{"note":"` + content + `"}}]}]}`
	}
	plain := round42Estimate(t, body(strings.Repeat("a", 1000)))
	// 1 000 quotes reach the wire as 2 000 bytes: the JSON text carries a
	// backslash before each one.
	quoted := round42Estimate(t, body(strings.Repeat("\\\"", 1000)))

	// 1 000 quotes marshal to 2 000 bytes: 1 000 more than 1 000 plain
	// characters, i.e. 250 tokens the prompt really carries.
	if d := quoted - plain; d < 240 || d > 260 {
		t.Errorf("a quoted argument string estimated %d against %d for the same number of plain characters (difference %d, want ~250): the escapes the converter writes into the prompt are prompt bytes and must be charged in full", quoted, plain, d)
	}
}

// TestAServerToolUseIsChargedLikeItsToolUseTwin is A42-5. A decoded
// server_tool_use was charged len(json.Marshal(block)) raw while the tool_use
// arm beside it went through clampedJSONBytes, which subtracts the transport
// escapes the receiver decodes away — so one call cost two prices, and a call
// full of quotes cost twice its twin.
func TestAServerToolUseIsChargedLikeItsToolUseTwin(t *testing.T) {
	args := strings.Repeat(`\"`, 4000) // 8 000 bytes of escaped quotes
	calls := []struct {
		name  string
		block string
	}{
		{"tool_use", `{"type":"tool_use","id":"c1","name":"Bash","input":{"cmd":"` + args + `"}}`},
		{"server_tool_use", `{"type":"server_tool_use","id":"c1","name":"Bash","input":{"cmd":"` + args + `"}}`},
	}
	est := func(block string) int {
		return round42Estimate(t, `{"model":"m","max_tokens":64,"messages":[{"role":"assistant","content":[`+block+`]}]}`)
	}
	a, b := est(calls[0].block), est(calls[1].block)
	// The two blocks differ in their type name alone (7 characters), which is
	// under two tokens; anything more means the escapes are being charged twice.
	if d := b - a; d < -4 || d > 4 {
		t.Errorf("the same call estimated %d as a server_tool_use and %d as a tool_use (difference %d): both arms charge the call's JSON with its transport escapes discounted, so the two spellings of one call cost the same (%d tokens would be the raw serialization)", b, a, d, 8000/4)
	}
}

// TestANestedSearchResultImageIsDescribedNotChargedTheAllowance is C42-1's
// describe half. An image reached through a search_result's passages has no
// carrier on that path, so the converter describes it in one line — and the
// walk charged it the image allowance instead, ~4 100 bytes against a
// five-byte notice, on a prompt the model was never sent.
func TestANestedSearchResultImageIsDescribedNotChargedTheAllowance(t *testing.T) {
	body := func(data string) string {
		return fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"search_result","title":"t","content":[{"type":"text","text":"ok"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}]}]}`, data)
	}
	small := round42Estimate(t, body("QUJDRA"))
	big := round42Estimate(t, body(strings.Repeat("QUJDRA", 100000)))
	if d := big - small; d > 3 || d < 0 {
		t.Errorf("a 600 KB screenshot among a search_result's passages measured %d against %d for the same block with six bytes of base64: the converter DESCRIBES it there, so the only difference the estimate may charge is the digits of the size it names (%d charged)", big, small, d)
	}
	if big > 100 {
		t.Errorf("a screenshot among a search_result's passages was charged %d estimated tokens: it reaches the prompt as one line (%d tokens would be the payload alone)", big, 600000/4)
	}

	// The same passage list one level up: a search_result as a message's own
	// content element, read by the message-level walk rather than the
	// tool_result one. Both walks charge what the converter writes for the
	// passage list, so the two shapes of the same block cost the same.
	direct := func(data string) string {
		return fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"search_result","title":"t","content":[{"type":"text","text":"ok"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}]}`, data)
	}
	smallDirect := round42Estimate(t, direct("QUJDRA"))
	bigDirect := round42Estimate(t, direct(strings.Repeat("QUJDRA", 100000)))
	if d := bigDirect - smallDirect; d > 3 || d < 0 {
		t.Errorf("a 600 KB screenshot among a MESSAGE-LEVEL search_result's passages measured %d against %d for the same block with six bytes of base64 (%d charged): the describe rule is the passage list's own, at both levels", bigDirect, smallDirect, d)
	}
	if bigDirect > 100 {
		t.Errorf("a screenshot among a MESSAGE-LEVEL search_result's passages was charged %d estimated tokens: it is described there too, so the allowance for a carried image may not appear in the estimate (%d tokens would be the payload alone)", bigDirect, 600000/4)
	}
	if d := bigDirect - big; d > 40 || d < -40 {
		t.Errorf("the same passage list measured %d tokens as a message's content and %d nested in a tool_result: both are read by searchResultText, so the two shapes of one block cost the same", bigDirect, big)
	}
}

// TestANestedToolResultImageIsCarriedLikeATopLevelOne is C42-1's carry half,
// and the divergence the describe half exists to prevent: an image that IS a
// tool_result's own content has a carrier — the converter appends it to the
// message's images — so it is charged the allowance the product's other two
// measures charge a carried image, and not nothing at all.
func TestANestedToolResultImageIsCarriedLikeATopLevelOne(t *testing.T) {
	const data = "QUJDRA" // six bytes; the payload's size is the point below
	nested := round42Estimate(t, fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}]}`, data))
	top := round42Estimate(t, fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}`, data))

	if nested < 900 || nested > 1200 {
		t.Errorf("an image that IS a tool_result's content estimated %d tokens: the converter carries it, so it is charged the 4096-byte allowance (~1024 tokens) the other two measures charge a carried image", nested)
	}
	if d := top - nested; d > 100 || d < -100 {
		t.Errorf("the same image estimated %d nested in a tool_result and %d at the top level (difference %d): both are carried, so both pay the same allowance", nested, top, d)
	}
	// And the payload itself is never what is charged, at any size.
	huge := round42Estimate(t, fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}]}`, strings.Repeat("QUJDRA", 100000)))
	if huge > 1200 {
		t.Errorf("a 600 KB screenshot nested in a tool_result was charged %d estimated tokens (%d would be the payload alone)", huge, 600000/4)
	}
}
