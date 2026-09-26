package launch

// relay_upstream_oversize_integrity_test.go — a relayed upstream response
// larger than the cap was silently cut to 16 MiB (2026-09-26 audit).
//
// The relay reads the upstream body through a LimitReader, which is the right
// way to bound it, and then checked nothing: io.ReadAll over a reached limit
// returns the truncated bytes with NO error. The truncated body was relayed
// with a Content-Length computed from itself, so it looked like a complete,
// well-formed response to the client — the two things that followed (the
// redaction pass, the length fix-up) both treated it as the whole document.
//
// The callers are the GET /v1/models passthroughs, where the body IS the
// document: a client that gets a prefix of a model catalog parses it happily
// and concludes the models it cannot see do not exist. A cap has to be able to
// say no, or it is not a cap.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const relayBodyCap = 16 << 20

func truncateForError(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func oversizeBody(n int) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(strings.Repeat("a", n))),
	}
}

// Over the cap: the client gets a failure. A 200 whose body stops at the limit
// is the bug — the client cannot tell it apart from the real document.
func TestAnOversizeUpstreamResponseIsRefusedNotTruncated(t *testing.T) {
	rec := httptest.NewRecorder()
	relayUpstreamResponse(rec, oversizeBody(relayBodyCap+1))

	if rec.Code == http.StatusOK {
		t.Errorf("a %d-byte upstream body was relayed as a %d-byte HTTP %d — the client reads that as the whole document, so every model past the cut is silently gone",
			relayBodyCap+1, rec.Body.Len(), rec.Code)
	}
	if got := rec.Body.String(); len(got) >= relayBodyCap {
		t.Errorf("the refusal carries %d bytes of the upstream document with it", len(got))
	}
	if got := rec.Body.String(); !strings.Contains(got, "limit") {
		t.Errorf("the client is not told why the relay failed: %q", truncateForError(got))
	}
}

// At the cap: relayed whole. The bound must not become an off-by-one that
// refuses the largest response the proxy was built to carry.
func TestAnUpstreamResponseAtTheCapIsRelayedWhole(t *testing.T) {
	rec := httptest.NewRecorder()
	relayUpstreamResponse(rec, oversizeBody(relayBodyCap))

	if rec.Code != http.StatusOK {
		t.Fatalf("a body of exactly the cap was refused: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != relayBodyCap {
		t.Errorf("body is %d bytes for an upstream response of %d", rec.Body.Len(), relayBodyCap)
	}
	if got := rec.Header().Get("Content-Length"); got != "16777216" {
		t.Errorf("Content-Length %q does not match the %d bytes written", got, rec.Body.Len())
	}
}

// The same thing one level out, where the client is the launched tool: an
// oversize catalog from a plan row's own base is an error response, not a
// short catalog.
func TestAnOversizeCatalogIsNotRelayedAsAShortCatalog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[`+strings.Repeat(`{"id":"m"},`, 2_000_000)+`{"id":"last"}]}`)
	}))
	t.Cleanup(upstream.Close)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	anthropicModelsPassthrough(rec, req, upstream.URL+"/v1/models", "x-api-key", "sk-test")

	if rec.Code == http.StatusOK {
		t.Errorf("an oversize catalog was relayed as HTTP %d with %d bytes — the tail of the list, including the last entry, is gone and the client cannot tell",
			rec.Code, rec.Body.Len())
	}
}
