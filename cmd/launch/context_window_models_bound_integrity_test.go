package launch

// context_window_models_bound_integrity_test.go — the context-window probe
// decoded whatever the upstream's /models answered, with no bound at all. The
// body is written by a host the user does not control (a remote, a mirror, a
// router), and this probe runs on EVERY launch before the first token: an
// endless or hostile model list — a stream that never ends, or one that
// reports millions of entries — allocates as much memory as it can, on the
// startup path, with the decoder happily following it. Every other
// upstream-facing read in this proxy is bounded; this one was not
// (2026-09-26 audit).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// modelsJSONUpstream serves a fixed body on any path.
func modelsJSONUpstream(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAnOversizedModelsDocumentDoesNotResolveAContextWindow(t *testing.T) {
	// A syntactically valid document well past the bound: the decoder must stop
	// at the bound and fail closed (0), not read the whole thing.
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i := 0; i < 400_000; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"model-` + strconv.Itoa(i) + `","context_length":262144}`)
	}
	b.WriteString(`]}`)
	if b.Len() < 8<<20 {
		t.Fatalf("test setup: body is %d bytes, want it past the probe's bound", b.Len())
	}
	srv := modelsJSONUpstream(t, []byte(b.String()))

	got := defaultRemoteContextWindow(proxyRoute{BaseURL: srv.URL, UpstreamModel: "model-1"})
	if got != 0 {
		t.Errorf("defaultRemoteContextWindow = %d for a %d-byte model list, want 0 — the probe reads an upstream-authored document with no size bound, on the startup path", got, b.Len())
	}
}

// The control: an ordinary list — larger than any hand-written /models but
// well inside the bound — still resolves, so the cap does not disable the
// probe for real hosts.
func TestAnOrdinaryModelsDocumentStillResolves(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i := 0; i < 500; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"model-` + strconv.Itoa(i) + `","context_length":131072}`)
	}
	b.WriteString(`,{"id":"kat-awq","context_length":262144}]}`)
	srv := modelsJSONUpstream(t, []byte(b.String()))

	if got := defaultRemoteContextWindow(proxyRoute{BaseURL: srv.URL, UpstreamModel: "kat-awq"}); got != 262144 {
		t.Errorf("defaultRemoteContextWindow = %d, want 262144", got)
	}
	// And max_model_len-only hosts (bare vLLM) keep working.
	body, err := json.Marshal(map[string]any{"data": []map[string]any{{"id": "kat-awq", "max_model_len": 32768}}})
	if err != nil {
		t.Fatal(err)
	}
	srv2 := modelsJSONUpstream(t, body)
	if got := defaultRemoteContextWindow(proxyRoute{BaseURL: srv2.URL, UpstreamModel: "kat-awq"}); got != 32768 {
		t.Errorf("defaultRemoteContextWindow = %d, want 32768 from max_model_len", got)
	}
}
