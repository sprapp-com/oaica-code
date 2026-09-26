package launch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
	"github.com/ollama/ollama/envconfig"
)

const clineLaunchProvider = "ollama"

// Cline implements Runner and Editor for the Cline CLI integration
type Cline struct{}

func (c *Cline) String() string { return "Cline" }

func (c *Cline) Run(model string, _ []LaunchModel, args []string) error {
	forceTools, args := extractForceTools(args)
	if err := gateOpenAITools(model, forceTools); err != nil {
		return err
	}

	bin, err := ensureClineInstalled()
	if err != nil {
		return err
	}

	launchArgs := clineLaunchArgs(model, args)
	cmd := exec.Command(bin, launchArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func ensureClineInstalled() (string, error) {
	if _, err := exec.LookPath("cline"); err == nil {
		return "cline", nil
	}

	if _, err := exec.LookPath("npm"); err != nil {
		return "", fmt.Errorf("cline is not installed and required dependencies are missing\n\nInstall the following first:\n  npm (Node.js): https://nodejs.org/\n\nThen re-run:\n  oaica launch cline")
	}

	ok, err := ConfirmPrompt("Cline is not installed. Install with npm?")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("cline installation cancelled")
	}

	fmt.Fprintf(os.Stderr, "\nInstalling Cline...\n")
	cmd := exec.Command("npm", "install", "-g", "cline@latest")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to install cline: %w", err)
	}

	if _, err := exec.LookPath("cline"); err != nil {
		return "", fmt.Errorf("cline was installed but the binary was not found on PATH\n\nYou may need to restart your shell")
	}

	fmt.Fprintf(os.Stderr, "%sCline installed successfully%s\n\n", ansiGreen, ansiReset)
	return "cline", nil
}

func clineLaunchArgs(model string, extra []string) []string {
	return extra
}

func (c *Cline) Paths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	var paths []string
	for _, p := range []string{
		clineProvidersPath(home),
		clineLegacyGlobalStatePath(home),
	} {
		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		}
	}
	return paths
}

func (c *Cline) Edit(models []LaunchModel) error {
	if len(models) == 0 {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	providersPath := clineProvidersPath(home)
	legacyPath := clineLegacyGlobalStatePath(home)

	// Both documents are parsed before either is written, so a malformed one is
	// an error the user sees before oaica has already rewritten the other file.
	// This pass is validation only: the documents that actually get written are
	// read again inside their own locks below, which is what orders the writers.
	if _, err := readClineConfig(providersPath); err != nil {
		return err
	}
	if _, err := readClineConfig(legacyPath); err != nil {
		return err
	}

	// Each of the two documents is rewritten whole from a snapshot of itself, so
	// each store's load-mutate-save runs under that store's own lock and the read
	// happens INSIDE it. Read outside and the lock orders only the publishes: two
	// `oaica launch cline` commands whose writes overlap each publish a snapshot
	// taken before the other's entry landed, and the launch that renames last
	// decides the file while both report success. The two stores are separate
	// files, so they take separate locks (taken in this order by every caller, so
	// two commands cannot deadlock against each other). Both files live in
	// Cline's own data directory, so the locks are keyed under ~/.oaica/locks
	// (foreignStoreLockBase) rather than dropped inside ~/.cline — oaica writes
	// two entries into that tree and a stray lock file beside the user's settings
	// is not part of that deal (2026-09-26 audit, thirteenth round).
	if err := fileutil.WithFileLock(foreignStoreLockBase(providersPath), func() error {
		providersConfig, err := readClineConfig(providersPath)
		if err != nil {
			return err
		}
		return writeClineProvidersConfig(providersPath, providersConfig, models[0].Name)
	}); err != nil {
		return err
	}

	return fileutil.WithFileLock(foreignStoreLockBase(legacyPath), func() error {
		legacyConfig, err := readClineConfig(legacyPath)
		if err != nil {
			return err
		}
		return writeClineLegacyGlobalState(legacyPath, legacyConfig, models[0].Name)
	})
}

func clineProvidersPath(home string) string {
	return filepath.Join(home, ".cline", "data", "settings", "providers.json")
}

func clineLegacyGlobalStatePath(home string) string {
	return filepath.Join(home, ".cline", "data", "globalState.json")
}

func clineOllamaRootURL() string {
	return strings.TrimRight(envconfig.ConnectableHost().String(), "/")
}

func clineProviderBaseURL() string {
	return clineOllamaRootURL() + "/v1"
}

// clineProviderBaseURLFor is the provider base URL Cline should use: the
// remote's direct base for a user-remote model, otherwise the daemon's /v1.
func clineProviderBaseURLFor(model string) string {
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return strings.TrimRight(ep.BaseURL, "/")
	}
	return clineProviderBaseURL()
}

