package launch

// router_error_body_redaction_integrity_test.go — the router's error body was
// printed verbatim, unbounded, and unredacted (2026-09-26 audit, tenth round).
//
// oaicaRouterError.Error() prints the body straight into the picker's warning
// line and doctor output. Two things follow. The router — like every vendor
// gateway in redactUpstreamDiagnosis's doc — names the key it refused in prose
// ("invalid api key sk-live-…"), which no shape-based rule can recognise
// because the process that SENT the key is the only one that knows the string.
// And the body is read up to httpbody.DiagnosticMax (64 MiB), so one non-JSON
// answer to a failed /v1/models put megabytes into the message a user pastes
// into a support ticket.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestARouterErrorBodyIsRedactedAndBounded(t *testing.T) {
	const key = "sk-live-LAUNCHSENT-9876543210"
	t.Setenv("OAICA_API_KEY", key)

	// The router names the key it refused, then pads.
	body := `{"error":{"message":"invalid api key ` + key + `"}}` + strings.Repeat("x", 3<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+key {
			t.Errorf("the request carried %q, want the configured key", got)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	_, _, err := oaicaFetchCloudModelEntriesLiveUncached(srv.URL, "")
	if err == nil {
		t.Fatalf("premise: a 401 answered without error")
	}
	var re *oaicaRouterError
	if !errors.As(err, &re) {
		t.Fatalf("premise: the error is %T, want *oaicaRouterError (its Error() is what prints the body)", err)
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("the router error prints the key the request carried: %q — the router names the credential it refused, and this is the one process that knows the string", err)
	}
	if len(err.Error()) > 1000 {
		t.Errorf("a %d-byte body produced a %d-byte message: %q", len(body), len(err.Error()), err.Error())
	}
	// The diagnosis survives: which host, what status.
	if !strings.Contains(err.Error(), srv.URL) || !strings.Contains(err.Error(), "401") {
		t.Errorf("redaction removed the diagnosis too: %q", err)
	}
}

// A body that is plain prose, with no credential in it, is still bounded — the
// bound cannot depend on the router cooperating with our shapes.
func TestAPlainProseRouterBodyIsStillBounded(t *testing.T) {
	t.Setenv("OAICA_API_KEY", "sk-live-LAUNCHSENT-9876543210")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("upstream unavailable. ", 200000)))
	}))
	defer srv.Close()

	_, _, err := oaicaFetchCloudModelEntriesLiveUncached(srv.URL, "")
	if err == nil {
		t.Fatalf("premise: a 502 answered without error")
	}
	if len(err.Error()) > 1000 {
		t.Errorf("a prose body of %d bytes produced a %d-byte error", 200000*len("upstream unavailable. "), len(err.Error()))
	}
}

// A short, clean body passes through unchanged — the bound and the redaction
// must not mangle a diagnosis that has nothing to hide.
func TestACleanRouterBodyIsUnchanged(t *testing.T) {
	t.Setenv("OAICA_API_KEY", "sk-live-LAUNCHSENT-9876543210")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded for this key"}}`))
	}))
	defer srv.Close()

	_, _, err := oaicaFetchCloudModelEntriesLiveUncached(srv.URL, "")
	if err == nil {
		t.Fatalf("premise: a 429 answered without error")
	}
	if !strings.Contains(err.Error(), "rate limit exceeded for this key") {
		t.Errorf("a clean router message was rewritten: %q", err)
	}
}
