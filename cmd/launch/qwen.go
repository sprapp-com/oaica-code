package launch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ollama/ollama/cmd/config"
	"github.com/ollama/ollama/cmd/internal/fileutil"
	"github.com/ollama/ollama/envconfig"
)

const qwenOllamaEnvKey = "OLLAMA_API_KEY"

// qwenProviderNameSuffix marks a provider entry this package wrote, so a later
// launch can recognise and replace it instead of appending a duplicate.
const qwenProviderNameSuffix = " (Ollama)"

var qwenGOOS = runtime.GOOS

type Qwen struct{}

func (q *Qwen) String() string { return "Qwen Code" }

func (q *Qwen) findPath() (string, error) {
	if p, err := exec.LookPath("qwen"); err == nil {
		return p, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	var candidates []string
	switch qwenGOOS {
	case "darwin":
		candidates = []string{
			"/opt/homebrew/bin/qwen",
			"/usr/local/bin/qwen",
			filepath.Join(home, ".npm-global", "bin", "qwen"),
			filepath.Join(home, ".local", "bin", "qwen"),
			filepath.Join(home, "Library", "Application Support", "qwen", "bin", "qwen"),
		}
		candidates = append(candidates, qwenNVMCandidatePaths(home)...)
	case "windows":
		candidates = []string{
			filepath.Join(qwenWindowsAppData(home), "npm", "qwen.cmd"),
			filepath.Join(qwenWindowsAppData(home), "npm", "qwen.exe"),
			filepath.Join(qwenWindowsLocalAppData(home), "npm", "qwen.cmd"),
			filepath.Join(qwenWindowsLocalAppData(home), "npm", "qwen.exe"),
			filepath.Join(home, "AppData", "Local", "Programs", "qwen", "qwen.exe"),
			filepath.Join(home, "AppData", "Roaming", "qwen", "bin", "qwen.exe"),
		}
	default:
		candidates = []string{
			filepath.Join(home, ".npm-global", "bin", "qwen"),
			filepath.Join(home, ".local", "bin", "qwen"),
			filepath.Join(home, ".cargo", "bin", "qwen"),
			"/usr/local/bin/qwen",
		}
		candidates = append(candidates, qwenNVMCandidatePaths(home)...)
	}

	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("qwen binary not found (checked PATH and common npm install locations)")
}

func qwenNVMCandidatePaths(home string) []string {
	matches, err := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin", "qwen"))
	if err != nil {
		return nil
	}
	return matches
}

func qwenWindowsAppData(home string) string {
	if appData := os.Getenv("APPDATA"); appData != "" {
		return appData
	}
	return filepath.Join(home, "AppData", "Roaming")
}

func qwenWindowsLocalAppData(home string) string {
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		return localAppData
	}
	return filepath.Join(home, "AppData", "Local")
}

func ensureQwenInstalled() (string, error) {
	if path, err := (&Qwen{}).findPath(); err == nil {
		return path, nil
	}

	if err := checkQwenInstallerDependencies(); err != nil {
		return "", err
	}

	ok, err := ConfirmPrompt("Qwen Code is not installed. Install now?")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("qwen installation cancelled")
	}

	bin, args, err := qwenInstallerCommand(qwenGOOS)
	if err != nil {
		return "", err
	}
	if qwenGOOS != "windows" && len(args) > 0 {
		// The unix plan runs the verified download's temp file itself
		// (qwenInstallerCommand's single argument), so this is the only place
		// it can be removed — kimi's installer does the same. On Windows the
		// plan copies it into %TEMP% and runs that copy instead, and that arm
		// removes both the copy and the verified file itself (a child
		// PowerShell's `finally` is the only place that survives an installer
		// that fails part way).
		defer os.Remove(args[0])
	}

	fmt.Fprintf(os.Stderr, "\nInstalling Qwen Code...\n")
	shimDir, cleanup, err := qwenInstallShimDir()
	if err != nil {
		return "", err
	}
	defer cleanup()

	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = qwenInstallerEnv(os.Environ(), shimDir)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to install qwen: %w", err)
	}

	path, err := (&Qwen{}).findPath()
	if err != nil {
		return "", fmt.Errorf("qwen was installed but the binary was not found on PATH\n\nYou may need to restart your shell")
	}

	fmt.Fprintf(os.Stderr, "%sQwen Code installed successfully%s\n\n", ansiGreen, ansiReset)
	return path, nil
}

