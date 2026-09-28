package server

// round94_midstream_stated_status_test.go — leg 1, F94-L1-1 (2026-09-29 audit,
// round 94).
//
// A remote that states a failure MID-STREAM states the status its own clients
// are answered with (`{"error": "...", "status": 404}` — the frame
// server/routes.go:2286-2289 keeps for exactly that reason, and the frame this
// leg's own runner lane writes). The relay lane reads that frame back through
// api/client.go's `stream`, whose body-level status branches only look at the
// HTTP response's own status — a 200 stream, therefore no branch at all — and
// whose in-loop `errorResponse` struct carried the error text and the
// signin_url but not the status. The frame's stated status was dropped there,
// so `errors.As(err, &api.StatusError)` never matched a mid-stream refusal and
// the already-written arm of the relay's answer wrote a frame with no status
// field at all. Measured on this leg before the fix, on one peer body
// (a content chunk, then `{"error":"the runner died","status":404}`):
//
//	native chat    stream=false  500 {"error":"the runner died"}
//	native chat    stream=true   200 <chunk>   {"error":"the runner died"}
//	anthropic      stream=false  500 api_error
//	anthropic      stream=true   200 ... event: error  api_error
//
// where the runner lane answers the same event 404 / `not_found_error` on both
// arms. One upstream cause, four surfaces, two arms, three different readings
// (2026-09-29 audit, round 94, F94-L1-1).
//
// The second half pins what the fix must NOT do: a mid-stream error frame that
// states no status keeps the relay's existing answer. A status invented from a
// field that was never about the failure would type every arm's refusal from
// nothing, which is a worse lie than the one above.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/middleware"
)

// r94StatingPeer answers every turn with one content chunk and then the given
// error frame, as a 200 ndjson stream.
func r94StatingPeer(t *testing.T, frame string) *httptest.Server {
	t.Helper()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"test","created_at":"2026-01-01T00:00:00Z","message":{"role":"assistant","content":"half "},"done":false}`+"\n")
		_, _ = io.WriteString(w, frame+"\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(peer.Close)
	return peer
}

// r94RelayModel registers the remote-host model r93RelayModel builds, under this
// round's name.
func r94RelayModel(t *testing.T, peer *httptest.Server, name string) Server {
	t.Helper()
	setTestHome(t, t.TempDir())
	u, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_REMOTES", u.Hostname())
	s := Server{}
	yes := true
	w := createRequest(t, s.CreateHandler, api.CreateRequest{
		Model: name, RemoteHost: peer.URL, From: "test",
		Info: map[string]any{"capabilities": []string{"completion"}}, Stream: &yes,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("premise: creating the remote-host model answered %d: %s", w.Code, w.Body.String())
	}
	return s
}

// r94RelayedMidStreamStatus drives one relayed turn whose upstream fails
// mid-stream, through one surface on one arm, and reports the status and the
// exact body the client was handed.
func r94RelayedMidStreamStatus(t *testing.T, frame, path string, mw gin.HandlerFunc, stream bool) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	peer := r94StatingPeer(t, frame)
	s := r94RelayModel(t, peer, "r94-relay")
	router := gin.New()
	if mw != nil {
		router.Use(mw)
	}
	if path == "/api/generate" {
		router.POST(path, s.GenerateHandler)
	} else {
		router.POST(path, s.ChatHandler)
	}
	local := httptest.NewServer(router)
	t.Cleanup(local.Close)

	lit := "false"
	if stream {
		lit = "true"
	}
	var body string
	switch path {
	case "/api/generate":
		body = `{"model":"r94-relay","stream":` + lit + `,"prompt":"hi"}`
	case "/v1/messages":
		body = `{"model":"r94-relay","stream":` + lit + `,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	default:
		body = `{"model":"r94-relay","stream":` + lit + `,"messages":[{"role":"user","content":"hi"}]}`
	}
	resp, err := http.Post(local.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, strings.TrimSpace(string(out))
}

// r94Surfaces is every surface a relayed turn can be read from.
func r94Surfaces() []struct {
	name string
	path string
	mw   gin.HandlerFunc
} {
	return []struct {
		name string
		path string
		mw   gin.HandlerFunc
	}{
		{"native chat", "/api/chat", nil},
		{"native generate", "/api/generate", nil},
		{"openai chat", "/v1/chat/completions", middleware.ChatMiddleware()},
		{"anthropic", "/v1/messages", middleware.AnthropicMessagesMiddleware()},
	}
}

