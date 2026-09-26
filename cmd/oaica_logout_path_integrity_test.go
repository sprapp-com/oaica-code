package cmd

// oaica_logout_path_integrity_test.go — `oaica auth logout <name>` pasted the
// provider name straight into a URL path (2026-09-26 audit).
//
// The name comes from the command line. Interpolated raw, a name containing
// "/" or "?" is not one path segment any more: it addresses a different
// resource on the operator's router, up to and including walking out of the
// providers collection, and the DELETE it sends is a real one. Everything
// oaica sends to a router it does not own has to be the request the user
// asked for.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestLoggingOutEscapesTheProviderNameIntoThePath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	t.Setenv("OAICA_HOST", srv.URL)
	t.Setenv("OAICA_ADMIN_KEY", "admin-test")

	name := "weird/../name?x=1"
	if err := oaicaAuthLogout(name); err != nil {
		t.Fatalf("oaicaAuthLogout: %v", err)
	}

	// The router has to receive the name as ONE path segment: the escaped path
	// is the prefix plus the name escaped, so no separator the name carries can
	// address anything else.
	const prefix = "/v1/admin/providers/"
	if want := prefix + url.PathEscape(name); gotPath != want {
		t.Errorf("logout of %q sent %q, want %q — a name pasted into a path is a different request, and this one is a DELETE", name, gotPath, want)
	}
}
