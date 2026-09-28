package api

// round94_stated_status_collision_test.go — leg 1, F94-L1-1's second face
// (2026-09-29 audit, round 94).
//
// The `status` key is not this client's to type. A PROGRESS line states one too,
// as a string — `{"status":"success"}` ends a pull, `{"status":"pulling
// manifest"}` starts one — and the refusal reading added for F94-L1-1 first took
// the key as an int. Every progress line then failed to decode, and the stream
// returned the line itself as the error: measured end to end, `oaica launch`
// answered `failed to pull missing-model: {"status":"success"}` and no local
// model could be pulled at all.
//
// The reading a frame has to earn is therefore narrow: the error frame's status
// is a JSON NUMBER in the refusal range, and a frame that names a string, a
// success code or nothing at all is not a refusal. Both halves of the reading
// are pinned here, and the progress line is pinned as the regression it was.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// r94StatedLines serves the given raw lines as an ndjson stream, exactly as
// written, and returns the client's stream error.
func r94StatedLines(t *testing.T, lines ...string) error {
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

	client := NewClient(&url.URL{Scheme: "http", Host: ts.Listener.Addr().String()}, http.DefaultClient)
	return client.stream(t.Context(), http.MethodPost, "/api/pull", nil, func([]byte) error { return nil })
}

// A progress line's string status is the line's own bookkeeping, not a refusal.
// Before the fix every one of these failed to decode and the whole line was
// returned as the error.
func TestAProgressLinesStatusIsNotARefusal(t *testing.T) {
	for _, line := range []string{
		`{"status":"success"}`,
		`{"status":"pulling manifest"}`,
		`{"status":"verifying sha256 digest","digest":"sha256:abc"}`,
		`{"status":"downloading","total":100,"completed":50}`,
		`{"status":"success","total":100,"completed":100}`,
	} {
		t.Run(line, func(t *testing.T) {
			if err := r94StatedLines(t, line); err != nil {
				t.Errorf("the line %s was answered %v, want no error: `status` carries a progress line's own string status, so reading it as a number rejects every line of a pull and the caller is told the line itself was the failure (2026-09-29 audit, round 94, F94-L1-1)", line, err)
			}
		})
	}
}

// A mid-stream refusal states its status as a NUMBER, and the client types it.
// This is the F94-L1-1 half: the relay lane reads this error back and needs the
// stated status to reach the client, rather than a bare errors.New.
func TestAMidStreamRefusalKeepsItsStatedStatus(t *testing.T) {
	err := r94StatedLines(t, `{"error":"the runner died","status":404}`)
	var statusErr StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("a refusing frame was answered %v (%T), want a StatusError: the relay lane reads this error back to answer its own client, and a bare error carries no status for it to state (2026-09-29 audit, round 94, F94-L1-1)", err, err)
	}
	if statusErr.StatusCode != http.StatusNotFound {
		t.Errorf("the stated status was read as %d, want 404: the frame names the status its own clients are answered", statusErr.StatusCode)
	}
	if statusErr.ErrorMessage != "the runner died" {
		t.Errorf("the stated message is %q, want the frame's own", statusErr.ErrorMessage)
	}
}

// A frame that states a status which is not a refusal states no refusal. The
// status is a field about the failure only when it names one; a 200, a 0 or a
// bare message keeps the plain error the peer's own text is.
func TestAFrameWhoseStatusIsNotARefusalStaysAPlainError(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"no status at all", `{"error":"something went wrong"}`},
		{"a success status", `{"error":"something went wrong","status":200}`},
		{"a zero status", `{"error":"something went wrong","status":0}`},
		{"a status above 599", `{"error":"something went wrong","status":700}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := r94StatedLines(t, tc.line)
			if err == nil {
				t.Fatalf("the line %s was answered no error at all", tc.line)
			}
			var statusErr StatusError
			if errors.As(err, &statusErr) {
				t.Errorf("the line %s was typed as %+v: a status that names no refusal is not a cause, and typing one from it states a failure the peer never stated (2026-09-29 audit, round 94, F94-L1-1)", tc.line, statusErr)
			}
			if !strings.Contains(err.Error(), "something went wrong") {
				t.Errorf("the line %s was answered %v, want the peer's own message", tc.line, err)
			}
		})
	}
}

// The reading itself, over the shapes the wire can hand it — including the ones
// json.Unmarshal would refuse if the field were still a string.
func TestStatedRefusalStatusReadsOnlyARefusalNumber(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
		ok   bool
	}{
		{`404`, 404, true},
		{`400`, 400, true},
		{`599`, 599, true},
		{`500`, 500, true},
		{`"success"`, 0, false},
		{`"404"`, 0, false},
		{`200`, 0, false},
		{`399`, 0, false},
		{`600`, 0, false},
		{`0`, 0, false},
		{`-1`, 0, false},
		{`1.5`, 0, false},
		{`null`, 0, false},
		{`{}`, 0, false},
		{`[]`, 0, false},
		{``, 0, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, ok := statedRefusalStatus(json.RawMessage(tc.raw))
			if got != tc.want || ok != tc.ok {
				t.Errorf("statedRefusalStatus(%s) = (%d, %v), want (%d, %v)", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}
}
