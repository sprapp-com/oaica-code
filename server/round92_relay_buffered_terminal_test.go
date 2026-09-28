package server

// round92_relay_buffered_terminal_test.go — leg 1, F92-L1-5 (2026-09-29 audit,
// round 92).
//
// Round 91 taught both remote-host relay branches that a turn which never
// finishes is a failure, and stated that failure in the framing the relay writes
// (F91-L1-2). For a STREAMING client that framing is ndjson and the frame is one
// more value in it. For a client that asked for a single document it is not: the
// relay's `fn` wrote each chunk straight to the writer, so a Done-less document
// from the remote was already a whole JSON document on the wire with a 200
// before the turn was known to be over, and the failure frame appended after it
// made the body two top-level values — not JSON at all. Measured before the fix,
// with a remote answering a `stream:false` relay with a whole Done-less
// document, on all three client surfaces: 200, `validJSON=false`, and on
// /v1/responses the sentence was swallowed as well. A client that cannot parse
// the body cannot retry on the cause either.
//
// The fix holds a chunk that does not end the turn and writes it only when the
// turn does end, so the failure frame is never appended to a document that is
// already on the wire. Both halves of that are pinned here: the hold (a Done-less
// turn is refused as a whole document) and the release (a turn that ends still
// carries every chunk the remote sent — without it the hold would turn today's
// ndjson into a silently truncated document).
//
// NOT in scope here, and recorded instead: a remote that answers a `stream:false`
// relay with MORE THAN ONE chunk still reaches a buffered client as one ndjson
// line per chunk, which the three middleware surfaces refuse with a 500. That is
// true before and after this fix (measured), it needs a merge of the chunks that
// does not exist on this branch, and this fix does not make it worse — which is
// the whole job of the release half below.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/middleware"
)

const r92RelaySentence = "upstream stream ended before the response was complete"

// r92Relay answers every remote request with the given documents, one per line,
// then closes cleanly. Each document carries BOTH spellings of the text, because
// the same remote is asked by both relay branches: /api/chat decodes into a
// ChatResponse, /api/generate into a GenerateResponse.
func r92Relay(t *testing.T, docs ...string) *httptest.Server {
	t.Helper()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		for _, d := range docs {
			io.WriteString(w, d+"\n")
		}
	}))
	t.Cleanup(remote.Close)
	return remote
}

func r92Doc(text string, done bool) string {
	doneTail := `,"done":false}`
	if done {
		doneTail = `,"done":true,"done_reason":"stop"}`
	}
	return `{"model":"test","message":{"role":"assistant","content":"` + text + `"},"response":"` + text + `"` + doneTail
}