func qwenInstallShimDir() (string, func(), error) {
	dir, err := os.MkdirTemp("", "ollama-qwen-install-*")
	if err != nil {
		return "", nil, err
	}

	cleanup := func() {
		_ = os.RemoveAll(dir)
	}

	if qwenGOOS == "windows" {
		for _, name := range []string{"qwen.cmd", "qwen.bat"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("@echo off\r\nexit /b 0\r\n"), 0o755); err != nil {
				cleanup()
				return "", nil, err
			}
		}
		return dir, cleanup, nil
	}

	if err := os.WriteFile(filepath.Join(dir, "qwen"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

func qwenInstallerEnv(env []string, shimDir string) []string {
	out := make([]string, 0, len(env)+1)
	pathEntry := "PATH=" + shimDir
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, "PATH") {
			pathEntry = key + "=" + shimDir + string(os.PathListSeparator) + value
			continue
		}
		out = append(out, entry)
	}
	return append(out, pathEntry)
}

func checkQwenInstallerDependencies() error {
	switch qwenGOOS {
	case "windows":
		if _, err := exec.LookPath("powershell"); err != nil {
			return fmt.Errorf("qwen is not installed and required dependencies are missing\n\nInstall the following first:\n  PowerShell: https://learn.microsoft.com/powershell/\n\nThen re-run:\n  oaica launch qwen")
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
			return fmt.Errorf("qwen is not installed and required dependencies are missing\n\nInstall the following first:\n  %s\n\nThen re-run:\n  oaica launch qwen", strings.Join(missing, "\n  "))
		}
	}
	return nil
}

// qwenInstallScriptURL / qwenInstallBatURL are the upstream installer scripts.
// Package vars so a test can point them at a local server and drive
// installer_dl.go's real checksum gate; production points them at the vendor.
var (
	qwenInstallScriptURL = "https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen.sh"
	qwenInstallBatURL    = "https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen.bat"
)

// qwenInstallerScript downloads this platform's installer through
// installer_dl.go's verified download — the same gate every other installer in
// this package uses (audit L3) — and returns the temp file it wrote. The bytes
// are hashed and a SHA-256 pin (built-in, or OAICA_INSTALL_SHA256_<URL>) is
// enforced before anything is allowed to run them.
func qwenInstallerScript(goos string) (string, error) {
	switch goos {
	case "windows":
		return fetchInstallerScriptFn(qwenInstallBatURL)
	case "darwin", "linux":
		return fetchInstallerScriptFn(qwenInstallScriptURL)
	default:
		return "", fmt.Errorf("unsupported platform for qwen install: %s", goos)
	}
}

// qwenInstallerCommand is the command that runs the installer for a platform.
// On darwin/linux that is bash plus exactly one argument: the verified file.
// It used to be
//
//	bash -c "set -o pipefail; curl -fsSL <url> | sed ... | bash"
//
// which piped a remote script straight into a shell with no integrity check at
// all — a compromised CDN, a DNS hijack or a MITM on the user's network
// executed attacker code with the user's privileges, and a download that died
// mid-way could splice into a syntactically valid partial script. The fetch now
// goes through qwenInstallerScript, so the exact bytes that were hashed are the
// bytes that run (2026-09-26 audit, tenth round).
//
// The sed stage that used to strip the script's trailing block (up to and
// including its final `exec qwen`) is gone with it: the shim qwen that
// qwenInstallShimDir puts first on PATH is what keeps that last exec from
// launching the app, which is how the Windows path already worked. On Windows
// the verified file is copied to a .bat name before it runs — the download is
// shared code and writes a .sh, and `& $installer` needs cmd to recognise the
// extension.
func qwenInstallerCommand(goos string) (string, []string, error) {
	path, err := qwenInstallerScript(goos)
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
			// The copy is what makes this arm unlike the unix one: cmd has to
			// recognise the extension, and the verified download always writes
			// a .sh. Two things about how that copy is made matter.
			//
			// The name is random, not the fixed "install-qwen.bat": %TEMP% is
			// shared, so a fixed name is a path another local process can
			// predict and pre-create, and Copy-Item -Force onto an existing
			// reparse point writes through it — the bytes that then run from
			// `& $installer` need not be the bytes installer_dl.go verified.
			//
			// And both files are removed in a finally, so they go even when the
			// installer fails or throws, and so does $verified — the caller's
			// removal is unix-only (it is args[0] there, and this arm does not
			// hand back the file it runs), and the copy must not be the one file
			// of this pair that outlives the install. Left behind, the verified
			// script and the copy it was rewritten into accumulated in the
			// user's %TEMP% on every attempt (2026-09-26 audit, round 16).
			"$verified = " + psQuote(path) + "; $installer = Join-Path $env:TEMP ('install-qwen-' + [System.IO.Path]::GetRandomFileName() + '.bat'); try { Copy-Item -LiteralPath $verified -Destination $installer -Force; $content = Get-Content -Raw -Path $installer; $content = $content -replace '(?m)^\\s*call qwen\\s*$', 'REM call qwen'; Set-Content -Path $installer -Value $content -Encoding ASCII; & $installer } finally { Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue; Remove-Item -LiteralPath $verified -Force -ErrorAction SilentlyContinue }",
		}, nil
	case "darwin", "linux":
		return "bash", []string{path}, nil
	default:
		return "", nil, fmt.Errorf("unsupported platform for qwen install: %s", goos)
	}
}

