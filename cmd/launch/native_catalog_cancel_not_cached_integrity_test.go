package launch

// native_catalog_cancel_not_cached_integrity_test.go — a caller that hung up
// poisoned the native catalog for the whole process (2026-09-26 audit, ninth
// round).
//
// The catalog fetch runs on the CALLER's context, deliberately: it is started
// from resolveNativeModelAlias inside a live /v1/messages handler, and a client
// that goes away must be able to stop it (see
// native_catalog_cancellable_integrity_test.go). But nativeModelCatalog then
// cached whatever came back for nativeModelCatalogFailureTTL — and a
// cancellation is not a statement about the upstream. One Claude Code request
// that hung up mid-crossover wrote "the catalog is broken" into the cache, and
// for the next 30 seconds every other session on this machine resolved its
// model aliases through that failure: the tier fell back to the bare alias and
// the real API answered "model: fable" not found, on a catalog that was up the
// whole time.
//
// The fix keeps the two apart: a fetch that ended because its CALLER's context
// was done publishes nothing and leaves the cache stale, so the next live
// caller refetches. A fetch that failed on its own — its own timeout, a 5xx, no
// credential — is still cached, which is what the failure TTL is for.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A cancelled caller's fetch must not be remembered as a catalog failure.
func TestACancelledCallersFetchIsNotCached(t *testing.T) {
	arrived, release := blockingCatalogUpstream(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := nativeModelCatalog(ctx)
		done <- err
	}()
	<-arrived
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("premise: the fetch a cancellable caller started returned %v, want context.Canceled", err)
	}
	release()

	// The next caller is a live request with a healthy upstream. It must get
	// the catalog, not this process's cancelled fetch.
	live, cancelLive := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelLive()
	entries, err := nativeModelCatalog(live)
	if err != nil {
		t.Fatalf("the catalog was asked again after a caller cancelled and answered %v — a client that hung up wrote a failure into a process-wide cache that every later session reads for nativeModelCatalogFailureTTL (%s), so tiers fall back to the bare alias and the real API answers \"model: fable\" not found", err, nativeModelCatalogFailureTTL)
	}
	if len(entries) == 0 {
		t.Fatalf("the refetch returned no entries and no error, which is not a catalog: %+v", entries)
	}
}

// The other half: a fetch that fails on its OWN account is still remembered.
// The failure TTL exists so the rest of the launch does not pay the timeout
// again, and "do not cache the caller's cancel" must not have removed that.
func TestARealCatalogTimeoutIsStillCached(t *testing.T) {
	resetNativeModelCatalog()
	t.Cleanup(resetNativeModelCatalog)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-r.Context().Done() // never answer: let the fetch's own timeout fire
	}))
	defer srv.Close()
	oldUpstream := nativeAnthropicModelsUpstream
	nativeAnthropicModelsUpstream = srv.URL
	t.Cleanup(func() { nativeAnthropicModelsUpstream = oldUpstream })

	oldTimeout := nativeModelCatalogTimeout
	nativeModelCatalogTimeout = 150 * time.Millisecond
	t.Cleanup(func() { nativeModelCatalogTimeout = oldTimeout })
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	if _, err := nativeModelCatalog(context.Background()); err == nil {
		t.Fatalf("premise: a fetch whose upstream never answers returned no error")
	}
	if _, err := nativeModelCatalog(context.Background()); err == nil {
		t.Errorf("the remembered failure was not returned on the second call")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("the catalog was asked %d times for one failed fetch — a failure this process's own timeout produced must be remembered for nativeModelCatalogFailureTTL (%s), or every tier lookup in the launch pays the timeout again",
			got, nativeModelCatalogFailureTTL)
	}
}
