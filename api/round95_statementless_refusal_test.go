package api

// round95_statementless_refusal_test.go — leg 1, F95-L1-1's client half
// (2026-09-29 audit, round 95).
//
// A mid-stream error frame that states a refusal STATUS and no message is one the
// runner lane really writes, and the client did not read it as a refusal at all.
// `server/routes.go:3254` and `:3382` push
// `gin.H{"error": serr.ErrorMessage, "status": serr.StatusCode}` — whose source is
// `statusErrorMessage` in `llm/llama_server.go` — so a failure with an empty
// message reaches the wire as `{"error":"","status":500}`. `stream` consulted
// that frame only for its `error` field, found it empty, and handed the frame to
// the caller's chunk writer as if it were a response chunk: every surface on the
// relay lane was then answered a junk empty chunk and a synthesised 502 in place
// of the status the peer stated.
//
// The reading pinned here is the one round 94 installed for a frame that DOES
// carry a message: the `status` key is a refusal only when it is a JSON number in
// the refusal range, and a frame that names a refusal is a refusal whether or not
// a message came with it. That is already this function's doctrine at the
// response level — a non-2xx response with no body line at all is a StatusError
// (round 93) — and the frame level now says the same thing.
//
// The negative halves stay pinned with it: a progress line's string status is not
// a refusal, and both a progress line and an ordinary content frame still reach
// the caller's chunk writer.

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// r95StatedLines serves the given raw lines as an ndjson stream, exactly as
// written, and returns the client's stream error along with every line that
// reached the chunk writer.
func r95StatedLines(t *testing.T, lines ...string) (error, []string) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected http.Flusher")
		}
		for _, l := range lines {
			fmt.Fprintln(w, l)
			flusher.Flush()
		}
	}))
	t.Cleanup(ts.Close)

	var wrote []string
	client := NewClient(&url.URL{Scheme: "http", Host: ts.Listener.Addr().String()}, http.DefaultClient)
	err := client.stream(t.Context(), http.MethodPost, "/api/chat", nil, func(bts []byte) error {
		wrote = append(wrote, string(bts))
		return nil
	})
	return err, wrote
}

// A refusing frame that states no message is still a refusal. Before the fix this
// frame was written to the caller as a response chunk, so one upstream cause
// became a junk empty chunk and a 502 sentence about a stream that "ended early".
func TestAStatementlessRefusalFrameIsStillARefusal(t *testing.T) {
	err, wrote := r95StatedLines(t, `{"error":"","status":500}`)
	var statusErr StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("the frame was answered %v (%T) and wrote %d chunk(s), want a StatusError and nothing written: an empty `error` field is not an absent cause — the runner lane pushes exactly this frame for a failure whose text is empty, and read as a chunk it becomes a junk empty chunk plus a synthesised 502 (2026-09-29 audit, round 95, F95-L1-1): %v",
			err, err, len(wrote), wrote)
	}
	if statusErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("the stated status was read as %d, want 500", statusErr.StatusCode)
	}
	if statusErr.ErrorMessage != "" {
		t.Errorf("the stated message is %q, want the empty message the frame's own field holds — a reading that invents one states a cause the peer never stated", statusErr.ErrorMessage)
	}
	if len(wrote) != 0 {
		t.Errorf("the frame reached the chunk writer as %v, want nothing written (2026-09-29 audit, round 95, F95-L1-1)", wrote)
	}
}

// A frame that states a refusal and no message FIELD at all is read the same way,
// because the status is a cause on its own: that is what this function already
// says one level up, where a non-2xx response with no body line is a StatusError
// (round 93). No producer in this repo writes such a frame yet; the reading is
// pinned so the two levels cannot drift apart.
func TestARefusalStatusWithNoMessageFieldIsARefusal(t *testing.T) {
	for _, tc := range []struct {
		line string
		want int
	}{
		{`{"status":429}`, http.StatusTooManyRequests},
		{`{"status":500,"total":100}`, http.StatusInternalServerError},
	} {
		t.Run(tc.line, func(t *testing.T) {
			err, wrote := r95StatedLines(t, tc.line)
			var statusErr StatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("the line %s was answered %v (%T) and wrote %d chunk(s), want a StatusError: a status in the refusal range is a cause whether or not a message came with it (2026-09-29 audit, round 95, F95-L1-1): %v",
					tc.line, err, err, len(wrote), wrote)
			}
			if statusErr.StatusCode != tc.want {
				t.Errorf("the line %s was typed with status %d, want the %d it stated", tc.line, statusErr.StatusCode, tc.want)
			}
			if len(wrote) != 0 {
				t.Errorf("the line %s reached the chunk writer as %v, want nothing written", tc.line, wrote)
			}
		})
	}
}

// A frame that carries both keeps the message it carried: the new reading must
// not shadow the one round 94 installed for a frame that states a cause.
func TestARefusalFrameThatStatesBothKeepsItsMessage(t *testing.T) {
	err, wrote := r95StatedLines(t, `{"error":"the runner died","status":404}`)
	var statusErr StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("the frame was answered %v (%T), want a StatusError", err, err)
	}
	if statusErr.StatusCode != http.StatusNotFound || statusErr.ErrorMessage != "the runner died" {
		t.Errorf("the frame was typed as %+v, want the status it stated and the message it carried", statusErr)
	}
	if len(wrote) != 0 {
		t.Errorf("the frame reached the chunk writer as %v, want nothing written", wrote)
	}
}

// The negative halves: neither a progress line nor an ordinary content frame is a
// refusal, and both still reach the chunk writer. The new reading runs on the
// same path these do, so it is pinned as not having swallowed them.
func TestTheRefusalReadingSwallowsNoOtherFrame(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"a pull's success line", `{"status":"success"}`},
		{"a pull's progress line", `{"status":"pulling manifest"}`},
		{"a content chunk", `{"model":"m","created_at":"2026-01-01T00:00:00Z","message":{"role":"assistant","content":"hi"},"done":false}`},
		{"a final chat frame", `{"model":"m","created_at":"2026-01-01T00:00:00Z","message":{"role":"assistant","content":""},"done":true}`},
		{"a generate chunk", `{"model":"m","response":"hi","done":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err, wrote := r95StatedLines(t, tc.line)
			if err != nil {
				t.Errorf("the frame %s was answered %v, want no error", tc.line, err)
			}
			if len(wrote) != 1 || wrote[0] != tc.line {
				t.Errorf("the frame %s reached the chunk writer as %v, want the line itself exactly once — a frame no arm may read as a refusal still has to reach the client it was written for", tc.line, wrote)
			}
		})
	}
}
