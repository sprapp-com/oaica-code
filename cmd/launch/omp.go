package launch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/ollama/ollama/cmd/config"
	"github.com/ollama/ollama/cmd/internal/fileutil"
	"github.com/ollama/ollama/envconfig"
	"github.com/ollama/ollama/types/model"
	"gopkg.in/yaml.v3"
)

const (
	ompIntegrationName = "omp"
	ompProviderName    = "ollama"
	ompSetupVersion    = 1
	ompWebSearchPlugin = "@ollama/pi-web-search"
)

// OMP implements Runner for the OMP coding-agent integration.
type OMP struct{}

func (o *OMP) String() string { return "OMP" }

func (o *OMP) Paths() []string {
	var paths []string
	for _, pathFn := range []func() (string, error){ompModelsPath, ompConfigPath} {
		path, err := pathFn()
		if err != nil {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			paths = append(paths, path)
		}
	}
	return paths
}

func (o *OMP) Configure(model string) error {
	return o.ConfigureWithModels(model, []LaunchModel{fallbackLaunchModel(model)})
}

func (o *OMP) ConfigureWithModels(primary string, models []LaunchModel) error {
	if primary == "" {
		return nil
	}
	if len(models) == 0 {
		models = []LaunchModel{fallbackLaunchModel(primary)}
	}
	if err := writeOMPModelsConfig(primary, models); err != nil {
		return err
	}
	return writeOMPAgentConfig()
}

func (o *OMP) CurrentModel() string {
	cfg, err := readOMPModelsConfig()
	if err != nil {
		return ""
	}
	provider, ok := ompProvider(cfg)
	if !ok {
		return ""
	}
	if !ompProviderHealthy(provider) {
		return ""
	}
	models, _ := provider["models"].([]any)
	for _, raw := range models {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := entry["id"].(string)
		if id == "" {
			continue
		}
		// A remote config stores the bare upstream id; report the picker name
		// the launch used, so this answer agrees with what the launcher saved.
		baseURL, _ := provider["baseUrl"].(string)
		if picker := ompRemotePickerName(baseURL, id); picker != "" {
			return picker
		}
		return id
	}
	return ""
}

func (o *OMP) Onboard() error {
	return config.MarkIntegrationOnboarded(ompIntegrationName)
}

func (o *OMP) RequiresInteractiveOnboarding() bool { return false }

func (o *OMP) args(model string, extra []string) []string {
	var args []string
	if model != "" {
		args = append(args, "--model", ompModelName(model))
	}
	args = append(args, extra...)
	return args
}

func ompModelName(model string) string {
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return ep.UpstreamModel
	}
	if strings.HasPrefix(model, "ollama/") {
		return model
	}
	return "ollama/" + model
}

func (o *OMP) findPath() (string, error) {
	if p, err := exec.LookPath("omp"); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	for _, dir := range []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".bun", "bin"),
	} {
		for _, name := range ompExecutableNames() {
			fallback := filepath.Join(dir, name)
			if _, err := os.Stat(fallback); err == nil {
				return fallback, nil
			}
		}
	}
	return "", exec.ErrNotFound
}

func ompExecutableNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"omp.exe", "omp.cmd", "omp.bat"}
	}
	return []string{"omp"}
}

