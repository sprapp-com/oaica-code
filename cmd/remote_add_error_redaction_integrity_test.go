package cmd

// remote_add_error_redaction_integrity_test.go — the CLI half of the
// `oaica remote add` credential echo (2026-09-26 audit, ninth round).
//
// main.go prints whatever the command returns with cobra.CheckErr, and this
// command sets SilenceErrors, so the error value IS the terminal output. This
// drives the real command and asserts on exactly that value.

import (
	"io"
	"strings"
	"testing"
)

const addEchoKey = "sk-live-ERRORECHO-0123456789"

func TestRemoteAddCliNeverPrintsTheKeyItRefused(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", t.TempDir()+"/remotes.json")

	inputs := []string{
		"https://" + addEchoKey + "@api.example.com/v1 with space",
		"https://" + addEchoKey + "@api.example.com/v1\nnext-line",
		"api.example.com/v1?key=" + addEchoKey,
		"ftp://" + addEchoKey + "@api.example.com",
		"https://" + addEchoKey + "@",
		"https://" + addEchoKey + "@api.example.com:99999999/v1",
	}

	for _, in := range inputs {
		root := NewCLI()
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs([]string{"remote", "add", "box", "--base-url", in})
		err := root.Execute()
		if err == nil {
			t.Errorf("`oaica remote add --base-url <key-bearing url>` succeeded; these inputs were chosen to fail validation")
			continue
		}
		// What main.go hands cobra.CheckErr, i.e. what the user reads.
		printed := err.Error()
		if strings.Contains(printed, addEchoKey) {
			t.Errorf("`oaica remote add` put the credential in its own error message, which the terminal, the CI log and the shell history all keep:\n%s", printed)
		}
		if !strings.Contains(printed, "api.example.com") {
			t.Errorf("the refusal lost the host it needs to be actionable:\n%s", printed)
		}
	}
}
