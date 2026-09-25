package launch

// CLI-level coverage for the tier flags. The unit tests in
// tier_routing_test.go call Claude.Run directly, which is exactly the call
// that *has* the refusals — so they cannot see a bug in the layer above it
// that decides what reaches Run at all. Both findings below lived there: the
// extraction into the passthrough list dropped empty values before Run could
// refuse them, and it did so for every integration, not just the one that
// consumes the flags.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// fakeClaudeOnPath puts a `claude` that does nothing on PATH, so a launch that
// gets past routing reaches a child instead of failing "not installed".
func fakeClaudeOnPath(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// tierFlagTestEnv is the minimum a launch needs to get as far as tier
// resolution: a home, a remote to route to, dead discovery stubs, and a child
// binary. The refusals under test happen before any of it is used, but a
// launch that does not refuse would otherwise fail for an unrelated reason and
// make the assertion pass for the wrong cause.
func tierFlagTestEnv(t *testing.T) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	withInteractiveSession(t, false)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	fakeClaudeOnPath(t)
}

// A tier flag's value carried through the CLI as an EMPTY string (`--oversize=`,
// or a "$VAR" that expanded to nothing) was dropped when the passthrough list
// was built, so tier_routing.go's refusal for the empty spelling never ran and
// a plan's stored leg was inherited instead — byte-identical to passing no flag
// at all. The gate is now "was the flag passed", not "is the value non-empty"
// (2026-09-26 audit).
func TestLaunchCmd_EmptyTierValueReachesTheRefusal(t *testing.T) {
	for _, c := range []struct {
		flag string
		args []string
	}{
		{"--oversize", []string{"claude", "--model", "box/kat-awq", "--oversize="}},
		{"--route-policy", []string{"claude", "--model", "box/kat-awq", "--route-policy="}},
		{"--sonnet-model", []string{"claude", "--model", "box/kat-awq", "--sonnet-model="}},
		{"--haiku-model", []string{"claude", "--model", "box/kat-awq", "--haiku-model="}},
		{"--plan", []string{"claude", "--model", "box/kat-awq", "--plan="}},
	} {
		t.Run(c.flag, func(t *testing.T) {
			tierFlagTestEnv(t)
			cmd := LaunchCmd(func(*cobra.Command, []string) error { return nil }, func(*cobra.Command) {})
			cmd.SetArgs(c.args)
			cmd.SetOut(os.Stderr)
			cmd.SetErr(os.Stderr)
			err := cmd.ExecuteContext(t.Context())
			if err == nil {
				t.Fatalf("%v was accepted; want an error naming %s", c.args, c.flag)
			}
			if !strings.Contains(err.Error(), c.flag) {
				t.Errorf("%v gave %q, want an error naming %s", c.args, err.Error(), c.flag)
			}
		})
	}
}

// Same rule one line up: a tier flag whose value slot swallowed the NEXT flag
// ("--sonnet-model --wizard") pinned the tier to the literal string "--wizard"
// and consumed the wizard flag, so the wizard never ran and the bogus id went
// to the child, where the first subagent request failed against a model of that
// name (2026-09-26 audit).
func TestLaunchCmd_TierFlagValueIsNotTheNextFlag(t *testing.T) {
	for _, c := range []struct {
		flag string
		args []string
	}{
		{"--sonnet-model", []string{"claude", "--model", "box/kat-awq", "--sonnet-model", "--wizard"}},
		{"--haiku-model", []string{"claude", "--model", "box/kat-awq", "--haiku-model", "--wizard"}},
	} {
		t.Run(c.flag, func(t *testing.T) {
			tierFlagTestEnv(t)
			cmd := LaunchCmd(func(*cobra.Command, []string) error { return nil }, func(*cobra.Command) {})
			cmd.SetArgs(c.args)
			cmd.SetOut(os.Stderr)
			cmd.SetErr(os.Stderr)
			err := cmd.ExecuteContext(t.Context())
			if err == nil || !strings.Contains(err.Error(), c.flag) {
				t.Errorf("%v gave %v, want an error naming %s", c.args, err, c.flag)
			}
		})
	}
}

