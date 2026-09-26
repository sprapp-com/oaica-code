package launch

// bare_local_name_not_claimed_by_remote_integrity_test.go — a bare model name
// the LOCAL daemon serves was claimed by a user remote that happens to
// advertise the same id (2026-09-27 audit, round 17).
//
// resolveBareRemoteModel exists so `--model kat-awq` — the exact id a user's
// own box shows — reaches that box instead of the pull path. It asked only
// whether exactly ONE configured remote advertises the id, and never whether
// the local daemon serves it. So a user with a LAN box (or a second ollama
// host) configured as a remote, serving an id they also have pulled locally,
// had the LOCAL picker row re-resolved as the remote's: the editors wrote that
// remote's base URL AND its bearer token into the integration's config, and the
// launch silently ran on the other machine.
//
// Round 16 made the same collision abort instead: pi and openclaw refuse a
// mixed daemon+remote selection, and both classified the local row as the
// remote's, so the launch died naming a remote the user never selected.
//
// The rule this pins: a bare id is a local model's name (ollama's own naming);
// only "<remote>/<id>" names a remote in the picker. The bare-to-remote mapping
// stays for ids the daemon does NOT serve — that is the trap it was written for.

import (
	"testing"
)

func TestABareNameTheLocalDaemonServesIsNotClaimedByARemote(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[
	  {"name":"box","base_url":"http://third-party.invalid/v1","api_key":"KEY_BOX","tool_format":"tool_calls"}
	]}`)

	// One remote advertises "shared-model"; the local daemon serves it too.
	// "box-only-model" is advertised by the remote and is NOT local — the
	// kat-awq case the bare mapping exists for.
	stubUserRemoteModels(t, []LaunchModel{
		{Name: "box/shared-model"},
		{Name: "box/box-only-model"},
	}, nil)
	stubLocalDaemonIDs(t, map[string]bool{
		"shared-model":    true,
		"shared-model:latest": true,
	})

	// The write half: nothing that stores an endpoint may classify the local
	// name as the remote's.
	ep, ok := resolveRemoteEndpoint("shared-model")
	if ok {
		t.Errorf("resolveRemoteEndpoint(\"shared-model\") resolved to the remote %q at %s with token %q, but the local daemon serves that id — the model the user pulled runs on the other machine",
			ep.Name, ep.BaseURL, ep.Token)
	}
	if got, want := clineModelIDFor("shared-model"), "shared-model"; got != want {
		t.Errorf("clineModelIDFor(local bare name) = %q, want %q", got, want)
	}
	if got, want := clineProviderBaseURLFor("shared-model"), clineProviderBaseURL(); got != want {
		t.Errorf("clineProviderBaseURLFor(local bare name) = %q, want the daemon's %q", got, want)
	}

	// And the refusal paths must not fire on a local-only selection: two local
	// models fit pi's one provider slot.
	if err := piRejectMixedEndpoints([]LaunchModel{{Name: "shared-model"}, {Name: "qwen3:8b"}}); err != nil {
		t.Errorf("a selection of two local models was refused because one of them shares an id with a remote: %v", err)
	}

	// Control: the bare id NO daemon serves still resolves to its remote.
	ep, ok = resolveRemoteEndpoint("box-only-model")
	if !ok {
		t.Fatalf("the bare id only the remote serves no longer resolves to it — the `--model <remote's own id>` case is broken")
	}
	if ep.Name != "box" {
		t.Errorf("resolveRemoteEndpoint(\"box-only-model\") named remote %q, want box", ep.Name)
	}
}
