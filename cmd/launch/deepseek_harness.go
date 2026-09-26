package launch

import (
	"fmt"
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
	deepSeekHarnessIntegrationName = "dsh"
	deepSeekHarnessNpmPackage      = "@deepseek-ai/dsh@latest"
	deepSeekHarnessProvider        = "ollama"
	deepSeekHarnessAPIKeyEnv       = "OLLAMA_LAUNCH_DSH_API_KEY"
	deepSeekHarnessWebSettings     = "web-search-deepseek"
)

var (
	deepSeekHarnessLookPath = exec.LookPath
	deepSeekHarnessCommand  = exec.Command
	deepSeekHarnessGOOS     = runtime.GOOS
)

// DeepSeekHarness is the Ollama-managed DeepSeek Harness integration.
// It redirects only the settings provider for this invocation to an
// Ollama-owned document. The user's normal DSH_HOME, profiles, sessions,
// credentials, and patch layers remain available and untouched.
type DeepSeekHarness struct{}

func (d *DeepSeekHarness) String() string { return "DeepSeek Harness" }

func (d *DeepSeekHarness) Run(_ string, _ []LaunchModel, args []string) error {
	if err := validateDeepSeekHarnessArgs(args); err != nil {
		return err
	}

	bin, err := deepSeekHarnessLookPath("dsh")
	if err != nil {
		return fmt.Errorf("dsh is not installed: %w", err)
	}
	patchPath, err := deepSeekHarnessPatchPath()
	if err != nil {
		return err
	}

	cmd, err := deepSeekHarnessExecutableCommand(bin, deepSeekHarnessLaunchArgs(patchPath, args))
	if err != nil {
		return err
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = deepSeekHarnessLaunchEnv(os.Environ())
	return cmd.Run()
}

func deepSeekHarnessLaunchArgs(patchPath string, args []string) []string {
	launchArgs := []string{"web", "--patch", patchPath}
	return append(launchArgs, args...)
}

func validateDeepSeekHarnessArgs(args []string) error {
	for _, arg := range args {
		if arg == "--patch" || strings.HasPrefix(arg, "--patch=") {
			return fmt.Errorf("conflicting extra argument %q: oaica launch dsh manages --patch", arg)
		}
	}
	return nil
}

func deepSeekHarnessLaunchEnv(env []string) []string {
	return deepSeekHarnessUpsertEnv(env, deepSeekHarnessAPIKeyEnv, "ollama")
}

func deepSeekHarnessUpsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, prefix+value)
}

func ensureDeepSeekHarnessInstalled() (string, error) {
	if path, err := deepSeekHarnessLookPath("dsh"); err == nil {
		return path, nil
	}
	npm, err := deepSeekHarnessLookPath("npm")
	if err != nil {
		return "", fmt.Errorf("dsh is not installed and npm (Node.js) is required\n\nInstall Node.js first:\n  https://nodejs.org/\n\nThen re-run:\n  oaica launch dsh")
	}

	ok, err := ConfirmPrompt("DeepSeek Harness is not installed. Install with npm?")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("deepseek harness installation cancelled")
	}

	fmt.Fprintln(os.Stderr, "\nInstalling DeepSeek Harness...")
	cmd, err := deepSeekHarnessNpmCommand(npm, []string{"install", "-g", deepSeekHarnessNpmPackage})
	if err != nil {
		return "", err
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to install deepseek harness: %w", err)
	}

	path, err := deepSeekHarnessLookPath("dsh")
	if err != nil {
		return "", fmt.Errorf("deepseek harness was installed but dsh was not found on PATH\n\nYou may need to restart your shell")
	}
	fmt.Fprintf(os.Stderr, "%sDeepSeek Harness installed successfully%s\n\n", ansiGreen, ansiReset)
	return path, nil
}

func deepSeekHarnessExecutableCommand(bin string, args []string) (*exec.Cmd, error) {
	return deepSeekHarnessNodeShimCommand(bin, []string{"node_modules", "@deepseek-ai", "dsh", "lib", "bin.js"}, args)
}

func deepSeekHarnessNpmCommand(bin string, args []string) (*exec.Cmd, error) {
	return deepSeekHarnessNodeShimCommand(bin, []string{"node_modules", "npm", "bin", "npm-cli.js"}, args)
}

