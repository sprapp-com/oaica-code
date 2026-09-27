package main

// round56_run_and_image_label_integrity_test.go — round 56's cross-leg findings
// on the shape of a turn that holds text AND images (F56-4, F56-5), the other
// side of the pin in anthropic/round56_run_and_image_label_integrity_test.go.
//
// The local leg and the client proxy write a turn as an ollama api.Message — a
// content string plus an images list, with no media type — while this leg
// writes the OpenAI parts array. So for one client body:
//
//   - [text A, image X, text B] keeps its order here (three parts) and is folded
//     there ("A\n\nB" with X after it);
//   - an image's data URL is labelled by its own magic bytes, then by the type
//     the client stated, then jpeg — while the door the other legs feed calls
//     anything it cannot sniff jpeg, since that wire carries no label to
//     translate.
//
// Both are REJECTED as findings rather than fixed (2026-09-28 audit, round 56,
// rejections), and pinned here so the trade is visible: folding this leg's parts
// to match would import the local side's invented "\n\n" separator and throw
// away an order the client wrote, on the one wire that can carry it — round 52
// taught the local leg the opposite lesson for the same reason; and forcing this
// leg's label down to the door's blanket jpeg would hand a backend a wrong type
// for a picture the client labelled correctly.

import (
	"encoding/json"
	"testing"
)

const r56RunBody = `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[` +
	`{"type":"text","text":"A"},` +
	`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},` +
	`{"type":"text","text":"B"}]}]}`

// r56Messages converts a body and returns the messages it puts upstream.
func r56Messages(t *testing.T, body string) []any {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, refusal := anthropicToOpenAI(req, true)
	if refusal != "" {
		t.Fatalf("refused: %s", refusal)
	}
	raw, err := json.Marshal(out["messages"])
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	var msgs []any
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatalf("unmarshal messages: %v (%s)", err, raw)
	}
	return msgs
}

func TestARunOfTextAndImagesKeepsItsArrivalOrder(t *testing.T) {
	msgs := r56Messages(t, r56RunBody)
	if len(msgs) != 1 {
		t.Fatalf("the turn became %d messages: %v", len(msgs), msgs)
	}
	m, _ := msgs[0].(map[string]any)
	parts, ok := m["content"].([]any)
	if !ok {
		t.Fatalf("the turn's content is %T (%v), want the parts array a turn with an image is written as", m["content"], m["content"])
	}
	var types []string
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		ty, _ := pm["type"].(string)
		types = append(types, ty)
	}
	want := []string{"text", "image_url", "text"}
	if len(types) != len(want) {
		t.Fatalf("the turn reached the model as %v, want %v", types, want)
	}
	for i, ty := range want {
		if types[i] != ty {
			t.Errorf("part %d is %q, want %q — the client wrote text, image, text and this wire can say so: the local leg folds the run because api.Message holds one content string and one images list, and folding here would be the lossy direction (round 56, F56-4, rejected)", i, types[i], ty)
			break
		}
	}
	// The gateway keeps the parts and no separator of its own between them.
	first, _ := parts[0].(map[string]any)
	if first["text"] != "A" {
		t.Errorf("the first part is %v, want \"A\": parts are not joined with a separator the client never wrote", first["text"])
	}
}
