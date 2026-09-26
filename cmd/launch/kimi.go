package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/envconfig"
)

// Kimi implements Runner for Kimi Code CLI integration.
type Kimi struct{}

const (
	kimiDefaultModelAlias     = "ollama"
	kimiDefaultMaxContextSize = 32768
)

var (
	kimiGOOS             = runtime.GOOS
	kimiModelShowTimeout = 5 * time.Second
)

func (k *Kimi) String() string { return "Kimi Code CLI" }

func (k *Kimi) args(config string, extra []string) []string {
	args := []string{"--config", config}
	args = append(args, extra...)
	return args
}

func (k *Kimi) Run(model string, _ []LaunchModel, args []string) error {
	forceTools, args := extractForceTools(args)
	if err := gateOpenAITools(model, forceTools); err != nil {
		return err
	}

	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("model is required")
	}
	if err := validateKimiPassthroughArgs(args); err != nil {
		return err
	}

	maxContextSize := resolveKimiMaxContextSize(model)

	bin, err := ensureKimiInstalled()
	if err != nil {
		return err
	}

	if kimiCLIKind(bin) == kimiCLIEnvConfig {
		// Kimi Code CLI: the provider (key included) goes in the child's
		// environment, which only its owner can read.
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), kimiModelEnv(model, maxContextSize)...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Archived kimi-cli: it takes the config as an argument and nothing else,
	// so the key is unavoidably in the child's argv. See the note on
	// buildKimiInlineConfig.
	config, err := buildKimiInlineConfig(model, maxContextSize)
	if err != nil {
		return fmt.Errorf("failed to build kimi config: %w", err)
	}
	cmd := exec.Command(bin, k.args(config, args)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// How the installed CLI is configured. Kimi Code CLI synthesizes a provider
// and model from the KIMI_MODEL_* environment variables — Moonshot documents
// that family as the one channel that reads a credential from the shell
// instead of config.toml. The archived Python CLI has no such channel and
// accepts only `--config <json>`.
const (
	kimiCLIEnvConfig = "env"
	kimiCLILegacy    = "legacy"
)

// kimiCLIKind reports which of the two CLIs `bin` is, by asking it: the
// archived one has --config-file in its help, the current one has no such
// option (it reads $KIMI_CODE_HOME/config.toml instead).
//
// When the probe fails the answer is the legacy CLI. That is the failing-safe
// choice: a legacy CLI handed KIMI_MODEL_* silently ignores them and runs its
// own default model, which looks like success and routes the user's work
// somewhere they did not choose, whereas a current CLI handed an unknown
// --config stops with an error.
func kimiCLIKind(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), kimiModelShowTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--help").CombinedOutput()
	if err != nil {
		return kimiCLILegacy
	}
	if strings.Contains(string(out), "--config-file") {
		return kimiCLILegacy
	}
	return kimiCLIEnvConfig
}

// kimiModelEnv is the KIMI_MODEL_* set that replaces buildKimiInlineConfig.
// Provider type "openai" is Kimi Code CLI's OpenAI-compatible protocol (its
// default, "kimi", points at Moonshot's own API).
func kimiModelEnv(model string, maxContextSize int) []string {
	return []string{
		"KIMI_MODEL_NAME=" + kimiModelIDFor(model),
		"KIMI_MODEL_API_KEY=" + kimiKeyFor(model),
		"KIMI_MODEL_BASE_URL=" + kimiBaseURLFor(model),
		"KIMI_MODEL_PROVIDER_TYPE=openai",
		fmt.Sprintf("KIMI_MODEL_MAX_CONTEXT_SIZE=%d", maxContextSize),
	}
}

func findKimiBinary() (string, error) {
	if path, err := exec.LookPath("kimi"); err == nil {
		return path, nil
	}

	home, _ := os.UserHomeDir()

	var candidates []string
	switch kimiGOOS {
	case "windows":
		candidates = appendWindowsKimiCandidates(candidates, filepath.Join(home, ".local", "bin"))
		candidates = appendWindowsKimiCandidates(candidates, filepath.Join(home, "bin"))

		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			candidates = appendWindowsKimiCandidates(candidates, filepath.Join(appData, "uv", "bin"))
		}
		if localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localAppData != "" {
			candidates = appendWindowsKimiCandidates(candidates, filepath.Join(localAppData, "uv", "bin"))
		}
	default:
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin", "kimi"),
			filepath.Join(home, "bin", "kimi"),
			filepath.Join(home, ".local", "share", "uv", "tools", "kimi-cli", "bin", "kimi"),
			filepath.Join(home, ".local", "share", "uv", "tools", "kimi", "bin", "kimi"),
		)

		if xdgDataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdgDataHome != "" {
			candidates = append(candidates,
				filepath.Join(xdgDataHome, "uv", "tools", "kimi-cli", "bin", "kimi"),
				filepath.Join(xdgDataHome, "uv", "tools", "kimi", "bin", "kimi"),
			)
		}

		// WSL users can inherit Windows env vars while launching from Linux shells.
		if profile := windowsPathToWSL(os.Getenv("USERPROFILE")); profile != "" {
			candidates = appendWindowsKimiCandidates(candidates, filepath.Join(profile, ".local", "bin"))
		}
		if appData := windowsPathToWSL(os.Getenv("APPDATA")); appData != "" {
			candidates = appendWindowsKimiCandidates(candidates, filepath.Join(appData, "uv", "bin"))
		}
		if localAppData := windowsPathToWSL(os.Getenv("LOCALAPPDATA")); localAppData != "" {
			candidates = appendWindowsKimiCandidates(candidates, filepath.Join(localAppData, "uv", "bin"))
		}
	}

	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("kimi binary not found")
}