// psQuote renders s as a PowerShell single-quoted literal; a quote inside is
// doubled, which is PowerShell's own escape.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func (q *Qwen) Run(model string, _ []LaunchModel, args []string) error {
	forceTools, args := extractForceTools(args)
	if err := gateOpenAITools(model, forceTools); err != nil {
		return err
	}

	qwenPath, err := q.findPath()
	if err != nil {
		return fmt.Errorf("qwen is not installed: %w", err)
	}

	cmd := exec.Command(qwenPath, qwenLaunchArgs(model, args)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = qwenLaunchEnv(model)
	return cmd.Run()
}

func (q *Qwen) Paths() []string {
	path, err := q.configPath()
	if err != nil {
		return nil
	}
	return []string{path}
}

func (q *Qwen) Configure(model string) error {
	if model == "" {
		return nil
	}

	configPath, err := q.configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}

	// ~/.qwen is qwen's OWN store, so the lock is keyed under ~/.oaica/locks
	// (foreignStoreLockBase) rather than dropped inside it, as the other foreign
	// stores do. It has to cover the read as well as the publish: readConfig is
	// called a frame down, and two overlapping writers that read before taking
	// the lock each publish a snapshot taken before the other's settings landed
	// — the launch that renames last decides model.name, auth.baseUrl,
	// modelProviders.openai and env.OLLAMA_API_KEY while both report success, so
	// one of them runs the other launch's model. qwen keeps its own keys in this
	// document too, so a write it made between oaica's read and rename is lost
	// the same way (2026-09-26 audit, fourteenth round).
	return fileutil.WithFileLock(foreignStoreLockBase(configPath), func() error {
		cfg, err := q.readConfig()
		if err != nil {
			return err
		}

		applyQwenOllamaConfig(cfg, model)

		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}

		return fileutil.WriteWithBackup(configPath, data, "qwen")
	})
}

func applyQwenOllamaConfig(cfg map[string]any, model string) {
	envCfg := qwenMap(cfg["env"])
	applyQwenOllamaKey(envCfg, model)
	cfg["env"] = envCfg

	modelProviders := qwenMap(cfg["modelProviders"])
	modelProviders["openai"] = qwenMergeOpenAIProviders(modelProviders["openai"], qwenProvider(model), qwenBaseURLFor(model))
	cfg["modelProviders"] = modelProviders

	security := qwenMap(cfg["security"])
	auth := qwenMap(security["auth"])
	auth["selectedType"] = "openai"
	auth["baseUrl"] = qwenBaseURLFor(model)
	security["auth"] = auth
	cfg["security"] = security

	modelCfg := qwenMap(cfg["model"])
	modelCfg["name"] = qwenModelIDFor(model)
	cfg["model"] = modelCfg
}

// applyQwenOllamaKey sets env.OLLAMA_API_KEY to what the configured provider
// needs, without destroying a value that already works.
//
// For a DAEMON-backed launch the value is the literal placeholder "ollama" —
// the local daemon does not check it — so an existing value is left alone:
// OLLAMA_API_KEY is also how ollama.com is reached, and
// `oaica launch qwen llama3.2` used to overwrite that with "ollama"
// (2026-09-26 audit, tenth round). A launch that needs no credential must not
// consume one.
//
// For a REMOTE-backed launch the base URL is ours and the provider only works
// with that remote's token, so the value is written: leaving a stale key there
// would be the silent-401 failure this function exists to prevent.
func applyQwenOllamaKey(envCfg map[string]any, model string) {
	if _, isRemote := resolveRemoteEndpoint(model); !isRemote {
		if existing, _ := envCfg[qwenOllamaEnvKey].(string); strings.TrimSpace(existing) != "" {
			return
		}
	}
	envCfg[qwenOllamaEnvKey] = qwenKeyFor(model)
}

