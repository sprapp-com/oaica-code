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
	// An explicit --model in the user's extra args wins (claude CLI takes
	// the first occurrence, so ours must not be appended at all).
	if model != "" && !hasClaudeModelFlag(extra) {
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

// nativeClaudePickerModels are the "claude/<alias>" picker entries: they run
// the REAL Claude Code binary with a clean environment (RunNative below) so
// Claude Code uses its own /login (Anthropic subscription) — or a plain
// ANTHROPIC_API_KEY — instead of the OAICA router. The alias maps directly to
// Claude Code's built-in --model aliases.
var nativeClaudePickerModels = []ModelItem{
	{Name: "claude/opus", Description: "Claude Code native (Anthropic login / API key) — bypasses OAICA billing"},
	{Name: "claude/sonnet", Description: "Claude Code native (Anthropic login / API key) — bypasses OAICA billing"},
	{Name: "claude/fable", Description: "Claude Code native (Anthropic login / API key) — bypasses OAICA billing"},
	// "anthropic/" aliases of the same three entries: OpenRouter's catalog
	// means typing "anthropic" surfaces a wall of openrouter/anthropic/…
	// rows; these make the native (subscription) path show up in the same
	// search instead of hiding behind the "claude/" prefix.
	{Name: "anthropic/opus", Description: "Claude Code native, Anthropic subscription (alias of claude/opus)"},
	{Name: "anthropic/sonnet", Description: "Claude Code native, Anthropic subscription (alias of claude/sonnet)"},
	{Name: "anthropic/fable", Description: "Claude Code native, Anthropic subscription (alias of claude/fable)"},
}

// isNativeClaudeModel reports whether name selects the native (non-OAICA)
// Claude Code path: "claude/<tier>" or its "anthropic/<tier>" alias, e.g.
// "claude/opus" / "anthropic/opus".
func isNativeClaudeModel(model string) bool {
	return strings.HasPrefix(model, "claude/") || strings.HasPrefix(model, "anthropic/")
}

// nativeClaudeModelTier splits "claude/opus" (or "anthropic/opus") into the
// Claude Code --model alias ("opus"). ok is false for anything that is not a
// native entry.
func nativeClaudeModelTier(model string) (string, bool) {
	for _, p := range []string{"claude/", "anthropic/"} {
		if rest, ok := strings.CutPrefix(model, p); ok {
			return rest, true
		}
	}
	return "", false
}

// runNative execs the real Claude Code binary with an untouched environment —
// no ANTHROPIC_BASE_URL/AUTH_TOKEN injection, no translation proxy. The tier
// becomes Claude Code's own --model alias; a user-supplied --model in extra
// wins over the picker alias.
func (c *Claude) runNative(tier string, extra []string) error {
	claudePath, err := ensureClaudeInstalled()
	if err != nil {
		return err
	}
	args := extra
	if tier != "" && !hasClaudeModelFlag(extra) {
		args = append([]string{"--model", tier}, extra...)
	}
	fmt.Fprintln(os.Stderr, "claude native: running Claude Code with your own Anthropic login / API key (not OAICA)")
	cmd := exec.Command(claudePath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ() // deliberately untouched — clean native environment
	return cmd.Run()
}

func hasClaudeModelFlag(args []string) bool {
	for i, a := range args {
		if a == "--model" {
			return true
		}
		if strings.HasPrefix(a, "--model=") || (i > 0 && args[i-1] == "--model") {
			return true
		}
	}
	return false
}

func ensureClaudeInstalled() (string, error) {
	if path, err := (&Claude{}).findPath(); err == nil {
		return path, nil
	}

	if err := checkClaudeInstallerDependencies(); err != nil {
		return "", err
	}

	// Phrased here rather than through ConfirmPrompt's generic
	// "<prompt> requires confirmation; re-run with --yes" so the
	// non-interactive error reads as a statement, not a question with a
	// suffix bolted on (audit 0.4.6, P2-2).
	if currentLaunchConfirmPolicy.requireYesMessage && !currentLaunchConfirmPolicy.yes {
		return "", fmt.Errorf("Claude Code is not installed; re-run with --yes to install it")
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
	os.Remove(args[len(args)-1]) // fetched installer temp file

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
		// Verified-download flow (audit L3): fetch to a temp file, check any
		// SHA-256 pin, then execute the file — never pipe straight into bash.
		path, err := fetchInstallerScriptFn("https://claude.ai/install.sh")
		if err != nil {
			return "", nil, err
		}
		return "bash", []string{path}, nil
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

// WantsFullModelChoices: the launch-tier wizard picks the secondary and
// compaction legs from the full inventory, so Run must receive every
// selectable model, not just the resolved primary (see launch.go).
func (c *Claude) WantsFullModelChoices() bool { return true }

// planPickerPrefix marks saved tier plans in the launch picker; selecting
// one replays the whole stored choice (see tier_plan_profiles.go).
const planPickerPrefix = "plan/"

func isPlanPickerModel(name string) bool {
	return strings.HasPrefix(name, planPickerPrefix)
}

// pickerPlanItems builds the saved-plan picker entries ("plan/<name>",
// freshest summary in the description). Plans saved via the wizard's
// "Save as plan" step appear here on the next launch.
func pickerPlanItems() []ModelItem {
	names, err := PlanSortedNames()
	if err != nil || len(names) == 0 {
		return nil
	}
	items := make([]ModelItem, 0, len(names))
	for _, n := range names {
		prof, err := PlanGet(n)
		if err != nil {
			continue
		}
		desc := "saved plan"
		if prof.Description != "" {
			desc = prof.Description
		} else {
			parts := []string{"primary " + prof.Model}
			if prof.SonnetModel != "" {
				parts = append(parts, "sonnet "+prof.SonnetModel)
			}
			if prof.HaikuModel != "" {
				parts = append(parts, "haiku "+prof.HaikuModel)
			}
			if prof.OversizeModel != "" {
				parts = append(parts, "oversize "+prof.OversizeModel)
			}
			if prof.RoutePolicy != "" {
				parts = append(parts, "policy "+prof.RoutePolicy)
			}
			desc = strings.Join(parts, " · ")
		}
		items = append(items, ModelItem{Name: planPickerPrefix + n, Description: desc})
	}
	return items
}