// Windows npm binaries are .cmd shims, which cannot be passed safely to
// CreateProcess with an argv. Invoke their JavaScript entrypoints with Node so
// passthrough arguments remain data rather than cmd.exe syntax.
func deepSeekHarnessNodeShimCommand(shim string, entrypointParts, args []string) (*exec.Cmd, error) {
	if deepSeekHarnessGOOS != "windows" || !deepSeekHarnessIsCommandShim(shim) {
		return deepSeekHarnessCommand(shim, args...), nil
	}

	node, err := deepSeekHarnessLookPath("node")
	if err != nil {
		return nil, fmt.Errorf("node is required to run %s on Windows: %w", filepath.Base(shim), err)
	}
	entrypoint := filepath.Join(append([]string{filepath.Dir(shim)}, entrypointParts...)...)
	if _, err := os.Stat(entrypoint); err != nil {
		return nil, fmt.Errorf("resolve Windows entrypoint for %s: %w", filepath.Base(shim), err)
	}
	return deepSeekHarnessCommand(node, append([]string{entrypoint}, args...)...), nil
}

func deepSeekHarnessIsCommandShim(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".cmd" || ext == ".bat"
}

func (d *DeepSeekHarness) Paths() []string {
	settingsPath, settingsErr := deepSeekHarnessSettingsPath()
	patchPath, patchErr := deepSeekHarnessPatchPath()
	if settingsErr != nil || patchErr != nil {
		return nil
	}
	return []string{settingsPath, patchPath}
}

func (d *DeepSeekHarness) Configure(modelName string) error {
	return d.ConfigureWithModels(modelName, []LaunchModel{fallbackLaunchModel(modelName)})
}

func (d *DeepSeekHarness) ConfigureWithModels(primary string, models []LaunchModel) error {
	if strings.TrimSpace(primary) == "" {
		return nil
	}
	if len(models) == 0 {
		models = []LaunchModel{fallbackLaunchModel(primary)}
	}
	if selected, ok := findLaunchModel(models, primary); ok {
		primary = selected.Name
	}
	if err := deepSeekHarnessRejectRemoteModels(primary, models); err != nil {
		return err
	}

	settingsPath, err := deepSeekHarnessSettingsPath()
	if err != nil {
		return err
	}
	// ~/.ollama/launch/dsh/settings.yaml is oaica's own store, so the lock goes
	// beside it, as the other oaica-owned stores do. It is read as a YAML
	// document, edited in place and published whole: two `oaica launch deepseek`
	// commands overlapping would each publish a snapshot taken before the
	// other's settings landed, and the launch that renamed last would silently
	// decide the file (2026-09-26 audit, twelfth round). The read happens inside
	// the lock, or the lock would order only the publishes.
	if err := fileutil.WithFileLock(settingsPath, func() error {
		return writeDeepSeekHarnessSettings(settingsPath, primary, models)
	}); err != nil {
		return err
	}

	patchPath, err := deepSeekHarnessPatchPath()
	if err != nil {
		return err
	}
	patchData, err := yaml.Marshal([]map[string]any{
		{
			"id": "settings",
			"config": map[string]any{
				"path": settingsPath,
			},
		},
	})
	if err != nil {
		return err
	}
	return writeDeepSeekHarnessFile(patchPath, patchData)
}

// deepSeekHarnessRejectRemoteModels refuses a selection the harness settings
// cannot route to.
//
// Every other integration that CAN translate a user-remote row does it with two
// things the harness store does not have: one endpoint per provider block, and a
// credential field. Harness settings hold a single llm-pi-ai provider (the
// daemon's baseURL, with apiKeyEnv naming the OLLAMA_LAUNCH_DSH_API_KEY oaica
// sets for the child), the web-search block points at that same daemon /v1, and
// the model rows carry no endpoint or key of their own — so a remote row written
// as id "box/big-model" is a name only the remote knows, posted to the daemon,
// which does not resolve namespaced remotes. Every request fails model-not-found
// after the launch has already validated a key, and the settings advertise the
// model as available.
//
// Refusing here keeps the failure at the point the user can act on it. Same
// shape and same answer as museRejectRemoteModels and codexAppRejectRemoteModels;
// lift this when the harness store grows a per-provider credential.
func deepSeekHarnessRejectRemoteModels(primary string, models []LaunchModel) error {
	reported := map[string]bool{}
	refuse := func(model LaunchModel) error {
		name := strings.TrimSpace(model.Name)
		if !model.Remote || name == "" || reported[name] {
			return nil
		}
		reported[name] = true
		return fmt.Errorf("the DeepSeek Harness cannot be pointed at the remote model %q: its settings carry a single endpoint and a single credential for every model row, both naming the local daemon, which does not resolve namespaced remotes. Add the remote to the harness's own settings, or launch a daemon-backed model", name)
	}

	// The primary is written as the default model and into the web-search block,
	// so it decides what every request names; check it first.
	if model, ok := findLaunchModel(models, primary); ok {
		if err := refuse(model); err != nil {
			return err
		}
	}
	for _, model := range models {
		if err := refuse(model); err != nil {
			return err
		}
	}
	return nil
}

