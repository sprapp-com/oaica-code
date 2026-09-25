package cmd

// oaica_run_flags_test.go — the `oaica run` flag surface, pinned to what the
// flags DO rather than to what their help says.
//
// Three flags inherited from upstream Ollama fed an embedding path this fork
// does not have: RunHandler never builds an api.EmbedRequest, so --truncate,
// --dimensions and --insecure all parsed and did nothing — --insecure's name
// promising a security posture nothing changed, --truncate's help promising a
// "(default: true)" behaviour its own registration contradicted. They were
// removed rather than silently accepted, so an ollama-migrating script fails
// loudly instead of believing they took effect (2026-09-26 audit). `push`,
// `pull` and `serve` keep their own --insecure, which those flows read.
//
// The other two findings were the opposite mistake: a flag whose help
// under-described it. --think accepts `max` (the code's own switch and its
// rejection message both name it) and --tool-format accepts four values, of
// which the help named two.

import (
	"os"
	"strings"
	"testing"
)

// cmdGoSource returns cmd/cmd.go's text. These assertions are on the
// REGISTRATION lines (the flag set is a local inside the command builder, so
// there is no package-level value to inspect).
func cmdGoSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// registrationLine returns the first line containing marker, trimmed.
func registrationLine(t *testing.T, src, marker string) string {
	t.Helper()
	for _, l := range strings.Split(src, "\n") {
		if strings.Contains(l, marker) {
			return strings.TrimSpace(l)
		}
	}
	t.Fatalf("cmd/cmd.go no longer contains %q — re-read it before trusting this test", marker)
	return ""
}

func TestRunRegistersNoFlagNothingReads(t *testing.T) {
	src := cmdGoSource(t)

	// --truncate and --dimensions: `run` never sends an embed request, so
	// nothing could read them. --insecure: RunHandler contains no reference
	// to it; the only GetBool("insecure") sites in cmd/ belong to push, pull
	// and serve.
	for _, gone := range []string{
		`runCmd.Flags().Bool("insecure"`,
		`runCmd.Flags().Bool("truncate"`,
		`runCmd.Flags().Int("dimensions"`,
	} {
		if strings.Contains(src, gone) {
			t.Errorf("cmd/cmd.go registers %s again — `oaica run` has no code path that reads it, so the flag would parse and do nothing (see this file's header for the 2026-09-26 audit finding)", gone)
		}
	}

	// The flows that DO read --insecure must keep it.
	for _, kept := range []string{
		`pushCmd.Flags().Bool("insecure"`,
		`serveCmd.Flags().Bool("insecure"`,
	} {
		if !strings.Contains(src, kept) {
			t.Errorf("cmd/cmd.go no longer registers %s, but that flow reads the flag — removing the reader is not the fix", kept)
		}
	}
}

func TestRunThinkHelpNamesEveryAcceptedValue(t *testing.T) {
	src := cmdGoSource(t)
	line := registrationLine(t, src, `runCmd.Flags().String("think"`)

	// The accepted set, taken from the switch that consumes the flag.
	if !strings.Contains(src, `case "high", "medium", "low", "max":`) {
		t.Fatalf("the --think switch changed; re-read cmd/cmd.go before trusting this test (the help below is pinned against what it accepts)")
	}
	for _, v := range []string{"true", "false", "high", "medium", "low", "max"} {
		if !strings.Contains(line, v) {
			t.Errorf("--think accepts %q (and the rejection message names it) but its help does not:\n  %s", v, line)
		}
	}
}

func TestToolFormatHelpNamesEveryAcceptedValue(t *testing.T) {
	src := cmdGoSource(t)
	line := registrationLine(t, src, `Flags().String("tool-format"`)

	// validRemoteToolFormats (launch/remote_cli.go) is the enforced set; the
	// help lives here and cannot see it, so the values are repeated — a value
	// added there without the help would make this test fail, which is the
	// point.
	for _, v := range []string{"tool_calls", "freeform", "xml", "none"} {
		if !strings.Contains(line, v) {
			t.Errorf("--tool-format accepts %q but its help does not name it:\n  %s", v, line)
		}
	}
	// The default is inferred from the wire (launch/user_remotes.go's
	// Descriptor), so a help that names only one default is half the story.
	for _, v := range []string{"openai", "anthropic"} {
		if !strings.Contains(line, v) {
			t.Errorf("--tool-format's help does not say what the default is for the %s wire:\n  %s", v, line)
		}
	}
}
