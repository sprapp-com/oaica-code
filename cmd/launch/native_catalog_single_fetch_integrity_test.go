package launch

// native_catalog_single_fetch_integrity_test.go — a launch resolved each
// native tier with its own /v1/models fetch, each with its own 10s timeout
// (2026-09-26 audit).
//
// resolveNativeModelAlias memoized per ALIAS, so the three tiers a launch
// needs (opus/sonnet/haiku — opusplan reads one, --sonnet-model another, the
// oversize slot a third) were three distinct cache keys and therefore three
// separate GETs of the same catalog. Reachable Anthropic: three round trips
// where one would do. Unreachable-but-blackholed (a firewall that drops rather
// than refuses, a stalled VPN): the launch sits for 10s, then 10s more, then
// 10s more — around half a minute of a CLI that has printed nothing — before
// the bare aliases go out and Claude Code rejects them. That is the worst kind
// of stall: nothing to read, nothing to wait for, and it lands before the
// child process has even started.
//
// One catalog, fetched once per launch, is the whole fix: the alias lookup is
// a match in data already in hand.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// stubNativeCatalogUpstream points the catalog GET at a server that answers
// with a real-shaped catalog and counts how many times it was asked. It also
// empties the alias cache so the test measures this launch's fetches, not a
// previous test's leftovers.
func stubNativeCatalogUpstream(t *testing.T) *int32 {
	t.Helper()
	resetNativeModelCatalog() // measure this launch's fetches, not a previous test's leftovers
	t.Cleanup(resetNativeModelCatalog)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"},
			{"id":"claude-sonnet-5","display_name":"Claude Sonnet 5"},
			{"id":"claude-haiku-4-5-20251001","display_name":"Claude Haiku 4.5"}
		]}`))
	}))
	t.Cleanup(srv.Close)

	oldUpstream := nativeAnthropicModelsUpstream
	nativeAnthropicModelsUpstream = srv.URL
	t.Cleanup(func() { nativeAnthropicModelsUpstream = oldUpstream })

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	return &hits
}

// The three native slots one launch sets up must cost ONE catalog fetch.
func TestEveryNativeTierOfOneLaunchCostsOneCatalogFetch(t *testing.T) {
	hits := stubNativeCatalogUpstream(t)

	for _, want := range []struct{ model, id string }{
		{"claude/opus", "claude-opus-5-5"},
		{"claude/sonnet", "claude-sonnet-5"},
		{"claude/haiku", "claude-haiku-4-5-20251001"},
	} {
		if got := claudeCodeModelAlias(want.model); got != want.id {
			t.Fatalf("claudeCodeModelAlias(%q) = %q, want %q", want.model, got, want.id)
		}
	}

	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("resolving one launch's three native tiers fetched the catalog %d times, want 1 — the fetch is the catalog, the tiers are lookups in it, and a blackholed network pays each fetch's full timeout before the child even starts", n)
	}
}

// A catalog that does not answer must not cost its timeout once per tier
// either: the first attempt's failure is what the rest of the launch sees.
func TestAHungCatalogCostsOneTimeoutNotOnePerTier(t *testing.T) {
	resetNativeModelCatalog()
	t.Cleanup(resetNativeModelCatalog)

	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked // accept, then never answer
	}))
	t.Cleanup(srv.Close)
	// Registered AFTER srv.Close, so it runs first (cleanups are LIFO): a
	// handler still parked on <-blocked would otherwise hold srv.Close open
	// forever.
	t.Cleanup(func() { close(blocked) })

	oldUpstream := nativeAnthropicModelsUpstream
	nativeAnthropicModelsUpstream = srv.URL
	t.Cleanup(func() { nativeAnthropicModelsUpstream = oldUpstream })

	oldTimeout := nativeModelCatalogTimeout
	nativeModelCatalogTimeout = 250 * time.Millisecond
	t.Cleanup(func() { nativeModelCatalogTimeout = oldTimeout })

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	start := time.Now()
	for _, tier := range []string{"opus", "sonnet", "haiku"} {
		if got := resolveNativeModelAlias(context.Background(), tier); got != tier {
			t.Fatalf("a hung catalog resolved %q to %q, want the bare tier unchanged", tier, got)
		}
	}
	elapsed := time.Since(start)

	// One timeout plus generous slack: the assertion sits midway between one
	// timeout (200ms) and three (600ms), so neither a loaded machine nor a
	// fixed-but-slow path can flip it by accident.
	if elapsed > 2*nativeModelCatalogTimeout {
		t.Errorf("three tiers against a catalog that never answers took %s (timeout %s) — each tier is paying the full timeout again", elapsed, nativeModelCatalogTimeout)
	}
}
