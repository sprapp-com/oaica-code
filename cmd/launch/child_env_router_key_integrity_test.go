package launch

// child_env_router_key_integrity_test.go — the OAICA router's API key rode into
// the launched agent's environment (2026-09-26 audit, eighth round).
//
// credentialEnvNames' own doc says it lists "every environment variable this
// plan reads a REAL upstream credential out of", and childEnv removes exactly
// those before the agent starts — the mechanism exists because the agent's Bash
// tool, an install hook, or a prompt-injected command can read any variable it
// was handed and spend the user's account. Every leg contributed its name
// (TokenEnv for a plan's tiers, KeyEnv for each route) except the router leg:
// its endpoint was built with Token: oaicaLaunchAPIKeyForEnv() and no TokenEnv,
// so OAICA_API_KEY — the one credential a plain `oaica launch claude` against
// the router is spending — stayed in the child's environment while the per
// launch proxy token was correctly substituted for everything else.
//
// The proxy already attaches the router's real key upstream, so the child needs
// none of it, which is what makes this a removal and not a special case.

import (
	"strings"
	"testing"
)

func TestTheRouterCredentialDoesNotReachTheChildEnvironment(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, []oaicaModelEntry{{ID: "glm-5.3-cloud"}}, nil)

	ep, err := resolveLaunchEndpoint("glm-5.3-cloud")
	if err != nil {
		t.Fatalf("premise: the router leg no longer resolves for a model the stubbed router lists: %v", err)
	}
	if ep.Source != sourceRouter {
		t.Fatalf("premise: %q resolved to source %v, not the router — this test is about the router leg", "glm-5.3-cloud", ep.Source)
	}

	plan := tierPlan{Primary: ep}
	if !hasString(plan.credentialEnvNames(), "OAICA_API_KEY") {
		t.Errorf("the router leg's credential is not named as one to scrub (%v) — credentialEnvNames is what childEnv removes, so the key stays in the agent's environment for any Bash tool call to read", plan.credentialEnvNames())
	}

	t.Setenv("OAICA_API_KEY", "sk-oaica-router-REALKEY")
	env := plan.childEnv("http://127.0.0.1:1", "tok-per-launch")
	for _, kv := range env {
		if strings.HasPrefix(kv, "OAICA_API_KEY=") {
			t.Errorf("the child environment still carries %s — a launched agent's Bash tool, an install hook, or a prompt-injected command reads it and spends the user's router account; the proxy already attaches this key upstream, so the child needs none of it", kv)
		}
	}
	// ...and the substitution the scrub exists to enable is still in place.
	got := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") {
			got = kv
		}
	}
	if got != "ANTHROPIC_AUTH_TOKEN=tok-per-launch" {
		t.Errorf("child env has %q, want the per-launch proxy token — the scrub must not take the credential the child is supposed to use", got)
	}
}

func hasString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
