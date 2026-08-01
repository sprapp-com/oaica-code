package launch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Claude implements Runner for Claude Code integration.
type Claude struct{}

func (c *Claude) String() string { return "Claude Code" }

func (c *Claude) args(model string, extra []string) []string {
	var args []string
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, extra...)
	return args
}

func (c *Claude) findPath() (string, error) {
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	for _, fallback := range []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, ".claude", "local", name),
	} {
		if _, err := os.Stat(fallback); err == nil {
			return fallback, nil
		}
	}
	return "", fmt.Errorf("claude binary not found")
}

// RunNative launches the real Claude Code binary with a CLEAN environment
// — none of the ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN overrides Run()
// injects to route through OAICA. Lets a user run Claude Code's own
// native `/login` (Anthropic OAuth device flow) or use a real Claude
// subscription, completely bypassing the router — a real, deliberate
// escape hatch from oaica-code's thin-client architecture for anyone who
// wants genuine upstream Claude Code instead. Exposed as `oaica
// claude-login` (cmd/claude_native.go).
func RunNative(args []string) error {
	claudePath, err := ensureClaudeInstalled()
	if err != nil {
		return err
	}
	cmd := exec.Command(claudePath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ() // deliberately untouched — no OAICA env injection at all
	return cmd.Run()
}

func (c *Claude) Run(model string, _ []LaunchModel, args []string) error {
	claudePath, err := ensureClaudeInstalled()
	if err != nil {
		return err
	}

	// The "--model" flag Claude Code itself sees, and the model name it
	// puts in each API request, must be the bare name — ":local" is a
	// picker-selection detail (see oaicaLocalTagSuffix's doc), the local
	// llama-server/cloud router should never see it.
	bareModel, _ := oaicaStripLocalTag(model)

	// Route through a local logging+forwarding proxy instead of pointing
	// ANTHROPIC_BASE_URL straight at the resolved host — logs
	// model/message-size/hard-signal features to ~/.oaica/requests.log
	// (never sent anywhere, no server cost) so we have real labeled data
	// to eventually evaluate/improve the router's flashplan classifier,
	// which has never been measured against real traffic. See
	// request_log.go's doc comment for the full reasoning. Falls back to
	// talking to the real host directly if the proxy fails to start —
	// logging must never be able to break a real launch.
	realHost := oaicaResolveHostForModel(model)
	anthropicBaseURL := realHost
	if ln, port, err := ListenLocalLoggingProxy(); err == nil {
		go func() {
			_ = RunLocalLoggingProxy(ln, realHost) // best-effort — see request_log.go
		}()
		anthropicBaseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	}

	cmd := exec.Command(claudePath, c.args(bareModel, args)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.Env = append(os.Environ(), c.envVars(model, anthropicBaseURL)...)
	return cmd.Run()
}

func (c *Claude) envVars(model, anthropicBaseURL string) []string {
	// THIS WAS THE REAL BUG (found via a live user repro that persisted
	// through multiple other fixes): envconfig.Host() reads OLLAMA_HOST,
	// defaulting to 127.0.0.1:11434 — a REAL, unrelated local Ollama
	// server that happens to be running on this box (different models
	// entirely: qwen2.5:7b, nanbeige-ternary, ...). Every prior fix
	// (router-side Jinja crash, Auto-mode disable) was real and necessary
	// but couldn't matter — Claude Code was never even reaching our OAICA
	// router. Confirmed by direct curl: 127.0.0.1:11434 has no "flashplan"
	// model, producing the exact "model 'flashplan' not found" error seen
	// in the user's debug logs. Use oaicaLaunchHost()/OAICA_API_KEY (this
	// package's own OAICA client, matching cmd/oaica_client.go's
	// equivalents) instead — the actual router this whole fork exists to
	// route through.
	//
	// oaicaResolveHostForModel — routes to a locally running `oaica serve`
	// ONLY when the caller picked the explicit "<model>:local" entry
	// (bare name always means cloud now, see oaicaLocalTagSuffix's doc).
	// Explicit OAICA_HOST still overrides both. The bare model name (tag
	// stripped) is what actually goes to the backend from here on — the
	// tag is a picker-selection detail, the local llama-server/cloud
	// router should never see it.
	bareModel, _ := oaicaStripLocalTag(model)

	env := []string{
		"ANTHROPIC_BASE_URL=" + anthropicBaseURL,
		"ANTHROPIC_API_KEY=",
		"ANTHROPIC_AUTH_TOKEN=" + oaicaLaunchAPIKeyForEnv(),
		"CLAUDE_CODE_ATTRIBUTION_HEADER=0",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_FEEDBACK_COMMAND=1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1",
	}

	env = append(env, c.modelEnvVars(bareModel)...)
	return env
}

func ensureClaudeInstalled() (string, error) {
	if path, err := (&Claude{}).findPath(); err == nil {
		return path, nil
	}

	if err := checkClaudeInstallerDependencies(); err != nil {
		return "", err
	}

	ok, err := ConfirmPrompt("Claude Code is not installed. Install now?")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("claude installation cancelled")
	}

	bin, args, err := claudeInstallerCommand(runtime.GOOS)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "\nInstalling Claude Code...\n")
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to install claude: %w", err)
	}

	path, err := (&Claude{}).findPath()
	if err != nil {
		return "", fmt.Errorf("claude was installed but the binary was not found on PATH\n\nYou may need to restart your shell")
	}

	fmt.Fprintf(os.Stderr, "%sClaude Code installed successfully%s\n\n", ansiGreen, ansiReset)
	return path, nil
}

