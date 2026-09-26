package launch

// local_serve_row_usable_integrity_test.go — a `oaica serve` row was reported
// unusable, so a launch of a model that is already live on this machine fell
// back to the picker (2026-09-27 audit, round 20).
//
// hasLocalModel answers "can this name be launched without pulling anything".
// Its loop skips every LaunchModel with Remote set, with a comment saying a
// user-defined remote needs no local pull — true — and the picker marks the
// local-serve family Remote as well (localServePickerRows), so a
// "<model>:local" row is skipped by the same continue. Nothing else in the
// function recognises it: it is not a user remote, isCloudModelName does not
// match it, and it is not native or a plan row. So `oaica launch opencode
// --model bonsai:local` — a model running on this very box — was answered
// "unusable", which reopens the picker, and on a headless `--yes` run fails
// with "model selection requires an interactive terminal".
//
// The same skip is right for a remote (its own endpoint serves it, and the
// name is not in this inventory) and wrong for a serve row, which IS in the
// inventory and is its own origin.

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// oneLiveServeRow returns the picker's inventory for a machine with exactly one
// `oaica serve`, built by the production producer (localServePickerRows) rather
// than by hand.
func oneLiveServeRow(t *testing.T) []LaunchModel {
	t.Helper()
	invServeSetup(t)
	backendPort := invServeBackend(t, "bonsai")
	port := invServeStart(t, "127.0.0.1", "", backendPort)
	writeLocalRegistry(t, []oaicaLocalServersRegistryEntry{{
		Model:     "bonsai",
		Origin:    fmt.Sprintf("http://127.0.0.1:%d", port),
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}})

	inventory := localServePickerRows()
	if len(inventory) != 1 {
		t.Fatalf("premise: the picker produced %d local-serve rows for one live server, so this test would not be about the row it names", len(inventory))
	}
	if inventory[0].Name != "bonsai:local" {
		t.Fatalf("premise: the row is named %q, want bonsai:local", inventory[0].Name)
	}
	return inventory
}

func TestAnOaicaServeRowIsUsable(t *testing.T) {
	inventory := oneLiveServeRow(t)

	if !hasLocalModel(inventory, "bonsai:local") {
		t.Errorf("hasLocalModel(bonsai:local) = false although the row is in the inventory and its server answers /health: the loop skips every row the picker marks Remote, which includes the local-serve family")
	}
	c := &launcherClient{}
	if !c.singleModelUsable(t.Context(), "bonsai:local", inventory) {
		t.Errorf("singleModelUsable(bonsai:local) = false: the model is served live on this machine, so the launch reopens the picker and a headless --yes run dies with \"model selection requires an interactive terminal\"")
	}
}

// Control: a name nothing serves is still unusable, so the check above is not
// passing because hasLocalModel answers true unconditionally.
func TestAnUnknownModelIsStillUnusable(t *testing.T) {
	inventory := oneLiveServeRow(t)

	if hasLocalModel(inventory, "not-a-model") {
		t.Error("hasLocalModel(not-a-model) = true, so this check proves nothing about the serve row")
	}
}
