package cmd

// launcher_menu_license_integrity_test.go — the bare `oaica` MENU launched
// integrations without the licence gate (2026-09-27 audit, round 22).
//
// `oaica launch <integration>` gates in LaunchCmd's PreRunE
// (cmd/cmd.go: launch.LaunchCmd(composeLaunchPrecondition(oaicaEnsureSignedIn,
// launch.RequireLicense), runInteractiveTUI)), but the menu is the other way
// into exactly the same launch and it dispatched straight to
// launchIntegration. An unlicensed box therefore refused `oaica launch claude`
// and cheerfully configured and started Claude from the menu — the way most
// users reach it. The gate now runs on both paths, from the same function.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ollama/ollama/cmd/launch"
	"github.com/ollama/ollama/cmd/tui"
	"github.com/spf13/cobra"
)

// stubLicenseGate approves, for the launcherDeps literals in tests that are
// not about the gate.
func stubLicenseGate() func(*cobra.Command, []string) error {
	return func(*cobra.Command, []string) error { return nil }
}

func TestLauncherMenuRunsTheLicenceGate(t *testing.T) {
	setCmdTestHome(t, t.TempDir())

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	want := errors.New("this launch needs a licence: no key found (run `oaica signin`)")
	gateCalls := 0
	continueLoop, err := runLauncherAction(cmd, tui.TUIAction{Kind: tui.TUIActionLaunchIntegration, Integration: "claude"}, launcherDeps{
		resolveRunModel:   unexpectedRunModelResolution(t),
		launchIntegration: unexpectedIntegrationLaunch(t),
		runModel:          unexpectedModelLaunch(t),
		ensureLicense: func(_ *cobra.Command, args []string) error {
			gateCalls++
			if len(args) != 1 || args[0] != "claude" {
				t.Errorf("licence gate called with args %v, want [claude] — the subcommand's PreRunE sees the integration name", args)
			}
			return want
		},
	})
	if gateCalls != 1 {
		t.Fatalf("licence gate ran %d times, want 1: the menu path had no gate at all before this", gateCalls)
	}
	if !errors.Is(err, want) {
		t.Fatalf("menu launch error = %v, want the gate's %v — a refusal that does not say why is not a gate", err, want)
	}
	if !continueLoop {
		t.Error("a refused launch ended the menu loop; the user should get the menu back")
	}
}

// TestLauncherMenuLaunchesOnceTheGateApproves is the control: the same action
// with an approving gate reaches launchIntegration and carries the request
// fields the menu can set.
func TestLauncherMenuLaunchesOnceTheGateApproves(t *testing.T) {
	setCmdTestHome(t, t.TempDir())

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	gateCalls := 0
	gate := func(*cobra.Command, []string) error { gateCalls++; return nil }
	var got launch.IntegrationLaunchRequest
	continueLoop, err := runLauncherAction(cmd, tui.TUIAction{Kind: tui.TUIActionLaunchIntegration, Integration: "claude", ForceConfigure: true}, launcherDeps{
		resolveRunModel: unexpectedRunModelResolution(t),
		launchIntegration: func(_ context.Context, req launch.IntegrationLaunchRequest) error {
			got = req
			return nil
		},
		runModel:      unexpectedModelLaunch(t),
		ensureLicense: gate,
	})
	if err != nil {
		t.Fatalf("approved launch = %v, want success", err)
	}
	if !continueLoop {
		t.Error("an approved launch did not continue the menu loop")
	}
	if got.Name != "claude" || !got.ForceConfigure {
		t.Errorf("launched %+v, want claude with ForceConfigure — the gate must not consume or alter the request", got)
	}
	if gateCalls != 1 {
		t.Errorf("licence gate calls = %d, want exactly one", gateCalls)
	}
}

// TestLauncherMenuWithoutAGateIsAnErrorNotASkip pins the nil contract: a
// caller that forgets to wire the gate gets a loud internal error instead of a
// silent unlicensed launch.
func TestLauncherMenuWithoutAGateIsAnErrorNotASkip(t *testing.T) {
	setCmdTestHome(t, t.TempDir())

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	continueLoop, err := runLauncherAction(cmd, tui.TUIAction{Kind: tui.TUIActionLaunchIntegration, Integration: "claude"}, launcherDeps{
		resolveRunModel:   unexpectedRunModelResolution(t),
		launchIntegration: unexpectedIntegrationLaunch(t),
		runModel:          unexpectedModelLaunch(t),
	})
	if err == nil {
		t.Fatal("a launcherDeps with no licence gate launched anyway, want an error")
	}
	if !strings.Contains(err.Error(), "licence check") {
		t.Errorf("error = %v, want it to name the missing licence check", err)
	}
	if continueLoop {
		t.Error("an un-wired gate must stop the menu, not loop on it")
	}
}
