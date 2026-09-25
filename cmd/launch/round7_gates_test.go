package launch

// round7_gates_test.go — two unprompted-egress paths the round-6 audit's
// consent sweep did not reach (2026-09-26 audit, second round):
//
//   - `hermes desktop` against an older hermes ran `hermes update` — a
//     network install that replaces the user's installed binary — with no
//     prompt, contradicting docs/ENTERPRISE.md row 6's "none of these
//     installers run unprompted";
//   - GET /v1/models relayed to the default leg's credential-backed upstream
//     without ever consulting the entitlement gate, so a caller denied that
//     leg still got its model inventory.

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// P3: an armed gate denies the model list too.
func TestModelsEndpointIsBehindTheEntitlementGate(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	hits := 0
	upstream := openAITestUpstream(&hits)
	defer upstream.Close()
	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "deepseek", BaseURL: upstream.URL + "/v1", Token: "sk-remote",
		UpstreamModel: "deepseek-v4-flash", Wire: "openai",
	}})

	withEntitlementGate(t, true, denyAllEntitlementCheck)

	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"deepseek/deepseek-v4-flash": route})
	resp, err := http.Get(proxy + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /v1/models answered %d with an armed deny-all gate, want 403 — the model list is served on this leg's credentials and must be gated with it", resp.StatusCode)
	}
	if hits != 0 {
		t.Errorf("the denied model list still reached the upstream: %d hit(s)", hits)
	}
}

// hermesStubBinary writes a `hermes` that reports version and logs every argv.
func hermesStubBinary(t *testing.T, dir, version string) (bin, logPath string) {
	t.Helper()
	logPath = filepath.Join(dir, "hermes.log")
	bin = filepath.Join(dir, "hermes")
	stub := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"--version\" ]; then echo \"hermes " + version + "\"; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

// P4: an old hermes is not updated without consent.
func TestHermesUpdateIsBehindAConsentGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hermes stub is a POSIX shell script")
	}

	for _, tc := range []struct {
		name       string
		approve    bool
		wantUpdate bool
	}{
		{name: "declined", approve: false},
		{name: "approved", approve: true, wantUpdate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			setTestHome(t, dir)
			bin, logPath := hermesStubBinary(t, dir, "v0.15.0") // older than hermesDesktopMinVersion

			prompts := 0
			old := DefaultConfirmPrompt
			DefaultConfirmPrompt = func(string, ConfirmOptions) (bool, error) {
				prompts++
				return tc.approve, nil
			}
			t.Cleanup(func() { DefaultConfirmPrompt = old })

			err := (&HermesDesktop{}).ensureHermesDesktopMinVersion(bin)
			if prompts == 0 {
				t.Fatalf("an outdated hermes was updated with no prompt shown at all — ENTERPRISE.md row 6 promises 'none of these installers run unprompted'")
			}

			ran, readErr := os.ReadFile(logPath)
			if readErr != nil {
				t.Fatalf("the hermes stub never ran, so this case proves nothing: %v", readErr)
			}
			updated := strings.Contains(string(ran), "update")

			if !tc.approve {
				if updated {
					t.Errorf("a DECLINED prompt still ran `hermes update`:\n%s", ran)
				}
				if err == nil {
					t.Error("a declined update returned nil — the launch would proceed with a hermes below the version floor it needs")
				}
			}
			if tc.approve && !updated {
				t.Errorf("an APPROVED prompt did not run `hermes update`, so this case is not exercising the update path:\n%s", ran)
			}
		})
	}
}
