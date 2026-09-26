package launch

// cross_host_models_path_integrity_test.go — an absolute models_path on ANOTHER
// HOST sent a remote's API key there (2026-09-26 audit, eleventh round).
//
// models_path may be absolute (user_remotes.go's modelsURL: a vendor whose list
// is versioned differently from its chat endpoint), and every consumer of the
// resulting URL attaches the row's credential to it — fetchRemoteModels, doctor's
// probeRemote, the context-window probe, and the proxy's own GET /v1/models
// passthrough. Nothing checked that the absolute URL was still on the row's own
// host, so a row reading
//
//	{"base_url":"https://box.example/v1","models_path":"https://collect.example/v1/models","api_key":"sk-..."}
//
// handed the key to collect.example on every picker sweep, every probe and every
// client model-list request. A key is the key for base_url's account; where it
// may be SENT is not the row's business to widen, and remotes.json is a file
// people copy between machines.
//
// No shipped row uses an absolute models_path on another host today — the one
// absolute path in the catalog is same-host — which is exactly why this is the
// latent half: the check belongs where the credential is attached, not in the
// row that happens to be safe right now.

import (
	"net/url"
	"strings"
	"testing"
)

const crossHostModelsPathRemotes = `{"remotes":[{"name":"box","base_url":"https://box.example/v1","models_path":"https://collect.example/v1/models","api_key":"sk-box-SECRET-1234567890","tool_format":"tool_calls"}]}`

func TestAnAbsoluteModelsPathOnAnotherHostIsRefused(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	withTempRemotesFile(t)
	writeRemotes(t, crossHostModelsPathRemotes)

	ep, ok := resolveRemoteEndpoint("box/big-model")
	if !ok {
		t.Fatal("premise: the row no longer resolves as a user remote")
	}
	if host := hostOfURL(t, ep.ModelsURL); host == "collect.example" {
		t.Errorf("ModelsURL = %q — oaica would send this row's API key to %q on every model-list fetch, doctor probe, context-window probe and client GET /v1/models; the key belongs to box.example", ep.ModelsURL, host)
	}
	// And the credential-bearing consumers take ModelsURL as it stands, so the
	// route built for the launch must not carry the foreign host either.
	leg := launchEndpoint{RemoteEndpoint: ep, Source: sourceUserRemote}
	if host := hostOfURL(t, routeFor(leg).ModelsURL); host == "collect.example" {
		t.Errorf("the launch route's ModelsURL = %q, still the foreign host", routeFor(leg).ModelsURL)
	}

	// The user must also be told, not left with a silently different list.
	stderr := captureStderr(t, func() { _, _ = loadUserRemotes() })
	if !strings.Contains(stderr, "models_path") {
		t.Errorf("loading the row printed %q — a models_path that is ignored must be named, or the user cannot tell why their model list came from somewhere else", stderr)
	}
}

// Controls: the two shapes models_path exists for must keep working.
func TestModelsPathOnTheRowsOwnHostIsHonoured(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	withTempRemotesFile(t)

	for _, tc := range []struct {
		name, modelsPath, want string
	}{
		{"absolute, same host", "https://box.example/api/v4/models", "https://box.example/api/v4/models"},
		{"relative", "/api/v4/models", "https://box.example/v1/api/v4/models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","models_path":"`+tc.modelsPath+`","api_key":"sk-box-SECRET-1234567890","tool_format":"tool_calls"}]}`)
			ep, ok := resolveRemoteEndpoint("box/big-model")
			if !ok {
				t.Fatalf("premise: the %s row no longer resolves", tc.name)
			}
			if ep.ModelsURL != tc.want {
				t.Errorf("ModelsURL = %q, want %q", ep.ModelsURL, tc.want)
			}
		})
	}
}

func hostOfURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("ModelsURL %q does not parse: %v", raw, err)
	}
	return u.Hostname()
}
