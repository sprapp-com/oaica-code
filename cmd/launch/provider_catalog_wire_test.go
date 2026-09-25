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

// A base URL that names /anthropic must declare the anthropic wire. This is the
// exact pairing that broke, and the default is openai — the dangerous direction.
func TestCatalog_AnthropicEndpointsDeclareAnthropicWire(t *testing.T) {
	checked := 0
	for _, e := range providerCatalog() {
		if !anthropicEndpointShaped(e.BaseURL) {
			continue
		}
		checked++
		r := userRemote{Name: e.Name, BaseURL: e.BaseURL, Wire: e.Wire, ToolFormat: e.ToolFormat}
		if got := r.Descriptor().Wire; got != "anthropic" {
			t.Errorf("%s: base_url %q is an Anthropic-compatible endpoint but wire resolves to %q — the proxy would POST %s/chat/completions and the plan answers 404 (regression: 5426c932 lost this row's wire field)",
				e.Name, e.BaseURL, got, r.openAIBase())
		}
	}
	if checked == 0 {
		t.Fatal("no anthropic-shaped endpoints in the catalog — this test verified nothing")
	}
}

// The converse guard: a row declaring the anthropic wire must build an upstream
// that is an Anthropic messages URL, with no doubled version segment. Catches
// both a base_url carrying "/v1" that openAIBase re-adds and a wrong wire.
func TestCatalog_AnthropicWireBuildsMessagesURL(t *testing.T) {
	for _, e := range providerCatalog() {
		r := userRemote{Name: e.Name, BaseURL: e.BaseURL, Version: e.Version, Wire: e.Wire, ToolFormat: e.ToolFormat}
		if r.Descriptor().Wire != "anthropic" {
			continue
		}
		upstream := r.openAIBase() + "/messages"
		if strings.Contains(upstream, "/v1/v1") {
			t.Errorf("%s: upstream %q repeats the version segment", e.Name, upstream)
		}
		if !strings.HasSuffix(upstream, "/v1/messages") {
			t.Errorf("%s: upstream %q is not an Anthropic messages endpoint", e.Name, upstream)
		}
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
