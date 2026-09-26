package launch

// cache_atomic_write_integrity_test.go — the cache files are written in place
// (2026-09-26 audit, fourth round).
//
// os.WriteFile opens the destination O_TRUNC and then writes: for as long as
// the body is being copied, the file on disk is a PREFIX of the document, and
// any other process reading it gets that prefix — empty for the first chunk.
// The stores under ~/.oaica already go through fileutil.WriteFileAtomic; the
// six caches did not, and a cache is read by a different process than the one
// that wrote it (`oaica provider sync` in one shell, `oaica provider list` or
// a launch in another).
//
// What the reader does with the prefix is the harm:
//
//   - providerCatalog() parses the synced copy and, on a parse error, simply
//     loses it — the run silently drops every provider and model row the sync
//     added, with no error reported;
//   - the sync path itself falls back to the cache when the network fails or
//     answers 304, and hands the prefix to parseProviderCatalogFileChecked,
//     which errors: "the cached provider catalog at <path> is not readable as
//     one — remove that file and run this again while online". The truncated
//     file is left on disk, so that is every later offline run.
//
// The test runs the real writer in a real child process and the real parser in
// this one, both on the same HOME: a goroutine in one process would be sharing
// a buffer, not the filesystem.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// cacheChildEnv marks the re-executed copy of this test binary. The value is
// "<home>|<source>|<iterations>".
const cacheChildEnv = "LAUNCH_CACHE_TEST_CHILD"

// bigProviderCatalogBody is a valid provider catalog big enough that copying it
// into the destination is not a single syscall — which is the whole window the
// reader has to observe a prefix.
func bigProviderCatalogBody(providers int) []byte {
	f := providerCatalogFile{Version: 1}
	for i := 0; i < providers; i++ {
		f.Providers = append(f.Providers, providerCatalogEntry{
			Name:    fmt.Sprintf("host-%04d", i),
			BaseURL: fmt.Sprintf("https://host-%04d.example.com/v1", i),
		})
	}
	b, err := json.Marshal(f)
	if err != nil {
		panic(err)
	}
	return b
}

// TestCatalogCacheWriterChildHelper is not a test of its own: the parent
// re-executes the test binary with cacheChildEnv set, and this drives the real
// sync write path repeatedly against one cache file.
//
// HOME is set here, after TestMain, for the same reason the store test sets its
// own env: everything this test redirects is resolved from the environment at
// call time, so the last writer of a variable wins.
func TestCatalogCacheWriterChildHelper(t *testing.T) {
	spec := os.Getenv(cacheChildEnv)
	if spec == "" {
		t.Skip("helper for TestCatalogCacheIsNeverReadTorn; runs in a child process")
	}
	parts := strings.SplitN(spec, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("bad child spec %q", spec)
	}
	home, source := parts[0], parts[1]
	iters, err := strconv.Atoi(parts[2])
	if err != nil {
		t.Fatalf("bad iteration count %q", parts[2])
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)

	for i := 0; i < iters; i++ {
		// A file:// URL drives the same code path as a real sync — fetch,
		// parse, write the cache — without a network.
		if _, err := ProviderSync("file://" + source); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
}

func TestCatalogCacheIsNeverReadTorn(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(source, bigProviderCatalogBody(4000), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(home, ".oaica", "cache", "providers", "providers.json")

	cmd := exec.Command(os.Args[0], "-test.run=TestCatalogCacheWriterChildHelper")
	cmd.Env = append(os.Environ(), cacheChildEnv+"="+home+"|"+source+"|300")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	var reads, torn int
	var firstTorn []byte
	var firstErr error
loop:
	for {
		select {
		case <-done:
			break loop
		default:
		}
		b, err := os.ReadFile(cache)
		if err != nil {
			continue // not written yet, or mid-rename: neither is a torn read
		}
		reads++
		if _, perr := parseProviderCatalogFileChecked(b); perr != nil {
			torn++
			if firstTorn == nil {
				firstTorn, firstErr = b, perr
			}
		}
	}
	if reads == 0 {
		t.Fatalf("the reader never saw the cache at all; child output:\n%s", out.String())
	}
	if torn != 0 {
		t.Errorf("%d of %d reads of the provider cache were a partial document (first was %d bytes, parse error: %v) — the cache is written in place, so a reader running beside a sync sees a prefix: providerCatalog() silently drops the synced provider and model rows for that run, and the sync path's own fallback errors with \"the cached provider catalog at %s is not readable as one — remove that file and run this again while online\" and leaves the truncated file behind for every later offline run",
			torn, reads, len(firstTorn), firstErr, cache)
	}
}
