package launch

// omp_single_endpoint_menu_remote_row_integrity_test.go — OMP's models.yml
// names ONE endpoint per provider entry, and the picker MENU is not a
// selection (2026-09-27 audit, round 19).
//
// `oaica launch omp` is handed `managedSingleConfigureModels`: the launch
// target plus every row the picker offers, resolved back to LaunchModels with
// their endpoint flags. writeOMPModelsConfig writes every one of them into the
// single `ollama` provider entry, whose baseUrl, api and credential come from
// the PRIMARY alone. A row routed somewhere else — a configured user remote
// when the launch is a daemon model, or the daemon's own models when the
// launch is a remote — is then advertised to OMP on an endpoint that cannot
// serve it: the model is offered, selected, and posted to the wrong host.
//
// The endpoint is what decides who belongs in that list, not the mere presence
// of a Remote flag: an ollama-cloud row ("glm-5.1:cloud") IS reached through
// the daemon and stays.

import (
	"os"
	"strings"
	"testing"
)

func ompMenuWithRemoteRows(t *testing.T) []LaunchModel {
	t.Helper()
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box-SECRET","tool_format":"tool_calls"},
		{"name":"zai","base_url":"https://zai.example/v1","api_key":"sk-zai-SECRET","tool_format":"tool_calls"}]}`)
	if _, ok := resolveRemoteEndpoint("box/big-model"); !ok {
		t.Fatal("premise: the box/big-model row does not resolve, so it would never be treated as a remote at all")
	}
	if _, ok := resolveRemoteEndpoint("zai/glm-4.6"); !ok {
		t.Fatal("premise: the zai/glm-4.6 row does not resolve")
	}

	menu := launchModelsFromNames([]string{"llama3.2"})
	return append(menu,
		LaunchModel{Name: "box/big-model", Remote: true},
		LaunchModel{Name: "zai/glm-4.6", Remote: true},
		LaunchModel{Name: "glm-5.1:cloud", Remote: true, Upstream: "glm-5.1:cloud"},
	)
}

func TestOMPDaemonLaunchAdvertisesOnlyTheRowsTheDaemonServes(t *testing.T) {
	home := t.TempDir()
	setOMPTestHome(t, home)
	menu := ompMenuWithRemoteRows(t)

	if err := writeOMPModelsConfig("llama3.2", menu); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ompModelsFixturePath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)

	if strings.Contains(text, "big-model") || strings.Contains(text, "glm-4.6") {
		t.Errorf("OMP offers remote rows on the daemon endpoint — the provider entry holds one baseUrl and one credential, both the daemon's, so those rows are posted to the daemon, which does not resolve them:\n%s", text)
	}
	if !strings.Contains(text, "llama3.2") {
		t.Errorf("the selected daemon model is missing:\n%s", text)
	}
	// The control: an ollama-cloud row is proxied BY the daemon, so it is on
	// this endpoint and must survive the filter.
	if !strings.Contains(text, "glm-5.1:cloud") {
		t.Errorf("an ollama-cloud row was dropped even though the daemon serves it:\n%s", text)
	}
}

func TestOMPRemoteLaunchAdvertisesOnlyThatRemotesRows(t *testing.T) {
	home := t.TempDir()
	setOMPTestHome(t, home)
	menu := ompMenuWithRemoteRows(t)

	if err := writeOMPModelsConfig("box/big-model", menu); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ompModelsFixturePath(t, home))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)

	if !strings.Contains(text, "baseUrl: https://box.example/v1") {
		t.Fatalf("the provider is not pointed at the selected remote:\n%s", text)
	}
	if !strings.Contains(text, "id: big-model") {
		t.Errorf("the selected remote model is missing from its own provider entry:\n%s", text)
	}
	for _, foreign := range []string{"id: glm-4.6", "id: llama3.2", "id: glm-5.1:cloud"} {
		if strings.Contains(text, foreign) {
			t.Errorf("OMP offers %q on the box endpoint, which does not serve it:\n%s", strings.TrimPrefix(foreign, "id: "), text)
		}
	}
}
