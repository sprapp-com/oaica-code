package launch

// endpoint_key_scope_integrity_test.go — the single-endpoint endpoint key and
// the `oaica serve` refusal disagreed with the router about two rows
// (2026-09-27 audit, round 22).
//
// 1. rejectServedModels was handed the whole MENU. managedSingleConfigureModels
//    returns the launch target plus every row the picker offers, and
//    prepareManagedSingleIntegration ran the refusal over that unfiltered list,
//    so one `oaica serve` running on the box refused an ordinary daemon-model
//    launch that never touched it — the exact failure singleEndpointModels was
//    added to prevent in round 19 (its own comment: "one row in the menu that
//    the user never picked refused an ordinary local launch"). The rows that
//    reach the store are the single-endpoint ones, and the refusal belongs to
//    them.
//
// 2. launchModelEndpointKey answered "daemon" for any bare name that looked
//    cloud-tagged, BEFORE asking whether a remote serves it — while the router
//    (resolveLaunchEndpoint) resolves user remotes first. A remote's own model
//    named "<id>:cloud" (an ollama box with a cloud model pulled returns
//    exactly such ids from /v1/models) was therefore keyed to the daemon: the
//    refusals stayed silent and the store was written with a namespaced remote
//    model the daemon does not serve. The Upstream arm keeps its priority — it
//    is the picker row's daemon-side identity, not a name.

import (
	"testing"
)

func TestAnUnselectedServeRowDoesNotRefuseAManagedSingleLaunch(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	serveRow := oneLiveServeRow(t)

	models := append([]LaunchModel{fallbackLaunchModel("llama3.2")}, serveRow...)
	if err := prepareManagedSingleIntegration(chatGPTIntegrationName, &CodexApp{}, "llama3.2", models); err != nil {
		t.Fatalf("prepareManagedSingleIntegration(chatgpt, llama3.2) = %v: the %q row is in the menu but not the launch, and a store that names one endpoint never write it — refusing the launch was worse (round 19)", err, serveRow[0].Name)
	}
}

// Control: selecting the serve row itself is still refused, so the fix above
// did not simply delete the refusal.
func TestASelectedServeRowIsStillRefusedByAManagedSingleLaunch(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	serveRow := oneLiveServeRow(t)

	models := append([]LaunchModel{serveRow[0]}, serveRow...)
	err := prepareManagedSingleIntegration(chatGPTIntegrationName, &CodexApp{}, serveRow[0].Name, models)
	if err == nil {
		t.Fatal("prepareManagedSingleIntegration(chatgpt, bonsai:local) = nil: the store dials the daemon, which does not serve that model")
	}
}

func TestARemoteCloudTaggedNameStaysWithItsRemote(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box","tool_format":"tool_calls"}]}`)

	const name = "box/gpt-oss:20b-cloud"
	if _, ok := resolveRemoteEndpoint(name); !ok {
		t.Fatalf("premise: %q does not resolve to the configured remote, so this test would not be about a remote's own cloud-tagged id", name)
	}

	row := LaunchModel{Name: name, Remote: true}
	if got := launchModelEndpointKey(row); got != "remote:https://box.example/v1" {
		t.Errorf("launchModelEndpointKey(%q) = %q: the router resolves user remotes first, so the key must not send this row to the daemon", name, got)
	}
	if daemonRoutedModel(row) {
		t.Error("daemonRoutedModel = true: the single-endpoint stores would be written with a model the daemon does not serve, and the refusals that catch that would stay silent")
	}
}
