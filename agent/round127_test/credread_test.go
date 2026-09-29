package round127_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ollama/ollama/agent"
	agenttools "github.com/ollama/ollama/agent/tools"
)

// Under `oaica agent --yes` every call is approved; the credential denylist is the only guard left.
func TestRound127CredentialReadIsRefusedOnEveryFileTool(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	key := filepath.Join(home, ".ssh", "id_ed25519")
	os.WriteFile(key, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n"), 0o600)
	cwd := t.TempDir()
	tc := agent.ToolContext{WorkingDir: cwd}

	_, berr := (&agenttools.Bash{}).Execute(context.Background(), tc, map[string]any{"command": "cat " + key})
	t.Logf("bash cat %s -> err=%v", key, berr)
	res, rerr := (&agenttools.Read{}).Execute(context.Background(), tc, map[string]any{"path": key})
	t.Logf("read %s -> err=%v content=%q", key, rerr, res.Content)
	if berr != nil && rerr == nil {
		t.Errorf("the bash door refuses the credential read, the read door serves it")
	}
	// edit too, and a relative path that resolves into a credential directory
	if _, eerr := (&agenttools.Edit{}).Execute(context.Background(), tc, map[string]any{"path": key, "old_text": "secret", "new_text": "x"}); eerr == nil {
		t.Errorf("edit touched a credential file")
	}
	tc2 := agent.ToolContext{WorkingDir: filepath.Join(home, ".ssh")}
	if _, rerr2 := (&agenttools.Read{}).Execute(context.Background(), tc2, map[string]any{"path": "id_ed25519"}); rerr2 == nil {
		t.Errorf("read of a relative path into ~/.ssh was served")
	}
	if _, eerr2 := (&agenttools.Edit{}).Execute(context.Background(), tc2, map[string]any{"path": "id_ed25519", "old_text": "secret", "new_text": "x"}); eerr2 == nil {
		t.Errorf("edit of a relative path into ~/.ssh was applied")
	}
	if b, _ := os.ReadFile(key); !strings.Contains(string(b), "secret") {
		t.Errorf("the credential file was modified: %q", b)
	}
}
