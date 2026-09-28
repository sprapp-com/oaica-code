package server

// round83_web_search_tool_type_integrity_test.go — leg 1, F83-L1-2
// (2026-09-28 audit, round 83).
//
// The cloud proxy decides whether a `/v1/messages` body must be served by the
// local surface or relayed, and it asked that question of the tool's `type`
// field TRIMMED while the anthropic middleware — the thing that installs the
// web-search takeover once the body is served locally — asked it exactly. One
// body, two verdicts: with the type spelled `" web_search_20250305"` the proxy
// called it a search turn and served it locally, and the middleware found no
// web_search tool in it and installed no takeover, so the turn was answered
// with neither arm. Measured, `hasAnthropicWebSearchTool` was true and
// `hasWebSearchTool` false for that body.
//
// Both readers ask anthropic.IsWebSearchToolType now: a type that is not the
// string the wire specified is not the tool, which is what the converter and
// the gateway leg already read.

import (
	"testing"

	"github.com/ollama/ollama/anthropic"
)

// TestTheCloudProxyAsksTheWebSearchToolTypeOneWay is F83-L1-2: the proxy's
// verdict on a body's tools is the one predicate, on every spelling.
func TestTheCloudProxyAsksTheWebSearchToolTypeOneWay(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		want bool
	}{
		{"web_search_20250305", true},
		{"web_search", true},
		{" web_search_20250305", false},
		{"web-search", false},
		{"WebSearch", false},
	} {
		body := []byte(`{"model":"m:cloud","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"` + tc.typ + `"}]}`)
		if got := hasAnthropicWebSearchTool(body); got != tc.want {
			t.Errorf("hasAnthropicWebSearchTool(%q) = %v, want %v: this decides whether the body is served locally at all, and the middleware that then looks for the tool reads the type the same exact way — one reader trimming the field made a search turn to the proxy and an ordinary one to the takeover, and the body was answered with neither (2026-09-28 audit, round 83, F83-L1-2)", tc.typ, got, tc.want)
		}
		if got := anthropic.IsWebSearchToolType(tc.typ); got != tc.want {
			t.Errorf("anthropic.IsWebSearchToolType(%q) = %v, want %v — the one predicate every leg asks (2026-09-28 audit, round 83, F83-L1-2)", tc.typ, got, tc.want)
		}
	}

	// The same reading through a body that carries several tools, only one of
	// which is the tool: the verdict is about the body, and it is the predicate's.
	body := []byte(`{"model":"m:cloud","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"Bash"},{"type":" web_search_20250305"}]}`)
	if hasAnthropicWebSearchTool(body) {
		t.Errorf("a body whose only web_search-typed tool is spelled with a leading space was called a search turn: the proxy serves such a body locally, and the middleware — which reads the type exactly, like the converter and the gateway — installs no takeover for it (2026-09-28 audit, round 83, F83-L1-2)")
	}
}
