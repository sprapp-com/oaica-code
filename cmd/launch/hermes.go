package launch

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/config"
	"github.com/ollama/ollama/cmd/internal/fileutil"
	"github.com/ollama/ollama/envconfig"
)

const (
	// https://github.com/NousResearch/hermes-agent/releases/tag/v2026.6.5
	hermesDesktopMinVersion = "v0.16.0"
	// The install scripts are NOT run from these strings: installer_dl.go
	// downloads each to a temp file, enforces its SHA-256 pin, and the file it
	// verified is what runs. A "curl … | bash" or "irm … | iex" here would be
	// the unverified form those arms were moved off (2026-09-26 audit).
	hermesUnixInstallURL    = "https://hermes-agent.nousresearch.com/install.sh"
	hermesWindowsInstallURL = "https://hermes-agent.nousresearch.com/install.ps1"
	hermesProviderName      = "Ollama"
	hermesProviderKey       = "ollama-launch"
	hermesLegacyKey         = "ollama"
	hermesPlaceholderKey    = "ollama"
	hermesGatewaySetupHint  = "hermes gateway setup"
	hermesGatewaySetupTitle = "Connect a messaging app now?"
)

var (
	hermesGOOS      = runtime.GOOS
	hermesLookPath  = exec.LookPath
	hermesCommand   = exec.Command
	hermesUserHome  = os.UserHomeDir
	hermesOllamaURL = envconfig.ConnectableHost
)

var hermesMessagingEnvGroups = [][]string{
	{"TELEGRAM_BOT_TOKEN"},
	{"DISCORD_BOT_TOKEN"},
	{"SLACK_BOT_TOKEN"},
	{"SIGNAL_ACCOUNT"},
	{"EMAIL_ADDRESS"},
	{"TWILIO_ACCOUNT_SID"},
	{"MATRIX_ACCESS_TOKEN", "MATRIX_PASSWORD"},
	{"MATTERMOST_TOKEN"},
	{"WHATSAPP_PHONE_NUMBER_ID"},
	{"DINGTALK_CLIENT_ID"},
	{"FEISHU_APP_ID"},
	{"WECOM_BOT_ID"},
	{"WEIXIN_ACCOUNT_ID"},
	{"BLUEBUBBLES_SERVER_URL"},
	{"WEBHOOK_ENABLED"},
}

// Hermes is intentionally not an Editor integration: launch owns one primary
// model and the local Ollama endpoint, while Hermes keeps its own discovery and
// switching UX after startup.
type Hermes struct{}

func (h *Hermes) String() string { return "Hermes Agent" }

func (h *Hermes) Run(_ string, _ []LaunchModel, args []string) error {
	// Hermes reads its primary model from config.yaml. launch configures that
	// default model ahead of time so we can keep runtime invocation simple and
	// still let Hermes discover additional models later via its own UX.
	bin, err := h.binary()
	if err != nil {
		return err
	}
	if err := h.runGatewaySetupPreflight(args, func() error {
		return hermesAttachedCommand(bin, "gateway", "setup").Run()
	}); err != nil {
		return err
	}
	return hermesAttachedCommand(bin, args...).Run()
}

type HermesDesktop struct {
	Hermes
}

func (h *HermesDesktop) String() string { return "Hermes Desktop" }

func (h *HermesDesktop) Run(_ string, _ []LaunchModel, args []string) error {
	bin, err := h.binary()
	if err != nil {
		return err
	}
	if err := h.ensureHermesDesktopMinVersion(bin); err != nil {
		return err
	}
	return hermesAttachedCommand(bin, h.launchArgs(args)...).Run()
}

func (h *HermesDesktop) ensureHermesDesktopMinVersion(bin string) error {
	if hermesGOOS == "windows" {
		return nil
	}
	version := hermesVersionOf(bin)
	if version == "" {
		return nil
	}
	if semver.Compare(version, hermesDesktopMinVersion) >= 0 {
		return nil
	}
	// `hermes update` reaches out and replaces the installed hermes — the same
	// class of unprompted network install that ENTERPRISE.md row 6 promises
	// does not happen ("none of these installers run unprompted"). The
	// version floor is real and the update is usually wanted, but "usually"
	// is not consent: ask, and on a decline say how to do it by hand instead
	// of failing the launch (2026-09-26 audit, second round).
	ok, cerr := ConfirmPrompt(fmt.Sprintf("Hermes %s is older than the minimum (%s) for `hermes desktop`; update now?", version, hermesDesktopMinVersion))
	if cerr != nil {
		return fmt.Errorf("hermes %s is older than the minimum version (%s) for `hermes desktop`, and the update prompt could not be shown (%v) — run `hermes update` yourself and retry",
			version, hermesDesktopMinVersion, cerr)
	}
	if !ok {
		return fmt.Errorf("hermes %s is older than the minimum version (%s) for `hermes desktop` — run `hermes update` yourself and retry",
			version, hermesDesktopMinVersion)
	}
	fmt.Fprintf(os.Stderr, "%sHermes %s is older than the minimum version (%s) for `hermes desktop`; updating...%s\n", ansiGray, version, hermesDesktopMinVersion, ansiReset)
	if err := hermesAttachedCommand(bin, "update").Run(); err != nil {
		return fmt.Errorf("failed to update hermes to %s or newer: %w", hermesDesktopMinVersion, err)
	}
	return nil
}

