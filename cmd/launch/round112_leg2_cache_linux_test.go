package launch

// round112_leg2_cache_linux_test.go — F112-L2-2, the write half. Linux-only on purpose:
// RLIMIT_FSIZE is how a full disk looks to a writer that fails part-way.
//
// CatalogSync wrote its cache with os.WriteFile, which truncates the live path first, so
// a write that failed part-way (a full disk, Ctrl-C, SIGKILL during `oaica model catalog
// sync`) destroyed the last good catalog: the sync returned an error and the picker had no
// catalog. ProviderSync, the sibling, writes atomically. The body and its ETag are now
// written through fileutil.WriteFileAtomic.

import (
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
)

func TestMine112AFailedCatalogWriteKeepsTheLastGoodCatalog(t *testing.T) {
	signal.Ignore(syscall.SIGXFSZ)
	t.Cleanup(func() { signal.Reset(syscall.SIGXFSZ) })
	setLaunchTestHome(t, t.TempDir())
	if _, err := CatalogSync(writeCatalogFile(t, modelsDevFixture)); err != nil {
		t.Fatal(err)
	}
	path, _ := catalogCachePath()
	good, _ := os.ReadFile(path)
	big := strings.Replace(modelsDevFixture, `"id": "groq",`, `"id": "groq", "pad": "`+strings.Repeat("x", 300000)+`",`, 1)
	src := writeCatalogFile(t, big)
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 100000, Max: old.Max}); err != nil {
		t.Fatal(err)
	}
	_, err := CatalogSync(src)
	_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	if err == nil {
		t.Fatalf("premise: the sync did not fail under the size limit")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(good) {
		t.Errorf("a failed sync left %d bytes where the last good catalog had %d — the live cache must survive a failed write (2026-09-29 audit, round 112, F112-L2-2)", len(after), len(good))
	}
	if _, ok := loadModelsDevCatalog(); !ok {
		t.Errorf("the picker's catalog is gone after a failed sync (2026-09-29 audit, round 112, F112-L2-2)")
	}
}