func (o *OMP) Run(model string, _ []LaunchModel, args []string) error {
	forceTools, args := extractForceTools(args)
	if err := gateOpenAITools(model, forceTools); err != nil {
		return err
	}

	ompPath, err := o.findPath()
	if err != nil {
		return fmt.Errorf("omp is not installed, install from https://omp.sh")
	}

	ensureOMPWebSearchPlugin(ompPath)

	cmd := exec.Command(ompPath, o.args(model, args)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}

func ensureOMPWebSearchPlugin(bin string) {
	if !shouldManageOllamaWebSearch() {
		fmt.Fprintf(os.Stderr, "%sCloud is disabled; skipping %s setup.%s\n", ansiGray, ompWebSearchPlugin, ansiReset)
		return
	}

	fmt.Fprintf(os.Stderr, "%sChecking OMP web search plugin...%s\n", ansiGray, ansiReset)

	installed, err := ompPluginInstalled(bin, ompWebSearchPlugin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s  Warning: could not check %s installation: %v%s\n", ansiYellow, ompWebSearchPlugin, err, ansiReset)
		return
	}

	verb := "Installing"
	warnVerb := "install"
	doneVerb := "Installed"
	if installed {
		verb = "Updating"
		warnVerb = "update"
		doneVerb = "Updated"
	}

	// The same consent gate every other agent's installer passes through:
	// docs/ENTERPRISE.md's egress table tells a reviewer "none of these
	// installers run unprompted", and this one did — `oaica launch omp`
	// reached `omp plugin install` (which fetches a package from npm) with no
	// prompt at all, including for a plugin the user never asked for
	// (2026-09-26 audit).
	ok, cerr := ConfirmPrompt(fmt.Sprintf("%s %s?", verb, ompWebSearchPlugin))
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "%s  Warning: could not ask about %s: %v%s\n", ansiYellow, ompWebSearchPlugin, cerr, ansiReset)
		return
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "%s  Skipping %s — install it yourself with `%s plugin install %s`%s\n",
			ansiYellow, ompWebSearchPlugin, bin, ompWebSearchPlugin, ansiReset)
		return
	}

	fmt.Fprintf(os.Stderr, "%s%s %s...%s\n", ansiGray, verb, ompWebSearchPlugin, ansiReset)
	cmd := exec.Command(bin, "plugin", "install", ompWebSearchPlugin)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s  Warning: could not %s %s: %v%s\n", ansiYellow, warnVerb, ompWebSearchPlugin, err, ansiReset)
		return
	}

	fmt.Fprintf(os.Stderr, "%s  ✓ %s %s%s\n", ansiGreen, doneVerb, ompWebSearchPlugin, ansiReset)
}

func ompPluginInstalled(bin, plugin string) (bool, error) {
	cmd := exec.Command(bin, "plugin", "list")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return false, err
		}
		return false, fmt.Errorf("%w: %s", err, msg)
	}

	versioned := plugin + "@"
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, versioned) || trimmed == plugin {
			return true, nil
		}
	}
	return false, nil
}

func ompModelsPath() (string, error) {
	dir, err := ompAgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "models.yml"), nil
}

func ompConfigPath() (string, error) {
	dir, err := ompAgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yml"), nil
}

func ompAgentDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); dir != "" {
		return dir, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	configDir := strings.TrimSpace(os.Getenv("PI_CONFIG_DIR"))
	if configDir == "" {
		configDir = ".omp"
	}
	if filepath.IsAbs(configDir) {
		return filepath.Join(configDir, "agent"), nil
	}
	return filepath.Join(home, configDir, "agent"), nil
}

func readOMPModelsConfig() (map[string]any, error) {
	path, err := ompModelsPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = make(map[string]any)
	}
	return cfg, nil
}

func writeOMPModelsConfig(primary string, models []LaunchModel) error {
	path, err := ompModelsPath()
	if err != nil {
		return err
	}
	// models.yml is OMP's own document, and oaica writes one provider in it and
	// publishes the whole file: the read, the merge and the write run under the
	// store's lock (keyed under ~/.oaica/locks, foreignStoreLockBase, because
	// this is another program's data directory). Without it two oaica commands
	// whose writes overlap each publish a snapshot taken before the other's
	// models landed, and the launch that renamed last decides the file
	// (2026-09-26 audit, twelfth round).
	return fileutil.WithFileLock(foreignStoreLockBase(path), func() error {
		return writeOMPModelsConfigLocked(path, primary, models)
	})
}