func hermesVersionOf(bin string) string {
	out, err := hermesCommand(bin, "--version").Output()
	if err != nil {
		return ""
	}
	firstLine := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return parseHermesVersion(firstLine)
}

func parseHermesVersion(firstLine string) string {
	for _, field := range strings.Fields(firstLine) {
		if semver.IsValid(field) {
			return field
		}
	}
	return ""
}

func (h *HermesDesktop) Onboard() error {
	return config.MarkIntegrationOnboarded("hermes-desktop")
}

func (h *HermesDesktop) launchArgs(args []string) []string {
	launchArgs := []string{"desktop"}
	if h.shouldSkipDesktopBuild(args) {
		launchArgs = append(launchArgs, "--skip-build")
	}
	return append(launchArgs, args...)
}

func (h *HermesDesktop) shouldSkipDesktopBuild(args []string) bool {
	if hermesDesktopHasFlag(args, "--skip-build", "--force-build", "--source", "--build-only", "--help", "-h") {
		return false
	}
	return h.packagedAppExists()
}

func (h *HermesDesktop) packagedAppExists() bool {
	for _, root := range hermesDesktopReleaseRoots() {
		for _, candidate := range hermesDesktopPackagedExecutableCandidates(root) {
			if _, err := os.Stat(candidate); err == nil {
				return true
			}
		}
	}
	return false
}

// These roots mirror Hermes' own install layout:
// install.sh uses ~/.hermes/hermes-agent for user installs and
// /usr/local/lib/hermes-agent for new Linux root installs; install.ps1
// and the bootstrap installer use %LOCALAPPDATA%\hermes\hermes-agent on
// Windows. HERMES_HOME and HERMES_INSTALL_DIR are installer-supported
// overrides.
func hermesDesktopReleaseRoots() []string {
	var installRoots []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		installRoots = append(installRoots, filepath.Clean(path))
	}

	if installDir := strings.TrimSpace(os.Getenv("HERMES_INSTALL_DIR")); installDir != "" {
		add(installDir)
	}
	if hermesHome := strings.TrimSpace(os.Getenv("HERMES_HOME")); hermesHome != "" {
		add(filepath.Join(hermesHome, "hermes-agent"))
	}

	home, err := hermesUserHome()
	if err == nil {
		switch hermesGOOS {
		case "windows":
			if localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localAppData != "" {
				add(filepath.Join(localAppData, "hermes", "hermes-agent"))
			}
			add(filepath.Join(home, ".hermes", "hermes-agent"))
		default:
			add(filepath.Join(home, ".hermes", "hermes-agent"))
			if hermesGOOS == "linux" {
				add(filepath.Join(string(filepath.Separator), "usr", "local", "lib", "hermes-agent"))
			}
		}
	}

	seen := make(map[string]bool, len(installRoots))
	releaseRoots := make([]string, 0, len(installRoots))
	for _, root := range installRoots {
		releaseRoot := filepath.Join(root, "apps", "desktop", "release")
		if seen[releaseRoot] {
			continue
		}
		seen[releaseRoot] = true
		releaseRoots = append(releaseRoots, releaseRoot)
	}
	return releaseRoots
}

func hermesDesktopPackagedExecutableCandidates(releaseRoot string) []string {
	switch hermesGOOS {
	case "darwin":
		matches, err := filepath.Glob(filepath.Join(releaseRoot, "mac*", "Hermes.app", "Contents", "MacOS", "Hermes"))
		if err != nil {
			return nil
		}
		return matches
	case "windows":
		return []string{
			filepath.Join(releaseRoot, "win-unpacked", "Hermes.exe"),
			filepath.Join(releaseRoot, "win-ia32-unpacked", "Hermes.exe"),
			filepath.Join(releaseRoot, "win-arm64-unpacked", "Hermes.exe"),
		}
	default:
		return []string{
			filepath.Join(releaseRoot, "linux-unpacked", "hermes"),
			filepath.Join(releaseRoot, "linux-unpacked", "Hermes"),
		}
	}
}

func hermesDesktopHasFlag(args []string, names ...string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		for _, name := range names {
			if arg == name {
				return true
			}
		}
	}
	return false
}

func (h *Hermes) Paths() []string {
	configPath, err := hermesConfigPath()
	if err != nil {
		return nil
	}
	return []string{configPath}
}

func (h *Hermes) Configure(model string) error {
	// Hermes' primary model is configured, not passed to Run, so the capability
	// gate applies here. --force-tools is not threaded through Configure.
	if err := gateOpenAITools(model, false); err != nil {
		return err
	}

	configPath, err := hermesConfigPath()
	if err != nil {
		return err
	}

	// The model inventory is read before the lock: it is a host round-trip, and
	// nothing about it belongs to the config file's critical section.
	models := h.listModels(model)

	// ~/.hermes/config.yaml belongs to Hermes, and oaica rewrites the whole
	// document from a snapshot of it (unknown keys are carried through, not
	// merged into the file). Two oaica commands whose writes overlap would each
	// publish a snapshot taken before the other's, so the read-modify-write runs
	// under the store's lock, keyed under ~/.oaica/locks as every foreign store
	// in this package is (2026-09-26 audit, twelfth round).
	return fileutil.WithFileLock(foreignStoreLockBase(configPath), func() error {
		return writeHermesConfig(configPath, model, models)
	})
}

