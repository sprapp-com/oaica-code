package openai

// round108_leg1_sampling_defaults_record_test.go — leg 1, round 108 (2026-09-29
// audit), F108-L1-1 and F108-L1-2. RECORDED, not changed.
//
// F108-L1-1: the OpenAI doors (chat, completions, responses) write temperature 1.0
// (and, on chat and completions, top_p 1.0) into the request options whether or
// not the client stated them, and request options override a Modelfile's
// PARAMETER lines, so an operator's tuned temperature holds on /v1/messages and
// /api/chat and is discarded on the OpenAI doors. Those defaults are OpenAI's own
// documented defaults, written by upstream ollama's compat layer on purpose so
// that an OpenAI client that omits a field gets the OpenAI behaviour, and every
// upstream merge carries them. Making them "unset unless stated" changes what
// every OpenAI-compat client of every model receives, and needs a decision on
// whose default wins (the model's or OpenAI's) that a test cannot make.
//
// F108-L1-2: /v1/completions reads top_p through a non-pointer float32 and tests it
// against 0.0, so an explicit top_p of 0 is treated as unset and widened to 1.0,
// where chat (a pointer) honours it. Same upstream code; the fix is a public type
// change on CompletionRequest.
//
// Each pin states the reading as it stands and goes red if it is changed.

import (
	"testing"
)

func TestMine108TheOpenAIDoorsWriteTheirOwnSamplingDefaults(t *testing.T) {
	chat, err := FromChatRequest(ChatCompletionRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if chat.Options["temperature"] != 1.0 || chat.Options["top_p"] != 1.0 {
		t.Errorf("chat writes temperature=%v top_p=%v, this record states OpenAI's 1.0/1.0 — if they are no longer written the Modelfile can win and this record is spent (2026-09-29 audit, round 108, F108-L1-1)", chat.Options["temperature"], chat.Options["top_p"])
	}
}

func TestMine108CompletionsTreatsAnExplicitTopPZeroAsUnset(t *testing.T) {
	req := CompletionRequest{Model: "m", Prompt: "hi", TopP: 0}
	out, err := FromCompleteRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if out.Options["top_p"] != float32(1.0) && out.Options["top_p"] != 1.0 {
		t.Errorf("completions writes top_p=%v for an explicit 0, this record states 1.0 — if it is now 0 the pointer fix has been made and this record is spent (2026-09-29 audit, round 108, F108-L1-2)", out.Options["top_p"])
	}
}