func writeOMPModelsConfigLocked(path, primary string, models []LaunchModel) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	cfg := make(map[string]any)
	switch existing, readErr := readOMPModelsConfig(); {
	case readErr == nil:
		cfg = existing
	case errors.Is(readErr, fs.ErrNotExist):
		// Nothing on disk yet — the empty map above is the whole document.
	default:
		// A document this function cannot parse is one it cannot preserve:
		// oaica models exactly one provider in that file, so writing the
		// parsed half back would delete every other provider the user has.
		return fmt.Errorf("refusing to update %s: %v — oaica writes only the ollama provider in that file, so rewriting a document it cannot read would delete your other providers", path, readErr)
	}

	provider := ensureOMPProvider(cfg, primary)
	existingByID := ompModelEntriesByID(provider)
	ordered := append([]LaunchModel(nil), models...)
	if model, ok := findLaunchModel(ordered, primary); ok {
		ordered = append([]LaunchModel{model}, removeLaunchModel(ordered, primary)...)
	} else {
		ordered = append([]LaunchModel{fallbackLaunchModel(primary)}, ordered...)
	}

	var merged []any
	seen := make(map[string]bool, len(ordered))
	for _, model := range ordered {
		if model.Name == "" || seen[model.Name] {
			continue
		}
		seen[model.Name] = true
		entry := ompModelConfig(model)
		if existing, ok := existingByID[model.Name]; ok {
			for key, value := range existing {
				if _, overridden := entry[key]; !overridden {
					entry[key] = value
				}
			}
		}
		merged = append(merged, entry)
	}

	for _, raw := range ompProviderModels(provider) {
		entry, ok := raw.(map[string]any)
		if !ok {
			merged = append(merged, raw)
			continue
		}
		id, _ := entry["id"].(string)
		if id == "" || seen[id] {
			continue
		}
		merged = append(merged, entry)
	}
	provider["models"] = merged

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return fileutil.WriteWithBackup(path, data, ompIntegrationName)
}

func writeOMPAgentConfig() error {
	path, err := ompConfigPath()
	if err != nil {
		return err
	}
	// Same shape over OMP's config.yml, and the same lock: it is read, one key is
	// set, and the whole document is published back, so a concurrent writer's
	// keys are deleted by whichever publish lands last unless the read happens
	// under the lock too (2026-09-26 audit, twelfth round).
	return fileutil.WithFileLock(foreignStoreLockBase(path), func() error {
		return writeOMPAgentConfigLocked(path)
	})
}

func writeOMPAgentConfigLocked(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	cfg := make(map[string]any)
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return err
		}
		if cfg == nil {
			cfg = make(map[string]any)
		}
	}
	cfg["setupVersion"] = ompSetupVersion

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return fileutil.WriteWithBackup(path, data, ompIntegrationName)
}

func ensureOMPProvider(cfg map[string]any, model string) map[string]any {
	providers, _ := cfg["providers"].(map[string]any)
	if providers == nil {
		providers = make(map[string]any)
		cfg["providers"] = providers
	}
	provider, _ := providers[ompProviderName].(map[string]any)
	if provider == nil {
		provider = make(map[string]any)
		providers[ompProviderName] = provider
	}

	ep, isRemote := resolveRemoteEndpoint(model)
	if !isRemote {
		// The daemon: omp talks to 127.0.0.1, which is genuinely
		// unauthenticated, and it serves the Responses API.
		provider["baseUrl"] = ompBaseURL()
		provider["api"] = "openai-responses"
		provider["auth"] = "none"
		delete(provider, "apiKey")
		provider["discovery"] = map[string]any{"type": "ollama"}
		return provider
	}

	// A user remote, reached directly. Everything here has to change with it:
	// the wire is Chat Completions (what a user remote serves), Ollama
	// discovery would probe /api/tags on a non-Ollama host, and the
	// credential goes in apiKey — OMP's own documented field, whose value the
	// provider's standard client sends as its auth header. `auth` is NOT
	// where the key goes: its values are apiKey (the default), none and
	// oauth, and writing a scheme name that does not exist leaves the request
	// unauthenticated while claiming otherwise. Nothing else supplies the
	// token either — OMP.Run passes the environment through untouched, where
	// OLLAMA_API_KEY, if set, belongs to ollama.com and must not be sent to
	// someone else's host.
	provider["baseUrl"] = strings.TrimRight(ep.BaseURL, "/")
	provider["api"] = "openai-completions"
	delete(provider, "discovery")
	if ep.Token != "" {
		provider["apiKey"] = ep.Token
		provider["auth"] = "apiKey"
	} else {
		delete(provider, "apiKey")
		provider["auth"] = "none"
	}
	return provider
}