func writeHermesConfig(configPath, model string, models []string) error {
	// The user's document is edited as a yaml.Node, not decoded into a map and
	// marshalled back: that rewrite deleted every comment in the file and
	// re-rendered every scalar (1.0 became 1, a hand-written date became the
	// encoder's timestamp) on every launch (2026-09-27 audit, round 17). Only
	// the keys oaica manages are set below; the rest of the file — other
	// providers' entries included — is left exactly as the user wrote it.
	document, err := readYAMLDocument(configPath)
	if err != nil {
		return fmt.Errorf("parse hermes config: %w", err)
	}
	root := yamlRootMapping(document)

	// The provider map is merged in Go (hermesUserProviders rebuilds the
	// managed entry and hermesWithoutManagedCustomProviders filters the
	// legacy list), so the value is decoded for that work and written back into
	// the document afterwards.
	providersValue, err := yamlNodeAsAny(yamlNodeValue(root, "providers"))
	if err != nil {
		return fmt.Errorf("parse hermes providers: %w", err)
	}
	providers := hermesUserProviders(providersValue)
	entry := hermesManagedProviderEntry(providers)
	if entry == nil {
		entry = make(map[string]any)
	}
	entry["name"] = hermesProviderName
	entry["api"] = hermesBaseURLFor(model)
	entry["default_model"] = hermesModelIDFor(model)
	entry["models"] = hermesStringListAny(models)

	providersNode := yamlEnsureMapping(root, "providers")
	if err := yamlSetValue(providersNode, hermesProviderKey, entry); err != nil {
		return err
	}
	// The legacy key is removed only when the entry under it is one oaica wrote:
	// "ollama" is Hermes' own provider name, so the entry there may be the
	// user's (2026-09-27 audit, round 21).
	if hermesLegacyProviderIsOurs(providers) {
		yamlDeleteKey(providersNode, hermesLegacyKey)
	}

	// Filtered as nodes, not as a decoded map: oaica removes the one entry it
	// owns and must leave the entries it does not — comments, key order,
	// formatting — exactly as the user wrote them (2026-09-27 audit, round 18).
	if customProviders := hermesPreservedCustomProvidersNode(yamlNodeValue(root, "custom_providers")); customProviders == nil {
		yamlDeleteKey(root, "custom_providers")
	} else {
		yamlSetNode(root, "custom_providers", customProviders)
	}

	// launch writes the minimum provider/default-model settings needed to
	// bootstrap Hermes against Ollama. The active provider stays on a
	// launch-owned key so /model stays aligned with the launcher-managed entry,
	// and the Ollama endpoint lives in providers: so the picker shows one row.
	modelSection := yamlEnsureMapping(root, "model")
	for key, value := range map[string]string{
		"provider": hermesProviderKey,
		"default":  hermesModelIDFor(model),
		"base_url": hermesBaseURLFor(model),
		"api_key":  hermesKeyFor(model),
	} {
		if err := yamlSetValue(modelSection, key, value); err != nil {
			return err
		}
	}

	// use Hermes' built-in web toolset for now.
	// TODO(parthsareen): move this to using Ollama web search
	// Appended as a node for the same reason as custom_providers above: the
	// user's own toolsets are theirs, comments included.
	yamlSetNode(root, "toolsets", hermesToolsetsNode(yamlNodeValue(root, "toolsets")))

	data, err := yamlMarshalDocument(document)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}
	return fileutil.WriteWithBackup(configPath, data, "hermes")
}

func (h *Hermes) CurrentModel() string {
	configPath, err := hermesConfigPath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}

	cfg := map[string]any{}
	if yaml.Unmarshal(data, &cfg) != nil {
		return ""
	}
	return hermesManagedCurrentModel(cfg)
}

