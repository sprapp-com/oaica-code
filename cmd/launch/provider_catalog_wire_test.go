package launch

// provider_catalog_wire_test.go — invariants between a catalog row's ENDPOINT
// and its declared WIRE, checked over the whole table.
//
// Why this file exists: `zai-coding-plan` points at z.ai's Anthropic-compatible
// path (https://api.z.ai/api/anthropic) and must declare wire "anthropic". A
// catalog rewrite (5426c932) re-emitted providers.json from a copy taken before
// 196130d8 added the field, so the row silently fell back to the default openai
// wire — oaica then POSTed /chat/completions at an Anthropic-shaped endpoint and
// z.ai answered 404 {"detail":"Not Found"}, surfacing in Claude Code as
// "502 upstream HTTP 404". Nothing failed to build, nothing failed to parse,
// and no test looked at the pairing. These tests do.

import (
	"strings"
	"testing"
)

// anthropicEndpointShaped reports whether a base URL is an Anthropic-compatible
// endpoint — the vendor documents it as an ANTHROPIC_BASE_URL, so requests go
// to <base>/v1/messages with an x-api-key header, never to /chat/completions.
func anthropicEndpointShaped(baseURL string) bool {
	b := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	return strings.HasSuffix(b, "/anthropic") || strings.Contains(b, "/anthropic/")
}

// No catalog row may point at a vendor's Anthropic-compatible path.
//
// oaica's translation proxy has exactly one upstream form, <base>/chat/
// completions (anthropic_openai_proxy.go); the Anthropic passthrough exists
// only for api.anthropic.com itself (proxyRoute.NativePassthrough, set from
// sourceNativeAnthropic). A row on an /anthropic path therefore cannot be
// driven by any launch: z.ai answered 404 {"detail":"Not Found"} to
// /anthropic/v1/chat/completions and MiniMax 404 "404 page not found" —
// surfacing as "502 upstream HTTP 404" in Claude Code. The plan rows were
// repointed at each vendor's OpenAI-compatible path (zai-coding-plan →
// /api/coding/paas/v4, the form models.dev declares; minimax-* →
// api.minimax.io / api.minimax.cn). This test keeps the next such row out:
// if an Anthropic-native remote is genuinely wanted, it needs a passthrough
// route first, not a base_url.
func TestCatalog_NoRowPointsAtAnAnthropicPath(t *testing.T) {
	for _, e := range providerCatalog() {
		if !anthropicEndpointShaped(e.BaseURL) {
			continue
		}
		r := userRemote{Name: e.Name, BaseURL: e.BaseURL, Version: e.Version, Wire: e.Wire, ToolFormat: e.ToolFormat}
		t.Errorf("%s: base_url %q is an Anthropic-shaped endpoint but the proxy only ever POSTs %s — repoint the row at the vendor's OpenAI-compatible path, or give it a passthrough route",
			e.Name, e.BaseURL, r.openAIBase()+"/chat/completions")
	}
}

// The verified endpoint each plan row must reach. These are live-curl results,
// not guesses: a "fix the URL" edit that looks plausible but was never tried
// against the vendor is exactly what produced the 404s above.
func TestCatalog_PlanRowsHitTheVerifiedEndpoint(t *testing.T) {
	want := map[string]string{
		"zai-coding-plan":        "https://api.z.ai/api/coding/paas/v4/chat/completions",
		"zai":                    "https://api.z.ai/api/paas/v4/chat/completions",
		"minimax-coding-plan":    "https://api.minimax.io/v1/chat/completions",
		"minimax-cn-coding-plan": "https://api.minimax.cn/v1/chat/completions",
	}
	seen := 0
	for _, e := range providerCatalog() {
		wantURL, ok := want[e.Name]
		if !ok {
			continue
		}
		seen++
		r := userRemote{Name: e.Name, BaseURL: e.BaseURL, Version: e.Version, Wire: e.Wire, ToolFormat: e.ToolFormat}
		if got := r.openAIBase() + "/chat/completions"; got != wantURL {
			t.Errorf("%s: upstream %q, want the verified %q", e.Name, got, wantURL)
		}
		if r.Descriptor().Wire != "openai" {
			t.Errorf("%s: wire = %q, want openai (these plans are driven through the OpenAI translation proxy)", e.Name, r.Descriptor().Wire)
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
