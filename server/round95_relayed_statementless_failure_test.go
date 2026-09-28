package server

// round95_relayed_statementless_failure_test.go — leg 1, F95-L1-1, F95-L1-3 and
// F95-L1-4 (2026-09-29 audit, round 95), end to end.
//
// A relayed refusal is the peer's refusal — body and status — and the runner
// lane is the reading it has to match. Three ways it did not:
//
// F95-L1-1: a frame that states a refusal STATUS and no message. The runner lane
// pushes exactly that frame when its own failure carried no text
// (`gin.H{"error": serr.ErrorMessage, "status": serr.StatusCode}`, routes.go:3254
// and :3382, whose source is `statusErrorMessage` in llm/llama_server.go), so it
// is a producer this repo owns. The api client read such a frame only for its
// `error` field, so it fell through to the relay's chunk writer: every streaming
// client on all four surfaces was handed a junk empty chunk and then a
// synthesised 502 "upstream stream ended before the response was complete" in
// place of the status the peer stated.
//
// F95-L1-4: given that frame, the relay's already-written arm fell back to
// `Error()` for a message the peer never wrote, so one input was answered
// `{"error":""}` on the runner lane and `{"error":"500 Internal Server Error"}`
// on the relay lane.
//
// F95-L1-3: a peer that refused the WHOLE turn as one document. The relay fixes
// its own Content-Type before it consults the peer (routes.go:2879-2883) and gin
// will not replace a header that is already set, so a native client was handed
// `application/x-ndjson` where the direct lane's byte-identical answer, and every
// translated surface on both lanes, is `application/json`.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
)

// r95StatementlessFrame is the frame the runner lane pushes for a failure whose
// message is empty.
const r95StatementlessFrame = `{"error":"","status":500}`

