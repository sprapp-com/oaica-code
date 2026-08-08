package agent

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

// TestAgentCmdFlagRegistration: the command exposes the documented flags with
// the right defaults, accepts arbitrary positional args, and wires the
// injected PreRunE. It deliberately avoids cmd.Execute(), which would drive
// runAgent into a real model resolve + network call.
func TestAgentCmdFlagRegistration(t *testing.T) {
	preRunCalled := false
	cmd := AgentCmd(func(c *cobra.Command, args []string) error {
		preRunCalled = true
		return nil
	})

	if cmd.Use != "agent [PROMPT]" {
		t.Errorf("Use = %q", cmd.Use)
	}
	if cmd.Short != "Run a streaming coding agent" {
		t.Errorf("Short = %q", cmd.Short)
	}

	for _, name := range []string{"model", "system", "max-turns", "yes", "cwd"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("--%s flag missing", name)
		}
	}
	if cmd.Flags().Lookup("max-turns").DefValue != "0" {
		t.Errorf("--max-turns default = %q, want 0", cmd.Flags().Lookup("max-turns").DefValue)
	}
	if cmd.Flags().Lookup("yes").DefValue != "false" {
		t.Errorf("--yes default = %q, want false", cmd.Flags().Lookup("yes").DefValue)
	}

	// Args accepts any positional arguments.
	if err := cmd.Args(cmd, []string{"read", "the", "repo"}); err != nil {
		t.Errorf("Args with arbitrary positions should pass: %v", err)
	}

	// PreRunE runs the injected sign-in check.
	if err := cmd.PreRunE(cmd, nil); err != nil {
		t.Fatalf("PreRunE: %v", err)
	}
	if !preRunCalled {
		t.Error("injected checkServerHeartbeat was not called")
	}
}

// TestAgentCmdPreRunEPropagatesError: the injected check's error surfaces.
func TestAgentCmdPreRunEPropagatesError(t *testing.T) {
	cmd := AgentCmd(func(c *cobra.Command, args []string) error {
		return errors.New("not signed in")
	})
	if err := cmd.PreRunE(cmd, nil); err == nil || err.Error() != "not signed in" {
		t.Errorf("err = %v, want the injected error", err)
	}
}