func qwenMap(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// qwenMergeOpenAIProviders puts our provider first and drops the entry it owned
// before, keeping every provider that is the user's.
//
// Ownership is "envKey is ours AND the name carries our suffix AND the base URL
// is either this launch's target or the daemon's". Requiring only the daemon's
// base URL — as this did — meant the entry written by a REMOTE-backed launch
// never matched and never went away, so each launch appended another dead copy
// (three launches of one remote model left three identical providers, all
// pointing at the remote with the same envKey) (2026-09-26 audit, tenth round).
// The suffix is required so a hand-written entry that merely uses the same env
// key ("Remote Ollama", in this package's own merge test) is not treated as
// ours.
func qwenMergeOpenAIProviders(value any, provider map[string]any, targetBaseURL string) []any {
	merged := []any{provider}
	for _, existing := range qwenProviderList(value) {
		if qwenIsOllamaProvider(existing) || qwenIsOurProvider(existing, targetBaseURL) {
			continue
		}
		merged = append(merged, existing)
	}
	return merged
}

// qwenIsOurProvider reports whether value is an entry this package wrote
// before, for the base URL being configured now or for the daemon.
func qwenIsOurProvider(value any, targetBaseURL string) bool {
	provider, ok := value.(map[string]any)
	if !ok {
		return false
	}
	envKey, _ := provider["envKey"].(string)
	if envKey != qwenOllamaEnvKey {
		return false
	}
	name, _ := provider["name"].(string)
	if !strings.HasSuffix(name, qwenProviderNameSuffix) {
		return false
	}
	baseURL, _ := provider["baseUrl"].(string)
	baseURL = strings.TrimRight(baseURL, "/")
	return baseURL == strings.TrimRight(targetBaseURL, "/") || baseURL == qwenBaseURL()
}

func qwenProviderList(value any) []any {
	switch providers := value.(type) {
	case []any:
		return providers
	case []map[string]any:
		out := make([]any, 0, len(providers))
		for _, provider := range providers {
			out = append(out, provider)
		}
		return out
	default:
		return nil
	}
}

func qwenIsOllamaProvider(value any) bool {
	provider, ok := value.(map[string]any)
	if !ok {
		return false
	}
	envKey, _ := provider["envKey"].(string)
	baseURL, _ := provider["baseUrl"].(string)
	return envKey == qwenOllamaEnvKey && strings.TrimRight(baseURL, "/") == qwenBaseURL()
}

// qwenManagedModelName is the model name oaica wrote into cfg, or "".
func qwenManagedModelName(cfg map[string]any) string {
	if modelCfg, ok := cfg["model"].(map[string]any); ok {
		if name, ok := modelCfg["name"].(string); ok {
			return strings.TrimSpace(name)
		}
	}

	modelProviders, ok := cfg["modelProviders"].(map[string]any)
	if !ok {
		return ""
	}

	providers, ok := modelProviders["openai"].([]any)
	if !ok || len(providers) == 0 {
		return ""
	}

	provider, ok := providers[0].(map[string]any)
	if !ok {
		return ""
	}

	name, _ := provider["id"].(string)
	return strings.TrimSpace(name)
}

// qwenManagedBaseURL is the endpoint the written config points at: the remote's
// own base for a user-remote model, otherwise the daemon's /v1
// (qwenBaseURLFor). security.auth.baseUrl is written by Configure beside the
// provider list; the provider entry is the fallback for a config that predates
// one of the two.
func qwenManagedBaseURL(cfg map[string]any) string {
	if security, ok := cfg["security"].(map[string]any); ok {
		if auth, ok := security["auth"].(map[string]any); ok {
			if baseURL, ok := auth["baseUrl"].(string); ok {
				if trimmed := strings.TrimSpace(baseURL); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	if modelProviders, ok := cfg["modelProviders"].(map[string]any); ok {
		if providers, ok := modelProviders["openai"].([]any); ok && len(providers) > 0 {
			if provider, ok := providers[0].(map[string]any); ok {
				baseURL, _ := provider["baseUrl"].(string)
				return strings.TrimSpace(baseURL)
			}
		}
	}
	return ""
}

// qwenConfiguredRemoteForBase returns the configured remote a Qwen endpoint
// belongs to, if any. Reads the remote store only — no network — so it is safe
// on every picker-state read.
func qwenConfiguredRemoteForBase(baseURL string) (userRemote, bool) {
	remotes, err := loadUserRemotes()
	if err != nil {
		return userRemote{}, false
	}
	for _, r := range remotes {
		if strings.TrimRight(r.openAIBase(), "/") == strings.TrimRight(baseURL, "/") {
			return r, true
		}
	}
	return userRemote{}, false
}

// qwenRemotePickerName maps a configured Qwen endpoint plus the upstream id
// written in the config back to the picker name the launch used
// ("box/big-model"). Configure stores the bare upstream id (qwenModelIDFor),
// which the launcher cannot match against what it saved, so CurrentModel
// translates it back. Returns "" when no configured remote serves that endpoint
// under that id.
func qwenRemotePickerName(baseURL, upstream string) string {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return ""
	}
	remote, ok := qwenConfiguredRemoteForBase(baseURL)
	if !ok {
		return ""
	}
	candidate := remote.Name + "/" + upstream
	ep, ok := resolveRemoteEndpoint(candidate)
	if !ok || strings.TrimRight(ep.BaseURL, "/") != strings.TrimRight(baseURL, "/") || ep.UpstreamModel != upstream {
		return ""
	}
	return candidate
}

func (q *Qwen) CurrentModel() string {
	cfg, err := q.readConfig()
	if err != nil {
		return ""
	}

	name := qwenManagedModelName(cfg)
	if name == "" {
		return ""
	}

	// A remote config stores the bare upstream id; report the picker name the
	// launch wrote, so this answer agrees with what the launcher saved. A
	// non-daemon endpoint counts only when a configured remote actually serves
	// it — anything else is a foreign config and keeps its own name.
	baseURL := qwenManagedBaseURL(cfg)
	if baseURL != "" && strings.TrimRight(baseURL, "/") != strings.TrimRight(qwenBaseURL(), "/") {
		if picker := qwenRemotePickerName(baseURL, name); picker != "" {
			return picker
		}
	}
	return name
}

func (q *Qwen) Onboard() error {
	return config.MarkIntegrationOnboarded("qwen")
}

func (q *Qwen) RequiresInteractiveOnboarding() bool { return false }

func (q *Qwen) configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine config path")
	}
	return filepath.Join(home, ".qwen", "settings.json"), nil
}

func (q *Qwen) readConfig() (map[string]any, error) {
	configPath, err := q.configPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}

	cfg := map[string]any{}
	// UseNumber: Configure writes this document back whole, and it is the
	// user's file — qwen keeps its own keys in it. Decoding into
	// map[string]any makes every number in it a float64, so an integer above
	// 2^53 comes back as a different number and 1.0 comes back as 1 — a config
	// changed by a launch that only meant to add a provider entry
	// (2026-09-26 audit, tenth round).
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse qwen config: %w", err)
	}

	return cfg, nil
}

