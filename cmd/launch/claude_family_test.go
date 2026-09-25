package launch

import "testing"

// claudeModelFamily decides which plan leg a Claude Code request lands on when
// the client sends a family id of its own (opusplan resolves its opus/haiku
// slots internally, so it never uses our ANTHROPIC_DEFAULT_*_MODEL values).
// Getting a tier wrong here bills the wrong leg silently — hence a table.
func TestClaudeModelFamily(t *testing.T) {
	cases := []struct {
		model string
		want  string
		ok    bool
	}{
		// Bare tiers: the aliases Claude Code resolves in its own catalog.
		{"opus", "opus", true},
		{"sonnet", "sonnet", true},
		{"haiku", "haiku", true},
		{"fable", "fable", true},
		// Un-tiered Claude names: the default leg owns them.
		{"claude", "claude", true},
		{"anthropic", "claude", true},
		{"anthropic.claude", "claude", true},
		// Current catalog ids: tier immediately after "claude-".
		{"claude-haiku-4-5-20251001", "haiku", true},
		{"claude-sonnet-4-5-20250929", "sonnet", true},
		{"claude-opus-4-1-20250805", "opus", true},
		{"claude-fable-5-1", "fable", true},
		// Legacy versioned ids: generation and date come FIRST, so a prefix
		// match misses and the tier is only findable as a segment.
		{"claude-3-5-sonnet-20241022", "sonnet", true},
		{"claude-3-5-haiku-20241022", "haiku", true},
		{"claude-3-opus-20240229", "opus", true},
		{"claude-3-sonnet", "sonnet", true},
		// Claude-shaped but tierless: default leg, never a raw forward.
		{"claude-2.1", "claude", true},
		{"claude-instant-1.2", "claude", true},
		{"claude-3-5", "claude", true},
		// Foreign ids must never be claimed: they belong to their upstream.
		{"glm-5.3", "", false},
		{"box/kat-awq", "", false},
		{"oaica-35b-a3b-vision", "", false},
		{"zai-coding-plan/glm-4.5-air", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := claudeModelFamily(c.model)
		if ok != c.ok || got != c.want {
			t.Errorf("claudeModelFamily(%q) = %q, %v; want %q, %v", c.model, got, ok, c.want, c.ok)
		}
	}
}

// The consequence of the family: with a native leg registered for that family,
// the legacy id must reach it, not the primary's model.
func TestProxyRouteTable_ResolveLegacyClaudeIDToItsFamilyLeg(t *testing.T) {
	primary := proxyRoute{Label: "primary", BaseURL: "http://primary", UpstreamModel: "glm-5.3"}
	sonnetLeg := proxyRoute{Label: "sonnet-leg", BaseURL: "http://sonnet", UpstreamModel: "claude/sonnet"}
	table := proxyRouteTable{
		Default:     primary,
		ByModel:     map[string]proxyRoute{},
		NativeTiers: map[string]proxyRoute{"sonnet": sonnetLeg},
	}
	route, upstream := table.resolve("claude-3-5-sonnet-20241022")
	if route.Label != sonnetLeg.Label || upstream != sonnetLeg.UpstreamModel {
		t.Fatalf("legacy sonnet id resolved to %q/%q, want the sonnet leg %q", route.Label, upstream, sonnetLeg.Label)
	}
	// And with no native leg for the family, it still lands on the default leg
	// with the default's upstream model (never the raw id).
	bare := proxyRouteTable{Default: primary, ByModel: map[string]proxyRoute{}}
	route, upstream = bare.resolve("claude-3-5-sonnet-20241022")
	if route.Label != primary.Label || upstream != primary.UpstreamModel {
		t.Fatalf("unconfigured family resolved to %q/%q, want the primary %q/%q", route.Label, upstream, primary.Label, primary.UpstreamModel)
	}
	// A foreign id is passed through untouched — it must reach its upstream.
	route, upstream = table.resolve("glm-5.3")
	if route.Label != primary.Label || upstream != "glm-5.3" {
		t.Fatalf("foreign id resolved to %q/%q, want the default leg with the id unchanged", route.Label, upstream)
	}
}
