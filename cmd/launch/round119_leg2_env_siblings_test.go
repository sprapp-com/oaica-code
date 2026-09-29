// round119_leg2_env_siblings_test.go — F119-L2-3: a catalog row's env[] siblings are scrubbed from a child (2026-09-29 audit, round 119).

package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRound119CatalogEnvSiblingSurvivesScrub(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	dir := filepath.Join(home, ".oaica", "cache", "catalog")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "modelsdev.json"), []byte(`{
 "google":{"id":"google","name":"Google","npm":"@ai-sdk/google","env":["GOOGLE_GENERATIVE_AI_API_KEY","GEMINI_API_KEY"],"models":{}},
 "sibprov":{"id":"sibprov","name":"Sib","api":"https://api.sib.example/v1","npm":"@ai-sdk/openai-compatible","env":["SIB_API_KEY","SIB_TOKEN"],"models":{}}
}`), 0o600)
	t.Setenv("GEMINI_API_KEY", "g-key-1")
	t.Setenv("GOOGLE_GENERATIVE_AI_API_KEY", "g-key-2")
	t.Setenv("SIB_API_KEY", "s-key-1")
	t.Setenv("SIB_TOKEN", "s-key-2")
	names := openclawCredentialEnvNames()
	t.Logf("scrub names: %v", names)
	for _, r := range builtinRemotes() {
		if r.Name == "google" || r.Name == "sibprov" {
			t.Logf("row %s api_key_env=%q key()=%q", r.Name, r.APIKeyEnv, r.key())
		}
	}
	env := directLaunchEnv()
	for _, kv := range env {
		for _, n := range []string{"GEMINI_API_KEY=", "GOOGLE_GENERATIVE_AI_API_KEY=", "SIB_API_KEY=", "SIB_TOKEN="} {
			if strings.HasPrefix(kv, n) {
				t.Errorf("RED: child env still holds %s (a credential the same catalog row lists)", kv)
			}
		}
	}
}
