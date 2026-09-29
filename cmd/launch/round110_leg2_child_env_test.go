package launch

// round110_leg2_child_env_test.go — leg 2, round 110 (2026-09-29 audit), F110-L2-2.
//
// `oaica launch claude` and `oaica launch openclaw` share one rule: no real key
// oaica could have read is left in the child's environment, because the agent's
// Bash tool, or a prompt-injected command, can read it (pinned by
// child_env_key_leak_integrity_test.go). The doors that build the child's
// environment from os.Environ() and append the one key the child is meant to hold —
// codex, copilot, kimi's env-config CLI, and the codex app's terminal path — passed
// every OTHER configured remote's key and the router's OAICA_API_KEY through, so a
// prompt-injected session could read and spend accounts it was never launched
// against. They now start from the scrubbed environment; the one key the child needs
// is appended after the scrub.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMine110ACodexChildSeesOnlyItsOwnKey(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	writeRemotes(t, `{"remotes":[
	 {"name":"deepseek","base_url":"http://deepseek:8080/v1","api_key_env":"DEEPSEEK_API_KEY","tool_format":"tool_calls"},
	 {"name":"zai","base_url":"http://zai:8080/v1","api_key_env":"ZAI_SECOND_KEY","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	bin := t.TempDir()
	out := filepath.Join(home, "childenv.txt")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'codex-cli 0.200.0'; exit 0; fi\nenv | grep -E 'KEY' > " + out + "\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("DEEPSEEK_API_KEY", "sk-deepseek-REAL")
	t.Setenv("ZAI_SECOND_KEY", "sk-zai-REAL")
	t.Setenv("OAICA_API_KEY", "sk-oaica-REAL")
	if err := (&Codex{}).Run("deepseek/deepseek-v4-flash", nil, []string{"--force-tools"}); err != nil {
		t.Fatalf("premise: codex did not run: %v", err)
	}
	b, _ := os.ReadFile(out)
	seen := string(b)
	if !strings.Contains(seen, "OPENAI_API_KEY=sk-deepseek-REAL") {
		t.Fatalf("premise: the child was not given the key it is meant to hold:\n%s", seen)
	}
	for _, leak := range []string{"sk-zai-REAL", "sk-oaica-REAL", "DEEPSEEK_API_KEY="} {
		if strings.Contains(seen, leak) {
			t.Errorf("the codex child's environment carries %q — another remote's key, the router key, or a second copy of its own must not be there:\n%s (2026-09-29 audit, round 110, F110-L2-2)", leak, seen)
		}
	}
}

// TestMine110TheDirectExecDoorsShareOneScrubbedEnvironment pins the shared helper
// the four doors use, so a fifth door has one function to call.
func TestMine110TheDirectExecDoorsShareOneScrubbedEnvironment(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	// One remote's api_key_env IS the variable a door hands its child ("OPENAI_API_KEY"):
	// the user's copy is scrubbed, and the door's own value must still arrive.
	writeRemotes(t, `{"remotes":[
	 {"name":"zai","base_url":"http://zai:8080/v1","api_key_env":"ZAI_SECOND_KEY","tool_format":"tool_calls"},
	 {"name":"oa","base_url":"http://oa:8080/v1","api_key_env":"OPENAI_API_KEY","tool_format":"tool_calls"}]}`)
	t.Setenv("ZAI_SECOND_KEY", "sk-zai-REAL")
	t.Setenv("OPENAI_API_KEY", "sk-users-own-REAL")
	t.Setenv("OAICA_API_KEY", "sk-oaica-REAL")
	t.Setenv("UNRELATED_TOOLING_VAR", "keep-me")
	env := strings.Join(directLaunchEnv("OPENAI_API_KEY=intended"), "\n")
	for _, leak := range []string{"sk-zai-REAL", "sk-oaica-REAL", "sk-users-own-REAL"} {
		if strings.Contains(env, leak) {
			t.Errorf("directLaunchEnv carries %q (2026-09-29 audit, round 110, F110-L2-2)", leak)
		}
	}
	if !strings.Contains(env, "OPENAI_API_KEY=intended") || !strings.Contains(env, "UNRELATED_TOOLING_VAR=keep-me") {
		t.Errorf("directLaunchEnv dropped the intended key or the user's own tooling variables:\n%s (2026-09-29 audit, round 110, F110-L2-2)", env)
	}
}

// r110FakeAgent installs a fake agent binary on PATH that records the KEY-bearing
// variables of its environment when run, answering --help and --version quietly.
func r110FakeAgent(t *testing.T, name, out string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in --help|--version) echo 'x 0.200.0'; exit 0;; esac\nenv | grep -E 'KEY' > " + out + "\n"
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

func r110TwoRemotes(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	setLaunchTestHome(t, home)
	writeRemotes(t, `{"remotes":[
	 {"name":"deepseek","base_url":"http://deepseek:8080/v1","api_key_env":"DEEPSEEK_API_KEY","tool_format":"tool_calls"},
	 {"name":"zai","base_url":"http://zai:8080/v1","api_key_env":"ZAI_SECOND_KEY","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	t.Setenv("DEEPSEEK_API_KEY", "sk-deepseek-REAL")
	t.Setenv("ZAI_SECOND_KEY", "sk-zai-REAL")
	t.Setenv("OAICA_API_KEY", "sk-oaica-REAL")
	return home
}

// TestMine110EveryDirectDoorScrubsTheOtherKeys runs copilot, kimi and the codex app's
// terminal path against a fake agent and reads what the child could see.
func TestMine110EveryDirectDoorScrubsTheOtherKeys(t *testing.T) {
	doors := map[string]struct {
		agent string
		run   func() error
		own   string
	}{
		"copilot": {"copilot", func() error {
			return (&Copilot{}).Run("deepseek/deepseek-v4-flash", nil, []string{"--force-tools"})
		}, "COPILOT_PROVIDER_API_KEY=sk-deepseek-REAL"},
		"kimi": {"kimi", func() error {
			return (&Kimi{}).Run("deepseek/deepseek-v4-flash", nil, []string{"--force-tools"})
		}, "KIMI_MODEL_API_KEY=sk-deepseek-REAL"},
		"codex app": {"codex", func() error { return defaultCodexAppOpenApp([]string{"x"}) }, "OPENAI_API_KEY=ollama"},
	}
	for name, d := range doors {
		t.Run(name, func(t *testing.T) {
			home := r110TwoRemotes(t)
			out := filepath.Join(home, "childenv.txt")
			r110FakeAgent(t, d.agent, out)
			if err := d.run(); err != nil {
				t.Fatalf("premise: %s did not run: %v", name, err)
			}
			b, _ := os.ReadFile(out)
			seen := string(b)
			if !strings.Contains(seen, d.own) {
				t.Fatalf("premise: the child was not given its own key %q:\n%s", d.own, seen)
			}
			for _, leak := range []string{"sk-zai-REAL", "sk-oaica-REAL", "DEEPSEEK_API_KEY="} {
				if strings.Contains(seen, leak) {
					t.Errorf("the %s child's environment carries %q (2026-09-29 audit, round 110, F110-L2-2):\n%s", name, leak, seen)
				}
			}
		})
	}
}
