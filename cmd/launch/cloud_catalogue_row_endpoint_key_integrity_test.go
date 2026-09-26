package launch

// cloud_catalogue_row_endpoint_key_integrity_test.go — two name-shaped bugs in
// the endpoint key the single-endpoint writers share (2026-09-27 audit,
// round 21, found by reviewing round 20's own fix).
//
// 1. launchModelEndpointKey resolved the row's NAME, and by then the name is not
//    the row's identity: findLaunchModel strips the "ollama/" picker prefix, so
//    an ollama-cloud catalogue row ("ollama/glm-5.3", daemon-side
//    "glm-5.3:cloud" in LaunchModel.Upstream) arrives as the bare "glm-5.3" —
//    which an unrelated user remote serving that same bare id claims. The row
//    the daemon serves was then routed to the remote, and the round-20 refusals
//    rejected it with a reason that was false for it. Colliding ids are
//    ordinary: the shipped catalogues use the same bare ids as ollama.com.
//
// 2. The same strip removed the namespace from a row whose remote is literally
//    named "ollama" ("ollama/meta-llama/llama-3"), so no remote lookup could
//    claim it and it fell through to "the daemon" — a false ACCEPT, writing a
//    model only the remote serves into a store that dials the daemon.

import (
	"testing"
)

func catalogueCloudRow(t *testing.T) LaunchModel {
	t.Helper()
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box","tool_format":"tool_calls"}]}`)

	old := bareRemoteModelIndex
	bareRemoteModelIndex = func() map[string][]string {
		return map[string][]string{"glm-5.3": {"box/glm-5.3"}}
	}
	t.Cleanup(func() { bareRemoteModelIndex = old })

	if _, ok := resolveRemoteEndpoint("glm-5.3"); !ok {
		t.Fatal("premise: the bare id does not resolve to the remote, so this test would not be about a colliding id")
	}
	return LaunchModel{Name: "glm-5.3", Remote: true, Upstream: "glm-5.3:cloud"}.WithCloudLimits()
}

func TestADaemonServedCloudCatalogueRowIsNotClaimedByARemote(t *testing.T) {
	row := catalogueCloudRow(t)

	if got := launchModelEndpointKey(row); got != "daemon" {
		t.Errorf("launchModelEndpointKey(glm-5.3 with upstream glm-5.3:cloud) = %q: the row is served by the daemon, which proxies the cloud id; only its NAME collides with a remote's bare id", got)
	}
	if !daemonRoutedModel(row) {
		t.Error("daemonRoutedModel = false for a daemon-proxied cloud row, so the single-endpoint refusals reject a model the daemon serves")
	}
	if err := (&CodexApp{}).ConfigureWithModels("glm-5.3", []LaunchModel{row}); err != nil {
		t.Errorf("the ChatGPT app refused a cloud model the daemon serves: %v", err)
	}
}

func TestARemoteNamedOllamaKeepsItsNamespace(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"ollama","base_url":"https://ollama-remote.example/v1","api_key":"sk-remote","tool_format":"tool_calls"}]}`)

	const name = "ollama/meta-llama/llama-3"
	if _, ok := resolveRemoteEndpoint(name); !ok {
		t.Fatalf("premise: %q does not resolve to the remote named ollama, so this test would not be about the strip", name)
	}

	row, ok := findLaunchModel([]LaunchModel{{Name: name, Remote: true}}, name)
	if !ok {
		t.Fatal("premise: the row is not found by its own name")
	}
	if row.Name != name {
		t.Errorf("findLaunchModel returned the name %q for the row %q: the ollama/ prefix is display-only for a DAEMON row, and a user remote literally named ollama keeps its namespace", row.Name, name)
	}
	if daemonRoutedModel(row) {
		t.Errorf("daemonRoutedModel = true for %q: the row is served by the remote, so a store that dials the daemon would be written with a model it cannot serve", name)
	}
}
