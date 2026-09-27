package launch

// round50_proxy_control_and_schema_test.go — round 50's findings on the
// client-side proxy leg and on the serve path it shares a package with. Every
// one was reproduced on the unmodified tree before it was fixed, and each is
// stated as the upstream request one of the other two legs sends for the same
// client body.
//
// C50-1: the client's thinking control never reached the upstream at all. The
// Anthropic request carries it as `thinking:{type:…}` (a switch) or
// `output_config.effort` (a level), FromMessagesRequest decodes both into
// api.ChatRequest.Think, and the proxy — the one leg that reads that value and
// then builds its own body — dropped it. A client that asked for thinking OFF
// was answered by an upstream whose default is ON, and paid for reasoning it
// had ruled out.
//
// A50-5: `tool_choice:{type:"none"}` reached the upstream as `"tool_choice":
// "none"` beside the dropped tool list. The metered gateway leg states NO
// choice field for the same body (the instruction is carried by the tool
// surface itself), so one body was two upstream requests.
//
// A50-6: a whitespace-only text block beside an image was trimmed away, so the
// message reached the upstream as an image with no text at all. The other legs
// keep the text the client stated.
//
// A50-1/2/3: the tool schema. The proxy forwards api.Tool values, which now
// carry the schema as the client wrote it (see api/types.go); this leg is where
// the loss was visible on the wire.
//
// A50-7 (serve path): a non-text block in a SYSTEM value was rendered as its
// own JSON, so an image in the system field put its base64 payload into the
// prompt as prose — charged to the context window and tokenized as noise —
// while neither sibling converter puts anything but the text blocks of a system
// array on the wire.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// round50UpstreamBody answers one request through the proxy and returns the
// upstream body it produced and the proxy's status.
func round50UpstreamBody(t *testing.T, name, body string) (map[string]any, int) {
	t.Helper()
	var captured string
	up := round49CapturingUpstream(t, &captured)
	proxy := startCalibProxy(t, up.URL, "r50-"+name)
	status, _ := round49Post(t, proxy, body)
	if status != http.StatusOK {
		return nil, status
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(captured), &got); err != nil {
		t.Fatalf("decode upstream body: %v\n%s", err, captured)
	}
	return got, status
}

