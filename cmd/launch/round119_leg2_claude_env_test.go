// round119_leg2_claude_env_test.go — F119-L2-4: the claude door scrubs every configured remote's key, as the sibling doors do (2026-09-29 audit, round 119).

package launch

import (
	"strings"
	"testing"
)

func TestRound119ClaudeChildEnvScrubsOtherRemotesKeys(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("OPENAI_API_KEY", "sk-openai-real")
	t.Setenv("DEEPSEEK_API_KEY", "sk-ds-real")
	t.Setenv("OAICA_API_KEY", "sk-router")
	t.Setenv("HARMLESS_VAR", "keep-me")
	plan := tierPlan{Primary: launchEndpoint{Source: sourceRouter, RemoteEndpoint: RemoteEndpoint{Name: "oaica", TokenEnv: "OAICA_API_KEY"}}}
	kept := false
	for _, kv := range plan.childEnv("http://127.0.0.1:1", "tok") {
		for _, n := range []string{"OPENAI_API_KEY=", "DEEPSEEK_API_KEY=", "OAICA_API_KEY="} {
			if strings.HasPrefix(kv, n) {
				t.Errorf("the claude child still holds %s", kv)
			}
		}
		if kv == "HARMLESS_VAR=keep-me" {
			kept = true
		}
	}
	if !kept {
		t.Errorf("the scrub removed a variable that is not a credential")
	}
}
