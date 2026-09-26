package launch

// openclaw_env_credential_integrity_test.go — the OpenClaw child inherited the
// launcher's own router credential and every configured remote's key
// (2026-09-26 audit, round 13).
//
// openclawEnv cleared a HARD-CODED list of eight provider variables instead of
// asking the tier plan which names it must scrub, while the Claude Code path
// goes through tierPlan.credentialEnvNames() + scrubCredentialEnv — the two
// functions that exist to keep an agent's Bash tool, an install hook, or a
// prompt-injected command from reading the keys oaica itself hands out or
// reads. The auditor's repro: openclawEnv() kept both OAICA_API_KEY (the
// launcher's own router token, declared as a leg's TokenEnv in tier_routing.go)
// and ZAI_API_KEY (a configured remote's api_key_env), while tierPlan.childEnv
// stripped both.
//
// The assertion is stated as the guarantee, not as a function name: every
// variable the plan scrubs must be absent from what openclawEnv() hands the
// child. That is what "the same guarantee" means, and it is checkable with the
// plan machinery the Claude Code path uses.

import (
	"strings"
	"testing"
)

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

// The router leg: the credential a plain `oaica launch` against the router is
// spending. The plan is built from the resolution code itself, so this also
// pins openclaw's idea of the name to tier_routing.go's.
func TestOpenclawEnvStripsTheRouterCredential(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, []oaicaModelEntry{{ID: "glm-5.3-cloud"}}, nil)

	ep, err := resolveLaunchEndpoint("glm-5.3-cloud")
	if err != nil {
		t.Fatalf("premise: the router leg no longer resolves for a model the stubbed router lists: %v", err)
	}
	if ep.Source != sourceRouter {
		t.Fatalf("premise: %q resolved to source %v, not the router", "glm-5.3-cloud", ep.Source)
	}
	plan := tierPlan{Primary: ep}
	if names := plan.credentialEnvNames(); !hasString(names, "OAICA_API_KEY") {
		t.Fatalf("premise: the router leg's credential is no longer named as one to scrub (%v)", names)
	}

	t.Setenv("OAICA_API_KEY", "sk-oaica-router-REALKEY")
	env := envMap(openclawEnv())
	for _, name := range plan.credentialEnvNames() {
		if v := env[name]; v != "" {
			t.Errorf("openclawEnv() kept %s=%s — that variable holds the real upstream credential, and the child inherits it for any Bash tool call, install hook or injected command to read", name, v)
		}
	}
	// Control: the ordinary environment still passes through. This is a
	// credential rule, not a clean slate.
	if _, ok := env["PATH"]; !ok {
		t.Errorf("openclawEnv() dropped PATH")
	}
}

// A configured remote's api_key_env — the audit's ZAI_API_KEY — including the
// second name of a comma-joined spec, which is the one keyEnvName does NOT
// pick and which stayed in a child for exactly that reason once before
// (child_env_key_env_siblings_integrity_test.go).
func TestOpenclawEnvStripsEveryConfiguredRemotesCredential(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"zai-plan","base_url":"https://api.z.ai/api/paas","api_key_env":"ZAI_API_KEY,BOX_KEY_B"}]}`)

	// The plan the Claude Code path would build for the same configuration:
	// one route per configured remote. Stated this way so the test cannot pass
	// by agreeing with a second, hand-written list.
	remotes, err := loadUserRemotes()
	if err != nil {
		t.Fatalf("premise: loadUserRemotes: %v", err)
	}
	var plan tierPlan
	for _, r := range remotes {
		if r.Name != "zai-plan" {
			continue
		}
		plan.Routes.Fallbacks = append(plan.Routes.Fallbacks, routeFor(launchEndpoint{
			Source:         sourceUserRemote,
			RemoteEndpoint: RemoteEndpoint{Name: r.Name, BaseURL: r.openAIBase(), APIKeyEnv: r.APIKeyEnv},
		}))
	}
	names := plan.credentialEnvNames()
	for _, want := range []string{"ZAI_API_KEY", "BOX_KEY_B"} {
		if !hasString(names, want) {
			t.Fatalf("premise: the plan does not scrub %s (%v)", want, names)
		}
	}

	t.Setenv("ZAI_API_KEY", "sk-zai-REALKEY")
	t.Setenv("BOX_KEY_B", "sk-box-REALKEY")

	env := envMap(openclawEnv())
	for _, name := range names {
		if v := env[name]; v != "" {
			t.Errorf("openclawEnv() kept %s=%s — it is a configured remote's api_key_env, which the Claude Code path strips (tierPlan.credentialEnvNames → childEnv) and this child must be no different: its Bash tool can read any variable it was handed", name, v)
		}
	}
	// Controls: an unrelated variable, and openclaw's own setting, still pass.
	if _, ok := env["PATH"]; !ok {
		t.Errorf("openclawEnv() dropped PATH — a credential rule must not become a clean slate")
	}
	if v := env["OPENCLAW_PLUGIN_STAGE_DIR"]; v == "" {
		t.Errorf("openclawEnv() no longer sets OPENCLAW_PLUGIN_STAGE_DIR, which is openclaw's own setting and nothing to do with credentials")
	}
}

// And the removal is a removal: the name is gone, not blanked — a blanked
// variable still tells the child it is there.
func TestOpenclawEnvRemovesTheNameRatherThanBlankingIt(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[]}`)
	t.Setenv("OAICA_API_KEY", "sk-oaica-router-REALKEY")

	for _, kv := range openclawEnv() {
		if strings.HasPrefix(kv, "OAICA_API_KEY=") {
			t.Errorf("the child environment still carries %q — scrubCredentialEnv removes the entry, and a blanked name still tells the child the variable is there", kv)
		}
	}
}
