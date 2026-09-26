package launch

// launch_tier_flags_without_integration_integrity_test.go — every launcher-level
// tier flag needs an integration name to apply to, and `--shard` was missing
// from the guard that says so (2026-09-27 audit, round 18).
//
// `oaica launch --shard box/kat-awq:3` exited 0, opened the TUI menu and threw
// the flag away, while `oaica launch --plan p` in the same position was refused
// with "flags and extra args require an integration name". The guard listed the
// tier flags by hand, one flag per name, and the list had drifted from
// tierFlagNames — which is the list the rest of the CLI reads them from. It now
// asks that list, so a flag cannot be added to one and forgotten in the other.

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestLaunchCmd_TierFlagsWithoutAnIntegrationNameAreNotSilentlyDropped(t *testing.T) {
	for _, args := range [][]string{
		{"--plan", "p"},
		{"--sonnet-model", "box/kat-awq"},
		{"--haiku-model", "box/kat-awq"},
		{"--oversize", "box/kat-awq"},
		{"--route-policy", "local-only"},
		{"--wizard"},
		{"--shard", "box/kat-awq:3"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			tierFlagTestEnv(t)
			tuiRan := false
			cmd := LaunchCmd(
				func(*cobra.Command, []string) error { return nil },
				func(*cobra.Command) { tuiRan = true },
			)
			cmd.SetArgs(args)
			cmd.SetOut(os.Stderr)
			cmd.SetErr(os.Stderr)

			err := cmd.ExecuteContext(t.Context())
			if err == nil {
				if tuiRan {
					t.Fatalf("%v opened the TUI menu and the flag was discarded — the user is left believing it applied", args)
				}
				t.Fatalf("%v was accepted with no error and no integration name to apply it to", args)
			}
			if !strings.Contains(err.Error(), "integration name") {
				t.Errorf("%v gave %q, want the 'flags and extra args require an integration name' refusal", args, err.Error())
			}
		})
	}
}