func qwenBaseURL() string {
	return strings.TrimRight(envconfig.Host().String(), "/") + "/v1"
}

// qwenBaseURLFor is the provider base URL Qwen should use: the remote's direct
// base for a user-remote model, otherwise the daemon's /v1.
func qwenBaseURLFor(model string) string {
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return strings.TrimRight(ep.BaseURL, "/")
	}
	return qwenBaseURL()
}

// qwenModelIDFor is the model id Qwen should use: the bare upstream id for a
// user-remote model, otherwise the picker name.
func qwenModelIDFor(model string) string {
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return ep.UpstreamModel
	}
	return model
}

// qwenKeyFor is the API key Qwen should use: the remote's token for a
// user-remote model, "ollama" for the daemon.
func qwenKeyFor(model string) string {
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return ep.Token
	}
	return "ollama"
}

func qwenProvider(model string) map[string]any {
	id := qwenModelIDFor(model)
	return map[string]any{
		"id":      id,
		"name":    id + qwenProviderNameSuffix,
		"baseUrl": qwenBaseURLFor(model),
		"envKey":  qwenOllamaEnvKey,
	}
}

func qwenLaunchArgs(model string, args []string) []string {
	launchArgs := append([]string{}, args...)
	if !qwenHasFlag(launchArgs, "--auth-type") {
		launchArgs = append([]string{"--auth-type", "openai"}, launchArgs...)
	}
	if model != "" && !qwenHasFlag(launchArgs, "--model", "-m") {
		launchArgs = append([]string{"--model", qwenModelIDFor(model)}, launchArgs...)
	}
	return launchArgs
}

func qwenLaunchEnv(model string) []string {
	env := os.Environ()
	env = qwenUpsertEnv(env, "OPENAI_API_KEY", qwenKeyFor(model))
	env = qwenUpsertEnv(env, "OPENAI_BASE_URL", qwenBaseURLFor(model))
	if model != "" {
		env = qwenUpsertEnv(env, "OPENAI_MODEL", qwenModelIDFor(model))
	}
	return env
}

func qwenUpsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	filtered := env[:0]
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, prefix+value)
}

func qwenHasFlag(args []string, names ...string) bool {
	for _, arg := range args {
		for _, name := range names {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				return true
			}
		}
	}
	return false
}