// r95Direct drives one chunk list through the REAL runner lane with a fake
// channel — the lane's own shape — on one surface and one arm, and reports the
// status, the Content-Type and the body the client read.
//
// The two native surfaces do not share a writer: /api/chat goes through
// writeChatResponse (which knows the stream flag) on both arms, while
// /api/generate's STREAMING arm goes through streamResponse, the same writer
// every streaming arm on this server uses. So the generate surface can only be
// driven here on its streaming arm; a buffered generate is written by the
// accumulation loop in GenerateHandler, which needs a real runner to reach.
func r95Direct(t *testing.T, stream bool, path string, mw gin.HandlerFunc, vals []any) (int, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if mw != nil {
		router.Use(mw)
	}
	router.POST("/x", func(c *gin.Context) {
		ch := make(chan any, len(vals))
		for _, v := range vals {
			ch <- v
		}
		close(ch)
		if path == "/api/generate" {
			streamResponse(c, ch)
			return
		}
		req := api.ChatRequest{Stream: &stream}
		writeChatResponse(c, req, ch)
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	body := `{"model":"test-model","stream":` + r95Lit(stream) + `,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	if path == "/api/generate" {
		body = `{"model":"test-model","stream":` + r95Lit(stream) + `,"prompt":"hi"}`
	}
	resp, err := http.Post(srv.URL+"/x", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(out)
}

// r95PeerRefusingWholeDocument answers every turn with one whole-document
// refusal, as a non-2xx response.
func r95PeerRefusingWholeDocument(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(peer.Close)
	return peer
}

// r95Relay drives one streaming request through the relay lane against the given
// peer, on one surface, and reports the status, the Content-Type and the body.
func r95Relay(t *testing.T, peer *httptest.Server, path string, mw gin.HandlerFunc, stream bool) (int, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	setTestHome(t, t.TempDir())
	u, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_REMOTES", u.Hostname())
	s := Server{}
	yes := true
	w := createRequest(t, s.CreateHandler, api.CreateRequest{
		Model: "r95-relay", RemoteHost: peer.URL, From: "test",
		Info: map[string]any{"capabilities": []string{"completion"}}, Stream: &yes,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("premise: creating the remote-host model answered %d: %s", w.Code, w.Body.String())
	}

	router := gin.New()
	if mw != nil {
		router.Use(mw)
	}
	router.POST(path, s.ChatHandler)
	local := httptest.NewServer(router)
	t.Cleanup(local.Close)

	body := `{"model":"r95-relay","stream":` + r95Lit(stream) + `,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(local.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(out)
}

// r95Lit is a JSON boolean literal for a stream flag.
func r95Lit(stream bool) string {
	if stream {
		return "true"
	}
	return "false"
}

// r95Lines is a body split into the frames a client reads, blank lines dropped.
func r95Lines(body string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// r95Statementless is the two-frame turn: one content chunk, then the
// statementless refusal the runner lane pushes for a failure with no text.
func r95Statementless() []any {
	return []any{
		api.ChatResponse{Message: api.Message{Role: "assistant", Content: "half "}},
		gin.H{"error": "", "status": http.StatusInternalServerError},
	}
}

// A failure that states no message is answered the STATUS the producer stated,
// on every surface and both arms — not a sentence the relay invented and not the
// synthesised 502 a stream that "ended early" earns. Before the fix the relayed
// streaming arms on all four surfaces carried a junk empty chunk and then
// `{"error":"upstream stream ended before the response was complete","status":502}`.
//
// The SENTENCE is not held to the runner lane's here: round 93 pinned the relay's
// reading of a message-less refusal on purpose (F95-L1-4, recorded — see
// writeRelayedStatusError), so the relay says the status text where the runner
// lane says nothing at all.
func TestARelayedStatementlessFailureStatesTheProducersStatus(t *testing.T) {
	const inventedForAStreamThatEndedEarly = "upstream stream ended before the response was complete"
	for _, tc := range r94Surfaces() {
		for _, stream := range []bool{false, true} {
			peer := r94StatingPeer(t, r95StatementlessFrame)
			gotCode, _, gotBody := r95Relay(t, peer, tc.path, tc.mw, stream)

			// The buffered arm has written nothing when the peer's failure is read,
			// so the stated status is the answer. The streaming arm's framing is
			// already on the wire by then — the same as the runner lane — so its
			// 200 stays and the status is stated in the frame below.
			want := http.StatusInternalServerError
			if stream {
				want = http.StatusOK
			}
			if gotCode != want {
				t.Errorf("%s, stream=%v: the relay answered %d, want %d — the peer stated 500, and no arm of any leg may read a stated status as a stream that ended early (2026-09-29 audit, round 95, F95-L1-1):\n%s",
					tc.name, stream, gotCode, want, gotBody)
			}
			if strings.Contains(gotBody, inventedForAStreamThatEndedEarly) {
				t.Errorf("%s, stream=%v: the relay named the failure from its own process where the peer stated 500 (2026-09-29 audit, round 95, F95-L1-1):\n%s", tc.name, stream, gotBody)
			}
			if stream && tc.mw == nil && !strings.Contains(gotBody, `"status":500`) {
				t.Errorf("%s, streamed: the client was handed %s, want the frame to carry the status the peer stated (2026-09-29 audit, round 95, F95-L1-1)", tc.name, gotBody)
			}
		}
	}
}

// The native surfaces' frames are held to the runner lane's own: the refusal is
// stated in ONE frame, and the streamed turn carries no extra one. Before the fix
// the streamed arm carried two — the relay's chunk writer had been handed the
// refusal as if it were a response chunk and wrote an empty one.
//
// The frame's MESSAGE differs and is not held equal: the peer wrote no message, so
// the runner lane states `{"error":""}` where the relay states
// `{"error":"Internal Server Error"}`. That is the divergence round 93 pinned on
// purpose and round 95 recorded rather than reverted (F95-L1-4 — see
// writeRelayedStatusError). What is pinned here is that the two differ in that one
// field and nowhere else, and that the relay states the peer's own status.
func TestARelayedStatementlessFrameIsTheRunnersFrame(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		// both arms on /api/chat; the generate surface's writer is only reachable
		// on the streaming arm (see r95Direct).
		arms []bool
	}{
		{"native chat", "/api/chat", []bool{false, true}},
		{"native generate", "/api/generate", []bool{true}},
	} {
		for _, stream := range tc.arms {
			wantCode, _, wantBody := r95Direct(t, stream, tc.path, nil, r95Statementless())
			gotCode, _, gotBody := r95Relay(t, r94StatingPeer(t, r95StatementlessFrame), tc.path, nil, stream)

			if gotCode != wantCode {
				t.Errorf("%s, stream=%v: the relay answered %d, want the runner lane's %d", tc.name, stream, gotCode, wantCode)
			}

			wantFrames, gotFrames := r95Lines(wantBody), r95Lines(gotBody)
			if len(gotFrames) != len(wantFrames) {
				t.Errorf("%s, stream=%v: the relay answered %d frame(s), want the runner lane's %d — a junk empty chunk is a frame no producer wrote (2026-09-29 audit, round 95, F95-L1-1):\n  relay  %s\n  direct %s",
					tc.name, stream, len(gotFrames), len(wantFrames), gotBody, wantBody)
				continue
			}
			if !stream {
				// One document, whose only field differs by the recorded F95-L1-4
				// divergence: the peer stated no message, so the runner lane states
				// none and the relay states the status text.
				if gotFrames[0] != `{"error":"Internal Server Error"}` {
					t.Errorf("%s, buffered: the relay answered %s, want the peer's status stated once and no sentence its own process invented (2026-09-29 audit, round 95, F95-L1-1):\n  relay  %s\n  direct %s", tc.name, gotBody, gotBody, wantBody)
				}
				if wantFrames[0] != `{"error":""}` {
					t.Errorf("%s, buffered: the runner lane answered %s, want the empty message this pin's divergence record names (2026-09-29 audit, round 95, F95-L1-4)", tc.name, wantBody)
				}
				continue
			}
			if len(wantFrames) != 2 {
				t.Errorf("%s, streamed: the runner lane answered %d frame(s), want a chunk and a refusal: %s", tc.name, len(wantFrames), wantBody)
				continue
			}
			// The turn's own chunk differs in the fields the relay re-stamps (its
			// model name); the frame that states the failure is the runner lane's,
			// but for the message the recorded divergence names.
			want, got := wantFrames[1], gotFrames[1]
			if got != `{"error":"Internal Server Error","status":500}` {
				t.Errorf("%s, streamed: the relay stated the failure as %s, want the peer's status 500 and no sentence its own process invented — `upstream stream ended before the response was complete` is what a chunk-handed refusal used to earn (2026-09-29 audit, round 95, F95-L1-1):\n  relay  %s\n  direct %s",
					tc.name, got, gotBody, wantBody)
			}
			if want != `{"error":"","status":500}` {
				t.Errorf("%s, streamed: the runner lane stated the failure as %s, want the empty message this pin's divergence record names (2026-09-29 audit, round 95, F95-L1-4)", tc.name, want)
			}
		}
	}
}

// A peer that refuses the whole turn as one document is answered the document's
// own media type on the native surface, as the runner lane answers it.
func TestARelayedWholeDocumentRefusalIsJSON(t *testing.T) {
	const peerBody = `{"error":"model not found"}`
	for _, tc := range r94Surfaces() {
		peer := r95PeerRefusingWholeDocument(t, http.StatusNotFound, peerBody)
		gotCode, gotCT, gotBody := r95Relay(t, peer, tc.path, tc.mw, true)
		if gotCode != http.StatusNotFound {
			t.Errorf("%s: the relayed client was answered %d, want the peer's 404: %s", tc.name, gotCode, gotBody)
		}
		if tc.mw == nil && !strings.HasPrefix(gotCT, "application/json") {
			t.Errorf("%s: the whole-document refusal reached the client as Content-Type %q, want application/json — the relay fixed its streaming header before it consulted the peer, so a client that branches on the media type read a refusal as a stream (2026-09-29 audit, round 95, F95-L1-3):\n%s", tc.name, gotCT, gotBody)
		}
	}

	// And the native surface's answer is the runner lane's answer, frame for
	// frame.
	wantCode, wantCT, wantBody := r95Direct(t, true, "/api/chat", nil, []any{
		gin.H{"error": "model not found", "status": http.StatusNotFound},
	})
	gotCode, gotCT, gotBody := r95Relay(t, r95PeerRefusingWholeDocument(t, http.StatusNotFound, peerBody), "/api/chat", nil, true)
	if gotCode != wantCode || gotBody != wantBody || gotCT != wantCT {
		t.Errorf("the relayed whole-document refusal answered %d %q %s, want the runner lane's %d %q %s",
			gotCode, gotCT, gotBody, wantCode, wantCT, wantBody)
	}
}
