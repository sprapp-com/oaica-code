package cmd

// help_text_truthfulness_integrity_test.go — help text that states something
// untrue (2026-09-26 audit, twelfth round).
//
// Two shapes, one rule: what `--help` says must be what the command does. A
// flag's usage string is read by a user deciding whether to type it, and it is
// the only documentation most flags have.
//
//  1. pflag's UnquoteUsage treats back-quoted text in a usage string as the
//     VALUE NAME, so `oaica plan set --help` rendered
//     `--description oaica plan list` — the flag appeared to take no value and
//     the description's own text was eaten as the placeholder. Silent, and
//     wrong in the one line a user reads to learn the flag's shape.
//
//  2. The shared env-docs loop attaches `OLLAMA_HOST` to the commands that
//     talk to a local daemon. `oaica pull` is not one of them: PullHandler
//     talks to api.oaica.com's /v1/manifest + /v1/pull and never consults
//     OLLAMA_HOST — which is exactly why pullCmd is deliberately not gated on
//     checkServerHeartbeat, as its own definition says. A user following that
//     line sets a variable that changes nothing and concludes the tool ignored
//     it. The control below pins the other half: the commands that DO read it
//     must keep documenting it.

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// walkCommands visits root and every command under it.
func walkCommands(root *cobra.Command, fn func(*cobra.Command)) {
	fn(root)
	for _, c := range root.Commands() {
		walkCommands(c, fn)
	}
}

// No flag usage may carry a back-quote: pflag would render it as the value
// name instead of as the `string`/`int` placeholder it actually is.
func TestNoFlagUsageStealsItsValueNameWithBackquotes(t *testing.T) {
	walkCommands(NewCLI(), func(c *cobra.Command) {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if strings.Contains(f.Usage, "`") {
				t.Errorf("%s --%s: usage %q contains a back-quote; pflag's UnquoteUsage takes back-quoted text as the flag's value name, so --help renders it as the placeholder instead of the text the user needs to read",
					c.CommandPath(), f.Name, f.Usage)
			}
		})
	})
}

// The env-docs the help prints must be env the command actually reads.
func TestPullHelpDoesNotDocumentEnvItNeverReads(t *testing.T) {
	root := NewCLI()
	pull, _, err := root.Find([]string{"pull"})
	if err != nil {
		t.Fatalf("premise: no pull command: %v", err)
	}
	if !strings.Contains(pull.UsageString(), "OLLAMA_HOST") {
		return // fixed the other way (nothing advertises it)
	}
	t.Errorf("`oaica pull --help` documents OLLAMA_HOST, but PullHandler talks to api.oaica.com's /v1/manifest + /v1/pull and never consults it (pullCmd is deliberately not gated on checkServerHeartbeat, unlike every command the env-docs loop is for) — a user following that line sets a variable that changes nothing")
}

// The control: commands that ARE daemon-backed keep documenting it, so the
// fix cannot be "narrow the loop until nothing is documented".
func TestDaemonBackedCommandsStillDocumentOLLAMA_HOST(t *testing.T) {
	root := NewCLI()
	for _, name := range []string{"list", "show", "run", "ps"} {
		c, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatalf("premise: no %s command: %v", name, err)
		}
		if !strings.Contains(c.UsageString(), "OLLAMA_HOST") {
			t.Errorf("`oaica %s --help` no longer documents OLLAMA_HOST, but it reads it", name)
		}
	}
}
