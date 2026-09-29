package tools

import (
	"context"
	"github.com/ollama/ollama/agent"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F129-L1-3 / F129-L1-4 (2026-09-29 audit, round 129): the bash classifier refuses the spellings of what it
// already refused.
func TestRound129BashClassifierSpellings(t *testing.T) {
	refused := []string{
		"/bin/cat ~/.ssh/id_rsa",
		`\cat ~/.ssh/id_rsa`,
		"cat ~/.ssh/./id_rsa",
		"cat ~/.ssh//id_rsa",
		"cat ~/.ssh/id_rs?",
		"cat ~/.ssh/id_*",
		`r\m -rf ~`,
		"echo ~ | xargs rm -rf",
		"rm -rf **",
		"rm -rf ?*",
		"rm -rf /**",
		"rm -rf /?*",
	}
	for _, c := range refused {
		if rejectUnsafeShellCommand(c) == nil {
			t.Errorf("%q must be refused", c)
		}
	}
	for _, c := range []string{"cat README.md", "rm -rf build", "echo hi | xargs rm -f", "ls ~/.ssh/../src"} {
		if err := rejectUnsafeShellCommand(c); err != nil {
			t.Errorf("%q must pass: %v", c, err)
		}
	}
}

func TestRound129BashRefusesAnInTreeSymlinkToCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_rsa"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(work, "keys")); err != nil {
		t.Skip(err)
	}
	if refuseCredentialWords(work, "cat keys/id_rsa") == nil {
		t.Error("cat keys/id_rsa must be refused through the symlink")
	}
	if err := refuseCredentialWords(work, "cat notes/plan.txt"); err != nil {
		t.Errorf("ordinary path refused: %v", err)
	}
}

func TestRound129BashExecuteRefusesTheSymlinkedCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_rsa"), []byte("SECRET"), 0o600)
	work := t.TempDir()
	if err := os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(work, "keys")); err != nil {
		t.Skip(err)
	}
	res, err := (&Bash{}).Execute(context.Background(), agent.ToolContext{WorkingDir: work}, map[string]any{"command": "cat keys/id_rsa"})
	if err == nil || strings.Contains(res.Content, "SECRET") {
		t.Errorf("bash served the credential through a symlink: err=%v content=%q", err, res.Content)
	}
}