// clineModelIDFor is the model id Cline should use: the bare upstream id for a
// user-remote model, otherwise the picker name.
func clineModelIDFor(model string) string {
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return ep.UpstreamModel
	}
	return model
}

// clineLegacyBaseURLFor is the server root the legacy global state records for
// a model: the remote's own root for a user-remote model, otherwise the
// daemon's (unchanged). The legacy state carries a root — the daemon value has
// never had the /v1 the providers.json entry carries — so the remote form drops
// its version prefix too (remoteBaseURL) and both shapes stay the same kind of
// value. Writing the daemon's root for a model only the remote serves sent
// Cline to 127.0.0.1 for it (2026-09-26 audit).
func clineLegacyBaseURLFor(model string) string {
	if remote, _, ok := findUserRemoteForModel(model); ok {
		return remoteBaseURL(remote)
	}
	return clineOllamaRootURL()
}

func readClineConfig(configPath string) (map[string]any, error) {
	config := make(map[string]any)
	if data, err := os.ReadFile(configPath); err == nil {
		// UseNumber: this document is the user's, and oaica writes it back
		// whole after adding one provider entry. Decoding into map[string]any
		// makes every number in it a float64, so an integer larger than 2^53
		// comes back as a different number and 1.0 comes back as 1 — a config
		// changed by a command that only meant to add a key (2026-09-26 audit).
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&config); err != nil {
			return nil, fmt.Errorf("failed to parse config: %w, at: %s", err, configPath)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return config, nil
}

// writeClineProvidersConfig publishes providers.json from config, which its
// caller (Cline.Edit) read under this store's lock — the lock has to cover the
// read as well as this write, so it is taken a frame up, where the document is
// read. Nothing here acquires it again: WithFileLock does not nest.
func writeClineProvidersConfig(configPath string, config map[string]any, model string) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}

	providers, _ := config["providers"].(map[string]any)
	if providers == nil {
		providers = make(map[string]any)
	}

	provider, _ := providers[clineLaunchProvider].(map[string]any)
	if provider == nil {
		provider = make(map[string]any)
	}
	settings, _ := provider["settings"].(map[string]any)
	if settings == nil {
		settings = make(map[string]any)
	}

	baseURL := clineProviderBaseURLFor(model)
	modelID := clineModelIDFor(model)
	previousModel, _ := settings["model"].(string)
	previousBaseURL, _ := settings["baseUrl"].(string)
	previousTokenSource, _ := provider["tokenSource"].(string)

	settings["provider"] = clineLaunchProvider
	settings["model"] = modelID
	settings["baseUrl"] = baseURL
	if ep, ok := resolveRemoteEndpoint(model); ok {
		settings["apiKey"] = ep.Token
	} else {
		delete(settings, "apiKey")
	}
	provider["settings"] = settings

	if previousModel != model || previousBaseURL != baseURL || previousTokenSource != "manual" {
		provider["updatedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
	} else if _, ok := provider["updatedAt"].(string); !ok {
		provider["updatedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	provider["tokenSource"] = "manual"
	providers[clineLaunchProvider] = provider

	config["version"] = float64(1)
	config["lastUsedProvider"] = clineLaunchProvider
	config["providers"] = providers

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteWithBackup(configPath, data, "cline")
}

// writeClineLegacyGlobalState publishes globalState.json from config, which its
// caller (Cline.Edit) read under this store's lock — the same arrangement as
// writeClineProvidersConfig above, for the same reason.
func writeClineLegacyGlobalState(configPath string, config map[string]any, model string) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}

	// The same two branches the providers.json write above takes: for a
	// user-remote model the state has to name the remote and the model id the
	// remote knows, or Cline reads it as a daemon model (2026-09-26 audit).
	baseURL := clineLegacyBaseURLFor(model)
	modelID := clineModelIDFor(model)
	config["ollamaBaseUrl"] = baseURL
	config["actModeApiProvider"] = clineLaunchProvider
	config["actModeOllamaModelId"] = modelID
	config["actModeOllamaBaseUrl"] = baseURL
	config["planModeApiProvider"] = clineLaunchProvider
	config["planModeOllamaModelId"] = modelID
	config["planModeOllamaBaseUrl"] = baseURL

	config["welcomeViewCompleted"] = true

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteWithBackup(configPath, data, "cline")
}