func appendWindowsKimiCandidates(candidates []string, dir string) []string {
	if strings.TrimSpace(dir) == "" {
		return candidates
	}

	return append(candidates,
		filepath.Join(dir, "kimi.exe"),
		filepath.Join(dir, "kimi.cmd"),
		filepath.Join(dir, "kimi.bat"),
	)
}

func windowsPathToWSL(path string) string {
	trimmed := strings.TrimSpace(path)
	if len(trimmed) < 3 || trimmed[1] != ':' {
		return ""
	}

	drive := strings.ToLower(string(trimmed[0]))
	rest := strings.ReplaceAll(trimmed[2:], "\\", "/")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		return filepath.Join("/mnt", drive)
	}

	return filepath.Join("/mnt", drive, rest)
}

func validateKimiPassthroughArgs(args []string) error {
	for _, arg := range args {
		switch {
		case arg == "--config", strings.HasPrefix(arg, "--config="):
			return fmt.Errorf("conflicting extra argument %q: oaica launch kimi manages --config", arg)
		case arg == "--config-file", strings.HasPrefix(arg, "--config-file="):
			return fmt.Errorf("conflicting extra argument %q: oaica launch kimi manages --config-file", arg)
		case arg == "--model", strings.HasPrefix(arg, "--model="):
			return fmt.Errorf("conflicting extra argument %q: oaica launch kimi manages --model", arg)
		case arg == "-m", strings.HasPrefix(arg, "-m="):
			return fmt.Errorf("conflicting extra argument %q: oaica launch kimi manages -m/--model", arg)
		}
	}
	return nil
}

// kimiBaseURLFor is the provider base URL Kimi should use: the remote's direct
// base for a user-remote model, otherwise the daemon's /v1.
func kimiBaseURLFor(model string) string {
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return strings.TrimRight(ep.BaseURL, "/")
	}
	return envconfig.ConnectableHost().String() + "/v1"
}

// kimiModelIDFor is the model id Kimi should use: the bare upstream id for a
// user-remote model, otherwise the picker name.
func kimiModelIDFor(model string) string {
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return ep.UpstreamModel
	}
	return model
}

// kimiKeyFor is the provider API key: the remote's token for a user-remote
// model, otherwise "ollama".
func kimiKeyFor(model string) string {
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return ep.Token
	}
	return "ollama"
}

// buildKimiInlineConfig renders the config the ARCHIVED kimi-cli wants.
//
// It is passed as one argv element, so the provider key it contains is in the
// child's command line, readable from /proc/<pid>/cmdline by any local user —
// a documented exception rather than an oversight (docs/ENTERPRISE.md). The
// current CLI gets kimiModelEnv instead, which is why this is only reached
// for a binary whose help still advertises --config-file.
func buildKimiInlineConfig(model string, maxContextSize int) (string, error) {
	cfg := map[string]any{
		"default_model": kimiDefaultModelAlias,
		"providers": map[string]any{
			kimiDefaultModelAlias: map[string]any{
				"type":     "openai_legacy",
				"base_url": kimiBaseURLFor(model),
				"api_key":  kimiKeyFor(model),
			},
		},
		"models": map[string]any{
			kimiDefaultModelAlias: map[string]any{
				"provider":         kimiDefaultModelAlias,
				"model":            kimiModelIDFor(model),
				"max_context_size": maxContextSize,
			},
		},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func resolveKimiMaxContextSize(model string) int {
	if l, ok := lookupCloudModelLimit(model); ok {
		return l.Context
	}

	client, err := api.ClientFromEnvironment()
	if err != nil {
		return kimiDefaultMaxContextSize
	}

	ctx, cancel := context.WithTimeout(context.Background(), kimiModelShowTimeout)
	defer cancel()
	resp, err := client.Show(ctx, &api.ShowRequest{Model: model})
	if err != nil {
		return kimiDefaultMaxContextSize
	}

	if n, ok := modelInfoContextLength(resp.ModelInfo); ok {
		return n
	}

	return kimiDefaultMaxContextSize
}

func modelInfoContextLength(modelInfo map[string]any) (int, bool) {
	for key, val := range modelInfo {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		switch v := val.(type) {
		case float64:
			if v > 0 {
				return int(v), true
			}
		case int:
			if v > 0 {
				return v, true
			}
		case int64:
			if v > 0 {
				return int(v), true
			}
		}
	}
	return 0, false
}

func ensureKimiInstalled() (string, error) {
	if path, err := findKimiBinary(); err == nil {
		return path, nil
	}

	if err := checkKimiInstallerDependencies(); err != nil {
		return "", err
	}

	ok, err := ConfirmPrompt("Kimi is not installed. Install now?")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("kimi installation cancelled")
	}

	bin, args, err := kimiInstallerCommand(kimiGOOS)
	if err != nil {
		return "", err
	}
	if kimiGOOS != "windows" && len(args) > 0 {
		// The unix plan runs the verified download's temp file itself
		// (kimiInstallerCommand's single argument), so this is the only place
		// it can be removed. On Windows the plan copies it into %TEMP%, runs
		// that copy, and removes both from inside the PowerShell command.
		defer os.Remove(args[0])
	}

	fmt.Fprintf(os.Stderr, "\nInstalling Kimi...\n")
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to install kimi: %w", err)
	}

	path, err := findKimiBinary()
	if err != nil {
		return "", fmt.Errorf("kimi was installed but the binary was not found on PATH\n\nYou may need to restart your shell")
	}

	fmt.Fprintf(os.Stderr, "%sKimi installed successfully%s\n\n", ansiGreen, ansiReset)
	return path, nil
}

