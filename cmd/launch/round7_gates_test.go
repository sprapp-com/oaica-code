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
	"strconv"
	"strings"
	"testing"
)

// P3: an armed gate denies the model list on every leg the gate governs — and
// does NOT gate api.anthropic.com, whose /v1/messages is deliberately open.
func TestModelsEndpointIsBehindTheEntitlementGate(t *testing.T) {
	// The remote legs: the catalogue is served on their credentials, so a
	// caller denied the leg must not get its inventory.
	t.Run("openai-wire remote", func(t *testing.T) {
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
	})

	t.Run("anthropic-wire remote (a plan row)", func(t *testing.T) {
		setLaunchTestHome(t, t.TempDir())
		hits := 0
		upstream := anthropicTestUpstream(&hits)
		defer upstream.Close()
		route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
			Name: "zai-coding-plan", BaseURL: upstream.URL, Token: "sk-remote",
			UpstreamModel: "glm-5.3", Wire: "anthropic",
		}})
		if !route.NativePassthrough {
			t.Fatalf("setup: an anthropic-wire remote is no longer NativePassthrough: %+v", route)
		}

		withEntitlementGate(t, true, denyAllEntitlementCheck)

		proxy := startEntitlementTestProxy(t, route, nil)
		resp, err := http.Get(proxy + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET /v1/models answered %d for a denied anthropic-wire remote, want 403", resp.StatusCode)
		}
		if hits != 0 {
			t.Errorf("the denied model list still reached the remote: %d hit(s)", hits)
		}
	})

	// The native claude/* leg: entitlement.go's contract is that it is NOT
	// gated (api.anthropic.com under the user's own credential). The gate has
	// to agree with POST /v1/messages on the same leg, or Claude Code gets a
	// 403 for the catalogue it validates models against while completions
	// still work.
	t.Run("native claude/* leg stays ungated, like its /v1/messages", func(t *testing.T) {
		setLaunchTestHome(t, t.TempDir())
		hits := 0
		upstream := anthropicTestUpstream(&hits)
		defer upstream.Close()
		prevUpstream := nativeAnthropicUpstream
		nativeAnthropicUpstream = upstream.URL
		t.Cleanup(func() { nativeAnthropicUpstream = prevUpstream })
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

		route := routeFor(launchEndpoint{Source: sourceNativeAnthropic, RemoteEndpoint: RemoteEndpoint{
			Name: "native-anthropic", UpstreamModel: "claude-opus-5", Wire: "anthropic",
		}})

		withEntitlementGate(t, true, denyAllEntitlementCheck)

		proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{route.UpstreamModel: route})
		resp, err := http.Get(proxy + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		msgCode, msgBody, _ := postEntitlementTestMessage(t, proxy, route.UpstreamModel)
		if (resp.StatusCode == http.StatusForbidden) != (msgCode == http.StatusForbidden) {
			t.Errorf("an armed gate answered GET /v1/models with %d but POST /v1/messages with %d — the two must agree on a native leg; entitlement.go's contract is that api.anthropic.com is not gated (2026-09-26 audit, third round): denied catalogue, served completions\nmessages body: %s",
				resp.StatusCode, msgCode, msgBody)
		}
	})
}

// hermesStubBinary writes a `hermes` that reports version and logs every argv.
func hermesStubBinary(t *testing.T, dir, version string) (bin, logPath string) {
	t.Helper()
	logPath = filepath.Join(dir, "hermes.log")
	bin = filepath.Join(dir, "hermes")
	stub := "#!/bin/sh\n" +
		"echo \"$@\" >> " + strconv.Quote(logPath) + "\n" + // quoted: a temp dir with a space in the name would otherwise split the path
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
