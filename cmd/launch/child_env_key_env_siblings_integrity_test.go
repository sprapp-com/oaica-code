package launch

// child_env_key_env_siblings_integrity_test.go — a remote's SECOND api_key_env
// name rode into the launched agent's environment (2026-09-26 audit, eleventh
// round).
//
// api_key_env accepts a comma-joined list (user_remotes.go's keyEnvNames: "the
// user may have exported any one of these"), and keyEnvName resolves it to the
// ONE name that is actually set, because the proxy's live re-read
// (resolveKey) has to os.Getenv a single variable. The child-env scrubber was
// fed that same single name: credentialEnvNames reads TokenEnv/KeyEnv, which
// hold the resolved name, so a row listing "BOX_KEY_A,BOX_KEY_B" scrubbed only
// whichever one was set and left its sibling — a real credential for the same
// account — in the environment of the agent it launches, for a Bash tool call,
// an install hook, or a prompt-injected command to read and spend.

import (
	"strings"
	"testing"
)

const siblingKeyEnvRemotes = `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key_env":"BOX_KEY_A,BOX_KEY_B","tool_format":"tool_calls"}]}`

func TestBothNamesOfACommaJoinedApiKeyEnvAreScrubbed(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	withTempRemotesFile(t)
	writeRemotes(t, siblingKeyEnvRemotes)

	// Both names set: keyEnvName picks the first SET one, so the scrubber that
	// only knows that name leaves the other behind.
	t.Setenv("BOX_KEY_A", "sk-box-A-REALKEY")
	t.Setenv("BOX_KEY_B", "sk-box-B-REALKEY")

	ep, err := resolveLaunchEndpoint("box/big-model")
	if err != nil {
		t.Fatalf("premise: the user-remote leg no longer resolves: %v", err)
	}
	plan := tierPlan{Primary: ep}

	for _, name := range []string{"BOX_KEY_A", "BOX_KEY_B"} {
		if !hasString(plan.credentialEnvNames(), name) {
			t.Errorf("%s is not named as a credential to scrub (%v) — credentialEnvNames is what childEnv removes", name, plan.credentialEnvNames())
		}
	}

	env := plan.childEnv("http://127.0.0.1:1", "tok-per-launch")
	for _, kv := range env {
		for _, name := range []string{"BOX_KEY_A", "BOX_KEY_B"} {
			if strings.HasPrefix(kv, name+"=") {
				t.Errorf("the child environment still carries %s — a launched agent's Bash tool, an install hook, or a prompt-injected command reads it and spends the user's box account; the proxy attaches this key upstream, so the child needs none of it", kv)
			}
		}
	}
}

// Control: a row naming ONE variable is still scrubbed, and the per-launch
// proxy token the child is supposed to use is still there.
func TestASingleApiKeyEnvNameIsStillScrubbed(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key_env":"BOX_KEY_SOLO","tool_format":"tool_calls"}]}`)
	t.Setenv("BOX_KEY_SOLO", "sk-box-solo-REALKEY")

	ep, err := resolveLaunchEndpoint("box/big-model")
	if err != nil {
		t.Fatalf("premise: the user-remote leg no longer resolves: %v", err)
	}
	plan := tierPlan{Primary: ep}
	env := plan.childEnv("http://127.0.0.1:1", "tok-per-launch")
	for _, kv := range env {
		if strings.HasPrefix(kv, "BOX_KEY_SOLO=") {
			t.Errorf("the single api_key_env name was not scrubbed: %s", kv)
		}
	}
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