// hermesConfiguredRemoteForBase returns the configured remote a Hermes endpoint
// belongs to, if any. Reads the remote store only — no network — so it is safe
// on every picker-state read.
func hermesConfiguredRemoteForBase(baseURL string) (userRemote, bool) {
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

// hermesRemotePickerName maps a remote endpoint plus the upstream id written in
// the config back to the picker name the launch used ("box/big-model"). The
// config stores the bare upstream id, which the launcher cannot match against
// what it saved, so CurrentModel translates it back. Returns "" when no
// configured remote serves that endpoint under that id.
func hermesRemotePickerName(baseURL, upstream string) string {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return ""
	}
	remote, ok := hermesConfiguredRemoteForBase(baseURL)
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

func (h *Hermes) Onboard() error {
	return config.MarkIntegrationOnboarded("hermes")
}

func (h *Hermes) RequiresInteractiveOnboarding() bool {
	return false
}

func (h *Hermes) RefreshRuntimeAfterConfigure() error {
	running, err := h.gatewayRunning()
	if err != nil {
		return fmt.Errorf("check Hermes gateway status: %w", err)
	}
	if !running {
		return nil
	}

	fmt.Fprintf(os.Stderr, "%sRefreshing Hermes messaging gateway...%s\n", ansiGray, ansiReset)
	if err := h.restartGateway(); err != nil {
		return fmt.Errorf("restart Hermes gateway: %w", err)
	}
	fmt.Fprintln(os.Stderr)
	return nil
}

func (h *Hermes) installed() bool {
	_, err := h.binary()
	return err == nil
}

func (h *Hermes) ensureInstalled() error {
	return h.ensureInstalledFor("hermes")
}

func (h *Hermes) ensureInstalledFor(command string) error {
	if h.installed() {
		return nil
	}

	var missing []string
	if hermesGOOS != "windows" {
		for _, dep := range []string{"bash", "curl", "git"} {
			if _, err := hermesLookPath(dep); err != nil {
				missing = append(missing, dep)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("Hermes is not installed and required dependencies are missing\n\nInstall the following first:\n  %s\n\nThen re-run:\n  oaica launch %s", strings.Join(missing, "\n  "), command)
	}

	ok, err := ConfirmPrompt("Hermes is not installed. Install now?")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("hermes installation cancelled")
	}

	fmt.Fprintf(os.Stderr, "\nInstalling Hermes...\n")
	if err := h.runInstallScript(); err != nil {
		return fmt.Errorf("failed to install hermes: %w", err)
	}

	if !h.installed() {
		return fmt.Errorf("hermes was installed but the binary was not found on PATH\n\nYou may need to restart your shell")
	}

	fmt.Fprintf(os.Stderr, "%sHermes installed successfully%s\n\n", ansiGreen, ansiReset)
	return nil
}

// hermesWindowsInstallerCommand returns the Windows install command. It asks
// installer_dl.go for a verified download — fetch to a temp file, enforce a
// SHA-256 pin when one exists — and runs the FILE it handed back, the same gate
// the unix arm below and the kimi/qwen/muse/claude installers use.
//
// This arm used to be
//
//	powershell -Command "& ([scriptblock]::Create((irm <url>))) -SkipSetup"
//
// which downloaded the script itself and evaluated it inline: no temp file, no
// hash, no pin, no unpinned warning, no bytes to review. Whatever the network
// answered ran with the user's privileges, so a compromised CDN, a DNS hijack
// or a MITM executed attacker code (2026-09-26 audit, thirteenth round).
func hermesWindowsInstallerCommand() (string, []string, error) {
	path, err := fetchInstallerScriptFn(hermesWindowsInstallURL)
	if err != nil {
		return "", nil, err
	}
	// The download is shared code and writes a .sh name, which PowerShell will
	// not execute as a script, so copy it to a .ps1 first — to a randomised
	// name, with both files removed in a finally, which is the pattern
	// qwenInstallerCommand documents in full (2026-09-27 audit, round 17).
	return "powershell.exe", []string{
		"-NoProfile",
		"-ExecutionPolicy",
		"Bypass",
		"-Command",
		"$verified = " + psQuote(path) + "; $installer = Join-Path $env:TEMP ('install-hermes-' + [System.IO.Path]::GetRandomFileName() + '.ps1'); try { Copy-Item -LiteralPath $verified -Destination $installer -Force; & $installer -SkipSetup } finally { Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue; Remove-Item -LiteralPath $verified -Force -ErrorAction SilentlyContinue }",
	}, nil
}

func (h *Hermes) runInstallScript() error {
	if hermesGOOS == "windows" {
		bin, args, err := hermesWindowsInstallerCommand()
		if err != nil {
			return err
		}
		return hermesAttachedCommand(bin, args...).Run()
	}
	// Verified-download flow (audit L3): fetch + SHA-pin check, then execute
	// the downloaded file instead of piping curl straight into bash.
	return runInstallerScriptFn(hermesUnixInstallURL, "--skip-setup")
}

func (h *Hermes) listModels(defaultModel string) []string {
	// The list is written into the provider entry the launch configures, beside
	// an `api` that is EITHER the daemon's /v1 or a user remote's base
	// (hermesBaseURLFor). Reading the daemon's inventory for both advertised the
	// local models — the daemon's, and every user-remote model the bare-id sweep
	// finds — as available on the remote, so the user picked one and Hermes
	// posted a model that remote does not serve (2026-09-27 audit, round 21,
	// F6). Each endpoint is now listed from itself, and a remote that cannot be
	// listed falls back to the model this launch configures rather than to
	// another endpoint's models.
	if remote, ok := hermesConfiguredRemoteForBase(hermesBaseURLFor(defaultModel)); ok {
		return hermesRemoteModelList(remote, defaultModel)
	}

	client := hermesOllamaClient()
	resp, err := client.List(context.Background())
	if err != nil {
		return []string{defaultModel}
	}

	models := make([]string, 0, len(resp.Models)+1)
	seen := make(map[string]struct{}, len(resp.Models)+1)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		models = append(models, name)
	}

	add(defaultModel)
	for _, entry := range resp.Models {
		add(entry.Name)
	}
	if len(models) == 0 {
		return []string{defaultModel}
	}
	return models
}

// hermesRemoteModelList is the model list for a provider whose api is a user
// remote: that remote's own inventory, in the bare ids the config selects by
// (remoteDisplayID), with the launched model first. The launched model is added
// from the endpoint oaica resolved rather than from the fetched list, so a
// remote whose /v1/models omits it (llama-server reports one entry per loaded
// model, and a proxy may report none) still gets a list that contains the model
// Hermes was just configured to select.
func hermesRemoteModelList(remote userRemote, defaultModel string) []string {
	models := []string{hermesModelIDFor(defaultModel)}
	seen := map[string]struct{}{models[0]: {}}
	ids, err := fetchRemoteModelsCached(remote)
	if err != nil {
		return models
	}
	for _, id := range ids {
		name := strings.TrimSpace(remoteDisplayID(id))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		models = append(models, name)
	}
	return models
}

func (h *Hermes) binary() (string, error) {
	if path, err := hermesLookPath("hermes"); err == nil {
		return path, nil
	}

	if hermesGOOS == "windows" {
		for _, fallback := range hermesWindowsBinaryFallbacks() {
			if _, err := os.Stat(fallback); err == nil {
				return fallback, nil
			}
		}
		return "", fmt.Errorf("hermes is not installed")
	}

	home, err := hermesUserHome()
	if err != nil {
		return "", err
	}
	fallback := filepath.Join(home, ".local", "bin", "hermes")
	if _, err := os.Stat(fallback); err == nil {
		return fallback, nil
	}

	return "", fmt.Errorf("hermes is not installed")
}

func hermesWindowsBinaryFallbacks() []string {
	var roots []string
	add := func(root string) {
		root = strings.TrimSpace(root)
		if root != "" {
			roots = append(roots, filepath.Clean(root))
		}
	}

	add(os.Getenv("HERMES_HOME"))
	add(os.Getenv("LOCALAPPDATA"))
	if home, err := hermesUserHome(); err == nil {
		add(filepath.Join(home, "AppData", "Local"))
	}

	seen := make(map[string]bool, len(roots))
	var fallbacks []string
	for _, root := range roots {
		if seen[root] {
			continue
		}
		seen[root] = true
		fallbacks = append(fallbacks, filepath.Join(root, "hermes-agent", "venv", "Scripts", "hermes.exe"))
		if filepath.Base(root) != "hermes" {
			fallbacks = append(fallbacks, filepath.Join(root, "hermes", "hermes-agent", "venv", "Scripts", "hermes.exe"))
		}
	}
	return fallbacks
}

func hermesHomePath() (string, error) {
	if hermesHome := strings.TrimSpace(os.Getenv("HERMES_HOME")); hermesHome != "" {
		return filepath.Clean(hermesHome), nil
	}
	if hermesGOOS == "windows" {
		if localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localAppData != "" {
			return filepath.Join(localAppData, "hermes"), nil
		}
		home, err := hermesUserHome()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "AppData", "Local", "hermes"), nil
	}
	home, err := hermesUserHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hermes"), nil
}

