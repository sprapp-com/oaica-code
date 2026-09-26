package launch

// local_serve_integration_refusal_integrity_test.go — a `oaica serve` row was
// written into an integration's config as a DAEMON model (2026-09-27 audit,
// round 21).
//
// Every integration writer resolves a launch model as "user remote, else the
// local daemon" (resolveRemoteEndpoint) and writes the daemon's base URL, the
// daemon's key and the row's own name for anything the remote lookup does not
// claim. A "<model>:local" row is a separate process on its own origin with its
// own credential, so the written config named an endpoint that has never heard
// of the model — the launch reported success and the first inference 404'd.
//
// The row was reachable in this path only since round 20: hasLocalModel used to
// skip the local-serve family, so the picker reopened and no writer ever saw
// one (local_serve_row_usable_integrity_test.go pins the readiness half). The
// refusal below says so instead of writing the wrong endpoint; routing a served
// model into a foreign config format is the follow-up.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnOaicaServeRowIsRefusedByAnIntegrationWriter(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	inventory := oneLiveServeRow(t)

	err := prepareEditorIntegration("cline", &Cline{}, inventory)
	if err == nil {
		t.Fatal("prepareEditorIntegration(cline, bonsai:local) = nil: the row was written into Cline's providers.json against the local daemon, which does not serve it")
	}
	if !strings.Contains(err.Error(), "bonsai:local") {
		t.Errorf("the refusal does not name the row it refused: %v", err)
	}
	if !strings.Contains(err.Error(), "oaica serve") {
		t.Errorf("the refusal does not say where the model is served: %v", err)
	}
	if _, statErr := os.Stat(clineProvidersPath(home)); !os.IsNotExist(statErr) {
		t.Errorf("Cline's providers.json was written despite the refusal (stat: %v)", statErr)
	}
}

// Control: an ordinary daemon row is still configured, so the check above is
// not passing because the writer refuses everything.
func TestADaemonRowIsStillWrittenByAnIntegrationWriter(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	if err := prepareEditorIntegration("cline", &Cline{}, []LaunchModel{fallbackLaunchModel("llama3.2")}); err != nil {
		t.Fatalf("prepareEditorIntegration(cline, llama3.2): %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".cline", "data", "settings", "providers.json")); statErr != nil {
		t.Errorf("Cline's providers.json was not written for a daemon row: %v", statErr)
	}
}
