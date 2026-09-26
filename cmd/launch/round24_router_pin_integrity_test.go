package launch

// round24_router_pin_integrity_test.go — a name that pins the OAICA router was
// read as a model the local daemon serves (2026-09-27 audit, round 24).
//
// launchModelEndpointKey answers "which endpoint does this row belong to", and
// daemonRoutedModel is that answer asked as "can a store that names the local
// daemon hold this row". A router SKU — the picker's "OAICA Models" rows, whose
// ids are the router's own ("oaica-35b-a3b-vision"), or a name carrying the
// explicit "router/"/"oaica/" prefix — fell through every arm to the daemon
// fallback. The ChatGPT app, the DeepSeek Harness and muse each name ONE base
// URL and one credential, both the daemon's, so the writer put the router's id
// into the store beside the daemon's endpoint: the launch reported success for
// a model the endpoint it wrote does not serve, and the first inference 404'd.
// Those three refusals are keyed on exactly this predicate, which is why they
// stayed silent.
//
// The prefix alone is not the test. `oaica/…` can name a USER REMOTE called
// "oaica", whose namespace is its own (round 21's rule for "ollama"), and the
// bare "oaica-…" form is how router SKUs reach the picker (tierItemName leaves
// them unprefixed so its pinned section can select exactly them).

import (
	"strings"
	"testing"
)

func TestARouterSkuIsNotADaemonRow(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box"}]}`)

	for _, tc := range []struct {
		name       string
		daemonSide bool
	}{
		{"oaica-35b-a3b-vision", false}, // the router's own id, bare — how the picker offers it
		{"router/oaica-35b-a3b-vision", false},
		{"oaica/oaica-35b-a3b-vision", false},
		{"llama3.2", true},         // the control: an ordinary local model
		{"kimi-k2.6:cloud", true},  // the control: the daemon's cloud spelling
		{"box/kat-awq", false},     // the control: a user remote
		{"box/oaica-thing", false}, // a remote's model, keyed by ITS endpoint — the remote arm runs first
		{"llama3.2:latest", true},  // a local tag
	} {
		got := daemonRoutedModel(LaunchModel{Name: tc.name})
		if got != tc.daemonSide {
			t.Errorf("daemonRoutedModel(%q) = %v, want %v — a store that names only the local daemon must not be told this row is one it can hold", tc.name, got, tc.daemonSide)
		}
	}
}

// TestAUserRemoteNamedOaicaKeepsItsNamespace: the router prefix is checked
// AFTER the remote arm, so `oaica/<id>` for a remote actually named "oaica" is
// that remote's model — and the refusal message must not claim the router for
// it. Same rule resolveLaunchEndpoint states for a remote named "router".
func TestAUserRemoteNamedOaicaKeepsItsNamespace(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"oaica","base_url":"https://oaica.example/v1","api_key":"sk-o"}]}`)

	row := LaunchModel{Name: "oaica/kat-awq"}
	// Asked of the NAME alone this pins the router — that is what the prefix
	// means — so this asserts the remote guard INSIDE routerPinnedName rather
	// than the arm order in launchModelEndpointKey: without the guard the key
	// would still come out "remote:…" (the remote arm runs first) while this
	// predicate answered true, and the refusal message would claim the router
	// for the remote's own model.
	if routerPinnedName(row.Name) {
		t.Error("a user remote named \"oaica\" had its model pinned to the router: its namespace is its own")
	}
	if daemonRoutedModel(row) {
		t.Error("a user remote named \"oaica\" was keyed to the local daemon: its base URL and key would be dropped as a no-op")
	}
	if strings.Contains(nonDaemonRowReason(row), "OAICA router") {
		t.Errorf("the refusal for a user remote named \"oaica\" claims the router: %q", nonDaemonRowReason(row))
	}
	if ep, ok := resolveRemoteEndpoint("oaica/kat-awq"); !ok || ep.Name != "oaica" || ep.UpstreamModel != "kat-awq" {
		t.Fatalf("premise: the remote did not resolve (%+v, %v)", ep, ok)
	}
}

// TestTheSingleEndpointWritersRefuseARouterSku is the consequence, driven
// through each store's own refusal: all three name the daemon and have nowhere
// to put the router's credential, so the launch must be refused where the
// spelling is the router's own id.
func TestTheSingleEndpointWritersRefuseARouterSku(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box"}]}`)

	router := LaunchModel{Name: "oaica-35b-a3b-vision"}
	rows := []LaunchModel{router, {Name: "llama3.2"}}

	// Control: an ordinary local model beside it is still accepted, so these
	// refusals are about the row and not about the list.
	if err := codexAppRejectNonDaemonModels("llama3.2", rows); err != nil {
		t.Fatalf("control: an ordinary local model was refused: %v", err)
	}

	if err := codexAppRejectNonDaemonModels(router.Name, rows); err == nil {
		t.Error("the ChatGPT app accepted a router SKU: its config names the local daemon and has no credential field, so the router's id would be posted to the daemon")
	} else if !strings.Contains(err.Error(), "router") {
		t.Errorf("the ChatGPT app's refusal does not say where the model actually is: %v", err)
	}
	if err := deepSeekHarnessRejectNonDaemonModels(router.Name, rows); err == nil {
		t.Error("the DeepSeek Harness accepted a router SKU: its settings carry the daemon's endpoint and one credential")
	}
	if err := museRejectNonDaemonModels([]LaunchModel{router}); err == nil {
		t.Error("muse accepted a router SKU: its catalog names the daemon's single endpoint")
	}

	// A user remote named "oaica" must not be told it is the router when it is
	// refused for the ordinary reason.
	if err := (&CodexApp{}).ConfigureWithModels(router.Name, rows); err == nil {
		t.Error("the ChatGPT app configured itself for a router SKU")
	}
}

// TestASingleEndpointStoreKeepsTheRouterRowsTogether is the filter half: a
// store written for a router primary may only hold rows that belong to the
// SAME endpoint, so the daemon's models drop out of it rather than being
// advertised from the daemon's catalogue while the router is dialled.
func TestASingleEndpointStoreKeepsTheRouterRowsTogether(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box"}]}`)

	rows := []LaunchModel{
		{Name: "oaica-35b-a3b-vision"},
		{Name: "oaica-small-7b"},
		{Name: "llama3.2"},
		{Name: "box/kat-awq"},
	}
	kept := singleEndpointModels("oaica-35b-a3b-vision", rows)
	if len(kept) != 2 {
		t.Fatalf("singleEndpointModels kept %d rows (%v), want the two router rows", len(kept), launchModelNames(kept))
	}
	for _, m := range kept {
		if daemonRoutedModel(m) {
			t.Errorf("%q survived a filter keyed to the router", m.Name)
		}
	}
}