// TestTheThinkingControlReachesTheUpstream is C50-1. The switch the client
// stated is the switch the upstream is handed — in the shape the native chat
// wire carries it, which is the value the local leg sends for the same body.
func TestTheThinkingControlReachesTheUpstream(t *testing.T) {
	const head = `"model":"kat-awq","max_tokens":16,"messages":[{"role":"user","content":"hi"}]`
	for _, c := range []struct {
		name string
		body string
		want any // nil = the field must be absent
	}{
		{"unstated", `{` + head + `}`, nil},
		{"enabled", `{` + head + `,"thinking":{"type":"enabled"}}`, true},
		{"disabled", `{` + head + `,"thinking":{"type":"disabled"}}`, false},
		{"effort", `{` + head + `,"output_config":{"effort":"high"}}`, "high"},
		{"effort-medium", `{` + head + `,"output_config":{"effort":"medium"}}`, "medium"},
		{"switch_wins_over_effort", `{` + head + `,"thinking":{"type":"enabled"},"output_config":{"effort":"high"}}`, true},
		{"disabled_wins_over_effort", `{` + head + `,"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, status := round50UpstreamBody(t, "think-"+c.name, c.body)
			if status != http.StatusOK {
				t.Fatalf("status %d", status)
			}
			think, present := got["think"]
			if c.want == nil {
				if present {
					t.Fatalf("the upstream was handed think=%v for a body that stated no thinking control", think)
				}
				return
			}
			if !present {
				t.Fatalf("the upstream was handed no think field for %s — the client's switch was dropped on the floor and the upstream applied its own default", c.name)
			}
			if !reflect.DeepEqual(think, c.want) {
				t.Errorf("the upstream was handed think=%#v, want %#v — the local leg sends exactly this value for the same body", think, c.want)
			}
		})
	}
}

// TestToolChoiceNoneStatesNoChoiceField is A50-5. The instruction is carried by
// the tool surface, and no sibling leg puts a choice field on the wire beside
// it.
func TestToolChoiceNoneStatesNoChoiceField(t *testing.T) {
	const head = `"model":"kat-awq","max_tokens":16,"messages":[{"role":"user","content":"go"}]`
	tool := `{"name":"Read","input_schema":{"type":"object"}}`

	t.Run("none", func(t *testing.T) {
		got, status := round50UpstreamBody(t, "choice-none", `{`+head+`,"tools":[`+tool+`],"tool_choice":{"type":"none"}}`)
		if status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		if tc, present := got["tool_choice"]; present {
			t.Errorf("the upstream was handed tool_choice=%v; the gateway leg states no choice field for this body", tc)
		}
		if tools, present := got["tools"]; present && tools != nil {
			t.Errorf("the upstream was handed tools=%v; the instruction is carried by dropping the tool surface", tools)
		}
	})

	t.Run("auto", func(t *testing.T) {
		got, status := round50UpstreamBody(t, "choice-auto", `{`+head+`,"tools":[`+tool+`],"tool_choice":{"type":"auto"}}`)
		if status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		if got["tool_choice"] != "auto" {
			t.Errorf("the upstream was handed tool_choice=%#v, want auto", got["tool_choice"])
		}
		if tools, _ := got["tools"].([]any); len(tools) != 1 {
			t.Errorf("the upstream was handed %d tool(s), want 1 — auto keeps the tool surface", len(tools))
		}
	})
}

// TestATextPartIsKeptBesideAnImage is A50-6. Whitespace is text the client
// stated; an empty string is not a part at all, which is the same turn the
// local leg sends (its content is the empty string either way).
func TestATextPartIsKeptBesideAnImage(t *testing.T) {
	const img = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}`
	parts := func(t *testing.T, content string) []map[string]any {
		t.Helper()
		got, status := round50UpstreamBody(t, "parts", `{"model":"kat-awq","max_tokens":16,"messages":[{"role":"user","content":[`+content+`]}]}`)
		if status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		msgs, _ := got["messages"].([]any)
		if len(msgs) == 0 {
			t.Fatalf("no upstream message: %v", got)
		}
		last, _ := msgs[len(msgs)-1].(map[string]any)
		arr, ok := last["content"].([]any)
		if !ok {
			t.Fatalf("content is %#v, want the parts array an image forces", last["content"])
		}
		var out []map[string]any
		for _, p := range arr {
			pm, _ := p.(map[string]any)
			out = append(out, pm)
		}
		return out
	}

	got := parts(t, `{"type":"text","text":"   "},`+img)
	if len(got) != 2 {
		t.Fatalf("the upstream got %d part(s), want the text part and the image: %v", len(got), got)
	}
	if got[0]["type"] != "text" || got[0]["text"] != "   " {
		t.Errorf("part 0 is %#v, want the whitespace text the client stated — trimming it sent the model an image with no text", got[0])
	}
	if got[1]["type"] != "image_url" {
		t.Errorf("part 1 is %#v, want the image part", got[1])
	}

	if got := parts(t, `{"type":"text","text":""},`+img); len(got) != 1 {
		t.Errorf("an empty text block produced %d part(s), want the image alone — an empty text is not a part on this wire", len(got))
	}
}

// TestASchemaIsForwardedAsTheClientWroteIt is A50-1/2/3 on the wire this leg
// builds. The schema the model is asked about is the schema the client sent.
func TestASchemaIsForwardedAsTheClientWroteIt(t *testing.T) {
	const schema = `{"$defs":{"loc":{"type":"object"}},` +
		`"type":"object","title":"Read","additionalProperties":false,` +
		`"properties":{"path":{"type":"string","format":"uri","pattern":"^/","minLength":1,"default":"/tmp"},` +
		`"ref":{"$ref":"#/$defs/loc"}},"required":["path"]}`

	got, status := round50UpstreamBody(t, "schema", `{"model":"kat-awq","max_tokens":16,"messages":[{"role":"user","content":"go"}],"tools":[{"name":"Read","input_schema":`+schema+`}]}`)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("the upstream was handed %d tool(s), want 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	var want map[string]any
	if err := json.Unmarshal([]byte(schema), &want); err != nil {
		t.Fatalf("probe schema: %v", err)
	}
	if !reflect.DeepEqual(fn["parameters"], want) {
		encoded, _ := json.Marshal(fn["parameters"])
		t.Errorf("the upstream was handed\n%s\nwant\n%s\n— the schema was rebuilt from a struct that names five keys", encoded, schema)
	}
}

// TestASchemaReachesTheUpstreamAsTheClientWroteIt is the same finding at the
// byte level: the schema is forwarded as the bytes it arrived as, so the key
// order the client chose survives the hop. A schema is part of the prompt, and
// a prompt that is re-serialised key-by-key is a prompt the provider's cache
// never sees again.
func TestASchemaReachesTheUpstreamAsTheClientWroteIt(t *testing.T) {
	// Deliberately not in the struct's field order: `$defs` first and
	// `properties` before `required` is how a generator that emits `$defs`
	// writes it.
	const schema = `{"$defs":{"loc":{"type":"object"}},"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`

	var captured string
	up := round49CapturingUpstream(t, &captured)
	proxy := startCalibProxy(t, up.URL, "r50-schema-bytes")
	status, _ := round49Post(t, proxy, `{"model":"kat-awq","max_tokens":16,"messages":[{"role":"user","content":"go"}],"tools":[{"name":"Read","input_schema":`+schema+`}]}`)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if !strings.Contains(captured, `"parameters":`+schema) {
		var got map[string]any
		_ = json.Unmarshal([]byte(captured), &got)
		if tools, _ := got["tools"].([]any); len(tools) == 1 {
			tool, _ := tools[0].(map[string]any)
			fn, _ := tool["function"].(map[string]any)
			if encoded, err := json.Marshal(fn["parameters"]); err == nil {
				t.Errorf("the upstream was handed parameters\n%s\nwant the schema as the client wrote it\n%s\n— the schema was rebuilt key by key, so the prompt is not the prompt the client sent", encoded, schema)
				return
			}
		}
		t.Errorf("the upstream body does not carry the client's schema:\n%s", captured)
	}
}

// TestAStopListThatIsNotStringsIsRefused is C50-4 on the proxy leg. The
// gateway leg refuses this body; before round 50 this leg served it with an
// empty stop sequence on the wire, which matches at every position.
func TestAStopListThatIsNotStringsIsRefused(t *testing.T) {
	const head = `"model":"kat-awq","max_tokens":16,"messages":[{"role":"user","content":"go"}]`

	for _, bad := range []string{`[null]`, `["a",null]`, `[1]`, `["a",1]`, `"STOP"`, `5`} {
		var captured string
		up := round49CapturingUpstream(t, &captured)
		proxy := startCalibProxy(t, up.URL, "r50-stop-bad")
		status, _ := round49Post(t, proxy, `{`+head+`,"stop_sequences":`+bad+`}`)
		if status != http.StatusBadRequest {
			t.Errorf("stop_sequences:%s was answered %d; the gateway leg answers 400 for the same body", bad, status)
		}
		if captured != "" {
			t.Errorf("stop_sequences:%s reached the upstream: %s", bad, captured)
		}
	}

	got, status := round50UpstreamBody(t, "stop-ok", `{`+head+`,"stop_sequences":["STOP","END"]}`)
	if status != http.StatusOK {
		t.Fatalf("a stated stop list was answered %d", status)
	}
	stops, _ := got["stop"].([]any)
	if len(stops) != 2 || stops[0] != "STOP" || stops[1] != "END" {
		t.Errorf("the upstream was handed stop=%v, want the client's own list", got["stop"])
	}
}

// TestAnImageTheUpstreamCannotTakeIsReSentAsOneItCan is A50-4's real half. The
// upstream this proxy talks to is oaica's own OpenAI door, and that door
// carries exactly four image types in a data URL — jpeg, jpg, png and webp
// (openai.decodeImageURL, pinned by openai.TestDecodeImageURL). The sniffer
// here also recognises GIF, so a client that sent a GIF got a data URL its own
// upstream answers with 400 "invalid image input": the local leg serves the
// same body (raw bytes, no label, and the runner labels it from the content),
// so one client body was a served turn on one leg and a hard failure on the
// other.
//
// The fix is the one llm.llamaServerMediaBytes already uses for WebP: what the
// wire cannot carry is transcoded into something it can, rather than forwarded
// under a label that is not true.
func TestAnImageTheUpstreamCannotTakeIsReSentAsOneItCan(t *testing.T) {
	// 1x1 GIF89a and 1x1 PNG, the same fixtures the door's own test uses.
	const gif1x1 = "R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"
	const png1x1 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="
	const jpeg1x1 = "/9j/4AAQSkZJRg=="
	const webp1x1 = "UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA"

	sent := func(t *testing.T, mediaType, data string) (string, []byte) {
		t.Helper()
		body := `{"model":"kat-awq","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"` + mediaType + `","data":"` + data + `"}}]}]}`
		got, status := round50UpstreamBody(t, "img-"+mediaType, body)
		if status != http.StatusOK {
			t.Fatalf("the proxy answered %d for a %s image", status, mediaType)
		}
		msgs, _ := got["messages"].([]any)
		if len(msgs) == 0 {
			t.Fatalf("no upstream message: %v", got)
		}
		last, _ := msgs[len(msgs)-1].(map[string]any)
		parts, _ := last["content"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if pm["type"] != "image_url" {
				continue
			}
			iu, _ := pm["image_url"].(map[string]any)
			url, _ := iu["url"].(string)
			prefix, b64, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ";base64,")
			if !ok {
				t.Fatalf("%s was handed as %q, want a base64 data URL", mediaType, url)
			}
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				t.Fatalf("%s payload is not base64: %v", mediaType, err)
			}
			return prefix, raw
		}
		t.Fatalf("no image was handed on for a %s image: %v", mediaType, got)
		return "", nil
	}

	// The four the door carries, carried as the client stated them.
	for _, c := range []struct{ mediaType, data string }{
		{"image/png", png1x1},
		{"image/jpeg", jpeg1x1},
		{"image/webp", webp1x1},
	} {
		prefix, _ := sent(t, c.mediaType, c.data)
		if prefix != c.mediaType {
			t.Errorf("a %s image reached the upstream as %s", c.mediaType, prefix)
		}
	}

	// GIF is the one the sniffer knows and the door does not take.
	prefix, raw := sent(t, "image/gif", gif1x1)
	if prefix != "image/png" {
		t.Fatalf("a GIF reached the upstream as %s; the door refuses anything but jpeg/jpg/png/webp with 400 invalid image input, so this body is a hard failure on this leg and a served turn on the local leg", prefix)
	}
	if len(raw) < 8 || raw[0] != 0x89 || string(raw[1:4]) != "PNG" {
		t.Errorf("the re-encoded payload is not a PNG: % x", raw[:min(8, len(raw))])
	}
}

// TestANonTextSystemBlockIsNotProse is A50-7 on the serve path. A system value
// carries instruction TEXT; a block that is not text contributes nothing, and
// in particular an image's payload is never pasted into the prompt.
func TestANonTextSystemBlockIsNotProse(t *testing.T) {
	payload := strings.Repeat("QUJD", 64)
	body := `{"model":"m","max_tokens":16,"system":[{"type":"text","text":"alpha"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + payload + `"}}],"messages":[{"role":"user","content":"go"}]}`
	out, err := normalizeSystemMessages("/v1/messages", []byte(body))
	if err != nil {
		t.Fatalf("normalizeSystemMessages: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	sys, _ := parsed["system"].(string)
	if strings.Contains(sys, payload) {
		t.Errorf("the system prompt carries the image's payload (%d bytes) — it is charged to the context window and tokenizes as noise, and no other leg puts it there", len(sys))
	}
	if sys != "alpha" {
		t.Errorf("the system prompt is %q, want %q — the local and gateway converters take a system array's text blocks only", sys, "alpha")
	}

	// A system value of any other kind states nothing either: neither sibling
	// leg reads a number as prose.
	out, err = normalizeSystemMessages("/v1/messages", []byte(`{"model":"m","max_tokens":16,"system":5,"messages":[{"role":"user","content":"go"}]}`))
	if err != nil {
		t.Fatalf("normalizeSystemMessages: %v", err)
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if sys, _ := parsed["system"].(string); strings.Contains(sys, "5") {
		t.Errorf("the system prompt is %q; a system value that is neither a string nor an array of text blocks is not prose on any leg", sys)
	}
}
