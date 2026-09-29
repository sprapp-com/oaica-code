// round119_leg2_hostkey_test.go — F119-L2-2: the router key spelled in OAICA_HOST does not reach a child (2026-09-29 audit, round 119).

package launch

import (
	"strings"
	"testing"
)

func TestRound119RouterKeyInOAICAHostReachesChild(t *testing.T) {
	t.Setenv("OAICA_API_KEY", "sk-router-envkey")
	t.Setenv("OAICA_HOST", "https://sk-router-hostkey@router.example.com")
	_, k := splitRemoteUserinfo(oaicaLaunchHostRaw())
	t.Logf("oaica reads the router key from OAICA_HOST userinfo: %q ; oaicaLaunchAPIKeyForEnv with env unset would use it", k)
	plan := tierPlan{Primary: launchEndpoint{Source: sourceRouter, RemoteEndpoint: RemoteEndpoint{Name: "oaica", TokenEnv: "OAICA_API_KEY"}}}
	for name, env := range map[string][]string{
		"directLaunchEnv (codex/copilot/kimi/poolside)": directLaunchEnv(),
		"openclawEnv":                openclawEnv(),
		"tierPlan.childEnv (claude)": plan.childEnv("http://127.0.0.1:1", "tok"),
	} {
		var env1, host []string
		for _, kv := range env {
			if strings.HasPrefix(kv, "OAICA_API_KEY=") {
				env1 = append(env1, kv)
			}
			if strings.HasPrefix(kv, "OAICA_HOST=") {
				host = append(host, kv)
			}
		}
		t.Logf("%s: OAICA_API_KEY=%v OAICA_HOST=%v", name, env1, host)
		for _, h := range host {
			if strings.Contains(h, "sk-router-hostkey") {
				t.Errorf("RED %s: the router key spelled in OAICA_HOST reaches the child while OAICA_API_KEY is scrubbed", name)
			}
		}
	}
}
