package launch

import (
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
	return runChild(cmd)
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

// NarrowToStoredModels implements narrowingEditor. Cline's provider settings
// hold ONE model — settings.model beside the baseUrl it belongs to — and both
// of its stores are rewritten from that single value, so a multi-model
// selection is narrowed to the first before anything is written or recorded.
// Edit still writes models[0].Name (pinned by cline_test.go's "uses first model
// as primary"); this is the other half of that contract, and without it the
// integration state recorded every selected name while the store held one, so
// liveConfigMatches was false forever and each launch rewrote the file it had
// just read (2026-09-26 audit, fifteenth round).
func (c *Cline) NarrowToStoredModels(models []string) ([]string, []string) {
	if len(models) <= 1 {
		return models, nil
	}
	dropped := make([]string, 0, len(models)-1)
	dropped = append(dropped, models[1:]...)
	return models[:1], dropped
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

	// Each of the two documents is rewritten whole from a snapshot of itself, so
	// each store's load-mutate-save runs under that store's own lock and the read
	// happens INSIDE it. Read outside and the lock orders only the publishes: two
	// `oaica launch cline` commands whose writes overlap each publish a snapshot
	// taken before the other's entry landed, and the launch that renames last
	// decides the file while both report success (2026-09-26 audit, thirteenth
	// round). Both files live in Cline's own data directory, so the locks are
	// keyed under ~/.oaica/locks (foreignStoreLockBase) rather than dropped
	// inside ~/.cline — oaica writes two entries into that tree and a stray lock
	// file beside the user's settings is not part of that deal.
	//
	// BOTH locks are held across BOTH writes, in this order (providers, then
	// legacy) by every caller, so two commands cannot deadlock against each
	// other. Holding only the lock of the file being written — each lock
	// released before the next was taken, as this did until the sixteenth round
	// — orders writers of each file and leaves the PAIR unordered: two
	// overlapping launches can publish A's providers.json beside B's
	// globalState.json and neither document then describes a launch that
	// happened, while both commands report success. These two documents are one
	// selection's two halves, so they are published together or not at all.
	//
	// Both documents are also read before either is written: a malformed second
	// one must be an error the user sees before oaica has already rewritten the
	// first (pi's Edit, same reason). The SAME rule applies to a document that
	// parses but is not ours to repoint: the legacy endpoint check runs here, in
	// the read phase, and not only inside the legacy writer — that writer runs
	// second, so a launch refused for a foreign globalState.json had already
	// published (or created) providers.json by the time it refused, leaving the
	// pair split across two selections, which is the one thing these two files
	// are never allowed to be (2026-09-27 audit, round 26).
	return fileutil.WithFileLock(foreignStoreLockBase(providersPath), func() error {
		return fileutil.WithFileLock(foreignStoreLockBase(legacyPath), func() error {
			providersConfig, err := readClineConfig(providersPath)
			if err != nil {
				return err
			}
			legacyConfig, err := readClineConfig(legacyPath)
			if err != nil {
				return err
			}
			if err := clineRefuseForeignLegacyEndpoint(legacyConfig); err != nil {
				return err
			}
			// Both documents are BUILT before either is staged, so the refusals
			// that live in their builders (the user's own provider under the
			// "ollama" key, a foreign legacy endpoint) are raised here, in the
			// read phase, and not one document into the publish.
			providersData, err := clineProvidersConfigData(providersConfig, models[0])
			if err != nil {
				return err
			}
			legacyData, err := clineLegacyGlobalStateData(legacyConfig, models[0])
			if err != nil {
				return err
			}
			// fileutil.PublishAll, not the two writers one after the other: each
			// of those stages and renames its own file, so a second half that
			// could not be staged (a read-only directory, a full disk) failed
			// after the first had already been published and left the pair
			// describing two different selections, which is the one thing these
			// two files are never allowed to be (2026-09-27 audit, round 27,
			// F5).
			return fileutil.PublishAll(
				fileutil.PublishFile{Path: providersPath, Data: providersData, Integration: "cline"},
				fileutil.PublishFile{Path: legacyPath, Data: legacyData, Integration: "cline"},
			)
		})
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
	if ep, ok := resolveLaunchTargetEndpoint(model); ok {
		return strings.TrimRight(ep.BaseURL, "/")
	}
	return clineProviderBaseURL()
}

// clineModelIDFor is the model id Cline should use: the bare upstream id for a
// user-remote model, otherwise the picker name.
func clineModelIDFor(model string) string {
	if ep, ok := resolveLaunchTargetEndpoint(model); ok {
		return ep.UpstreamModel
	}
	return model
}

// clineWriteModelID is the model id Cline's two stores should hold for a row:
// the id the row's backend serves when its Name is a display-only picker label
// (LaunchModel.Upstream — an ollama-cloud catalogue row, named "ollama/gpt-oss"
// by the picker and served as "gpt-oss:cloud"), otherwise clineModelIDFor's own
// answer for the row's name (the bare upstream id for a user-remote model, the
// picker name otherwise).
//
// The picker label named the LOCAL model of that name for a cloud row, so Cline
// asked the daemon for a model that was absent or — worse — a different one of
// the same name, and the name stored never matched the name the launcher saves
// for the selection, so every launch rewrote both files (2026-09-27 audit,
// round 25).
func clineWriteModelID(model LaunchModel) string {
	if upstream := strings.TrimSpace(model.Upstream); upstream != "" {
		return upstream
	}
	return clineModelIDFor(model.Name)
}

// clineEndpointWasOurs reports whether a base URL recorded in Cline's ollama
// provider is one oaica writes: the local daemon's, or a configured remote's.
//
// "ollama" is Cline's own provider id as well as the key oaica writes under, so
// an entry there may be the USER's own Ollama provider (their LAN server, their
// key, or ollama.com). Taking that entry over rewrites its base URL and deletes
// its API key, which destroys a working configuration to serve the launch
// (2026-09-27 audit, round 21). An entry naming an endpoint oaica itself writes
// is the same provider by another name and is configured as before; anything
// else is refused, out loud, before the file is touched.
func clineEndpointWasOurs(recorded string) bool {
	recorded = strings.TrimRight(strings.TrimSpace(recorded), "/")
	if recorded == "" {
		// No endpoint recorded: Cline's own default, which is the daemon.
		return true
	}
	if recorded == strings.TrimRight(clineProviderBaseURL(), "/") || recorded == strings.TrimRight(clineOllamaRootURL(), "/") {
		return true
	}
	// The daemon's documented default address: a launch under a since-moved
	// OLLAMA_HOST wrote it and left it recorded here, and a later launch from a
	// shell without that export (an interactive .bashrc export is invisible to a
	// non-interactive invocation) read its own value back as the user's and
	// refused. Same rule as hermesEndpointWasOurs (2026-09-27 audit, round 22).
	for _, base := range []string{"http://127.0.0.1:11434", "http://localhost:11434", "http://[::1]:11434"} {
		if recorded == base || recorded == base+"/v1" {
			return true
		}
	}
	remotes, err := loadUserRemotes()
	if err != nil {
		// The rule findUserRemoteForModel uses: a corrupt store must not take
		// the built-in providers with it.
		remotes = builtinRemotes()
	}
	for _, r := range remotes {
		if strings.TrimRight(r.openAIBase(), "/") == recorded || strings.TrimRight(remoteBaseURL(r), "/") == recorded {
			return true
		}
	}
	return false
}

// clineRefuseForeignLegacyEndpoint refuses a legacy globalState.json whose
// Ollama endpoint oaica did not write.
//
// The document holds that endpoint three ways — ollamaBaseUrl plus each mode's
// own copy — and Cline keeps ONE Ollama endpoint for both modes, so a write
// repoints all of them together. A user whose Cline is set to their own server
// (a LAN box, ollama.com with a key) lost its endpoint and its model id here,
// in a document the providers guard never looks at (2026-09-27 audit, round
// 25). Redacted: the value is file-derived and may carry a credential in its
// userinfo or query.
//
// Called from BOTH the read phase of Cline.Edit and the top of
// writeClineLegacyGlobalState: the writers publish providers.json first, so a
// check that lived only in the legacy writer refused the launch after the
// first half of the pair was already on disk (2026-09-27 audit, round 26).
func clineRefuseForeignLegacyEndpoint(config map[string]any) error {
	for _, key := range []string{"actModeOllamaBaseUrl", "planModeOllamaBaseUrl", "ollamaBaseUrl"} {
		recorded, _ := config[key].(string)
		if strings.TrimSpace(recorded) == "" {
			continue
		}
		if !clineEndpointWasOurs(recorded) {
			return fmt.Errorf("Cline's legacy settings (%s) name an endpoint oaica did not write (%s), so it is yours: Cline keeps one Ollama endpoint for both modes, and pointing it at the local daemon would rewrite its base URL and model id. Remove or rename that endpoint in Cline's own settings, or launch a different integration", key, redactBaseURL(recorded))
		}
	}
	return nil
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
		// decodeJSONObject, not a bare Decode: a document that IS `null`
		// decodes into a nil map, which the write below then panics on
		// (2026-09-27 audit, round 19).
		doc, derr := decodeJSONObject(data)
		if derr != nil {
			return nil, fmt.Errorf("failed to parse config: %w, at: %s", derr, configPath)
		}
		config = doc
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return config, nil
}

// writeClineProvidersConfig publishes providers.json from config, which its
// caller (Cline.Edit) read under this store's lock — the lock has to cover the
// read as well as this write, so it is taken a frame up, where the document is
// read. Nothing here acquires it again: WithFileLock does not nest.
func writeClineProvidersConfig(configPath string, config map[string]any, model LaunchModel) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}
	data, err := clineProvidersConfigData(config, model)
	if err != nil {
		return err
	}
	return fileutil.WriteWithBackup(configPath, data, "cline")
}

// clineProvidersConfigData is providers.json as a write of model leaves it, in
// memory. The writer above, and the pair publish in Cline.Edit, both publish
// this document — one builder, so the bytes Edit stages with its sibling and
// the bytes a standalone call writes cannot differ.
func clineProvidersConfigData(config map[string]any, model LaunchModel) ([]byte, error) {
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

	baseURL := clineProviderBaseURLFor(model.Name)
	modelID := clineWriteModelID(model)
	previousModel, _ := settings["model"].(string)
	previousBaseURL, _ := settings["baseUrl"].(string)
	previousTokenSource, _ := provider["tokenSource"].(string)

	// Before anything is mutated: the entry under this key may be the user's own
	// Ollama provider, and the write below would repoint it at the daemon and
	// delete its key (2026-09-27 audit, round 21). Redacted, because the value
	// is file-derived and may carry credentials in its userinfo or query.
	if !clineEndpointWasOurs(previousBaseURL) {
		return nil, fmt.Errorf("Cline's %q provider is configured with an endpoint oaica did not write (%s), so it is yours: Cline has one provider under that name, and pointing it at the local daemon would rewrite its base URL and delete its API key. Remove or rename that provider in Cline's own settings, or launch a different integration", clineLaunchProvider, redactBaseURL(previousBaseURL))
	}

	settings["provider"] = clineLaunchProvider
	settings["model"] = modelID
	settings["baseUrl"] = baseURL
	if ep, ok := resolveLaunchTargetEndpoint(model.Name); ok {
		settings["apiKey"] = ep.Token
	} else {
		delete(settings, "apiKey")
	}
	provider["settings"] = settings

	if previousModel != modelID || previousBaseURL != baseURL || previousTokenSource != "manual" {
		provider["updatedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
	} else if _, ok := provider["updatedAt"].(string); !ok {
		provider["updatedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	provider["tokenSource"] = "manual"
	providers[clineLaunchProvider] = provider

	config["version"] = float64(1)
	config["lastUsedProvider"] = clineLaunchProvider
	config["providers"] = providers

	return json.MarshalIndent(config, "", "  ")
}

// writeClineLegacyGlobalState publishes globalState.json from config, which its
// caller (Cline.Edit) read under this store's lock — the same arrangement as
// writeClineProvidersConfig above, for the same reason.
func writeClineLegacyGlobalState(configPath string, config map[string]any, model LaunchModel) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}
	data, err := clineLegacyGlobalStateData(config, model)
	if err != nil {
		return err
	}
	return fileutil.WriteWithBackup(configPath, data, "cline")
}

// clineLegacyGlobalStateData is globalState.json as a write of model leaves it,
// in memory — the second half of the pair that Cline.Edit publishes with
// clineProvidersConfigData (see its comment).
func clineLegacyGlobalStateData(config map[string]any, model LaunchModel) ([]byte, error) {
	// Before anything is mutated: the same refusal writeClineProvidersConfig
	// makes, for the same reason. It ALSO runs a frame up, in Cline.Edit's read
	// phase, because this writer is the second of the pair: a launch refused
	// here would already have published providers.json. Same helper, so the two
	// call sites cannot drift (2026-09-27 audit, round 26).
	if err := clineRefuseForeignLegacyEndpoint(config); err != nil {
		return nil, err
	}

	// The same two branches the providers.json write above takes: for a
	// user-remote model the state has to name the remote and the model id the
	// remote knows, or Cline reads it as a daemon model (2026-09-26 audit).
	baseURL := clineLegacyBaseURLFor(model.Name)
	modelID := clineWriteModelID(model)
	config["ollamaBaseUrl"] = baseURL
	config["actModeApiProvider"] = clineLaunchProvider
	config["actModeOllamaModelId"] = modelID
	config["actModeOllamaBaseUrl"] = baseURL
	config["planModeApiProvider"] = clineLaunchProvider
	config["planModeOllamaModelId"] = modelID
	config["planModeOllamaBaseUrl"] = baseURL

	config["welcomeViewCompleted"] = true

	return json.MarshalIndent(config, "", "  ")
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

// clineStoreKey names one model as one of Cline's documents records it: the
// model id the entry holds, then the endpoint recorded beside it. The endpoint
// is half the identity — Cline's provider settings dial a base URL, so an entry
// keeping the daemon's URL for a model only a remote serves is a different
// model — and a key built from the id alone cannot tell them apart
// (2026-09-27 audit, round 27, F4). Joined with NUL: an id cannot contain one,
// so no two different entries build one key.
func clineStoreKey(modelID, baseURL string) string {
	return modelID + "\x00" + strings.TrimRight(baseURL, "/")
}

// clineProviderStoreKey is clineStoreKey for the providers.json half, which
// carries one field the legacy half does not: settings.apiKey, the credential
// the write sets from the live remote token (and deletes when the model has
// none). The declaration read the id and the endpoint only, so a store holding
// the key a REMOTE had before it was rotated away read as current — the launch
// skipped the write that would have replaced it, and Cline went on calling the
// remote with the old credential. Cline passes neither a key nor an endpoint to
// its child, so that file is the only credential it has for this remote
// (2026-09-27 audit, round 29, A-F1). Joined with NUL for the same reason as
// clineStoreKey: no id, endpoint or token contains one, so two different
// entries cannot build one key.
func clineProviderStoreKey(modelID, baseURL, apiKey string) string {
	return clineStoreKey(modelID, baseURL) + "\x00" + apiKey
}

// clineProviderTokenFor is the credential a write of this model leaves in
// settings.apiKey: the resolved remote's token, or "" (the field is deleted)
// for a model the daemon serves. The writer's own source, so the two cannot
// drift.
func clineProviderTokenFor(model string) string {
	if ep, ok := resolveLaunchTargetEndpoint(model); ok {
		return ep.Token
	}
	return ""
}

// clineHeldStoreKeys is the key of each half of Cline's pair as the documents
// hold it now, in the order Edit publishes them (providers.json, then
// globalState.json). A half that is absent, that names another provider, or
// that the provider settings are not currently using contributes no key: those
// are states in which the store does not hold this integration's selection, and
// the write that would publish it is what the caller is asking about.
func clineHeldStoreKeys(home string) []string {
	var keys []string
	if config, err := fileutil.ReadJSON(clineProvidersPath(home)); err == nil {
		if config["lastUsedProvider"] == clineLaunchProvider {
			providers, _ := config["providers"].(map[string]any)
			provider, _ := providers[clineLaunchProvider].(map[string]any)
			settings, _ := provider["settings"].(map[string]any)
			modelID, _ := settings["model"].(string)
			baseURL, _ := settings["baseUrl"].(string)
			apiKey, _ := settings["apiKey"].(string)
			if modelID != "" {
				keys = append(keys, clineProviderStoreKey(modelID, baseURL, apiKey))
			}
		}
	}
	if config, err := fileutil.ReadJSON(clineLegacyGlobalStatePath(home)); err == nil {
		if legacy, ok := clineLegacyHeldKeys(config); ok {
			keys = append(keys, legacy...)
		}
	}
	return keys
}

// clineLegacyHeldKeys is the projection of globalState.json a write leaves: the
// root endpoint, and the (id, endpoint) pair of EACH mode. The writer sets all
// four from the one model a launch resolves — Cline's own UI is where plan and
// act come apart, which is exactly why a half naming another model has to be
// visible here: reading the act pair alone let a store whose plan mode still
// ran the user's other model read as the state a write would leave, and the
// write that would have published this launch's model was skipped
// (2026-09-27 audit, round 32, A-F3). The root address is the third copy of the
// endpoint in this document, written from the same value
// (clineRefuseForeignLegacyEndpoint treats all three as one).
//
// ok is false for a document no write of this package leaves (a mode naming
// another provider, no model, no root): it contributes no key, and the
// caller's length check then reads it as drift.
func clineLegacyHeldKeys(config map[string]any) ([]string, bool) {
	rootURL, _ := config["ollamaBaseUrl"].(string)
	if rootURL == "" {
		return nil, false
	}
	keys := []string{clineRootStoreKey(rootURL)}
	for _, mode := range []string{"act", "plan"} {
		if config[mode+"ModeApiProvider"] != clineLaunchProvider {
			return nil, false
		}
		modelID, _ := config[mode+"ModeOllamaModelId"].(string)
		baseURL, _ := config[mode+"ModeOllamaBaseUrl"].(string)
		if modelID == "" {
			return nil, false
		}
		keys = append(keys, clineStoreKey(modelID, baseURL))
	}
	return keys, true
}

// clineRootStoreKey is the root endpoint's key. The empty id cannot collide
// with a model: every key clineStoreKey builds for a model carries a non-empty
// id.
func clineRootStoreKey(baseURL string) string { return clineStoreKey("", baseURL) }

// DeclaresSelection reports whether BOTH halves of Cline's pair already hold
// what a write of models would leave. Cline keeps one model (it is a narrowing
// editor) in two documents, and the pair is the selection: one half holding it
// is not the configuration the launch would publish, so the keys are compared
// as a whole list rather than one document at a time.
//
// The keys carry the endpoint as well as the id, which is what makes this
// answer about the store rather than about a name: a half holding the LOCAL
// model "gpt-oss" beside the daemon's URL, when the launch picked the cloud row
// of that name and would write "gpt-oss:cloud", used to read as current — the
// launch left Cline asking the daemon for a model this launch did not choose
// (2026-09-27 audit, round 27, F2), and a half holding a remote's id beside the
// daemon's URL did the same for a remote model (F4). The providers half carries
// the credential as well (round 29, A-F1).
func (c *Cline) DeclaresSelection(models []LaunchModel) bool {
	if len(models) != 1 {
		// Cline's stores hold one model; anything else is not a state they can
		// declare (NarrowToStoredModels narrows first, so this is the shape a
		// caller that skipped that would hit).
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	row := models[0]
	id := clineWriteModelID(row)
	want := []string{
		// The providers half carries the credential the write would leave there
		// (clineProviderStoreKey), so a store holding another one — a remote key
		// rotated away, or a key the write would delete — is drift. The legacy
		// half states no credential at all.
		clineProviderStoreKey(id, clineProviderBaseURLFor(row.Name), clineProviderTokenFor(row.Name)),
		// The legacy half: the root endpoint, then one pair per mode — both
		// written from this one model (clineLegacyHeldKeys).
		clineRootStoreKey(clineLegacyBaseURLFor(row.Name)),
		clineStoreKey(id, clineLegacyBaseURLFor(row.Name)),
		clineStoreKey(id, clineLegacyBaseURLFor(row.Name)),
	}
	held := clineHeldStoreKeys(home)
	if len(held) != len(want) {
		return false
	}
	return sameStoreStrings(held, want)
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
// name), so liveConfigMatches (sameModelSelection(editor.Models(), models),
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