// writeDeepSeekHarnessSettings is the load → mutate → publish half of
// ConfigureWithModels, run under the settings store's lock. The patch file the
// caller writes next is a fresh document marshalled from a literal, with no
// read-back of anything, so it needs no lock.
func writeDeepSeekHarnessSettings(settingsPath, primary string, models []LaunchModel) error {
	settings, err := readYAMLDocument(settingsPath)
	if err != nil {
		return fmt.Errorf("parse deepseek harness launch settings: %w", err)
	}
	if err := applyDeepSeekHarnessSettings(settings, primary, models, shouldManageOllamaWebSearch()); err != nil {
		return err
	}

	settingsData, err := yaml.Marshal(settings)
	if err != nil {
		return err
	}
	return writeDeepSeekHarnessFile(settingsPath, settingsData)
}

func readDeepSeekHarnessYAML(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]any), nil
		}
		return nil, err
	}
	settings := make(map[string]any)
	if err := yaml.Unmarshal(data, &settings); err != nil {
		return nil, err
	}
	if settings == nil {
		settings = make(map[string]any)
	}
	return settings, nil
}

func applyDeepSeekHarnessSettings(document *yaml.Node, primary string, models []LaunchModel, manageWebSearch bool) error {
	settings := document.Content[0]
	selected := yamlEnsureMapping(settings, "agent-default-model")
	for key, value := range map[string]string{
		"provider": deepSeekHarnessProvider,
		"model":    primary,
	} {
		if err := yamlSetValue(selected, key, value); err != nil {
			return err
		}
	}

	llm := yamlEnsureMapping(settings, "llm-pi-ai")
	providers := yamlEnsureMapping(llm, "providers")
	provider := yamlEnsureMapping(providers, deepSeekHarnessProvider)
	for key, value := range map[string]any{
		"displayName": "Ollama",
		"apiKeyEnv":   deepSeekHarnessAPIKeyEnv,
		"api":         "openai-completions",
		"baseURL":     deepSeekHarnessBaseURL(),
		"models":      deepSeekHarnessModelConfigs(primary, models),
	} {
		if err := yamlSetValue(provider, key, value); err != nil {
			return err
		}
	}

	if !manageWebSearch {
		return nil
	}

	// Harness's bundled search provider appends /messages to this /v1 base and
	// sends the Anthropic web_search server tool. This is separate from the main
	// model provider above; Harness does not expose a configured way to send the
	// native OpenAI Responses web_search tool.
	web := yamlEnsureMapping(settings, deepSeekHarnessWebSettings)
	for key, value := range map[string]string{
		"apiKeyEnv": deepSeekHarnessAPIKeyEnv,
		"baseURL":   deepSeekHarnessBaseURL(),
		"model":     primary,
	} {
		if err := yamlSetValue(web, key, value); err != nil {
			return err
		}
	}
	return nil
}

