package launch

// native_catalog_cancellable_integrity_test.go — the native catalog fetch held
// a process-global mutex across the network call and ran on
// context.Background(), so neither a caller's deadline nor its cancellation
// could reach it (2026-09-26 audit).
//
// nativeModelCatalog took sync.Mutex, checked the TTL, and called the fetch
// WITH THE LOCK STILL HELD, so "one catalog under concurrency" was true by
// making every other caller wait on a mutex. A mutex is not context-aware: a
// second caller arriving during a stalled fetch sat in Lock() until the fetch's
// own 10s HTTP timeout expired, and its own deadline was never consulted.
//
// The path that matters is a live request. resolveNativeModelAlias runs on the
// oversize-to-native crossover, inside the /v1/messages handler
// (anthropic_openai_proxy.go:1445), and a client that hangs up — or a proxy
// with a shorter timeout than ours — leaves that goroutine parked. Claude Code
// issues several requests at once, so one stalled catalog GET also queued every
// other request behind it.
//
// The fix is single-flight rather than locking: the fetch runs outside the
// mutex, on the CALLER's context, and callers that arrive while it runs wait on
// its completion channel or on their own context, whichever ends first.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// blockingCatalogUpstream accepts the catalog GET and never answers until the
// test releases it, reporting when the request has arrived. Fetches are
// reported once per server, not once per request: the assertion is "was the
// catalog asked at all".
func blockingCatalogUpstream(t *testing.T) (arrived <-chan struct{}, release func()) {
	t.Helper()
	resetNativeModelCatalog()
	t.Cleanup(resetNativeModelCatalog)

	var once, releaseOnce sync.Once
	got := make(chan struct{})
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(got) })
		<-block
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5","display_name":"Claude Sonnet 5"}]}`))
	}))
	t.Cleanup(srv.Close)
	// Registered AFTER srv.Close, so it runs first (cleanups are LIFO): a
	// handler still parked on <-block would otherwise hold srv.Close open
	// forever.
	t.Cleanup(func() { releaseOnce.Do(func() { close(block) }) })

	oldUpstream := nativeAnthropicModelsUpstream
	nativeAnthropicModelsUpstream = srv.URL
	t.Cleanup(func() { nativeAnthropicModelsUpstream = oldUpstream })

	// Long enough that a caller waiting on the OLD shape (the fetch's own lock)
	// is unambiguously past every deadline this file asserts, short enough that
	// a regression finishes the test instead of hanging it.
	oldTimeout := nativeModelCatalogTimeout
	nativeModelCatalogTimeout = 2 * time.Second
	t.Cleanup(func() { nativeModelCatalogTimeout = oldTimeout })

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	return got, func() { releaseOnce.Do(func() { close(block) }) }
}

// startBackgroundFetch begins a catalog fetch that will block in the upstream
// and returns a channel carrying nothing but its error.
func startBackgroundFetch(t *testing.T) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := nativeModelCatalog(context.Background())
		done <- err
	}()
	return done
}

// The mechanism: a caller that arrives during an in-flight fetch waits on its
// OWN deadline, not on the fetch. If it comes back with the fetch's error after
// the fetch's full timeout, it was blocked on the mutex.
func TestAWaitingCallerIsNotHeldByTheFetch(t *testing.T) {
	arrived, release := blockingCatalogUpstream(t)
	first := startBackgroundFetch(t)
	<-arrived

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := nativeModelCatalog(ctx)
	elapsed := time.Since(start)

	if elapsed >= nativeModelCatalogTimeout {
		t.Fatalf("a caller with a 100ms deadline waited %s while a catalog fetch was running — it is waiting on the fetch's mutex, which no context can end, so every request behind a stalled catalog GET pays that GET's full timeout", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a caller that gave up returned %v, want its own context's DeadlineExceeded", err)
	}

	release()
	if err := <-first; err != nil {
		t.Fatalf("the caller that started the fetch failed once the catalog answered: %v", err)
	}
}

// The outcome, at the layer the request actually reaches: resolving an alias
// with a context that is already done must not wait for a catalog fetch, and
// must fall back to the bare tier rather than hanging or guessing.
func TestACancelledRequestDoesNotWaitForTheCatalog(t *testing.T) {
	arrived, release := blockingCatalogUpstream(t)
	first := startBackgroundFetch(t)
	<-arrived

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	got := resolveNativeModelAlias(ctx, "sonnet")
	elapsed := time.Since(start)

	if elapsed >= nativeModelCatalogTimeout {
		t.Fatalf("resolving a tier for a request that was already cancelled took %s — a client that hung up leaves its goroutine parked for the catalog fetch's whole timeout", elapsed)
	}
	if got != "sonnet" {
		t.Errorf("a cancelled resolution returned %q, want the bare tier %q unchanged — a cancelled lookup must not resolve to some model the client never asked for", got, "sonnet")
	}

	release()
	if err := <-first; err != nil {
		t.Fatalf("the caller that started the fetch failed once the catalog answered: %v", err)
	}
}

// And the fetch a caller does start is its own, on its own context: a
// cancellation must stop it before a request goes out, not after its timeout.
func TestARequestThatStartsTheFetchCancelsIt(t *testing.T) {
	arrived, release := blockingCatalogUpstream(t)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := nativeModelCatalog(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a fetch started on an already-cancelled context returned %v, want context.Canceled — the fetch is still running on a background context no caller can reach", err)
	}
	if elapsed := time.Since(start); elapsed >= nativeModelCatalogTimeout {
		t.Errorf("a fetch on an already-cancelled context took %s", elapsed)
	}
	select {
	case <-arrived:
		t.Errorf("a cancelled caller still asked the catalog upstream")
	case <-time.After(200 * time.Millisecond):
	}
}