func (c *Cline) Models() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	if model := clineProviderModel(home); model != "" {
		return []string{model}
	}

	config, err := fileutil.ReadJSON(clineLegacyGlobalStatePath(home))
	if err != nil {
		return nil
	}

	switch config["actModeApiProvider"] {
	case "ollama":
	default:
		return nil
	}

	modelID, _ := config["actModeOllamaModelId"].(string)
	if modelID == "" {
		return nil
	}
	baseURL, _ := config["actModeOllamaBaseUrl"].(string)
	return []string{clinePickerNameFor(modelID, baseURL)}
}

func clineProviderModel(home string) string {
	config, err := fileutil.ReadJSON(clineProvidersPath(home))
	if err != nil {
		return ""
	}
	if config["lastUsedProvider"] != clineLaunchProvider {
		return ""
	}
	providers, _ := config["providers"].(map[string]any)
	provider, _ := providers[clineLaunchProvider].(map[string]any)
	settings, _ := provider["settings"].(map[string]any)
	model, _ := settings["model"].(string)
	baseURL, _ := settings["baseUrl"].(string)
	return clinePickerNameFor(model, baseURL)
}

// clinePickerNameFor is the inverse of clineModelIDFor: the picker name a
// stored entry was written for.
//
// Both of Cline's stores keep the remote's endpoint beside the id — the
// provider settings as the remote's base URL, the legacy state as its root —
// and they store, for a user-remote model, the bare upstream id. Answering
// with that id names a model the launcher never saved (it saves the picker
// name), so liveConfigMatches (slices.Equal(editor.Models(), models),
// launch.go) was false on every run and each launch rewrote the config it had
// just read (2026-09-26 audit, thirteenth round — the same fix pi, droid and
// hermes carry). Either recorded form matches, because the two stores record
// the endpoint differently.
func clinePickerNameFor(modelID, recordedBase string) string {
	if modelID == "" || recordedBase == "" {
		return modelID
	}
	want := strings.TrimRight(recordedBase, "/")
	if want == strings.TrimRight(clineProviderBaseURL(), "/") || want == strings.TrimRight(clineOllamaRootURL(), "/") {
		// The daemon's own endpoint: an entry here is a picker name already.
		return modelID
	}
	remotes, err := loadUserRemotes()
	if err != nil {
		// The rule findUserRemoteForModel uses: a corrupt store must not take
		// the built-in providers with it.
		remotes = builtinRemotes()
	}
	for _, r := range remotes {
		if strings.TrimRight(r.openAIBase(), "/") != want && strings.TrimRight(remoteBaseURL(r), "/") != want {
			continue
		}
		// Only a name that resolves back to the same model is that model's
		// picker name; anything else is left as it was found.
		candidate := r.Name + "/" + modelID
		if ep, ok := resolveRemoteEndpoint(candidate); ok && ep.UpstreamModel == modelID {
			return candidate
		}
	}
	return modelID
}
