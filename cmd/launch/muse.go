package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/internal/fileutil"
	"github.com/ollama/ollama/envconfig"
)

// Muse implements Runner and Editor for Meta's Muse Code CLI.
//
// Muse has no flag or environment variable that can supply a model catalog, and
// it will not start without one: its provider fetches <origin>/muse-code/models
// before the first inference call, reusing only the scheme, host and port of the
// configured base URL. The undocumented model_catalog key in settings.json is
// the one way to satisfy that without standing up the endpoint, so launch has to
// write a settings file.
//
// That file is also where the base URL lives, and endpoint_transport is a single
// global provider switch rather than an additive model list — writing it into
// ~/.config/muse/settings.json would repoint the user's whole muse install. So
// launch keeps its own config root and passes it to muse as XDG_CONFIG_HOME,
// leaving a Meta-backed muse and `oaica launch muse` free to coexist.
type Muse struct{}

const (
	// Muse checks every catalog row against the session's provider and profile
	// and drops the ones that disagree; "tbh" is the profile its sessions run
	// under.
	museProviderID = "meta"
	museProfileID  = "tbh"

	// Every row needs a context and an output limit, and the inventory only
	// reports an output limit for cloud models, so local models land on these.
	museFallbackContextLimit = 32768
	museFallbackOutputLimit  = 32768

	// museLoadTimeout bounds the default model's preload; a cold load of a
	// large model is tens of seconds, and on timeout the row falls back to
	// the inventory value rather than blocking the launch.
	museLoadTimeout = 5 * time.Minute

	museRowDescription = "Served by Ollama"
)

var museGOOS = runtime.GOOS

// museInstallCommand runs Meta's official installer, which places the muse
// launcher in ~/.local/bin and downloads the matching binary next to it.
// museInstallCommand overrides the install invocation for tests. When nil
// (production), ensureMuseInstalled uses the verified-download helper
// (installer_dl.go) instead of piping Meta's installer into bash.
var museInstallCommand []string

// ensureMuseInstalled returns the muse binary path, offering to run the
// official installer when it is missing and verifying the binary afterward.
func ensureMuseInstalled() (string, error) {
	if path, err := findMuse(); err == nil {
		return path, nil
	}

	ok, err := ConfirmPrompt("Muse is not installed. Install now?")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("muse installation cancelled")
	}

	fmt.Fprintf(os.Stderr, "\nInstalling Muse...\n")
	if museInstallCommand == nil {
		if err := runInstallerScriptFn("https://dev.meta.ai/install.sh"); err != nil {
			return "", fmt.Errorf("failed to install muse: %w", err)
		}
	} else {
		cmd := exec.Command(museInstallCommand[0], museInstallCommand[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("muse installation failed: %w", err)
		}
	}

	path, err := findMuse()
	if err != nil {
		return "", fmt.Errorf("muse installer finished but the binary was not found")
	}
	return path, nil
}

// museCatalogRow is one row of muse's settings-side model catalog. Muse requires
// model_id, provider_id, profile_id and both limits; the rest only affect how
// the model reads in muse's picker.
type museCatalogRow struct {
	ModelID      string `json:"model_id"`
	ProviderID   string `json:"provider_id"`
	ProfileID    string `json:"profile_id"`
	DisplayLabel string `json:"display_label"`
	Visibility   string `json:"visibility"`
	DisplayOrder int    `json:"display_order"`
	IsDefault    bool   `json:"is_default"`
	ContextLimit int    `json:"context_limit"`
	OutputLimit  int    `json:"output_limit"`
	Description  string `json:"description"`
}

func (m *Muse) String() string { return "Muse Code" }

func (m *Muse) Supported() error {
	if museGOOS == "windows" {
		return fmt.Errorf("Warning: Muse is not currently supported on Windows")
	}
	return nil
}

func (m *Muse) Run(model string, models []LaunchModel, args []string) error {
	if err := m.Supported(); err != nil {
		return err
	}
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("model is required")
	}

	bin, err := ensureMuseInstalled()
	if err != nil {
		return err
	}

	runModels := museApplyLoadedContext(museRunModels(model, models))
	// When Edit already wrote this catalog, Run only refreshes loaded limits.
	// Preserve Edit's backup by avoiding a second backup for that refresh.
	backup := !slices.Equal(m.Models(), launchModelNames(runModels))
	if err := writeMuseSettingsFile(runModels, backup); err != nil {
		return fmt.Errorf("failed to configure muse: %w", err)
	}

	configHome, err := museConfigHome()
	if err != nil {
		return err
	}

	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+configHome)
	return cmd.Run()
}

func (m *Muse) Edit(models []LaunchModel) error {
	return writeMuseSettings(models)
}

func (m *Muse) Paths() []string {
	settingsPath, err := museSettingsPath()
	if err != nil {
		return nil
	}
	if _, err := os.Stat(settingsPath); err != nil {
		return nil
	}
	return []string{settingsPath}
}

