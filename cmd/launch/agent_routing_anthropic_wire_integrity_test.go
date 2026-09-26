package launch

// agent_routing_anthropic_wire_integrity_test.go — an OpenAI-wire integration
// accepted a user remote that speaks the Anthropic wire (2026-09-26 audit,
// tenth round).
//
// remotes.json's `wire` field decides the protocol a box speaks, and the
// translation proxy's own comment records what happens when the two are
// mismatched: "the translation path ... cannot reach an Anthropic endpoint at
// all: z.ai's /api/anthropic answers 404 {"detail":"Not Found"} to
// /chat/completions". The capability gate refused only on TOOL FORMAT
// (ToolReliable), so every OpenAI-wire integration — omp, hermes, droid, pi,
// cline, qwen, codex, opencode ... — configured itself against the Anthropic
// host, posted /chat/completions, and got a 404 the user had to decode. The
// same remote was already handled correctly by the picker's model-list fetch
// (user_remotes.go's fetchRemoteModels sets x-api-key for this wire).

import (
	"strings"
	"testing"
)

// anthropicWireRemotes is a remote that speaks /v1/messages, fully
// tool-reliable — so the OLD gate had nothing to refuse on.
const anthropicWireRemotes = `{"remotes":[{"name":"zai-anthropic","base_url":"https://api.z.ai/api/anthropic","wire":"anthropic","tool_format":"tool_calls","tool_reliable":true,"api_key":"sk-zai-SECRET-1234567890"}]}`

func TestAnOpenAIWireIntegrationRefusesAnAnthropicWireRemote(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	withTempRemotesFile(t)
	writeRemotes(t, anthropicWireRemotes)

	err := gateOpenAITools("zai-anthropic/glm-5", false)
	if err == nil {
		t.Fatal("an OpenAI-wire integration accepted an anthropic-wire remote: its client would post /chat/completions to a /v1/messages endpoint and get a 404")
	}
	msg := err.Error()
	if !strings.Contains(msg, "anthropic") {
		t.Errorf("the refusal does not say what is wrong: %q", msg)
	}
	if !strings.Contains(msg, "claude") {
		t.Errorf("the refusal does not point at the integration that can use it: %q", msg)
	}

	// --force-tools downgrades the TOOL-FORMAT gate to a warning. It must not
	// touch this: the request shape is wrong, not the tool format, so forcing
	// would send guaranteed-404 traffic.
	if err := gateOpenAITools("zai-anthropic/glm-5", true); err == nil {
		t.Error("--force-tools forced past the wire mismatch, which it cannot fix")
	}
	if err := gateOpenAITools("zai-anthropic/glm-5", false); err == nil {
		t.Error("the refusal must survive")
	}
}

// Controls: the wire check must not break the shapes that do work.
func TestTheWireGateLeavesWorkingPairsAlone(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"https://box.example/v1","tool_format":"tool_calls","api_key":"sk-box-SECRET-1234567890"},
		{"name":"zai-anthropic","base_url":"https://api.z.ai/api/anthropic","wire":"anthropic","tool_format":"tool_calls","tool_reliable":true,"api_key":"sk-zai-SECRET-1234567890"}]}`)

	// An OpenAI-wire remote still passes on the OpenAI wire.
	if err := gateOpenAITools("box/big-model", false); err != nil {
		t.Errorf("an ordinary OpenAI-wire remote was refused: %v", err)
	}
	// A daemon model passes untouched.
	if err := gateOpenAITools("gemma4", false); err != nil {
		t.Errorf("a local model was refused: %v", err)
	}
	// The anthropic-wire remote is still usable from the integration that
	// speaks its protocol — Claude Code / cmd/agent.
	remotes, err := loadUserRemotes()
	if err != nil {
		t.Fatal(err)
	}
	var got userRemote
	for _, r := range remotes {
		if r.Name == "zai-anthropic" {
			got = r
		}
	}
	if got.Name == "" {
		t.Fatal("the anthropic-wire remote did not load")
	}
	if err := gateUserRemoteTools(got, "glm-5", toolWireAnthropic, false); err != nil {
		t.Errorf("the anthropic-wire remote was refused on the Anthropic wire, where it belongs: %v", err)
	}
}
