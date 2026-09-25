package launch

// provider_catalog_wire_test.go — invariants between a catalog row's ENDPOINT
// and its declared WIRE, checked over the whole table.
//
// Why this file exists: `zai-coding-plan` pointed at z.ai's Anthropic-compatible
// path (https://api.z.ai/api/anthropic) while declaring (after a bad re-emit, see
// 5426c932) the default openai wire — oaica then POSTed /chat/completions at an
// Anthropic-shaped endpoint and z.ai answered 404 {"detail":"Not Found"},
// surfacing in Claude Code as "502 upstream HTTP 404". Nothing failed to build,
// nothing failed to parse, and no test looked at the pairing. These tests do.
//
// The rule the pair must satisfy (2026-09-25, second revision): a row's base and
// its wire are ONE decision. An /anthropic base is only reachable with wire
// "anthropic" (the proxy forwards to <base>/v1/messages untranslated); anything
// else is reached through the OpenAI translation path (<base>/chat/completions).

import (
	"strings"
	"testing"
)

// anthropicEndpointShaped reports whether a base URL is an Anthropic-compatible
// endpoint — the vendor documents it as an ANTHROPIC_BASE_URL, so requests go
// to <base>/v1/messages with an x-api-key header, never to /chat/completions.
// api.anthropic.com itself counts (the `anthropic` row, the origin of the
// shape); vendors mirror it under an /anthropic path.
func anthropicEndpointShaped(baseURL string) bool {
	b := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	return b == "https://api.anthropic.com" ||
		strings.HasSuffix(b, "/anthropic") || strings.Contains(b, "/anthropic/")
}

// Base and wire must agree, in both directions: an /anthropic base reached on
// the openai wire answers 404 to /chat/completions (the shipped bug), and a
// vendor's plain OpenAI base reached on the anthropic wire gets an untranslated
// /v1/messages it does not serve.
func TestCatalog_WireMatchesEndpointShape(t *testing.T) {
	for _, e := range providerCatalog() {
		r := userRemote{Name: e.Name, BaseURL: e.BaseURL, Version: e.Version, Wire: e.Wire, ToolFormat: e.ToolFormat}
		wire := r.Descriptor().Wire
		anthropicShaped := anthropicEndpointShaped(e.BaseURL)
		switch {
		case anthropicShaped && wire != "anthropic":
			t.Errorf("%s: base_url %q is an Anthropic-shaped endpoint but wire = %q — the proxy would POST %s at it; give the row \"wire\": \"anthropic\"",
				e.Name, e.BaseURL, wire, r.openAIBase()+"/chat/completions")
		case !anthropicShaped && wire == "anthropic":
			t.Errorf("%s: wire = \"anthropic\" but base_url %q is not an Anthropic-shaped endpoint — the proxy would POST %s at it",
				e.Name, e.BaseURL, r.openAIBase()+"/messages")
		}
	}
}

// The endpoint each plan row must reach, and the credential surface it reaches
// it on. These are live-curl results, not guesses: a "fix the URL" edit that
// looks plausible but was never tried against the vendor is exactly what
// produced the 404s above.
func TestCatalog_PlanRowsHitTheVerifiedEndpoint(t *testing.T) {
	want := map[string]struct {
		url  string
		wire string
	}{
		// Anthropic-compatible surface, verified 200 with this plan's own key.
		"zai-coding-plan": {"https://api.z.ai/api/anthropic/v1/messages", "anthropic"},
		// Anthropic-compatible surface, verified 200 with this plan's own key.
		"minimax-coding-plan": {"https://api.minimax.io/anthropic/v1/messages", "anthropic"},
		// Route existence + wire confirmed (Anthropic-shaped 401, out-of-region
		// key); no valid CN key available here, so not verified end to end.
		"minimax-cn-coding-plan": {"https://api.minimax.cn/anthropic/v1/messages", "anthropic"},
		// Per-token platform API, OpenAI-compatible: coding-plan keys are
		// rejected here (429 "no resource package"), this is the 'zai' row.
		"zai": {"https://api.z.ai/api/paas/v4/chat/completions", "openai"},
	}
	seen := 0
	for _, e := range providerCatalog() {
		wantRow, ok := want[e.Name]
		if !ok {
			continue
		}
		seen++
		r := userRemote{Name: e.Name, BaseURL: e.BaseURL, Version: e.Version, Wire: e.Wire, ToolFormat: e.ToolFormat}
		upstream := r.openAIBase() + "/chat/completions"
		if r.Descriptor().Wire == "anthropic" {
			target, _, _, ok := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
				Name: e.Name, BaseURL: r.openAIBase(), UpstreamModel: "m", Wire: r.Descriptor().Wire, Token: "sk-x",
			}}).anthropicPassthroughTarget()
			if !ok {
				t.Fatalf("%s: no passthrough target resolved for an anthropic-wire row", e.Name)
			}
			upstream = target
		}
		if upstream != wantRow.url {
			t.Errorf("%s: upstream %q, want the verified %q", e.Name, upstream, wantRow.url)
		}
		if r.Descriptor().Wire != wantRow.wire {
			t.Errorf("%s: wire = %q, want %q", e.Name, r.Descriptor().Wire, wantRow.wire)
		}
	}
	if seen != len(want) {
		t.Fatalf("checked %d of %d plan rows — a row was renamed or removed without updating this list", seen, len(want))
	}
}

// Every row's upstream must be well-formed whatever its wire: one scheme, no
// doubled slash, no repeated version segment, and the endpoint suffix matching
// the wire. A row added with a copied base_url is the likely offender.
func TestCatalog_UpstreamURLsAreWellFormed(t *testing.T) {
	for _, e := range providerCatalog() {
		r := userRemote{Name: e.Name, BaseURL: e.BaseURL, Version: e.Version, Wire: e.Wire, ToolFormat: e.ToolFormat}
		base := r.openAIBase()
		suffix, wantEnd := "/chat/completions", "/chat/completions"
		if r.Descriptor().Wire == "anthropic" {
			suffix, wantEnd = "/messages", "/v1/messages"
		}
		upstream := base + suffix
		switch {
		case !strings.HasPrefix(upstream, "https://") && !strings.HasPrefix(upstream, "http://"):
			t.Errorf("%s: upstream %q has no scheme", e.Name, upstream)
		case strings.Contains(strings.TrimPrefix(strings.TrimPrefix(upstream, "https://"), "http://"), "//"):
			t.Errorf("%s: upstream %q has a doubled slash", e.Name, upstream)
		case strings.Contains(upstream, "/v1/v1"):
			t.Errorf("%s: upstream %q repeats the version segment", e.Name, upstream)
		case !strings.HasSuffix(upstream, wantEnd):
			t.Errorf("%s: upstream %q does not end in %q", e.Name, upstream, wantEnd)
		}
	}
}
