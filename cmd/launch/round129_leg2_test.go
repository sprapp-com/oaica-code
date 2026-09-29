package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

// F129-L2-1 (2026-09-29 audit, round 129): OLLAMA_NO_CLOUD binds the launch door as it binds the agent door.
func TestRound129NoCloudPolicyBindsTheLaunchDoor(t *testing.T) {
	t.Setenv("OLLAMA_NO_CLOUD", "1")
	t.Setenv("OAICA_HOST", "")
	c, err := api.ClientFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if disabled, known := cloudStatusDisabled(context.Background(), c); !disabled || !known {
		t.Errorf("cloudStatusDisabled = %v,%v under OLLAMA_NO_CLOUD", disabled, known)
	}
	if got := ollamaCloudEntries(); len(got) != 0 {
		t.Errorf("ollamaCloudEntries listed %d cloud models under OLLAMA_NO_CLOUD", len(got))
	}
}

// F129-L2-2: doctor quotes a remote's wire and base_url as `remote list` does.
func TestRound129DoctorQuotesHostileRemoteFields(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "remotes.json")
	body, _ := json.Marshal(map[string]any{"remotes": []map[string]any{{
		"name": "evil", "base_url": "http://127.0.0.1:1/v1\x1b]52;c;AAAA\a", "wire": "openai\x1b[2J",
	}}})
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OAICA_REMOTES_FILE", file)
	var out bytes.Buffer
	doctorChecks(&out)
	if strings.ContainsAny(out.String(), "\x1b\a") {
		t.Errorf("doctor output carries a control sequence: %q", out.String())
	}
}
