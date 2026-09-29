package server

// round109_leg1_cloud_proxy_identity_headers_test.go — leg 1, round 109
// (2026-09-29 audit), F109-L1-1.
//
// The cloud proxy copied every client request header to ollama.com except the
// hop-by-hop ones and overwrote only Authorization. The Anthropic SDK always sends
// x-api-key, so a client holding a real Anthropic key and pointed at the local
// server had that key delivered to a third party on every :cloud request; browser
// cookies and the caller's X-Forwarded-For went with it, and the upstream's own
// Set-Cookie came back and landed under the local origin. The remote-alias relay
// door builds fresh headers and forwarded none of it. The proxy is the operator's
// signed request, not the client's.

import (
	"net/http"
	"testing"
)

func TestMine109TheCloudProxyDoesNotForwardTheClientsIdentity(t *testing.T) {
	src := http.Header{
		"Content-Type":        {"application/json"},
		"Accept":              {"application/json"},
		"User-Agent":          {"anthropic-sdk"},
		"Anthropic-Version":   {"2023-06-01"},
		"X-Stainless-Lang":    {"js"},
		"X-Api-Key":           {"sk-ant-REALSECRET"},
		"Api-Key":             {"k2"},
		"Cookie":              {"session=abc"},
		"Proxy-Authorization": {"Basic x"},
		"X-Forwarded-For":     {"10.9.9.9"},
		"X-Forwarded-Host":    {"h"},
		"X-Real-Ip":           {"10.9.9.9"},
		"Forwarded":           {"for=10.9.9.9"},
	}
	dst := http.Header{}
	copyProxyRequestHeaders(dst, src)
	for _, h := range []string{"X-Api-Key", "Api-Key", "Cookie", "Proxy-Authorization", "X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Forwarded"} {
		if v := dst.Get(h); v != "" {
			t.Errorf("the proxied request carries %s: %q — the client's credential and address are not the operator's signed request (2026-09-29 audit, round 109, F109-L1-1)", h, v)
		}
	}
	// What the upstream needs to serve the request is still sent.
	for _, h := range []string{"Content-Type", "Accept", "User-Agent", "Anthropic-Version", "X-Stainless-Lang"} {
		if dst.Get(h) == "" {
			t.Errorf("the proxied request lost %s (2026-09-29 audit, round 109, F109-L1-1)", h)
		}
	}
}

func TestMine109TheCloudProxyDoesNotRelayAnUpstreamCookie(t *testing.T) {
	src := http.Header{
		"Content-Type": {"application/json"},
		"Set-Cookie":   {"sid=upstream; Domain=ollama.com"},
		"Retry-After":  {"3"},
	}
	dst := http.Header{}
	copyProxyResponseHeaders(dst, src)
	if v := dst.Get("Set-Cookie"); v != "" {
		t.Errorf("the client received Set-Cookie %q — an upstream cookie must not land under the local origin (2026-09-29 audit, round 109, F109-L1-1)", v)
	}
	if dst.Get("Retry-After") != "3" || dst.Get("Content-Type") == "" {
		t.Errorf("the response lost a header the client needs: %v (2026-09-29 audit, round 109, F109-L1-1)", dst)
	}
}