func hermesConfigPath() (string, error) {
	home, err := hermesHomePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.yaml"), nil
}

func hermesBaseURL() string {
	return strings.TrimRight(hermesOllamaURL().String(), "/") + "/v1"
}

// hermesBaseURLFor is the provider endpoint Hermes should talk to: the remote's
// direct base for a user-remote model, otherwise the daemon's /v1.
func hermesBaseURLFor(model string) string {
	if ep, ok := resolveLaunchTargetEndpoint(model); ok {
		return strings.TrimRight(ep.BaseURL, "/")
	}
	return hermesBaseURL()
}

// hermesModelIDFor is the default model Hermes should select, answered by
// childModelIDFor: the bare upstream id for a user-remote model, and otherwise
// the id the endpoint it is written beside actually serves. The base URL
// sibling already resolved the launch target; returning the raw spelling
// instead wrote a model name the daemon does not have (2026-09-27 audit,
// round 23).
func hermesModelIDFor(model string) string {
	return childModelIDFor(model)
}

// hermesKeyFor is the provider API key: the remote's token for a user-remote
// model, otherwise the placeholder Hermes uses for the daemon.
func hermesKeyFor(model string) string {
	if ep, ok := resolveLaunchTargetEndpoint(model); ok {
		return ep.Token
	}
	return hermesPlaceholderKey
}

func hermesEnvPath() (string, error) {
	home, err := hermesHomePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".env"), nil
}

func (h *Hermes) runGatewaySetupPreflight(args []string, runSetup func() error) error {
	if len(args) > 0 || !isInteractiveSession() || currentLaunchConfirmPolicy.yes || currentLaunchConfirmPolicy.requireYesMessage {
		return nil
	}
	if h.messagingConfigured() {
		return nil
	}

	fmt.Fprintf(os.Stderr, "\nHermes can message you on Telegram, Discord, Slack, and more.\n\n")
	ok, err := ConfirmPromptWithOptions(hermesGatewaySetupTitle, ConfirmOptions{
		YesLabel: "Yes",
		NoLabel:  "Set up later",
	})
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := runSetup(); err != nil {
		return fmt.Errorf("hermes messaging setup failed: %w\n\nTry running: %s", err, hermesGatewaySetupHint)
	}
	return nil
}