func checkClaudeInstallerDependencies() error {
	switch runtime.GOOS {
	case "windows":
		if _, err := exec.LookPath("powershell"); err != nil {
			return fmt.Errorf("claude is not installed and required dependencies are missing\n\nInstall the following first:\n  PowerShell: https://learn.microsoft.com/powershell/\n\nThen re-run:\n  ollama launch claude")
		}
	default:
		var missing []string
		if _, err := exec.LookPath("curl"); err != nil {
			missing = append(missing, "curl: https://curl.se/")
		}
		if _, err := exec.LookPath("bash"); err != nil {
			missing = append(missing, "bash: https://www.gnu.org/software/bash/")
		}
		if len(missing) > 0 {
			return fmt.Errorf("claude is not installed and required dependencies are missing\n\nInstall the following first:\n  %s\n\nThen re-run:\n  ollama launch claude", strings.Join(missing, "\n  "))
		}
	}
	return nil
}

func claudeInstallerCommand(goos string) (string, []string, error) {
	switch goos {
	case "windows":
		return "powershell", []string{
			"-NoProfile",
			"-ExecutionPolicy",
			"Bypass",
			"-Command",
			"irm https://claude.ai/install.ps1 | iex",
		}, nil
	case "darwin", "linux":
		return "bash", []string{
			"-c",
			"curl -fsSL https://claude.ai/install.sh | bash",
		}, nil
	default:
		return "", nil, fmt.Errorf("unsupported platform for claude install: %s", goos)
	}
}

// modelEnvVars returns Claude Code env vars that route all model tiers through Ollama.
func (c *Claude) modelEnvVars(model string) []string {
	env := []string{
		"ANTHROPIC_DEFAULT_OPUS_MODEL=" + model,
		"ANTHROPIC_DEFAULT_SONNET_MODEL=" + model,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=" + model,
		"CLAUDE_CODE_SUBAGENT_MODEL=" + model,
		// Claude Code's "Auto" model-tier carousel (2.1.x+) issues its own
		// background classifier calls against a model id that is NOT covered
		// by ANTHROPIC_DEFAULT_*_MODEL/CLAUDE_CODE_SUBAGENT_MODEL above. Against
		// a third-party router that doesn't recognize Anthropic's real model
		// IDs, those background calls 404 with error.type=model_not_found,
		// which the CLI surfaces to the user as "There's an issue with the
		// selected model (<model>). It may not exist or you may not have
		// access to it." even though the explicitly selected model works fine.
		// Disable Auto mode and pin its classifier model as a belt-and-suspenders
		// fallback so it can't independently address a model our router doesn't have.
		"CLAUDE_CODE_ENABLE_AUTO_MODE=0",
		"CLAUDE_CODE_AUTO_MODE_MODEL=" + model,
	}

	if isCloudModelName(model) {
		if l, ok := lookupCloudModelLimit(model); ok {
			env = append(env, "CLAUDE_CODE_AUTO_COMPACT_WINDOW="+strconv.Itoa(l.Context))
		}
	}

	return env
}
