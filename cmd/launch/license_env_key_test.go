package launch

// license_env_key_test.go — README documents OAICA_LICENSE_KEY as the
// alternative to running `oaica activate` ("per machine (or `export
// OAICA_LICENSE_KEY=...`)"), and lists it in the environment-variable table as
// the licence key for gated models. The launch gate read ~/.oaica/license.json
// and nothing else, so a user who took the documented alternative was told to
// go buy the licence they had already exported (2026-09-26 audit). Only
// `pull`/`serve` read the variable, which is why the mismatch survived: the
// promise is about `launch`.
//
// The env key is validated and never persisted — it stays the deployment's
// secret rather than becoming a file this process drops in the user's home.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLicenseGateAcceptsTheDocumentedEnvVar(t *testing.T) {
	dir := t.TempDir()
	setLaunchTestHome(t, dir)

	licensePath := filepath.Join(dir, ".oaica", "license.json")
	if _, err := os.Stat(licensePath); err == nil {
		t.Fatalf("premise: %s already exists, so this test cannot tell the env path from the file path", licensePath)
	}

	t.Setenv("OAICA_LICENSE_KEY", testLicenseKey)
	if err := requireLicenseLive(nil, nil); err != nil {
		t.Errorf("with OAICA_LICENSE_KEY set to a valid key and no license.json, the launch gate refused: %v\n— README documents the env var as the alternative to `oaica activate`, and this is that sentence made executable", err)
	}

	// The key's presence in the environment must not leave an activation
	// file behind: a secret manager's key is not this process's to write.
	if _, err := os.Stat(licensePath); err == nil {
		t.Errorf("the env-supplied key wrote %s — an injected secret must not be persisted to the user's home", licensePath)
	}

	// And with neither an env key nor a file, the gate still refuses — this
	// is a new way to satisfy the gate, not a way to remove it.
	t.Setenv("OAICA_LICENSE_KEY", "")
	if err := requireLicenseLive(nil, nil); err == nil {
		t.Error("the launch gate passed with no license.json and no OAICA_LICENSE_KEY — the gate no longer gates")
	}
}