func (h *Hermes) messagingConfigured() bool {
	envVars, err := h.gatewayEnvVars()
	if err != nil {
		return false
	}
	for _, group := range hermesMessagingEnvGroups {
		for _, key := range group {
			if strings.TrimSpace(envVars[key]) != "" {
				return true
			}
		}
	}
	return false
}

func (h *Hermes) gatewayEnvVars() (map[string]string, error) {
	envVars := make(map[string]string)

	envFilePath, err := hermesEnvPath()
	if err != nil {
		return nil, err
	}
	switch data, err := os.ReadFile(envFilePath); {
	case err == nil:
		for key, value := range hermesParseEnvFile(data) {
			envVars[key] = value
		}
	case os.IsNotExist(err):
		// nothing persisted yet
	default:
		return nil, err
	}

	for _, group := range hermesMessagingEnvGroups {
		for _, key := range group {
			if value, ok := os.LookupEnv(key); ok {
				envVars[key] = value
			}
		}
	}

	return envVars, nil
}

func (h *Hermes) gatewayRunning() (bool, error) {
	status, err := h.gatewayStatusOutput()
	if err != nil {
		return false, err
	}
	return hermesGatewayStatusRunning(status), nil
}

func (h *Hermes) gatewayStatusOutput() (string, error) {
	bin, err := h.binary()
	if err != nil {
		return "", err
	}
	out, err := hermesCommand(bin, "gateway", "status").CombinedOutput()
	return string(out), err
}

func (h *Hermes) restartGateway() error {
	bin, err := h.binary()
	if err != nil {
		return err
	}
	return hermesAttachedCommand(bin, "gateway", "restart").Run()
}

func hermesGatewayStatusRunning(output string) bool {
	status := strings.ToLower(output)
	switch {
	case strings.Contains(status, "gateway is not running"):
		return false
	case strings.Contains(status, "gateway service is stopped"):
		return false
	case strings.Contains(status, "gateway service is not loaded"):
		return false
	case strings.Contains(status, "gateway is running"):
		return true
	case strings.Contains(status, "gateway service is running"):
		return true
	case strings.Contains(status, "gateway service is loaded"):
		return true
	default:
		return false
	}
}

func hermesParseEnvFile(data []byte) map[string]string {
	out := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}

		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			switch {
			case value[0] == '"' && value[len(value)-1] == '"':
				if unquoted, err := strconv.Unquote(value); err == nil {
					value = unquoted
				}
			case value[0] == '\'' && value[len(value)-1] == '\'':
				value = value[1 : len(value)-1]
			}
		}

		out[key] = value
	}
	return out
}

func hermesOllamaClient() *api.Client {
	// Hermes queries the same launch-resolved Ollama host that launch writes
	// into config, so model discovery follows the configured endpoint.
	return api.NewClient(hermesOllamaURL(), http.DefaultClient)
}

func applyHermesManagedProviders(cfg map[string]any, baseURL string, model string, models []string) {
	providers := hermesUserProviders(cfg["providers"])
	entry := hermesManagedProviderEntry(providers)
	if entry == nil {
		entry = make(map[string]any)
	}
	entry["name"] = hermesProviderName
	entry["api"] = baseURL
	entry["default_model"] = model
	entry["models"] = hermesStringListAny(models)
	providers[hermesProviderKey] = entry
	if hermesLegacyProviderIsOurs(providers) {
		delete(providers, hermesLegacyKey)
	}
	cfg["providers"] = providers

	customProviders := hermesWithoutManagedCustomProviders(cfg["custom_providers"])
	if len(customProviders) == 0 {
		delete(cfg, "custom_providers")
		return
	}
	cfg["custom_providers"] = customProviders
}

