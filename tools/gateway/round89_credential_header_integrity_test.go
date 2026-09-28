package main

// round89_credential_header_integrity_test.go — leg 3, F89-L3-4
// (2026-09-29 audit, round 89).
//
// This endpoint answers /v1/messages. An Anthropic client — the official SDK
// included — sends its key in `x-api-key`, which is the very header the proxy
// strips before it talks to a third-party upstream (2026-09-01 audit H1). The
// endpoint read only Authorization, so the caller it exists to serve was
// answered 401 while the code that strips the header said in its own comment
// that Anthropic-wire clients send it. Measured on the round-89 probe, one
// /v1/messages request per spelling: `Authorization: Bearer sk` 200,
// `x-api-key: sk` 401, `api-key: sk` 401.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheCredentialIsReadWhereTheClientPutsIt is the F89-L3-4 pin: one key,
// presented in any of the ways the wires this gateway answers put it, is one
// caller — and a key nobody stored is still refused.
func TestTheCredentialIsReadWhereTheClientPutsIt(t *testing.T) {
	up := round44Upstream(t, nil, "application/json",
		`{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	const ask = `{"model":"kat-awq","max_tokens":8,"messages":[{"role":"user","content":"go"}]}`

	for _, tc := range []struct {
		name string
		set  func(*http.Request)
		want int
	}{
		{"Authorization: Bearer sk", func(r *http.Request) { r.Header.Set("Authorization", "Bearer sk") }, 200},
		{"Authorization with a lowercase scheme", func(r *http.Request) { r.Header.Set("Authorization", "bearer sk") }, 200},
		{"a bare Authorization", func(r *http.Request) { r.Header.Set("Authorization", "sk") }, 200},
		{"x-api-key, the Anthropic wire", func(r *http.Request) { r.Header.Set("X-Api-Key", "sk") }, 200},
		{"api-key, the Azure wire", func(r *http.Request) { r.Header.Set("api-key", "sk") }, 200},
		{"an Authorization nobody stored", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, 401},
		{"an x-api-key nobody stored", func(r *http.Request) { r.Header.Set("X-Api-Key", "nope") }, 401},
		{"no credential at all", func(r *http.Request) {}, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := round39Gateway(t, up, nil)
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(ask))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			tc.set(req)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.want {
				t.Errorf("%s answered %d, want %d — one key presented on any of the wires this endpoint answers is one caller, and a key nobody stored is still refused (2026-09-29 audit, round 89, F89-L3-4):\n%s",
					tc.name, resp.StatusCode, tc.want, body)
			}
		})
	}
}

// TestOnlyTheCredentialTheCallerPresentedReachesTheUpstream is F89-L3-4's other
// half: reading a header the proxy also strips must not start forwarding it.
// The upstream sees the gateway's own credential or none, never the caller's.
func TestOnlyTheCredentialTheCallerPresentedReachesTheUpstream(t *testing.T) {
	var seen http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	t.Cleanup(up.Close)
	srv, _ := round39Gateway(t, up, nil)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"kat-awq","max_tokens":8,"messages":[{"role":"user","content":"go"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "sk")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("the turn answered %d, want 200", resp.StatusCode)
	}
	if got := seen.Get("X-Api-Key"); got != "" {
		t.Errorf("the caller's x-api-key reached the upstream as %q: reading the header where the client put it must not start relaying it to a third-party endpoint (2026-09-29 audit, round 89, F89-L3-4)", got)
	}
}
