package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// F131-L2-1 (2026-09-29 audit, round 131): temp files of pulls that are gone are swept; a live one is not.
func TestRound131StalePullTempsAreSwept(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "m.gguf")
	stale := filepath.Join(dir, "m.gguf.partial-111")
	live := filepath.Join(dir, "m.gguf.partial-222")
	for _, p := range []string{stale, live} {
		os.WriteFile(p, []byte("x"), 0o600)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(stale, old, old)
	f, stop, err := createPullTemp(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	defer f.Close()
	if _, err := os.Stat(stale); err == nil {
		t.Error("a stale temp file survived the sweep")
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("a live pull's temp file was swept")
	}
}

// F131-L2-3: a failing pull's body is redacted and printable.
func TestRound131PullErrorBodyIsQuotedAndRedacted(t *testing.T) {
	t.Setenv("OAICA_LICENSE_KEY", "sk-license-9f3c")
	got := readPullErrorBody(strings.NewReader("bad auth header: Bearer sk-license-9f3c\x1b]0;pwned\a\x1b[2K"))
	if strings.ContainsAny(got, "\x1b\a") || strings.Contains(got, "sk-license-9f3c") {
		t.Errorf("pull error body leaks: %q", got)
	}
}
