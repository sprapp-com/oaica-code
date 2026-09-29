package launch

// round102_leg2_indexless_prose_test.go — leg 2, round 102 (2026-09-29 audit),
// F102-L2-1.
//
// Round 78 taught the INDEXED path that an entry which names nothing and
// repeats a call's finished arguments is not more of that call, whatever id it
// restates — its bytes are the model's prose. The index-less path's clause still
// required an EMPTY id, so the same entry was appended to the call there:
//
//	[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},
//	 {"id":"c1","type":"function","function":{"arguments":"{\"a\":1}"}}]   finish tool_calls
//	  streamed  call:c1|Bash|{"_raw":"{\"a\":1}{\"a\":1}"}   — a call no tool can run
//	  document  text:"{\"a\":1}" call:c1|Bash|{"a":1}
//
// and under `finish_reason:"length"` the joined bytes no longer parse, so the
// flush's truncation gate dropped the CALL as well and the client was told
// max_tokens with nothing at all, where both document arms keep the call and the
// text. The clause now asks only that the fragment names nothing, which is what
// the indexed path has asked since round 78.
//
// The other readings this round measured on this leg are records, in
// round102_leg2_id_and_order_records_test.go — this file holds the one fix.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r102L2Arm is one arm's whole-turn reading: the stop reason and the ordered
// content blocks, text and calls alike, so an ordering divergence is visible.
type r102L2Arm struct {
	stop   string
	blocks []string
}

func (a r102L2Arm) String() string {
	return fmt.Sprintf("stop=%s blocks=%v", a.stop, a.blocks)
}

// r102L2Run spells one body down both arms of this leg — the streamed deltas and
// the buffered whole completion — and answers each unjudged.
func r102L2Run(t *testing.T, frames []string, whole string) (streamed, buffered r102L2Arm) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	script := strings.Join(frames, "\n\n") + "\n\n"
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
		_, _ = io.WriteString(w, whole)
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})

	code, body := round54PostMessage(t, proxy, "glm-5.3", true)
	if code != http.StatusOK {
		t.Fatalf("stream arm status %d\n%s", code, body)
	}
	streamed = r102L2ParseStream(t, body)

	code, wholeBody := round54PostMessage(t, proxy, "glm-5.3", false)
	if code != http.StatusOK {
		t.Fatalf("whole arm status %d\n%s", code, wholeBody)
	}
	buffered = r102L2ParseDoc(t, wholeBody)
	return streamed, buffered
}

func r102L2ParseStream(t *testing.T, body string) r102L2Arm {
	t.Helper()
	var arm r102L2Arm
	type blk struct {
		kind string
		id   string
		name string
		text strings.Builder
		args strings.Builder
	}
	var cur *blk
	flush := func(b *blk) {
		if b == nil {
			return
		}
		switch b.kind {
		case "text":
			arm.blocks = append(arm.blocks, fmt.Sprintf("text:%q", b.text.String()))
		case "tool_use":
			arm.blocks = append(arm.blocks, fmt.Sprintf("call:%s|%s|%s", b.id, b.name, r102L2Tidy(b.args.String())))
		}
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		raw, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "content_block_start":
			cb, _ := ev["content_block"].(map[string]any)
			if cb == nil {
				continue
			}
			flush(cur)
			cur = &blk{kind: fmt.Sprint(cb["type"])}
			if s, ok := cb["id"].(string); ok {
				cur.id = s
			}
			if s, ok := cb["name"].(string); ok {
				cur.name = s
			}
			if s, ok := cb["text"].(string); ok {
				cur.text.WriteString(s)
			}
		case "content_block_delta":
			d, _ := ev["delta"].(map[string]any)
			if d == nil || cur == nil {
				continue
			}
			if s, ok := d["text"].(string); ok {
				cur.text.WriteString(s)
			}
			if s, ok := d["partial_json"].(string); ok {
				cur.args.WriteString(s)
			}
		case "content_block_stop":
			flush(cur)
			cur = nil
		case "message_delta":
			if d, _ := ev["delta"].(map[string]any); d != nil {
				if s, ok := d["stop_reason"].(string); ok {
					arm.stop = s
				}
			}
		}
	}
	return arm
}

func r102L2ParseDoc(t *testing.T, body string) r102L2Arm {
	t.Helper()
	var arm r102L2Arm
	var resp struct {
		StopReason string           `json:"stop_reason"`
		Content    []map[string]any `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("whole arm did not parse: %v\n%s", err, body)
	}
	arm.stop = resp.StopReason
	for _, b := range resp.Content {
		switch b["type"] {
		case "text":
			arm.blocks = append(arm.blocks, fmt.Sprintf("text:%q", fmt.Sprint(b["text"])))
		case "tool_use":
			raw, _ := json.Marshal(b["input"])
			arm.blocks = append(arm.blocks, fmt.Sprintf("call:%v|%v|%s", b["id"], b["name"], string(raw)))
		}
	}
	return arm
}

func r102L2Tidy(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func r102L2Fin(finish string) string {
	return `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"` + finish + `"}]}`
}

func r102L2Whole(calls, finish string) string {
	return `{"id":"c","object":"chat.completion","model":"glm-5.3",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[` + calls + `]},` +
		`"finish_reason":"` + finish + `"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`
}

func r102L2Report(t *testing.T, name, want string, frames []string, whole string) {
	t.Helper()
	streamed, buffered := r102L2Run(t, frames, whole)
	if got := streamed.String(); got != want {
		t.Errorf("the streamed arm reads %s, want %s — this pin states the reading the fragment arm owes the document arms on one body (2026-09-29 audit, round 102, %s)", got, want, name)
	}
	if buffered.String() != want {
		t.Errorf("the buffered arm reads %s, want %s — the document arm is the reading this pin holds the fragment arm to, so it must not have moved either (2026-09-29 audit, round 102, %s)", buffered.String(), want, name)
	}
}

// TestMine102AnIndexlessNamelessEntryRestatingTheCallsBytesIsProse is F102-L2-1.
func TestMine102AnIndexlessNamelessEntryRestatingTheCallsBytesIsProse(t *testing.T) {
	call := `{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	twin := `{"id":"c1","type":"function","function":{"arguments":"{\"a\":1}"}}`
	want := `stop=tool_use blocks=[text:"{\"a\":1}" call:c1|Bash|{"a":1}]`
	t.Run("a completed turn", func(t *testing.T) {
		r102L2Report(t, "F102-L2-1", want,
			[]string{r59L2Frame(call), r59L2Frame(twin), r59L2Fin},
			r102L2Whole(call+","+twin, "tool_calls"))
	})
	t.Run("a truncated turn keeps the call", func(t *testing.T) {
		r102L2Report(t, "F102-L2-1", want,
			[]string{r59L2Frame(call), r59L2Frame(twin), r102L2Fin("length")},
			r102L2Whole(call+","+twin, "length"))
	})
}

