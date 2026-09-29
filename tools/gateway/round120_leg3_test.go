package main

// Round 120 leg 3 (2026-09-29 audit): a client cannot switch off headers the gateway sets for the
// upstream by listing them in Connection (F120-L3-1), and its Host does not reach the upstream (F120-L3-2).

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func round120Upstream(t *testing.T, got *sync.Map) *httptest.Server {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		got.Store("metered", r.Header.Get("X-Oaica-Metered"))
		got.Store("ct", r.Header.Get("Content-Type"))
		got.Store("host", r.Host)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(up.Close)
	return up
}

func TestRound120ConnectionHeaderCannotStripTheGatewaysHeaders(t *testing.T) {
	var got sync.Map
	up := round120Upstream(t, &got)
	srv, _ := r107Gw(t, up, []gwKey{{SHA256: keyHash("sk"), Label: "k"}})
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		got = sync.Map{}
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(`{"model":"kat-awq","max_tokens":5,"messages":[{"role":"user","content":"go"}]}`))
		req.Header.Set("Authorization", "Bearer sk")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connection", "X-Oaica-Metered, Content-Type")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if v, _ := got.Load("metered"); v != "1" {
			t.Errorf("%s: the upstream received X-Oaica-Metered=%q, want the gateway's own 1", path, v)
		}
		if v, _ := got.Load("ct"); v != "application/json" {
			t.Errorf("%s: the upstream received Content-Type=%q", path, v)
		}
	}
}

func TestRound120ClientHostDoesNotReachTheUpstream(t *testing.T) {
	var got sync.Map
	up := round120Upstream(t, &got)
	srv, _ := r107Gw(t, up, []gwKey{{SHA256: keyHash("sk"), Label: "k"}})
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`))
	req.Host = "victim-tenant.example"
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if v, _ := got.Load("host"); v == "victim-tenant.example" {
		t.Errorf("the client's Host reached the upstream: %q", v)
	}
}