func deepSeekHarnessModelConfigs(primary string, models []LaunchModel) []any {
	ordered := append([]LaunchModel(nil), models...)
	if selected, ok := findLaunchModel(ordered, primary); ok {
		ordered = append([]LaunchModel{selected}, removeLaunchModel(ordered, primary)...)
	} else {
		ordered = append([]LaunchModel{fallbackLaunchModel(primary)}, ordered...)
	}

	configs := make([]any, 0, len(ordered))
	seen := make(map[string]bool, len(ordered))
	for _, item := range ordered {
		if item.Name == "" || seen[item.Name] {
			continue
		}
		seen[item.Name] = true
		entry := map[string]any{
			"id":    item.Name,
			"name":  item.Name,
			"input": []string{"text"},
		}
		if slices.Contains(item.Capabilities, model.CapabilityVision) {
			entry["input"] = []string{"text", "image"}
		}
		if item.ContextLength > 0 {
			entry["contextWindow"] = item.ContextLength
		}
		if item.MaxOutputTokens > 0 {
			entry["maxTokens"] = item.MaxOutputTokens
		}
		configs = append(configs, entry)
	}
	return configs
}

func (d *DeepSeekHarness) CurrentModel() string {
	settingsPath, err := deepSeekHarnessSettingsPath()
	if err != nil {
		return ""
	}
	if !deepSeekHarnessPatchHealthy(settingsPath) {
		return ""
	}
	settings, err := readDeepSeekHarnessYAML(settingsPath)
	if err != nil {
		return ""
	}

	selected, _ := settings["agent-default-model"].(map[string]any)
	if selected == nil || selected["provider"] != deepSeekHarnessProvider {
		return ""
	}
	modelName, _ := selected["model"].(string)
	if modelName == "" {
		return ""
	}

	llm, _ := settings["llm-pi-ai"].(map[string]any)
	providers, _ := llm["providers"].(map[string]any)
	provider, _ := providers[deepSeekHarnessProvider].(map[string]any)
	if !deepSeekHarnessProviderHealthy(provider, modelName) {
		return ""
	}
	if !shouldManageOllamaWebSearch() {
		return modelName
	}
	web, _ := settings[deepSeekHarnessWebSettings].(map[string]any)
	if !deepSeekHarnessWebProviderHealthy(web, modelName) {
		return ""
	}
	return modelName
}

func deepSeekHarnessPatchHealthy(settingsPath string) bool {
	patchPath, err := deepSeekHarnessPatchPath()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(patchPath)
	if err != nil {
		return false
	}
	var patches []struct {
		ID     string `yaml:"id"`
		Config struct {
			Path string `yaml:"path"`
		} `yaml:"config"`
	}
	if err := yaml.Unmarshal(data, &patches); err != nil || len(patches) != 1 {
		return false
	}
	return patches[0].ID == "settings" && patches[0].Config.Path == settingsPath
}

func deepSeekHarnessProviderHealthy(provider map[string]any, modelName string) bool {
	if provider == nil || provider["api"] != "openai-completions" || provider["apiKeyEnv"] != deepSeekHarnessAPIKeyEnv {
		return false
	}
	baseURL, _ := provider["baseURL"].(string)
	if strings.TrimRight(baseURL, "/") != strings.TrimRight(deepSeekHarnessBaseURL(), "/") {
		return false
	}
	models, _ := provider["models"].([]any)
	for _, raw := range models {
		entry, _ := raw.(map[string]any)
		if entry["id"] == modelName {
			return true
		}
	}
	return false
}

func deepSeekHarnessWebProviderHealthy(web map[string]any, modelName string) bool {
	if web == nil || web["apiKeyEnv"] != deepSeekHarnessAPIKeyEnv || web["model"] != modelName {
		return false
	}
	baseURL, _ := web["baseURL"].(string)
	return strings.TrimRight(baseURL, "/") == strings.TrimRight(deepSeekHarnessBaseURL(), "/")
}

func (d *DeepSeekHarness) Onboard() error {
	return config.MarkIntegrationOnboarded(deepSeekHarnessIntegrationName)
}

func (d *DeepSeekHarness) RequiresInteractiveOnboarding() bool { return false }

func deepSeekHarnessBaseURL() string {
	return strings.TrimRight(envconfig.ConnectableHost().String(), "/") + "/v1"
}

func deepSeekHarnessConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ollama", "launch", "dsh"), nil
}

func deepSeekHarnessSettingsPath() (string, error) {
	dir, err := deepSeekHarnessConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.yaml"), nil
}

func deepSeekHarnessPatchPath() (string, error) {
	dir, err := deepSeekHarnessConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ollama.cordis.yml"), nil
}

func writeDeepSeekHarnessFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if err := fileutil.WriteWithBackup(path, data, deepSeekHarnessIntegrationName); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