// The tier flags describe Claude Code's tier split and are read back out of
// the passthrough list by Claude.Run alone (tier_routing.go's extractors).
// Building that list for every integration put them in the child's argv:
// `oaica launch codex --model kat-awq --plan p` ran `codex … --plan p`, which
// codex rejected with "error: unexpected argument '--plan' found", exit 2 — a
// flag the user aimed at oaica, reported by an agent that never saw it. They
// are refused by name now; silently dropping them was the other option and
// says nothing, which is how the caller ends up believing a plan applied
// (2026-09-26 audit).
func TestLaunchCmd_TierFlagsAreRefusedForIntegrationsThatDoNotReadThem(t *testing.T) {
	for _, c := range []struct {
		flag string
		args []string
	}{
		{"--plan", []string{"codex", "--model", "box/kat-awq", "--plan", "p"}},
		{"--sonnet-model", []string{"codex", "--model", "box/kat-awq", "--sonnet-model", "box/kat-awq"}},
		{"--route-policy", []string{"codex", "--model", "box/kat-awq", "--route-policy", "local-only"}},
		{"--shard", []string{"codex", "--model", "box/kat-awq", "--shard", "box/kat-awq:3"}},
	} {
		t.Run(c.flag, func(t *testing.T) {
			tierFlagTestEnv(t)
			cmd := LaunchCmd(func(*cobra.Command, []string) error { return nil }, func(*cobra.Command) {})
			cmd.SetArgs(c.args)
			cmd.SetOut(os.Stderr)
			cmd.SetErr(os.Stderr)
			err := cmd.ExecuteContext(t.Context())
			if err == nil {
				t.Fatalf("%v was accepted; want a refusal naming %s", c.args, c.flag)
			}
			if !strings.Contains(err.Error(), c.flag) {
				t.Errorf("%v gave %q, want a refusal naming %s", c.args, err.Error(), c.flag)
			}
			if !strings.Contains(err.Error(), "Claude Code") {
				t.Errorf("%v gave %q — the refusal should say where the flag does apply", c.args, err.Error())
			}
		})
	}
}

// ...and the same flags still work for the integration that does read them,
// so the gate above cannot be satisfied by refusing everything.
func TestLaunchCmd_TierFlagsStillTravelToClaude(t *testing.T) {
	tierFlagTestEnv(t)
	cmd := LaunchCmd(func(*cobra.Command, []string) error { return nil }, func(*cobra.Command) {})
	cmd.SetArgs([]string{"claude", "--model", "box/kat-awq", "--route-policy", "local-only", "--sonnet-model", "box/kat-awq"})
	cmd.SetOut(os.Stderr)
	cmd.SetErr(os.Stderr)
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("claude with tier flags: %v", err)
	}
}

// A user-remote model resolves to the "chat" wire (most remotes speak
// /v1/chat/completions, not /v1/responses), but codex's self-check on the
// config it had just written demanded "responses" unconditionally — so every
// `oaica launch codex --model <remote>/<id>` died on its own validation with
// "generated Codex config missing model_providers.ollama-launch.wire_api =
// \"responses\"", before codex ever started. The write site and the check now
// agree by construction: the expected wire is passed to the validator instead
// of recomputed from a different model name (2026-09-26 audit).
func TestLaunchCmd_CodexWithARemoteModelPassesItsOwnConfigCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake codex binary is a /bin/sh script")
	}
	tierFlagTestEnv(t)

	binDir := t.TempDir()
	// codex is version-probed before it is launched ("codex-cli 0.140.0").
	fake := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'codex-cli 0.140.0'; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd := LaunchCmd(func(*cobra.Command, []string) error { return nil }, func(*cobra.Command) {})
	cmd.SetArgs([]string{"codex", "--model", "box/kat-awq"})
	cmd.SetOut(os.Stderr)
	cmd.SetErr(os.Stderr)
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("codex with a remote model: %v", err)
	}
}
