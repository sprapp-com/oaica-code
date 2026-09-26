package launch

// configure_only_headless_integrity_test.go — `--config`, documented as
// "Configure without launching", launched the integration anyway under --yes,
// and under a headless run without --yes it configured and THEN failed the
// command (2026-09-27 audit, round 22).
//
// launchAfterConfiguration's configure-only arm was a confirm prompt and not a
// return. The two non-interactive policies answer that prompt without a user:
// auto-approve (--yes) returns true without printing anything, so the agent was
// exec'd; require-yes returns an error naming --yes, which after a successful
// configure-only run is the wrong verdict — the work the user asked for was
// already on disk, and a provisioning script got a non-zero exit for it.
// Neither is what the flag promises, so both stop after the configuration and
// the interactive prompt keeps its convenience.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// configureOnlyEnv makes "cline" look installed so the launch tail can run, and
// returns the runner whose Run records the launch.
func configureOnlyEnv(t *testing.T) *launcherManagedRunner {
	t.Helper()
	setTestHome(t, t.TempDir())
	if runtime.GOOS == "windows" {
		t.Skip("the fake integration is an sh stub on PATH")
	}
	binDir := t.TempDir()
	writeFakeBinary(t, binDir, "cline")
	t.Setenv("PATH", binDir)
	_ = os.MkdirAll(filepath.Join(binDir, "unused"), 0o755)

	runner := &launcherManagedRunner{}
	withIntegrationOverride(t, "cline", runner)
	return runner
}

func TestLaunchAfterConfigurationHonoursConfigureOnlyHeadlessly(t *testing.T) {
	for name, policy := range map[string]launchConfirmPolicy{
		"auto-approve (--yes)": {yes: true},
		"headless (no --yes)":  {requireYesMessage: true},
	} {
		t.Run(name, func(t *testing.T) {
			runner := configureOnlyEnv(t)
			restore := withLaunchConfirmPolicy(policy)
			defer restore()

			err := launchAfterConfiguration("cline", runner, "llama3.2",
				launchModelsFromNames([]string{"llama3.2"}),
				IntegrationLaunchRequest{ConfigureOnly: true})
			if err != nil {
				t.Errorf("configure-only run = %v, want success: the configuration is the work the flag asked for, and neither headless policy has a user to answer the launch prompt", err)
			}
			if runner.ranModel != "" {
				t.Errorf("--config launched %q: the flag's help says \"Configure without launching\", and the headless policy answered the prompt itself", runner.ranModel)
			}
		})
	}
}

// TestLaunchAfterConfigurationStillAsksInteractively is the control: with a
// user at the terminal the prompt stays, and answering yes launches.
func TestLaunchAfterConfigurationStillAsksInteractively(t *testing.T) {
	old := DefaultConfirmPrompt
	asked := false
	DefaultConfirmPrompt = func(string, ConfirmOptions) (bool, error) {
		asked = true
		return true, nil
	}
	t.Cleanup(func() { DefaultConfirmPrompt = old })

	runner := configureOnlyEnv(t)
	restore := withLaunchConfirmPolicy(launchConfirmPolicy{})
	defer restore()

	if err := launchAfterConfiguration("cline", runner, "llama3.2",
		launchModelsFromNames([]string{"llama3.2"}),
		IntegrationLaunchRequest{ConfigureOnly: true}); err != nil {
		t.Fatalf("interactive configure-only run = %v, want success", err)
	}
	if !asked {
		t.Error("the interactive run was not asked whether to launch, so the convenience the prompt exists for is gone")
	}
	if runner.ranModel != "llama3.2" {
		t.Errorf("the interactive answer yes did not launch (ranModel = %q)", runner.ranModel)
	}
}