// The status a peer states mid-stream is the status every surface answers with,
// on both arms — the same reading the runner lane hands a direct client.
func TestAMidStreamStatedStatusReachesEverySurfaceOnBothArms(t *testing.T) {
	const frame = `{"error":"the runner died","status":404}`
	for _, tc := range r94Surfaces() {
		code, got := r94RelayedMidStreamStatus(t, frame, tc.path, tc.mw, false)
		if code != http.StatusNotFound {
			t.Errorf("%s, buffered: answered %d, want %d — the status the peer stated is the cause: %s", tc.name, code, http.StatusNotFound, got)
		}
		if !strings.Contains(got, "the runner died") {
			t.Errorf("%s, buffered: the client was handed %s, want the peer's message", tc.name, got)
		}

		code, got = r94RelayedMidStreamStatus(t, frame, tc.path, tc.mw, true)
		if code != http.StatusOK {
			// The relay's own framing is on the wire by the time the peer's
			// failure is read; the status is stated in the frame, as it is on
			// the runner lane.
			t.Errorf("%s, streamed: answered %d, want %d with the status in the frame", tc.name, code, http.StatusOK)
		}
		if !strings.Contains(got, "the runner died") {
			t.Errorf("%s, streamed: the client was handed %s, want the peer's message", tc.name, got)
		}
		if tc.mw == nil && !strings.Contains(got, `"status":404`) {
			t.Errorf("%s, streamed: the client was handed %s, want the frame to carry the status the peer stated — a refusal that names no status is read as this process's own generic failure, and the retry the producer asked for is never signalled (2026-09-29 audit, round 94, F94-L1-1)", tc.name, got)
		}
		if tc.mw != nil && !strings.Contains(got, "not_found_error") {
			t.Errorf("%s, streamed: the client was handed %s, want the error type the stated status names — the middleware reads that field to type the refusal, so the same upstream cause must not read api_error here and not_found_error when the client did not stream (2026-09-29 audit, round 94, F94-L1-1)", tc.name, got)
		}
	}
}

// The translated surfaces read the same status out of the buffered arm's body
// too: the relay writes the peer's message and the stated status, and the
// middleware types the refusal from it on both arms.
func TestAMidStreamStatedStatusIsTypedTheSameOnBothArms(t *testing.T) {
	const frame = `{"error":"the runner died","status":404}`
	for _, tc := range r94Surfaces() {
		if tc.mw == nil {
			continue
		}
		_, buffered := r94RelayedMidStreamStatus(t, frame, tc.path, tc.mw, false)
		_, streamed := r94RelayedMidStreamStatus(t, frame, tc.path, tc.mw, true)
		if !strings.Contains(buffered, "not_found_error") {
			t.Errorf("%s, buffered: the client was handed %s, want not_found_error", tc.name, buffered)
		}
		if !strings.Contains(streamed, "not_found_error") {
			t.Errorf("%s, streamed: the client was handed %s, want not_found_error", tc.name, streamed)
		}
	}
}

// A mid-stream error frame that states no status keeps the relay's existing
// answer. The status is not invented: nothing in that frame was ever about a
// status, and typing every arm's refusal from it would state a cause the peer
// never stated.
func TestAMidStreamFailureThatStatesNoStatusKeepsItsBareMessage(t *testing.T) {
	const frame = `{"error":"the runner died"}`
	for _, tc := range r94Surfaces() {
		code, got := r94RelayedMidStreamStatus(t, frame, tc.path, tc.mw, false)
		if code != http.StatusInternalServerError {
			t.Errorf("%s, buffered: answered %d, want %d — a frame that states no status states no status", tc.name, code, http.StatusInternalServerError)
		}
		if !strings.Contains(got, "the runner died") {
			t.Errorf("%s, buffered: the client was handed %s, want the peer's message", tc.name, got)
		}
		if strings.Contains(got, `"status"`) {
			t.Errorf("%s, buffered: the client was handed %s, want no status field — the peer never stated one", tc.name, got)
		}

		_, got = r94RelayedMidStreamStatus(t, frame, tc.path, tc.mw, true)
		if !strings.Contains(got, "the runner died") {
			t.Errorf("%s, streamed: the client was handed %s, want the peer's message", tc.name, got)
		}
	}
}