func ompBaseURL() string {
	return strings.TrimRight(envconfig.ConnectableHost().String(), "/") + "/v1"
}

// ompProviderHealthy reports whether the ollama provider holds the config
// oaica wrote — for the daemon or for one of the configured user remotes.
// Both shapes are checked, because both are written by ensureOMPProvider:
// demanding the daemon's base URL made CurrentModel report "" for the config
// the same launch had just written (2026-09-26 audit, tenth round).
func ompProviderHealthy(provider map[string]any) bool {
	baseURL, _ := provider["baseUrl"].(string)
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return false
	}
	api, _ := provider["api"].(string)
	auth, _ := provider["auth"].(string)
	discovery, _ := provider["discovery"].(map[string]any)
	discoveryType := ""
	if discovery != nil {
		discoveryType, _ = discovery["type"].(string)
	}

	if baseURL == strings.TrimRight(ompBaseURL(), "/") {
		return api == "openai-responses" && auth == "none" && discoveryType == "ollama"
	}

	// A remote base URL is ours only when a configured remote actually serves
	// it; anything else is a config oaica did not write.
	if _, ok := ompConfiguredRemoteForBase(baseURL); !ok {
		return false
	}
	if api != "openai-completions" || discoveryType == "ollama" {
		return false
	}
	key, _ := provider["apiKey"].(string)
	if strings.TrimSpace(key) == "" {
		// A remote with no credential: the only honest scheme.
		return auth == "none"
	}
	return auth == "apiKey"
}

// ompConfiguredRemoteForBase returns the configured remote a provider base URL
// belongs to, if any. Reads the remote store only — no network — so it is safe
// on every picker-state read.
func ompConfiguredRemoteForBase(baseURL string) (userRemote, bool) {
	remotes, err := loadUserRemotes()
	if err != nil {
		return userRemote{}, false
	}
	for _, r := range remotes {
		if strings.TrimRight(r.openAIBase(), "/") == baseURL {
			return r, true
		}
	}
	return userRemote{}, false
}

// ompRemotePickerName maps a provider's base URL plus the model id written in
// models.yml back to the picker name the launch used ("box/big-model"). The
// config stores the bare upstream id, which is not a name the launcher can
// check (a bare id matches neither a saved picker name nor a remote row), so
// CurrentModel translates it back. Returns "" when no configured remote serves
// that base URL under that id.
func ompRemotePickerName(baseURL, upstream string) string {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return ""
	}
	remote, ok := ompConfiguredRemoteForBase(strings.TrimRight(baseURL, "/"))
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

func ompProvider(cfg map[string]any) (map[string]any, bool) {
	providers, ok := cfg["providers"].(map[string]any)
	if !ok {
		return nil, false
	}
	provider, ok := providers[ompProviderName].(map[string]any)
	return provider, ok
}

func ompProviderModels(provider map[string]any) []any {
	models, _ := provider["models"].([]any)
	return models
}

func ompModelEntriesByID(provider map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any)
	for _, raw := range ompProviderModels(provider) {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := entry["id"].(string); id != "" {
			out[id] = entry
		}
	}
	return out
}

func ompModelConfig(modelInfo LaunchModel) map[string]any {
	id := modelInfo.Name
	if ep, ok := resolveRemoteEndpoint(modelInfo.Name); ok {
		id = ep.UpstreamModel
	}
	entry := map[string]any{
		"id":   id,
		"name": id,
	}
	input := []string{"text"}
	if slices.Contains(modelInfo.Capabilities, model.CapabilityVision) {
		input = append(input, "image")
	}
	entry["input"] = input

	if modelInfo.ContextLength > 0 {
		entry["contextWindow"] = modelInfo.ContextLength
	}
	if modelInfo.MaxOutputTokens > 0 {
		entry["maxTokens"] = modelInfo.MaxOutputTokens
	}
	return entry
}

func removeLaunchModel(models []LaunchModel, name string) []LaunchModel {
	out := make([]LaunchModel, 0, len(models))
	for _, model := range models {
		if launchModelMatches(model.Name, name) || launchModelMatches(name, model.Name) {
			continue
		}
		out = append(out, model)
	}
	return out
}
