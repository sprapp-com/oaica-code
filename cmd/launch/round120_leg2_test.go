package launch

// Round 120 leg 2 (2026-09-29 audit): catalog env[] names that are not secrets are neither a row's
// key nor scrubbed from a child (F120-L2-1); the gateway token is scrubbed on every door
// (F120-L2-2); every credential form in OAICA_HOST is dropped from a child (F120-L2-3).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func round120Catalog(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	dir := filepath.Join(home, ".oaica", "cache", "catalog")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "modelsdev.json"), []byte(`{
 "cfw":{"id":"cfw","name":"Cfw","api":"https://api.cfw.example/v1","npm":"@ai-sdk/openai-compatible","env":["CFW_ACCOUNT_ID","CFW_API_KEY"],"models":{}},
 "prv":{"id":"prv","name":"Prv","api":"https://api.prv.example/v1","npm":"@ai-sdk/openai-compatible","env":["PRV_API_KEY","PRV_ENDPOINT"],"models":{}}
}`), 0o600)
}

func TestRound120NonSecretCatalogEnvNamesAreNotCredentials(t *testing.T) {
	round120Catalog(t)
	t.Setenv("CFW_ACCOUNT_ID", "acct-1234")
	t.Setenv("CFW_API_KEY", "cf-real-secret")
	t.Setenv("PRV_API_KEY", "prv-secret")
	t.Setenv("PRV_ENDPOINT", "https://prv.internal")
	for _, r := range builtinRemotes() {
		if r.Name == "cfw" && r.key() != "cf-real-secret" {
			t.Errorf("row cfw authenticates with %q, not the provider's key", r.key())
		}
	}
	env := strings.Join(directLaunchEnv(), "\n")
	for _, keep := range []string{"CFW_ACCOUNT_ID=acct-1234", "PRV_ENDPOINT=https://prv.internal"} {
		if !strings.Contains(env, keep) {
			t.Errorf("the child lost %s, which is not a credential", keep)
		}
	}
	for _, gone := range []string{"CFW_API_KEY=", "PRV_API_KEY="} {
		if strings.Contains(env, gone) {
			t.Errorf("the child still holds %s", gone)
		}
	}
}

func TestRound120GatewayTokenIsScrubbedOnEveryDoor(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("OAICA_GATEWAY_TOKEN", "gw-secret-token")
	plan := tierPlan{Primary: launchEndpoint{Source: sourceRouter, RemoteEndpoint: RemoteEndpoint{Name: "oaica", TokenEnv: "OAICA_API_KEY"}}}
	for name, env := range map[string][]string{"directLaunchEnv": directLaunchEnv(), "claude childEnv": plan.childEnv("http://127.0.0.1:1", "tok")} {
		for _, kv := range env {
			if strings.HasPrefix(kv, "OAICA_GATEWAY_TOKEN=") {
				t.Errorf("%s: the child holds %s", name, kv)
			}
		}
	}
}

func TestRound120OaicaHostCredentialFormsAreDropped(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	for host, secret := range map[string]string{
		"https://user:PASSKEY@r.example":      "PASSKEY",
		"https://r.example/?key=QUERYKEY":     "QUERYKEY",
		"https://sk-router-hostkey@r.example": "sk-router-hostkey",
	} {
		t.Setenv("OAICA_HOST", host)
		for _, kv := range directLaunchEnv() {
			if strings.HasPrefix(kv, "OAICA_HOST=") {
				if strings.Contains(kv, secret) {
					t.Errorf("the child env still carries %q in %q", secret, kv)
				}
				if !strings.Contains(kv, "r.example") {
					t.Errorf("the host itself was lost: %q", kv)
				}
			}
		}
	}
}
