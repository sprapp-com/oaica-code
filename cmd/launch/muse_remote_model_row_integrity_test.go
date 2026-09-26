package launch

// muse_remote_model_row_integrity_test.go — muse was the one editor in this
// package with no remote translation, so a user-remote picker row was written
// into muse's settings as a DAEMON model (2026-09-26 audit, fourteenth round).
//
// Every sibling translates a remote row to the remote's own endpoint and model
// id (clineModelIDFor/clineProviderBaseURLFor, qwenModelIDFor/qwenBaseURLFor,
// piModelIDFor, droid, hermes, omp, opencode, vscode, openclaw). muse
// wrote model.Name — the picker string "box/big-model" — into both the
// top-level model and every model_catalog[].model_id, against
// envconfig.ConnectableHost()+"/v1" with auth "none". The daemon does not
// resolve namespaced remotes, so every request failed model-not-found, after
// the launch had already prompted for the remote's key.
//
// Muse cannot be fixed by translation: endpoint_transport is ONE global
// provider switch for the whole document (see the Muse doc comment), so a
// selection mixing a daemon model with a remote one is unrepresentable, and a
// remote's credential has no field oaica can write — the schema is not
// published and muse is not installed here to read it off. The write is
// therefore refused, before the file is touched, with the model named.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMuseRefusesARemoteModel: the write must fail loudly and leave the store
// alone, rather than publishing a config that cannot work.
func TestMuseRefusesARemoteModel(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box-SECRET","tool_format":"tool_calls"}]}`)
	if _, ok := resolveRemoteEndpoint("box/big-model"); !ok {
		t.Fatal("premise: the box/big-model row does not resolve, so it would never be treated as a remote at all")
	}

	err := (&Muse{}).Edit([]LaunchModel{{Name: "box/big-model", Remote: true}})
	if err == nil {
		t.Fatalf("Muse.Edit of a remote model succeeded; want a refusal — muse's settings carry a single endpoint for every catalog row, so the picker name %q would be sent to the local daemon, which does not resolve namespaced remotes", "box/big-model")
	}
	if !strings.Contains(err.Error(), "box/big-model") {
		t.Errorf("refusal %q does not name the model it refused", err)
	}

	settingsPath, perr := museSettingsPath()
	if perr != nil {
		t.Fatal(perr)
	}
	data, rerr := os.ReadFile(settingsPath)
	if rerr == nil && strings.Contains(string(data), "box/big-model") {
		t.Errorf("the refused model is in the settings file anyway:\n%s", string(data))
	}
}

// Control: a daemon-backed model still writes, and the settings file keeps the
// shape muse reads — so the refusal is scoped to remote rows, not to the store.
func TestMuseStillWritesDaemonModels(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box-SECRET"}]}`)

	if err := (&Muse{}).Edit(testLaunchModels("qwen3:8b")); err != nil {
		t.Fatalf("Muse.Edit of a daemon model = %v, want success", err)
	}

	settingsPath, err := museSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if !strings.Contains(string(data), "qwen3:8b") {
		t.Errorf("the daemon model is missing from the settings file:\n%s", string(data))
	}
	if !strings.Contains(string(data), filepath.Join("muse", "settings.json")) && !strings.Contains(string(data), "endpoint_transport") {
		t.Errorf("the settings file lost its transport block:\n%s", string(data))
	}
}
