package launch

// openclaw_null_device_scope_integrity_test.go — a `null` device record in the
// gateway's pairing file panicked the launch (2026-09-27 audit, round 20).
//
// patchDeviceScopes rewrites ~/.openclaw/devices/paired.json to add the scopes
// the current gateway baseline expects. It looks its device up with
// `dev, ok := devices[deviceID]`, which is TRUE for a key whose value is JSON
// `null` — the key exists — while dev itself is nil, and patchScopes then writes
// `obj[key] = existing` into that nil map: "assignment to entry in nil map",
// from a best-effort helper that only ever means to return quietly. It runs
// during a launch, so the launch dies with a Go stack trace instead of the
// re-pair hint the integration prints for every other failure.
//
// Same class as the `null` document the round's json_null_document_integrity
// test covers, one level down: the document parses, but a member of it is null.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenclawPatchScopesSurvivesANullDeviceRecord(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	identityDir := filepath.Join(home, ".openclaw", "identity")
	devicesDir := filepath.Join(home, ".openclaw", "devices")
	for _, dir := range []string{identityDir, devicesDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(identityDir, "device-auth.json"),
		[]byte(`{"deviceId":"dev-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	paired := filepath.Join(devicesDir, "paired.json")
	// dev-1 is the local device, and its record is the literal null. dev-2 is an
	// ordinary record, so the patch below still has work to do.
	seed := `{"dev-1":null,"dev-2":{"scopes":["operator.read"],"tokens":{}}}`
	if err := os.WriteFile(paired, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	patchDeviceScopes()

	data, err := os.ReadFile(paired)
	if err != nil {
		t.Fatalf("the pairing file is gone: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("the pairing file is no longer JSON: %v\n%s", err, data)
	}
	if v, ok := doc["dev-1"]; !ok || v != nil {
		t.Errorf("the null record was replaced with something oaica invented: %v", v)
	}
}