// r92RelaySurface runs one request against the named client surface on a remote
// model and returns the status and body.
func r92RelaySurface(t *testing.T, remote *httptest.Server, path, body string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := r91RelayModel(t, remote, "r92-relay")
	router := gin.New()
	switch path {
	case "/api/chat":
		router.POST(path, s.ChatHandler)
	case "/api/generate":
		router.POST(path, s.GenerateHandler)
	case "/v1/chat/completions":
		router.POST(path, middleware.ChatMiddleware(), s.ChatHandler)
	case "/v1/messages":
		router.POST(path, middleware.AnthropicMessagesMiddleware(), s.ChatHandler)
	case "/v1/responses":
		router.POST(path, middleware.ResponsesMiddleware(), s.ChatHandler)
	default:
		t.Fatalf("unknown surface %s", path)
	}
	local := httptest.NewServer(router)
	t.Cleanup(local.Close)

	resp, err := http.Post(local.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(out)
}

// The non-streaming surfaces, which are the ones a `stream:false` client can
// reach. Kept in one place so the two claims below cover the same set.
var r92BufferedSurfaces = []struct{ name, path, body string }{
	{"chat", "/api/chat", `{"model":"r92-relay","stream":false,"messages":[{"role":"user","content":"hi"}]}`},
	{"generate", "/api/generate", `{"model":"r92-relay","stream":false,"prompt":"hi"}`},
	{"openai chat", "/v1/chat/completions", `{"model":"r92-relay","messages":[{"role":"user","content":"hi"}],"stream":false}`},
	{"anthropic", "/v1/messages", `{"model":"r92-relay","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":false}`},
	{"responses", "/v1/responses", `{"model":"r92-relay","input":"hi","stream":false}`},
}

// A non-streaming client is told one thing: this turn did not finish. Whichever
// way the remote stopped without saying so — one document, or several —
func TestABufferedRelayTurnThatNeverFinishedIsRefusedAsAWholeDocument(t *testing.T) {
	for _, remote := range []struct {
		name string
		docs []string
	}{
		{"one Done-less document", []string{r92Doc("half a ", false)}},
		{"two Done-less documents", []string{r92Doc("half a ", false), r92Doc("sentence", false)}},
	} {
		for _, tc := range r92BufferedSurfaces {
			code, body := r92RelaySurface(t, r92Relay(t, remote.docs...), tc.path, tc.body)

			if code == http.StatusOK {
				t.Errorf("%s (%s): a relayed turn that never finished was sold to a non-streaming client as %d: %.200s\n"+
					"one document, one verdict (2026-09-29 audit, round 92, F92-L1-5)", tc.name, remote.name, code, body)
			}
			// Whatever the surface's envelope is, the body must be ONE JSON value:
			// that is the contract a stream:false client parses.
			if !json.Valid([]byte(strings.TrimSpace(body))) {
				t.Errorf("%s (%s): the non-streaming client was handed a body that is not JSON (%d): %.240q\n"+
					"a failure frame may not be appended to a document that is already on the wire (F92-L1-5)",
					tc.name, remote.name, code, body)
			}
			if !strings.Contains(body, r92RelaySentence) {
				t.Errorf("%s (%s): the refusal does not state the cause the other arms state: %d %.200s",
					tc.name, remote.name, code, body)
			}
		}
	}
}

// And a turn that does finish is unchanged: 200, the remote's text, no failure
// sentence, in one document — the shape a conforming remote answers a
// `stream:false` relay with, and the shape that was already on the wire before
// this fix.
func TestABufferedRelayTurnThatFinishedIsStillOneDocument(t *testing.T) {
	const answered = "the whole answer"
	for _, tc := range r92BufferedSurfaces {
		code, body := r92RelaySurface(t, r92Relay(t, r92Doc(answered, true)), tc.path, tc.body)
		if code != http.StatusOK {
			t.Errorf("%s: a completed relayed turn answered %d: %.200s", tc.name, code, body)
			continue
		}
		if !json.Valid([]byte(strings.TrimSpace(body))) {
			t.Errorf("%s: a completed relayed turn is not JSON: %.240q", tc.name, body)
		}
		if !strings.Contains(body, answered) {
			t.Errorf("%s: the answer the remote sent is not in the document: %.240q", tc.name, body)
		}
		if strings.Contains(body, r92RelaySentence) {
			t.Errorf("%s: a completed turn states a failure: %.240q", tc.name, body)
		}
	}
}

// The release half. A remote that answers a `stream:false` relay with more than
// one chunk must not lose the chunks that came before the done chunk: holding
// them without releasing them would turn today's answer (every chunk, as ndjson
// — see F92-L1-6 below) into a document that silently drops text. These are the
// two native surfaces, because the framing for a buffered multi-chunk turn is
// what it has always been and the middleware surfaces refuse it (F92-L1-6,
// recorded, not this finding).
func TestABufferedRelayTurnKeepsEveryChunkItHeld(t *testing.T) {
	owed := []string{"the whole ", "answer"}
	for _, tc := range []struct{ name, path, body string }{
		{"chat", "/api/chat", `{"model":"r92-relay","stream":false,"messages":[{"role":"user","content":"hi"}]}`},
		{"generate", "/api/generate", `{"model":"r92-relay","stream":false,"prompt":"hi"}`},
	} {
		code, body := r92RelaySurface(t, r92Relay(t, r92Doc(owed[0], false), r92Doc(owed[1], true)), tc.path, tc.body)
		if code != http.StatusOK {
			t.Errorf("%s: a completed multi-chunk relayed turn answered %d: %.200s", tc.name, code, body)
			continue
		}
		for _, want := range owed {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the client is missing %q of the turn the remote sent: %.240q\n"+
					"a held chunk must be released when the turn ends (F92-L1-5)", tc.name, want, body)
			}
		}
		if strings.Contains(body, r92RelaySentence) {
			t.Errorf("%s: a completed turn states a failure: %.240q", tc.name, body)
		}
	}
}

// The streaming arm keeps the framing round 91 gave it: the sentence rides the
// ndjson stream, which is where a client that asked for a stream reads it. (Its
// own pin is round91_remote_relay_terminal_test.go; this is the same producer, so
// the two arms are held side by side.)
func TestTheStreamingArmStillStatesTheFailureInTheFraming(t *testing.T) {
	code, body := r92RelaySurface(t, r92Relay(t, r92Doc("half a ", false)), "/api/chat",
		`{"model":"r92-relay","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK || !strings.Contains(body, r92RelaySentence) {
		t.Errorf("the streaming arm answered %d without the sentence: %.200s", code, body)
	}
}
