package server

// round93_relayed_refusal_body_test.go — leg 1, F93-L1-1 (2026-09-29 audit,
// round 93).
//
// A relayed refusal is the PEER's refusal, and the client must be handed it in
// the shape the runner lane hands the same failure. The relay holds an
// api.StatusError built from the peer's response — ErrorMessage is the `error`
// field the peer wrote, Status is the peer's HTTP status line — and marshalling
// that whole struct wrote the struct's own untagged fields into the body:
// measured on this leg before the fix,
//
//	{"StatusCode":429,"Status":"429 Too Many Requests","error":"the runner is busy"}
//
// where the peer answered `{"error": "the runner is busy"}` and the runner lane
// answers a direct client the same bare object (pinned at
// routes_generate_test.go:1474). The middleware writers READ this body back:
// middleware/openai.go's writeError decodes it into api.StatusError and encodes
// Error(), which joins Status and ErrorMessage — so the OpenAI and Responses
// surfaces answered "429 Too Many Requests: the runner is busy" for one verdict
// the direct answer, and the native and Anthropic surfaces, both state as "the
// runner is busy". Two fields no native client has ever been sent, on a leg
// whose whole contract is that one upstream verdict reads the same from every
// surface (2026-09-29 audit, round 93, F93-L1-1).
//
// The fallback for a peer that stated no message is pinned too: its status line
// is all the cause there is, and an empty string reaches the middleware's
// generic "something went wrong" sentence instead.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
)

// r93RefusingRemote is a peer that answers every turn with one non-200 status
// and, when there is a message, the body a real ollama writes for it.
func r93RefusingRemote(t *testing.T, status int, message string) *httptest.Server {
	t.Helper()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		if message != "" {
			_ = json.NewEncoder(w).Encode(gin.H{"error": message})
		}
	}))
	t.Cleanup(remote.Close)
	return remote
}

// r93RelayModel is r91RelayModel's setup with this round's name, so the pin
// stands on its own.
func r93RelayModel(t *testing.T, remote *httptest.Server, name string) Server {
	t.Helper()
	setTestHome(t, t.TempDir())
	p, err := url.Parse(remote.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_REMOTES", p.Hostname())
	s := Server{}
	yes := true
	w := createRequest(t, s.CreateHandler, api.CreateRequest{
		Model: name, RemoteHost: remote.URL, From: "test",
		Info: map[string]any{"capabilities": []string{"completion"}}, Stream: &yes,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("premise: creating the remote-host model answered %d: %s", w.Code, w.Body.String())
	}
	return s
}

// r93RelayedRefusal drives one relayed turn through one native path and reports
// the status and the exact body the client was handed.
func r93RelayedRefusal(t *testing.T, remote *httptest.Server, path string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := r93RelayModel(t, remote, "r93-relay")
	router := gin.New()
	body := `{"model":"r93-relay","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	if path == "/api/chat" {
		router.POST(path, s.ChatHandler)
	} else {
		router.POST(path, s.GenerateHandler)
		body = `{"model":"r93-relay","stream":false,"prompt":"hi"}`
	}
	local := httptest.NewServer(router)
	t.Cleanup(local.Close)
	resp, err := http.Post(local.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, strings.TrimSpace(string(out))
}

// The peer's body is the client's body: the relay adds nothing to it, on either
// native path.
func TestARelayedRefusalIsThePeersOwnBody(t *testing.T) {
	want := `{"error":"the runner is busy"}`
	for _, path := range []string{"/api/chat", "/api/generate"} {
		remote := r93RefusingRemote(t, http.StatusTooManyRequests, "the runner is busy")
		code, got := r93RelayedRefusal(t, remote, path)
		if code != http.StatusTooManyRequests {
			t.Errorf("%s: answered %d, want %d", path, code, http.StatusTooManyRequests)
		}
		if got != want {
			t.Errorf("%s: the client was handed %s, want %s — the relay states the peer's refusal, not this process's struct for it (2026-09-29 audit, round 93, F93-L1-1)", path, got, want)
		}
	}
}

// A peer that stated no message still stated a status, and that status is the
// cause the client is given.
func TestARelayedRefusalThatStatesNoMessageKeepsItsStatus(t *testing.T) {
	want := `{"error":"429 Too Many Requests"}`
	for _, path := range []string{"/api/chat", "/api/generate"} {
		remote := r93RefusingRemote(t, http.StatusTooManyRequests, "")
		code, got := r93RelayedRefusal(t, remote, path)
		if code != http.StatusTooManyRequests {
			t.Errorf("%s: answered %d, want %d", path, code, http.StatusTooManyRequests)
		}
		if got != want {
			t.Errorf("%s: the client was handed %s, want %s — a peer that states no message still states its status (2026-09-29 audit, round 93, F93-L1-1)", path, got, want)
		}
	}
}

// The other way a peer states no message: an error object whose message is
// empty. That one DOES reach the relay as a status error, and its status line
// is the whole of the cause it stated.
func TestARelayedRefusalWithAnEmptyMessageKeepsItsStatus(t *testing.T) {
	want := `{"error":"429 Too Many Requests"}`
	for _, path := range []string{"/api/chat", "/api/generate"} {
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":""}`)
		}))
		code, got := r93RelayedRefusal(t, remote, path)
		remote.Close()
		if code != http.StatusTooManyRequests {
			t.Errorf("%s: answered %d, want %d", path, code, http.StatusTooManyRequests)
		}
		if got != want {
			t.Errorf("%s: the client was handed %s, want %s — an empty message and a status is a status (2026-09-29 audit, round 93, F93-L1-1)", path, got, want)
		}
	}
}
