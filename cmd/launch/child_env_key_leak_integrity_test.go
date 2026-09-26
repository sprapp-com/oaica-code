package launch

// child_env_key_leak_integrity_test.go — the launched agent inherited every
// credential variable this launch reads (2026-09-26 audit).
//
// The plan path hands the child a per-launch proxy token and injects each
// leg's real key upstream, and tier_routing.go's own doc states the property
// that buys: "no real key enters the child environment (where Claude Code's
// Bash tool could print it)". But the environment was os.Environ() plus the
// plan's own vars, and envVars blanks exactly one name (ANTHROPIC_API_KEY).
// Everything else the launcher reads a remote's key from — the documented way
// to configure one is to export its api_key_env — rode into the child, where
// any tool call, install hook or `env` in the transcript can read it, and a
// prompt-injected command can exfiltrate it and spend the user's account.

import (
	"strings"
	"testing"
)

func childEnvHas(env []string, name, value string) bool {
	for _, kv := range env {
		if kv == name+"="+value {
			return true
		}
	}
	return false
}

func childEnvNames(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok {
			out[name] = value
		}
	}
	return out
}

func TestTheLaunchedAgentDoesNotInheritTheProxyKeys(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[
		{"name":"deepseek","base_url":"http://deepseek:8080/v1","api_key_env":"DEEPSEEK_API_KEY","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	t.Setenv("DEEPSEEK_API_KEY", "sk-real-deepseek")
	t.Setenv("OAICA_UNRELATED_SETTING", "keep-me")

	plan, err := buildTierPlan("deepseek/deepseek-v4-flash", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	env := plan.childEnv("http://127.0.0.1:9", "tok-per-launch")

	if childEnvHas(env, "DEEPSEEK_API_KEY", "sk-real-deepseek") {
		t.Errorf("the launched agent inherited DEEPSEEK_API_KEY — the key this launch injects upstream on the child's behalf and the child therefore never needs; Claude Code's Bash tool, an install hook or a prompt-injected command reads it straight out of the environment and spends the user's account") // the env itself is NOT printed: a failing assertion would put every other live credential in the log
	}
	// Removed outright, not blanked: a blanked name still tells the child the
	// variable exists, and the doc's claim is that no real key is in there.
	if _, present := childEnvNames(env)["DEEPSEEK_API_KEY"]; present {
		t.Errorf("DEEPSEEK_API_KEY is still in the child environment")
	}

	// The child still works: the proxy token is there and unrelated settings
	// are untouched — the scrub is a credential rule, not a clean-slate env.
	if got := childEnvNames(env)["ANTHROPIC_AUTH_TOKEN"]; got != "tok-per-launch" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q in the child env, want the per-launch token — the scrub must not take the credential the child is supposed to have", got)
	}
	if got := childEnvNames(env)["OAICA_UNRELATED_SETTING"]; got != "keep-me" {
		t.Errorf("an unrelated environment variable was dropped (got %q)", got)
	}
	if got := childEnvNames(env)["PATH"]; got == "" {
		t.Error("PATH was dropped from the child environment")
	}
}

// The same for a key that only the SECOND name of a comma-joined api_key_env
// is set under: keyEnvName picks the one that is set, so that is the one the
// launch reads and the one the child must not inherit.
func TestTheSecondKeyEnvNameIsScrubbedToo(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[
		{"name":"zai","base_url":"http://zai:8080/v1","api_key_env":"ZAI_API_KEY,ZAI_GO_API_KEY","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	t.Setenv("ZAI_GO_API_KEY", "sk-zai-go")

	plan, err := buildTierPlan("zai/glm-5.3", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := childEnvNames(plan.childEnv("http://127.0.0.1:9", "tok"))["ZAI_GO_API_KEY"]; got != "" {
		t.Errorf("the second api_key_env name survived into the child environment (ZAI_GO_API_KEY=%q) — it is the variable this launch actually reads the key from", got)
	}
}
