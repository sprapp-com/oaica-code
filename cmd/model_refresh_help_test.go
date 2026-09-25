package cmd

// model_refresh_help_test.go — `oaica model refresh --help` is the one place a
// user is told what the picker's cache does and does not see. It claimed there
// was no cross-process cache at all, which was false in both directions: one
// existed (so a `ollama pull` could be invisible), and it did see some changes.

import (
	"os"
	"strings"
	"testing"
)

// TestModelRefreshHelpDescribesTheCacheTruthfully pins the shipped help text to
// what the picker actually does (2026-09-26 audit). It said there was no
// cross-process cache while one existed, and then said a `ollama pull` could
// stay invisible for an hour after the cache-hit path had been changed to
// re-read the daemon live. The text lives in cmd/cmd.go's modelRefreshCmd, a
// local variable, so the source is read rather than the command object.
func TestModelRefreshHelpDescribesTheCacheTruthfully(t *testing.T) {
	b, err := os.ReadFile("cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	// Scope the check to the refresh command's own Long text.
	start := strings.Index(src, "modelRefreshCmd := &cobra.Command{")
	if start < 0 {
		t.Fatal("cmd.go no longer defines modelRefreshCmd — move this test to wherever it went")
	}
	long := src[start:]
	if end := strings.Index(long, "Args: cobra.NoArgs"); end > 0 {
		long = long[:end]
	}

	if strings.Contains(long, "there is no cross-process cache") {
		t.Errorf("the help text claims there is no cross-process cache:\n%s", long)
	}
	// The daemon's list is re-read live on a cache hit, so the text must not
	// say a pull can lag.
	if strings.Contains(long, "cannot see") {
		t.Errorf("the help text still says something cannot be seen within the cache window:\n%s", long)
	}
	for _, want := range []string{"picker_cache.json", "refres", "ollama pull"} {
		if !strings.Contains(long, want) {
			t.Errorf("the help text does not mention %q, so a user cannot tell why a change is missing or how to force it:\n%s", want, long)
		}
	}
}