// hermesManagedCurrentModel reports the model oaica wrote into cfg, or "".
//
// The endpoint is read FROM the config rather than passed in: Configure writes
// two shapes — the daemon's /v1 for a local model, the remote's own base for a
// user-remote model (hermesBaseURLFor) — and checking only against the daemon
// made the remote shape, written by the same launch, read as a config oaica
// never touched (2026-09-26 audit, tenth round). A non-daemon endpoint counts
// only when a configured remote actually serves it; anything else is a foreign
// config and still reports nothing.
func hermesManagedCurrentModel(cfg map[string]any) string {
	modelCfg, _ := cfg["model"].(map[string]any)
	if modelCfg == nil {
		return ""
	}

	provider, _ := modelCfg["provider"].(string)
	if strings.TrimSpace(strings.ToLower(provider)) != hermesProviderKey {
		return ""
	}

	configBaseURL, _ := modelCfg["base_url"].(string)
	if strings.TrimSpace(configBaseURL) == "" {
		return ""
	}
	remoteName := ""
	var remote userRemote
	if hermesNormalizeURL(configBaseURL) != hermesNormalizeURL(hermesBaseURL()) {
		var ok bool
		remote, ok = hermesConfiguredRemoteForBase(configBaseURL)
		if !ok {
			return ""
		}
		remoteName = remote.Name
		// The credential the writer stores beside that endpoint
		// (hermesKeyFor -> the remote's token): a config still holding one the
		// remote has replaced is not the state a write would leave (round 32
		// for droid, round 33 here). Hermes' child inherits os.Environ()
		// untouched and ~/.hermes/.env is the messaging-only file, so this
		// field is the only credential it gets -- a rotated key meant every
		// request 401ed with the dead bearer until the user forced a
		// reconfigure.
		if key, _ := modelCfg["api_key"].(string); key != remote.key() {
			return ""
		}
	}

	current, _ := modelCfg["default"].(string)
	current = strings.TrimSpace(current)
	if current == "" {
		return ""
	}

	providers := hermesUserProviders(cfg["providers"])
	entry, _ := providers[hermesProviderKey].(map[string]any)
	if entry == nil {
		return ""
	}
	if hermesHasManagedCustomProvider(cfg["custom_providers"]) {
		return ""
	}

	apiURL, _ := entry["api"].(string)
	if hermesNormalizeURL(apiURL) != hermesNormalizeURL(configBaseURL) {
		return ""
	}

	defaultModel, _ := entry["default_model"].(string)
	if strings.TrimSpace(defaultModel) != current {
		return ""
	}

	// A remote config stores the bare upstream id; report the picker name the
	// launch used, so this answer agrees with what the launcher saved.
	if remoteName != "" {
		if picker := hermesRemotePickerName(configBaseURL, current); picker != "" {
			return picker
		}
	}
	return current
}

func hermesUserProviders(current any) map[string]any {
	switch existing := current.(type) {
	case map[string]any:
		out := make(map[string]any, len(existing))
		for key, value := range existing {
			out[key] = value
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(existing))
		for key, value := range existing {
			if s, ok := key.(string); ok {
				out[s] = value
			}
		}
		return out
	default:
		return make(map[string]any)
	}
}

func hermesCustomProviders(current any) []any {
	switch existing := current.(type) {
	case []any:
		return append([]any(nil), existing...)
	case []map[string]any:
		out := make([]any, 0, len(existing))
		for _, entry := range existing {
			out = append(out, entry)
		}
		return out
	default:
		return nil
	}
}

// hermesEndpointWasOurs reports whether an endpoint recorded in Hermes' config
// is one oaica writes: the local daemon's /v1, or a configured remote's base.
// The rule hermesManagedCurrentModel already applies to the model section, and
// the one the ownership tests below need — "Ollama" is Hermes' own name for
// Ollama, so a provider entry carrying it may be the USER's (their LAN server,
// their key), and an entry that cannot be shown to be ours is theirs.
// hermesEndpointWasOurs reports whether an endpoint recorded in Hermes' config
// is one oaica writes: the local daemon (at whatever address OLLAMA_HOST names,
// or at its documented default), or a configured remote's base.
func hermesEndpointWasOurs(api string) bool {
	api = hermesNormalizeURL(api)
	if api == "" {
		return false
	}
	if api == hermesNormalizeURL(hermesBaseURL()) || api == hermesNormalizeURL(hermesOllamaURL().String()) {
		return true
	}
	// The daemon's default address. A legacy entry written by an earlier oaica
	// carries it even when OLLAMA_HOST has since moved this machine's daemon
	// elsewhere, and it is also where a user's own local Ollama provider points
	// by default — the same provider, so an entry naming it is not theirs to
	// lose.
	for _, base := range []string{"http://127.0.0.1:11434", "http://localhost:11434", "http://[::1]:11434"} {
		if api == base || api == base+"/v1" {
			return true
		}
	}
	_, ok := hermesConfiguredRemoteForBase(api)
	return ok
}

// hermesEntryEndpoint reads the endpoint a provider or custom_providers entry
// records: the providers map uses "api", the legacy custom_providers shape
// "base_url".
func hermesEntryEndpoint(entry map[string]any) string {
	if api, _ := entry["api"].(string); strings.TrimSpace(api) != "" {
		return api
	}
	base, _ := entry["base_url"].(string)
	return base
}

func hermesManagedProviderEntry(providers map[string]any) map[string]any {
	if entry, _ := providers[hermesProviderKey].(map[string]any); entry != nil {
		return entry
	}
	if hermesLegacyProviderIsOurs(providers) {
		if entry, _ := providers[hermesLegacyKey].(map[string]any); entry != nil {
			return entry
		}
	}
	return nil
}

// hermesLegacyProviderIsOurs reports whether the entry under the legacy "ollama"
// key is one oaica wrote, and so may be consumed by — and removed after — this
// launch's write. Without the test, a user's own provider keyed "ollama" was
// adopted as the seed for oaica's entry and then deleted along with its key
// (2026-09-27 audit, round 21).
func hermesLegacyProviderIsOurs(providers map[string]any) bool {
	entry, _ := providers[hermesLegacyKey].(map[string]any)
	if entry == nil {
		return false
	}
	return hermesEndpointWasOurs(hermesEntryEndpoint(entry))
}