func checkKimiInstallerDependencies() error {
	switch kimiGOOS {
	case "windows":
		if _, err := exec.LookPath("powershell"); err != nil {
			return fmt.Errorf("kimi is not installed and required dependencies are missing\n\nInstall the following first:\n  PowerShell: https://learn.microsoft.com/powershell/\n\nThen re-run:\n  oaica launch kimi")
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
			return fmt.Errorf("kimi is not installed and required dependencies are missing\n\nInstall the following first:\n  %s\n\nThen re-run:\n  oaica launch kimi", strings.Join(missing, "\n  "))
		}
	}
	return nil
}

// The /kimi-code/ path installs Kimi Code CLI, the maintained successor.
// The bare /install.sh path installs the archived Python kimi-cli, which
// Moonshot's own docs say "should no longer be installed" (its repo carries
// the same notice): installing it left every new user on a CLI that ignores
// the KIMI_MODEL_* variables this integration configures (2026-09-26 audit).
const (
	kimiInstallScriptURL   = "https://code.kimi.com/kimi-code/install.sh"
	kimiInstallScriptURLPS = "https://code.kimi.com/kimi-code/install.ps1"
)

// kimiInstallerScript downloads this platform's installer through
// installer_dl.go's verified download — the same gate the unix arm already used
// — and returns the temp file it wrote. The bytes are hashed and a SHA-256 pin
// (built-in, or OAICA_INSTALL_SHA256_<URL>) is enforced before anything is
// allowed to run them.
func kimiInstallerScript(goos string) (string, error) {
	switch goos {
	case "windows":
		return fetchInstallerScriptFn(kimiInstallScriptURLPS)
	case "darwin", "linux":
		return fetchInstallerScriptFn(kimiInstallScriptURL)
	default:
		return "", fmt.Errorf("unsupported platform for kimi install: %s", goos)
	}
}

// kimiInstallerCommand is the command that runs the installer for a platform.
// Both arms run exactly one thing: the file installer_dl.go verified.
//
// The Windows arm used to be
//
//	powershell -Command "Invoke-RestMethod <url> | Invoke-Expression"
//
// which downloaded the script itself and evaluated it inline — no temp file, no
// hash, no pin, no unpinned warning. Whatever the network answered ran with the
// user's privileges: a compromised CDN, a DNS hijack or a MITM executed
// attacker code, and there was no bytes-to-review step at all. It now asks for
// the same verified download the unix arm does and runs the file it handed back
// — copied to a .ps1 name first, because the download is shared code and writes
// a .sh, which PowerShell will not execute as a script. Both the copy and the
// verified temp file are removed after the run (2026-09-26 audit, tenth round).
func kimiInstallerCommand(goos string) (string, []string, error) {
	path, err := kimiInstallerScript(goos)
	if err != nil {
		return "", nil, err
	}
	switch goos {
	case "windows":
		return "powershell", []string{
			"-NoProfile",
			"-ExecutionPolicy",
			"Bypass",
			"-Command",
			"$verified = " + psQuote(path) + "; $installer = Join-Path $env:TEMP 'install-kimi.ps1'; Copy-Item -LiteralPath $verified -Destination $installer -Force; & $installer; Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue; Remove-Item -LiteralPath $verified -Force -ErrorAction SilentlyContinue",
		}, nil
	case "darwin", "linux":
		return "bash", []string{path}, nil
	default:
		return "", nil, fmt.Errorf("unsupported platform for kimi install: %s", goos)
	}
}
