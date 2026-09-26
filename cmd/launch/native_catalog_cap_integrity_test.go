package launch

// native_catalog_cap_integrity_test.go — the native model catalog was read into
// memory without a bound (2026-09-26 audit).
//
// fetchNativeModelCatalog parsed the response with a bare
// json.NewDecoder(resp.Body).Decode, so how much memory it took was decided by
// whoever answered the GET: a hostile or broken endpoint (or a MITM on the way
// to api.anthropic.com) chose this process's peak allocation, and the fetch
// runs on the proxy's own path — resolveNativeModelAlias is reached from a live
// /v1/messages handler. Every other buffered-and-parsed body in this package
// goes through httpbody.ReadCapped for exactly this reason.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnOversizedNativeCatalogIsRefusedNotBuffered(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	// A catalog answer far larger than the cap, whose payload is still exactly
	// what the alias lookup wants: nothing about its CONTENT is wrong, only its
	// size, so a decoder that reads it would resolve the alias happily.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[`))
		_, _ = w.Write([]byte(strings.Repeat(`{"id":"claude-pad","display_name":"pad"},`, 64)))
		_, _ = w.Write([]byte(`{"id":"claude-sonnet-5","display_name":"Claude Sonnet 5"}]}`))
	}))
	defer srv.Close()

	oldUpstream := nativeAnthropicModelsUpstream
	nativeAnthropicModelsUpstream = srv.URL
	defer func() { nativeAnthropicModelsUpstream = oldUpstream }()

	oldMax := nativeModelCatalogMaxBytes
	nativeModelCatalogMaxBytes = 256
	defer func() { nativeModelCatalogMaxBytes = oldMax }()

	resetNativeModelCatalog()
	entries, err := nativeModelCatalog(context.Background())
	if err == nil {
		t.Errorf("an oversized catalog came back as %d entries with no error — the whole body was buffered and parsed, so whoever answers that GET decides how much memory this process takes", len(entries))
	}
	if len(entries) != 0 {
		t.Errorf("%d entries were taken from a body larger than the cap — an over-limit answer is not a body this command was going to use", len(entries))
	}

	// What a caller actually sees: the alias must not come from an answer the
	// proxy refused to read.
	resetNativeModelCatalog()
	if got := resolveNativeModelAlias(context.Background(), "sonnet"); got != "sonnet" {
		t.Errorf("resolveNativeModelAlias(sonnet) = %q, want the bare tier — it believed a catalog read out of a body larger than the cap", got)
	}
}
