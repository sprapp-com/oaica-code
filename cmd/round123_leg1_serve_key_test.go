package cmd

// F123-L1-5 (2026-09-29 audit, round 123): `oaica serve` takes its bearer key from the environment,
// so it need not sit in /proc/<pid>/cmdline.

import (
	"strings"
	"testing"
)

func TestRound123ServeKeyFromTheEnvironment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OAICA_SERVE_API_KEY", "k-0123456789abcdef")
	cli := NewCLI()
	cli.SetArgs([]string{"serve", "some-model", "--host", "0.0.0.0"})
	if err := cli.Execute(); err != nil && strings.Contains(err.Error(), "without --api-key") {
		t.Fatalf("OAICA_SERVE_API_KEY was ignored: %v", err)
	}
}
