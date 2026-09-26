package launch

// round25_router_sku_store_integrity_test.go — the router SKU family was
// refused only where a store named it in its own right (2026-09-27 audit,
// round 25).
//
// Round 24 taught the single-endpoint stores the router family through
// daemonRoutedModel, and round 22 gave the shared launch backstop the
// singleEndpointModels filter. The Editor writers went on resolving every row
// as "user remote, else the daemon", so a selection of an OAICA router SKU —
// the picker's own "OAICA Models" rows, whose ids are the router's
// ("oaica-35b-a3b-vision"), or any "router/"/"oaica/" spelling — was written
// into Cline's providers.json, pi's providers, opencode's cached config or
// droid's settings against the LOCAL DAEMON, under the router's id. The launch
// reported success for a model the endpoint it wrote does not have, and the
// first inference 404'd.
//
// The refusal belongs at the store writers and not on the shared launch path:
// the runner path is also how `oaica launch claude` reaches a router SKU, and
// claude CAN express one — Claude.Run plans the tiers and starts the
// Anthropic->OpenAI proxy that carries the row to the router. Refusing it there
// would break the one integration the family exists for, so the two halves are
// pinned separately below.

import (
	"runtime"
	"strings"
	"testing"
)

// TestAStoreThatNamesTheDaemonRefusesARouterSku drives every editor store
// through the choke point they share. The row is the router's own bare id, as
// the picker offers it.
func TestAStoreThatNamesTheDaemonRefusesARouterSku(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box","tool_format":"tool_calls"}]}`)

	router := LaunchModel{Name: "oaica-35b-a3b-vision"}

	for _, tc := range []struct {
		name   string
		editor Editor
	}{
		{"pi", &Pi{}},
		{"opencode", &OpenCode{}},
		{"droid", &Droid{}},
		{"cline", &Cline{}},
	} {
		err := prepareEditorIntegration(tc.name, tc.editor, []LaunchModel{router})
		if err == nil {
			t.Errorf("prepareEditorIntegration(%s, %q) = nil: the store was written with the router's id beside the local daemon's endpoint, which has never heard of the model — the launch reports success and the first request 404s", tc.name, router.Name)
			continue
		}
		if !strings.Contains(err.Error(), "OAICA router") {
			t.Errorf("%s's refusal does not say where the model actually is: %v", tc.name, err)
		}
		if !strings.Contains(err.Error(), router.Name) {
			t.Errorf("%s's refusal does not name the row it refused: %v", tc.name, err)
		}
	}
}

// TestAStoreStillRefusesAServeRowWhenTheRouterCheckRuns is the pairing half:
// the new refusal is additive, so `oaica serve`'s own row is still refused with
// its own reason rather than the router's.
func TestAStoreStillRefusesAServeRowWhenTheRouterCheckRuns(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box","tool_format":"tool_calls"}]}`)

	err := prepareEditorIntegration("droid", &Droid{}, []LaunchModel{{Name: "bonsai:local", LiveSource: liveSourceLocal}})
	if err == nil {
		t.Fatal("prepareEditorIntegration(droid, bonsai:local) = nil: the row is served by `oaica serve`, not by the daemon the store names")
	}
	if !strings.Contains(err.Error(), "oaica serve") {
		t.Errorf("refusal = %v, want the `oaica serve` reason for a serve row", err)
	}
}

// TestADaemonListedRouterShapedRowIsStillWritten is the control the whole rule
// rests on: provenance outranks a name shape. A model the DAEMON lists under a
// router-shaped id (LiveSource liveSourceDaemon, as daemonPickerRows sets it) is
// an ordinary daemon model, and refusing it would make an unrelated launch
// impossible.
func TestADaemonListedRouterShapedRowIsStillWritten(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)

	editor := &launcherEditorRunner{}
	rows := []LaunchModel{
		{Name: "oaica-small-7b", LiveSource: liveSourceDaemon},
		{Name: "llama3.2", LiveSource: liveSourceDaemon},
	}
	if err := prepareEditorIntegration("stubeditor", editor, rows); err != nil {
		t.Fatalf("prepareEditorIntegration over daemon-listed rows = %v: a model the daemon serves under a router-shaped name is a daemon model, and the picker can offer one", err)
	}
	if len(editor.edited) != 1 {
		t.Fatalf("the store was written %d times, want once", len(editor.edited))
	}
}

// TestTheRunnerPathStillCarriesARouterSku is the other side of the scoping
// decision: launchAfterConfiguration is shared with `oaica launch claude`, which
// reaches a router SKU through Claude.Run's tier plan and its local
// Anthropic->OpenAI proxy. The refusal must not follow the row onto that path.
func TestTheRunnerPathStillCarriesARouterSku(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake integration is an sh stub on PATH")
	}
	setTestHome(t, t.TempDir())
	binDir := t.TempDir()
	writeFakeBinary(t, binDir, "claude")
	t.Setenv("PATH", binDir)

	runner := &launcherManagedRunner{}
	withIntegrationOverride(t, "claude", runner)

	models := []LaunchModel{{Name: "oaica-35b-a3b-vision"}}
	if err := launchAfterConfiguration("claude", runner, "oaica-35b-a3b-vision", models, IntegrationLaunchRequest{}); err != nil {
		t.Errorf("launchAfterConfiguration(claude, router SKU) = %v, want success: claude reaches a router SKU through its tier plan and its own proxy, and the store-writer refusal must not be applied to the row being RUN", err)
	}
	if runner.ranModel != "oaica-35b-a3b-vision" {
		t.Errorf("ranModel = %q, want the router SKU", runner.ranModel)
	}
}