func (m *Muse) Models() []string {
	settingsPath, err := museSettingsPath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil
	}

	var settings struct {
		ModelCatalog []museCatalogRow `json:"model_catalog"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil
	}

	var models []string
	for _, row := range settings.ModelCatalog {
		if row.ModelID != "" {
			models = append(models, row.ModelID)
		}
	}
	return models
}

// findMuse locates the muse launcher, which installs itself into ~/.local/bin
// and is not always on PATH.
func findMuse() (string, error) {
	if path, err := exec.LookPath("muse"); err == nil {
		return path, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	fallback := filepath.Join(home, ".local", "bin", "muse")
	if info, err := os.Stat(fallback); err != nil || info.IsDir() {
		return "", fmt.Errorf("muse binary not found")
	}
	return fallback, nil
}

// museConfigHome is the XDG_CONFIG_HOME handed to muse at launch. Muse reads
// settings.json from $XDG_CONFIG_HOME/muse, so the file sits one level deeper.
func museConfigHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ollama", "launch", "muse-config"), nil
}

func museSettingsPath() (string, error) {
	configHome, err := museConfigHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(configHome, "muse", "settings.json"), nil
}

// museUserSettingsPath is where muse keeps settings.json when it runs on its own.
func museUserSettingsPath() (string, error) {
	if configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); configHome != "" {
		return filepath.Join(configHome, "muse", "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "muse", "settings.json"), nil
}

// museBaseSettings returns the document the generated config is layered onto.
//
// Muse persists its own settings back into whichever config root it was given,
// so once launch's file exists it becomes the base and everything muse recorded
// there survives the next launch. Before that, the user's own settings seed it so
// a first launch keeps their skills, hooks, MCP servers and TUI preferences.
// A launch-owned file that exists but cannot be parsed is an error: falling
// through would rewrite it and discard whatever muse persisted there.
func museBaseSettings() (map[string]any, error) {
	if path, err := museSettingsPath(); err == nil {
		settings, err := museReadSettings(path)
		switch {
		case err == nil && settings != nil:
			return settings, nil
		case err != nil && !os.IsNotExist(err):
			return nil, fmt.Errorf("read muse settings %s: %w", path, err)
		}
	}
	if path, err := museUserSettingsPath(); err == nil {
		if settings, err := museReadSettings(path); err == nil && settings != nil {
			return settings, nil
		}
	}
	return map[string]any{}, nil
}

// museReadSettings reads a settings document without letting its numbers
// through float64.
//
// writeMuseSettingsFile rewrites the whole document, and this file is also a
// LIVE document: muse persists its own settings into the config root launch
// hands it, so the values in it are not all oaica's and a number oaica does not
// model is still one muse wrote. Decoding into map[string]any makes every one
// of them a float64, and a float64 is not the number that was in the file —
// 2^53+1 comes back as 2^53, 1.0 comes back as 1 — so the next program to read
// it sees a different value. Same rule as cline, vscode, claude-desktop, pi and
// opencode apply to their own documents (2026-09-26 audit, tenth round).
func museReadSettings(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var settings map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&settings); err != nil {
		return nil, err
	}
	return settings, nil
}

// writeMuseSettings regenerates the settings file launch owns, replacing only
// the keys that decide which provider and models muse talks to.
func writeMuseSettings(models []LaunchModel) error {
	return writeMuseSettingsFile(models, true)
}

func writeMuseSettingsFile(models []LaunchModel, backup bool) error {
	if len(models) == 0 {
		return nil
	}
	if err := museRejectNonDaemonModels(models); err != nil {
		return err
	}

	settingsPath, err := museSettingsPath()
	if err != nil {
		return err
	}
	// The store is oaica's OWN (~/.ollama/launch/muse-config is the config root
	// launch hands muse as XDG_CONFIG_HOME), so unlike the stores of another
	// program the lock goes beside it, as the other oaica-owned launch stores do
	// (dsh). It has to cover the read as well as the publish: museBaseSettings
	// is read a frame down, and two overlapping writers that read before taking
	// the lock each publish a snapshot taken before the other's settings landed
	// — the launch that renames last decides the file while both report success
	// (2026-09-26 audit, thirteenth round).
	return fileutil.WithFileLock(settingsPath, func() error {
		return writeMuseSettingsFileLocked(settingsPath, models, backup)
	})
}

// museRejectNonDaemonModels refuses a selection muse's settings cannot express.
//
// Every other integration in this package translates a user-remote row to the
// remote's own endpoint and model id. Muse cannot: endpoint_transport is ONE
// global provider switch for the whole file (see the Muse doc comment above),
// so a selection that mixes a daemon model with a remote one has no
// representation at all, and a remote's credential has no field oaica can
// write — the schema is not published and muse is not installed here to read it
// off. Writing the picker name (what this did before) is worse than refusing:
// muse posts "box/big-model" to the local daemon, which does not resolve
// namespaced remotes, so every request fails model-not-found after the user has
// already answered the key prompt.
//
// Refusing here keeps the failure at the point the user can act on it. Lift
// this when muse's credential field is verifiable.
//
// The rows refused are the ones routed to an endpoint other than the local
// daemon (daemonRoutedModel): an ollama-cloud row carries Remote too, and the
// daemon serves and proxies it, so refusing it told the user to launch a
// daemon-backed model when that is exactly what they had selected (2026-09-27
// audit, round 20).
func museRejectNonDaemonModels(models []LaunchModel) error {
	for _, model := range models {
		if daemonRoutedModel(model) {
			continue
		}
		return fmt.Errorf("muse cannot be pointed at %q: muse's settings carry a single endpoint for every catalog row — the local daemon — and it has no credential field oaica can write. %s. Add the model to muse directly, or launch one the daemon serves", model.Name, nonDaemonRowReason(model))
	}
	return nil
}

// writeMuseSettingsFileLocked is the load-mutate-save half of
// writeMuseSettingsFile, run under the settings store's lock.
func writeMuseSettingsFileLocked(settingsPath string, models []LaunchModel, backup bool) error {
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return err
	}

	settings, err := museBaseSettings()
	if err != nil {
		return err
	}
	if _, ok := settings["schema_version"]; !ok {
		settings["schema_version"] = 1
	}
	settings["provider"] = museProviderID
	settings["model"] = models[0].Name
	settings["endpoint_transport"] = map[string]any{
		"base_url": envconfig.ConnectableHost().String() + "/v1",
		// Ollama wants no credential, and muse refuses to start on the default
		// "bearer" unless one is configured.
		"auth": "none",
	}
	settings["model_catalog"] = museCatalogRows(models)

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if backup {
		return fileutil.WriteWithBackup(settingsPath, data, "muse")
	}
	return fileutil.WriteFileAtomic(settingsPath, data, 0o600)
}

func museCatalogRows(models []LaunchModel) []museCatalogRow {
	rows := make([]museCatalogRow, 0, len(models))
	for i, model := range models {
		contextLimit := model.ContextLength
		if contextLimit <= 0 {
			contextLimit = museFallbackContextLimit
		}
		outputLimit := model.MaxOutputTokens
		if outputLimit <= 0 {
			outputLimit = min(contextLimit, museFallbackOutputLimit)
		}

		rows = append(rows, museCatalogRow{
			ModelID:      model.Name,
			ProviderID:   museProviderID,
			ProfileID:    museProfileID,
			DisplayLabel: model.Name,
			Visibility:   "visible",
			DisplayOrder: i,
			IsDefault:    i == 0,
			ContextLimit: contextLimit,
			OutputLimit:  outputLimit,
			Description:  museRowDescription,
		})
	}
	return rows
}

// museApplyLoadedContext overwrites the launched model's context length with
// the size the server actually loaded it at. Muse budgets prompt packing and
// compaction against the catalog row, and VRAM fit or server configuration may
// hold the loaded size below the model's trained maximum — which is all the
// inventory knows. Run-only on the selected model: editing the config must
// not load anything, and the other picker rows are not worth a load each.
func museApplyLoadedContext(models []LaunchModel) []LaunchModel {
	if len(models) == 0 || models[0].Remote {
		return models
	}
	models = cloneLaunchModels(models)
	if n := museLoadedContextLength(models[0].Name); n > 0 {
		models[0].ContextLength = n
	}
	return models
}

// museLoadedContextLength reports the effective context length of a model as
// the server loaded it, swappable so tests never touch a live server.
var museLoadedContextLength = loadedContextLength

// loadedContextLength loads model and reads the running instance's context
// length from the process list — the size the scheduler actually allocated.
// An empty generate request is ollama's load-only call: it returns once the
// model is resident without generating tokens, and the launch pays a load the
// first muse request would otherwise pay. Returns 0 when anything fails, and
// the caller keeps the inventory value.
func loadedContextLength(model string) int {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), museLoadTimeout)
	defer cancel()
	if err := client.Generate(ctx, &api.GenerateRequest{Model: model}, func(api.GenerateResponse) error { return nil }); err != nil {
		return 0
	}
	return LoadedContextWindow(ctx, client, model)
}

// museRunModels puts the model being launched first, since muse takes its
// default from the first visible row, and keeps the rest of the selection
// available in muse's picker.
func museRunModels(primary string, models []LaunchModel) []LaunchModel {
	resolved := make([]LaunchModel, 0, len(models)+1)
	appendModel := func(name string) {
		if name == "" || hasLaunchModel(resolved, name) {
			return
		}
		if model, ok := findLaunchModel(models, name); ok {
			resolved = append(resolved, model)
			return
		}
		resolved = append(resolved, fallbackLaunchModel(name))
	}

	appendModel(primary)
	for _, model := range models {
		appendModel(model.Name)
	}
	return resolved
}