func hermesWithoutManagedCustomProviders(current any) []any {
	customProviders := hermesCustomProviders(current)
	preserved := make([]any, 0, len(customProviders))

	for _, item := range customProviders {
		entry, _ := item.(map[string]any)
		if entry == nil {
			preserved = append(preserved, item)
			continue
		}
		if hermesManagedCustomProvider(entry) {
			continue
		}
		preserved = append(preserved, entry)
	}

	return preserved
}

func hermesHasManagedCustomProvider(current any) bool {
	for _, item := range hermesCustomProviders(current) {
		entry, _ := item.(map[string]any)
		if entry != nil && hermesManagedCustomProvider(entry) {
			return true
		}
	}
	return false
}

// hermesManagedCustomProvider reports whether a custom_providers entry is one
// oaica wrote. The name alone is not an ownership test: "Ollama" is Hermes' own
// name for Ollama, and a hand-written entry carrying it was deleted by an
// unrelated launch (2026-09-27 audit, round 21). The endpoint has to be one
// oaica writes as well.
func hermesManagedCustomProvider(entry map[string]any) bool {
	name, _ := entry["name"].(string)
	if !strings.EqualFold(strings.TrimSpace(name), hermesProviderName) {
		return false
	}
	return hermesEndpointWasOurs(hermesEntryEndpoint(entry))
}

func hermesNormalizeURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func hermesStringListAny(models []string) []any {
	out := make([]any, 0, len(models))
	for _, model := range dedupeModelList(models) {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		out = append(out, model)
	}
	return out
}

// hermesPreservedCustomProvidersNode returns the custom_providers sequence with
// the entry oaica manages removed and every other entry kept as the node the
// user wrote, or nil when nothing is left (the caller then deletes the key).
//
// A subtree oaica only FILTERS must not be re-encoded: decoding it to
// map[string]any and writing the map back deleted the comments and formatting
// inside every entry it deliberately preserved (2026-09-27 audit, round 18).
func hermesPreservedCustomProvidersNode(current *yaml.Node) *yaml.Node {
	if current == nil || current.Kind != yaml.SequenceNode {
		return nil
	}
	preserved := *current
	preserved.Content = nil
	for _, item := range current.Content {
		if hermesManagedCustomProviderNode(item) {
			continue
		}
		preserved.Content = append(preserved.Content, item)
	}
	if len(preserved.Content) == 0 {
		return nil
	}
	return &preserved
}

// hermesManagedCustomProviderNode reports whether a custom_providers entry is
// the one oaica writes — the same test hermesManagedCustomProvider makes on a
// decoded map, asked of the node instead.
func hermesManagedCustomProviderNode(entry *yaml.Node) bool {
	if entry == nil || entry.Kind != yaml.MappingNode {
		return false
	}
	name := yamlNodeValue(entry, "name")
	if name == nil || !strings.EqualFold(strings.TrimSpace(name.Value), hermesProviderName) {
		return false
	}
	// The name alone is not an ownership test — "Ollama" is Hermes' own name for
	// Ollama, so a hand-written entry carrying it was deleted by an unrelated
	// launch (2026-09-27 audit, round 21). The endpoint has to be one oaica
	// writes as well; the legacy shape records it as base_url.
	for _, key := range []string{"api", "base_url"} {
		if node := yamlNodeValue(entry, key); node != nil && hermesEndpointWasOurs(node.Value) {
			return true
		}
	}
	return false
}

// hermesToolsetsNode returns the toolsets sequence with "web" present, keeping
// the user's own entries as the nodes they wrote. A value that is not a sequence
// (a comma list, an empty value, a missing key) becomes one, as the encoder used
// to produce it, but the value node's own comments travel with it.
func hermesToolsetsNode(current *yaml.Node) *yaml.Node {
	if current != nil && current.Kind == yaml.SequenceNode {
		out := *current
		out.Content = append([]*yaml.Node(nil), current.Content...)
		if !hermesNodeListHas(&out, "web") {
			out.Content = append(out.Content, hermesScalarNode("web"))
		}
		return &out
	}

	out := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	if current != nil {
		out.HeadComment, out.LineComment, out.FootComment = current.HeadComment, current.LineComment, current.FootComment
	}

	var items []string
	if current != nil && current.Kind == yaml.ScalarNode {
		for _, part := range strings.Split(current.Value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				items = append(items, part)
			}
		}
	}
	if len(items) == 0 {
		items = []string{"hermes-cli"}
	}
	for _, item := range items {
		out.Content = append(out.Content, hermesScalarNode(item))
	}
	if !hermesNodeListHas(out, "web") {
		out.Content = append(out.Content, hermesScalarNode("web"))
	}
	return out
}

func hermesNodeListHas(sequence *yaml.Node, want string) bool {
	for _, item := range sequence.Content {
		if item.Kind == yaml.ScalarNode && item.Value == want {
			return true
		}
	}
	return false
}

func hermesScalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func hermesAttachedCommand(name string, args ...string) *exec.Cmd {
	cmd := hermesCommand(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}
