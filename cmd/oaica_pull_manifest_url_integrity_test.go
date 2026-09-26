package cmd

// oaica_pull_manifest_url_integrity_test.go — the model name went into the
// manifest URL verbatim, and the name's guard ran after the request
// (2026-09-26 audit).
//
// oaicaFetchManifest built its URL by concatenation:
//
//	oaicaHost() + "/v1/manifest/" + model
//
// so the name, which is a command-line argument, decided the request target
// rather than naming a model. `oaica pull 'x?foo=bar'` sent the router a query
// it never had (everything after "?" stops being a path), `x#frag` cut the
// request short at the fragment, and a name carrying a separator addressed a
// different endpoint on the same router.
//
// The guard for exactly this already existed — validateModelName refuses
// separators, "." and ".." — but the only callers in the pull path reached it
// through oaicaModelPath, which ran AFTER the manifest was fetched, so an
// invalid name was still put on the wire first. The check belongs at the
// network boundary: the request that carries the distribution licence as a
// bearer is the last place to discover the argument was never a model name.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// manifestURLRecorder answers every manifest request with a manifest and keeps
// the exact request target the client asked for.
func manifestURLRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.RequestURI+" | path="+r.URL.Path+" | query="+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":      "kat-awq",
			"size_bytes": 1,
			"pull_url":   "/v1/pull/kat-awq",
			"source":     "file",
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OAICA_HOST", srv.URL)
	return srv, func() []string { return append([]string(nil), seen...) }
}

// A name that is not a name is refused before anything is asked of the router.
func TestAnInvalidModelNameNeverReachesTheRouter(t *testing.T) {
	invPullSetup(t)
	_, requests := manifestURLRecorder(t)

	for _, name := range []string{"../../etc/passwd", "a/../../b", "sub/dir", `win\path`, "..", ".hidden", ""} {
		if _, err := oaicaFetchManifest(name); err == nil {
			t.Errorf("oaicaFetchManifest(%q) asked the router for a manifest — a model name is user input and this one is a path", name)
		}
	}

	if got := requests(); len(got) != 0 {
		t.Errorf("the router was asked for %d manifests before the names were checked:\n%s", len(got), strings.Join(got, "\n"))
	}
}

// The name is one path segment: no name can end the path and start a query, or
// cut the request off at a fragment.
func TestTheModelNameCannotChangeTheRequestTarget(t *testing.T) {
	invPullSetup(t)
	_, requests := manifestURLRecorder(t)

	for _, name := range []string{"kat?foo=bar", "kat#frag", "kat awq", "kat&x=1"} {
		if _, err := oaicaFetchManifest(name); err != nil {
			t.Fatalf("oaicaFetchManifest(%q): %v", name, err)
		}
	}

	got := requests()
	if len(got) != 4 {
		t.Fatalf("expected one manifest request per name, got %d: %v", len(got), got)
	}
	for i, name := range []string{"kat?foo=bar", "kat#frag", "kat awq", "kat&x=1"} {
		if !strings.Contains(got[i], "path=/v1/manifest/"+name) {
			t.Errorf("model %q was requested as %q — the name did not survive as one path segment", name, got[i])
		}
		if !strings.Contains(got[i], "query=") || !strings.HasSuffix(got[i], "query=") {
			t.Errorf("model %q put something in the router's query string: %q", name, got[i])
		}
	}
}

// And an ordinary name is still an ordinary path, unchanged.
func TestAnOrdinaryModelNameIsRequestedUnchanged(t *testing.T) {
	invPullSetup(t)
	_, requests := manifestURLRecorder(t)

	if _, err := oaicaFetchManifest("kat-awq"); err != nil {
		t.Fatalf("oaicaFetchManifest: %v", err)
	}
	got := requests()
	if len(got) != 1 {
		t.Fatalf("expected one request, got %v", got)
	}
	if !strings.Contains(got[0], "path=/v1/manifest/kat-awq") {
		t.Errorf("an ordinary model name was rewritten: %q", got[0])
	}
}
