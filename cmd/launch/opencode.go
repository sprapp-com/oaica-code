package launch

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/ollama/ollama/cmd/internal/fileutil"
	"github.com/ollama/ollama/envconfig"
)

const openCodeInstallScript = "curl -fsSL https://opencode.ai/install | bash"

// openCodeMaxRecentModels is how many entries opencode's `recent` list keeps:
// Edit prepends this integration's pairs and truncates the whole list to this
// length, so it is exactly the number of selected models the store can hold.
const openCodeMaxRecentModels = 10

var openCodeGOOS = runtime.GOOS

// OpenCode implements Runner and Editor for OpenCode integration.
// Config is passed via OPENCODE_CONFIG_CONTENT env var at launch time
// instead of writing to opencode's config files.
type OpenCode struct {
	configContent string // JSON config built by Edit, passed to Run via env var
}

func (o *OpenCode) String() string { return "OpenCode" }

// findOpenCode returns the opencode binary path, checking PATH first then the
// curl installer location (~/.opencode/bin) which may not be on PATH yet.
func findOpenCode() (string, bool) {
	if p, err := exec.LookPath("opencode"); err == nil {
		return p, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	name := "opencode"
	if openCodeGOOS == "windows" {
		name = "opencode.exe"
	}
	fallback := filepath.Join(home, ".opencode", "bin", name)
	if _, err := os.Stat(fallback); err == nil {
		return fallback, true
	}
	return "", false
}

func (o *OpenCode) Run(model string, models []LaunchModel, args []string) error {
	forceTools, args := extractForceTools(args)
	if err := gateOpenAITools(model, forceTools); err != nil {
		return err
	}

	opencodePath, err := ensureOpenCodeInstalled()
	if err != nil {
		return err
	}

	cmd := exec.Command(opencodePath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if content := o.resolveContent(model, models); content != "" {
		cmd.Env = append(cmd.Env, "OPENCODE_CONFIG_CONTENT="+content)
	}
	return runChild(cmd)
}

func ensureOpenCodeInstalled() (string, error) {
	if opencodePath, ok := findOpenCode(); ok {
		return opencodePath, nil
	}

	if err := checkOpenCodeInstallerDependencies(); err != nil {
		return "", err
	}

	ok, err := ConfirmPrompt("OpenCode is not installed. Install now?")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("opencode installation cancelled")
	}

	bin, args, err := openCodeInstallerCommand(openCodeGOOS)
	if err != nil {
		return "", err
	}
	if openCodeGOOS != "windows" && len(args) > 0 {
		// The unix plan runs the verified download itself (it is the command's
		// only argument), so this is the only place it can be removed — claude,
		// kimi and qwen all do the same. Without it every failed install left
		// one copy of a downloaded script in the user's temp dir (2026-09-27
		// audit, round 18).
		defer os.Remove(args[0])
	}

	fmt.Fprintf(os.Stderr, "\nInstalling OpenCode...\n")
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to install opencode: %w", err)
	}

	opencodePath, ok := findOpenCode()
	if !ok {
		return "", fmt.Errorf("opencode was installed but the binary was not found on PATH\n\nYou may need to restart your shell")
	}

	fmt.Fprintf(os.Stderr, "%sOpenCode installed successfully%s\n\n", ansiGreen, ansiReset)
	return opencodePath, nil
}

func checkOpenCodeInstallerDependencies() error {
	switch openCodeGOOS {
	case "windows":
		if _, err := exec.LookPath("npm"); err != nil {
			return fmt.Errorf("opencode is not installed and required dependencies are missing\n\nInstall the following first:\n  npm (Node.js): https://nodejs.org/\n\nThen re-run:\n  oaica launch opencode")
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
			return fmt.Errorf("opencode is not installed and required dependencies are missing\n\nInstall the following first:\n  %s\n\nThen re-run:\n  oaica launch opencode", strings.Join(missing, "\n  "))
		}
	}
	return nil
}

func openCodeInstallerCommand(goos string) (string, []string, error) {
	switch goos {
	case "windows":
		return "npm", []string{"install", "-g", "opencode-ai@latest"}, nil
	case "darwin", "linux":
		path, err := fetchInstallerScriptFn("https://opencode.ai/install")
		if err != nil {
			return "", nil, err
		}
		return "bash", []string{path}, nil
	default:
		return "", nil, fmt.Errorf("unsupported platform for opencode install: %s", goos)
	}
}

// resolveContent returns the inline config to send via OPENCODE_CONFIG_CONTENT.
// Returns content built by Edit if available, otherwise builds from model.json
// with the requested model as primary (e.g. re-launch with saved config).
func (o *OpenCode) resolveContent(model string, models []LaunchModel) string {
	if o.configContent != "" {
		return o.configContent
	}
	resolvedModels := resolveOpenCodeRunModels(model, models, readModelJSONModels())
	if len(resolvedModels) == 0 {
		return ""
	}
	content, err := buildInlineConfig(resolvedModels[0], resolvedModels)
	if err != nil {
		return ""
	}
	return content
}

func resolveOpenCodeRunModels(primary string, models []LaunchModel, stateModels []string) []LaunchModel {
	if primary == "" {
		return nil
	}

	resolved := make([]LaunchModel, 0, 1+len(models)+len(stateModels))
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
	for _, model := range stateModels {
		appendModel(model)
	}
	return resolved
}

func hasLaunchModel(models []LaunchModel, name string) bool {
	for _, model := range models {
		if launchModelMatches(model.Name, name) || launchModelMatches(name, model.Name) {
			return true
		}
	}
	return false
}

// NarrowToStoredModels implements narrowingEditor. opencode's state keeps at
// most openCodeMaxRecentModels entries in `recent` — Edit truncates the list
// after prepending this launch's pairs — so a longer selection can never be
// declared: the declaration asked for every one of them at the head of the
// list, read the store as drift forever, and the models past the tenth were
// absent from opencode's picker without a word (2026-09-27 audit, round 29,
// A-F3). The truncation keeps the first of the pairs, so what the store holds
// is the first openCodeMaxRecentModels of the selection.
func (o *OpenCode) NarrowToStoredModels(models []string) ([]string, []string) {
	if len(models) <= openCodeMaxRecentModels {
		return models, nil
	}
	return models[:openCodeMaxRecentModels], append([]string(nil), models[openCodeMaxRecentModels:]...)
}

func (o *OpenCode) Paths() []string {
	sp, err := openCodeStatePath()
	if err != nil {
		return nil
	}
	if _, err := os.Stat(sp); err == nil {
		return []string{sp}
	}
	return nil
}

// openCodeStatePath returns the path to opencode's model state file.
// TODO: this hardcodes the Linux/macOS XDG path. On Windows, opencode stores
// state under %LOCALAPPDATA% (or similar) — verify and branch on runtime.GOOS.
func openCodeStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "opencode", "model.json"), nil
}

func (o *OpenCode) Edit(models []LaunchModel) error {
	modelList := launchModelNames(models)
	if len(modelList) == 0 {
		return nil
	}

	// The state entries must name the block that DECLARES the model, and the
	// id it is declared under — the picker name is not that id for a user
	// remote, and "ollama" is not the daemon's block once a remote claims the
	// name. OpenCode.Edit used to write a hardcoded "ollama"/<picker name>,
	// blind to buildInlineConfig's partition (2026-09-26 audit).
	daemonID := opencodeDaemonProviderID(models)
	pairs := make([][2]string, 0, len(models))
	pairSet := make(map[[2]string]bool, len(models))
	for _, m := range models {
		pid, mid := opencodeProviderFor(m, daemonID)
		p := [2]string{pid, mid}
		pairs = append(pairs, p)
		pairSet[p] = true
	}

	content, err := buildInlineConfig(models[0], models)
	if err != nil {
		return err
	}
	o.configContent = content

	// Write model state file so models appear in OpenCode's model picker
	statePath, err := openCodeStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return err
	}

	// Load-mutate-save over a store oaica does not own, under oaica's own lock
	// (foreignStoreLockBase): two `oaica launch opencode` commands that overlap
	// each read this state, prepend the models of their own launch and write the
	// whole document, so the rename that lands last publishes its list over the
	// other's and that launch's models are gone from opencode's picker while
	// both launches look fine (2026-09-26 audit, eleventh round). The read sits
	// inside the lock, so it is a load of the newest document rather than of the
	// one this process saw on the way in. The lock is oaica's: opencode's own
	// writes to this file are not serialised by it.
	return fileutil.WithFileLock(foreignStoreLockBase(statePath), func() error {
		state := map[string]any{
			"recent":   []any{},
			"favorite": []any{},
			"variant":  map[string]any{},
		}
		if data, err := os.ReadFile(statePath); err == nil {
			// The defaults above are the shape of an absent file, not a licence to
			// overwrite an unreadable one: oaica writes `recent` only, and would
			// drop `favorite`/`variant` along with anything else it does not model.
			// decodeJSONObject, not a bare Decode: a document that IS `null`
			// decodes into a nil map, which the write below then panics on
			// (2026-09-27 audit, round 19).
			doc, derr := decodeJSONObject(data)
			if derr != nil {
				return fmt.Errorf("refusing to update %s: it is not valid JSON (%v) — oaica rewrites only the recent-models list, so rewriting a file it cannot read would delete the rest of your picker state", statePath, derr)
			}
			state = doc
		}

		recent, _ := state["recent"].([]any)

		modelSet := make(map[string]bool)
		for _, m := range modelList {
			modelSet[m] = true
		}

		// Filter out the models we're about to re-add: the exact pairs below, plus
		// any entry naming a daemon block (either spelling) by one of these
		// models' picker names — the shape an earlier config wrote before the
		// daemon id was renamed.
		newRecent := slices.DeleteFunc(slices.Clone(recent), func(entry any) bool {
			e, ok := entry.(map[string]any)
			if !ok {
				return false
			}
			pid, _ := e["providerID"].(string)
			modelID, _ := e["modelID"].(string)
			if pairSet[[2]string{pid, modelID}] {
				return true
			}
			if opencodeIsDaemonProviderID(pid) {
				return modelSet[modelID]
			}
			return false
		})

		// Prepend models in reverse order so first model ends up first
		for i := len(pairs) - 1; i >= 0; i-- {
			newRecent = slices.Insert(newRecent, 0, any(map[string]any{
				"providerID": pairs[i][0],
				"modelID":    pairs[i][1],
			}))
		}

		newRecent = newRecent[:min(len(newRecent), openCodeMaxRecentModels)]

		state["recent"] = newRecent

		stateData, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return err
		}
		return fileutil.WriteWithBackup(statePath, stateData, "opencode")
	})
}

// Models reports the models opencode's own state says a block oaica writes
// declares — the daemon block or a configured user remote (see
// opencodeProviderFor: those are the two partitions OpenCode.Edit writes).
// Entries naming any other provider are the history of the user's other
// opencode clients and are not this integration's to report.
//
// It answered nil, which is the one answer the launcher's drift term can never
// match (sameModelSelection fails on length), so every opencode launch
// re-resolved the inventory and rewrote a state file that had not changed —
// the failure every other integration's Models() was fixed for (2026-09-27
// audit, round 26).
//
// The ids are reported AS STORED, not translated to picker names: the stored
// id is what the writers embed (launchModelWriteID for a daemon row, the
// remote's own upstream id for a remote row), and the launcher compares its
// saved selection in that same vocabulary.
func (o *OpenCode) Models() []string {
	return opencodeStateModelIDs(opencodeIsOurProviderBlock)
}

// opencodeIsOurProviderBlock reports whether a provider id in opencode's state
// is one this integration writes: a daemon block (any spelling
// opencodeDaemonProviderID can produce) or a configured user remote's name —
// the block buildInlineConfig gives that remote's models.
func opencodeIsOurProviderBlock(pid string) bool {
	if opencodeIsDaemonProviderID(pid) {
		return true
	}
	if pid == "" {
		return false
	}
	// A remote's block id is the remote's name (opencodeProviderFor →
	// resolveRemoteEndpoint), so the name is looked up as a remote, not as a
	// "<remote>/<model>" picker name.
	remotes, err := loadUserRemotes()
	if err != nil {
		remotes = builtinRemotes()
	}
	for _, r := range remotes {
		if r.Name == pid {
			return true
		}
	}
	return false
}

// opencodeModelID is the model id opencode expects for a picker model: the
// daemon-side id for an ollama-cloud catalogue row (LaunchModel.Upstream), the
// bare upstream id for a user-remote model (the remote's
// /v1/chat/completions knows it as that, not the namespaced picker name),
// otherwise the full picker name (local/cloud, which opencode sends to the
// daemon as-is).
func opencodeModelID(m LaunchModel) string {
	return launchModelWriteID(m)
}

// opencodeDaemonProviderID is the provider-block id of the local daemon:
// "ollama", renamed when a user remote claims that name — see
// buildInlineConfig's comment on why two groups under one id must not happen.
// The config partition and the state file both answer to it.
//
// The replacement must itself be unclaimed: "ollama-local" is a legal remote
// name, and stopping at it re-created the very collision this function exists
// to avoid — with remotes named both "ollama" and "ollama-local", the daemon
// adopted the second remote's id, and the two groups merged under one block.
// Whichever was added first owned it, so either the daemon's models were
// declared under a third-party base URL with that remote's credential, or the
// remote's models were pointed at the daemon (2026-09-26 audit, round 16).
// Iterated instead, so the ordinary config — no remote named "ollama" at all —
// still gets the unchanged "ollama" and a config with only that one collision
// still gets "ollama-local".
func opencodeDaemonProviderID(models []LaunchModel) string {
	claimed := make(map[string]bool, len(models))
	for _, m := range models {
		if ep, ok := resolveRemoteEndpoint(m.Name); ok {
			claimed[ep.Name] = true
		}
	}
	if !claimed["ollama"] {
		return "ollama"
	}
	for i := 1; ; i++ {
		id := "ollama-local"
		if i > 1 {
			id = fmt.Sprintf("ollama-local-%d", i)
		}
		if !claimed[id] {
			return id
		}
	}
}

// opencodeIsDaemonProviderID reports whether a provider id is one the daemon
// block has used: "ollama", or the "ollama-local"/"ollama-local-N" renames
// opencodeDaemonProviderID falls back to. The two spellings were hard-coded at
// the two call sites, so a launch that had renamed the block twice — a remote
// literally named "ollama" AND one named "ollama-local" — left rows under
// "ollama-local-2" that no launch would clean up: they stayed in opencode's
// picker after the model was gone, and readModelJSONModels did not see the
// daemon's own rows, so the launch rewrote the state file on every run
// (2026-09-27 audit, round 21, F13).
func opencodeIsDaemonProviderID(pid string) bool {
	rest, ok := strings.CutPrefix(pid, "ollama")
	if !ok {
		return false
	}
	if rest == "" || rest == "-local" {
		return true
	}
	digits, ok := strings.CutPrefix(rest, "-local-")
	if !ok || digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// opencodeProviderFor reports which provider block declares m and the model id
// it is declared under — the same partition buildInlineConfig writes, so the
// state file's "<providerID>/<modelID>" entries name a block that exists and
// declares that id (a picker name is not a model id for a user remote: the
// remote's /v1 knows the bare upstream id).
func opencodeProviderFor(m LaunchModel, daemonID string) (providerID, modelID string) {
	if ep, ok := resolveRemoteEndpoint(m.Name); ok {
		return ep.Name, ep.UpstreamModel
	}
	// An ollama-cloud catalogue row is declared under the daemon's block (only
	// a user remote gets a block of its own), under the id the daemon serves it
	// as — the row's own Name is a display label (2026-09-27 audit, round 25).
	return daemonID, launchModelWriteID(m)
}

// buildInlineConfig produces the JSON string for OPENCODE_CONFIG_CONTENT.
// primary is the model to launch with, models is the full list of available models.
//
// Models are partitioned by endpoint into one provider block each: a daemon
// "ollama" provider for local/cloud models (byte-identical to the previous
// single-provider config), plus one named provider per user remote pointing
// directly at that remote's /v1 (or /v4) base. This lets an OpenAI-native
// remote (kat-coder) be reached by opencode directly while local models keep
// routing through the daemon.
func buildInlineConfig(primary LaunchModel, models []LaunchModel) (string, error) {
	if primary.Name == "" || len(models) == 0 {
		return "", fmt.Errorf("buildInlineConfig: primary and models are required")
	}

	type providerGroup struct {
		id      string
		name    string
		baseURL string
		apiKey  string
		models  []LaunchModel
	}
	// The daemon block's id is "ollama", which is also a legal USER REMOTE
	// name (`oaica remote add ollama --base-url ...`). Two groups under one id
	// are merged by byID, and whichever is added first owns the block: the
	// other backend's models were then declared under its base URL and
	// credential — local traffic exported to that remote, or the remote's
	// model pointed at the daemon (2026-09-26 audit). The rename applies only
	// when such a remote is actually present, so the ordinary config stays
	// byte-identical.
	localID := opencodeDaemonProviderID(models)

	var groups []*providerGroup
	byID := map[string]*providerGroup{}
	add := func(m LaunchModel, id, name, baseURL, apiKey string) {
		g := byID[id]
		if g == nil {
			g = &providerGroup{id: id, name: name, baseURL: baseURL, apiKey: apiKey}
			byID[id] = g
			groups = append(groups, g)
		}
		g.models = append(g.models, m)
	}
	for _, m := range models {
		if ep, ok := resolveRemoteEndpoint(m.Name); ok {
			add(m, ep.Name, ep.Name, ep.BaseURL, ep.Token)
		} else {
			add(m, localID, "Ollama", envconfig.ConnectableHost().String()+"/v1", "")
		}
	}

	providers := make(map[string]any, len(groups))
	for _, g := range groups {
		p := map[string]any{
			"npm":    "@ai-sdk/openai-compatible",
			"name":   g.name,
			"models": buildModelEntries(g.models),
		}
		options := map[string]any{"baseURL": g.baseURL}
		if g.apiKey != "" {
			options["apiKey"] = g.apiKey
		}
		p["options"] = options
		providers[g.id] = p
	}

	// Top-level model: "<providerId>/<modelId>".
	primaryProvider, primaryID := localID, opencodeModelID(primary)
	if ep, ok := resolveRemoteEndpoint(primary.Name); ok {
		primaryProvider = ep.Name
	}

	config := map[string]any{
		"$schema":  "https://opencode.ai/config.json",
		"provider": providers,
		"model":    primaryProvider + "/" + primaryID,
	}
	data, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// readModelJSONModels reads ollama model IDs from the opencode model.json state file
func readModelJSONModels() []string {
	// Any id the daemon block has used (see opencodeDaemonProviderID).
	return opencodeStateModelIDs(opencodeIsDaemonProviderID)
}

// opencodeStateEntries returns every entry of opencode's model state as its
// (provider block, model id) pair, in the order the state lists them — the one
// scan of this file, so the ids Edit writes and the ids a reader reports cannot
// drift apart. An entry missing either half is not an entry this file can read
// and is skipped.
func opencodeStateEntries() [][2]string {
	statePath, err := openCodeStatePath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		return nil
	}
	recent, _ := state["recent"].([]any)
	var entries [][2]string
	for _, entry := range recent {
		e, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		pid, _ := e["providerID"].(string)
		id, ok := e["modelID"].(string)
		if !ok || id == "" {
			continue
		}
		entries = append(entries, [2]string{pid, id})
	}
	return entries
}

// opencodeStateModelIDs returns the model id of every entry in opencode's model
// state whose provider block pred accepts, in the order the state lists them.
func opencodeStateModelIDs(pred func(string) bool) []string {
	var models []string
	for _, entry := range opencodeStateEntries() {
		if pred(entry[0]) {
			models = append(models, entry[1])
		}
	}
	return models
}

// opencodeStoreKey names one model the way opencode's state holds it: the
// provider BLOCK that declares it, then the model id under it. The block is the
// endpoint, so the daemon's "llama3.2" and a remote's are different keys and
// two different models. NUL-joins them because a block name and a model id both
// come from config and neither can contain NUL, so no pair of different entries
// can build one key.
func opencodeStoreKey(providerID, modelID string) string {
	return providerID + "\x00" + modelID
}

// opencodeHeldStoreKeys is the key of every entry of opencode's state, in state
// order — a foreign provider's block included.
//
// The entries are NOT filtered down to the blocks this integration writes. The
// key already names the block, so a foreign entry can never equal a key a
// launch would write; dropping it cost nothing and bought a correctness bug.
// opencode resolves the FIRST entry of `recent`, so a state whose head is
// another provider's model does not hold this selection even when our pair sits
// behind it — while Edit, which prepends, would have moved our pair in front.
// The filtered list read that store as current and the launch was skipped
// (2026-09-27 audit, round 28, F2).
func opencodeHeldStoreKeys() []string {
	entries := opencodeStateEntries()
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		keys = append(keys, opencodeStoreKey(entry[0], entry[1]))
	}
	return keys
}

// DeclaresSelection reports whether opencode's state already holds what a write
// of models would leave: the same (provider block, model id) pairs, in order,
// at the head of the recent list. Entries after them are the picker's history,
// which Edit leaves in place (its DeleteFunc removes only the pairs it is about
// to write), so they are not drift.
//
// The pairs are compared as pairs and not as ids: a model id alone cannot tell
// the daemon's block from a remote's, so a state keeping the remote's
// attribution while this launch would move the model to the daemon read as
// current and the launch left opencode dialling the remote (2026-09-27 audit,
// round 27, F3).
func (o *OpenCode) DeclaresSelection(models []LaunchModel) bool {
	if len(models) == 0 {
		return false
	}
	daemonID := opencodeDaemonProviderID(models)
	want := make([]string, 0, len(models))
	for _, m := range models {
		pid, mid := opencodeProviderFor(m, daemonID)
		want = append(want, opencodeStoreKey(pid, mid))
	}
	return declaresPrefix(opencodeHeldStoreKeys(), want)
}

func buildModelEntries(modelList []LaunchModel) map[string]any {
	models := make(map[string]any)
	for _, model := range modelList {
		id := opencodeModelID(model)
		entry := map[string]any{
			"name": id,
		}
		if model.HasCapability("vision") {
			entry["modalities"] = map[string]any{
				"input":  []string{"text", "image"},
				"output": []string{"text"},
			}
		}
		if model.HasCapability("thinking") {
			entry["reasoning"] = true
			if openCodeModelSupportsThinkingLevels(model) {
				entry["options"] = map[string]any{"reasoningEffort": "medium"}
				entry["variants"] = map[string]any{
					"low":    map[string]any{"reasoningEffort": "low"},
					"medium": map[string]any{"reasoningEffort": "medium"},
					"high":   map[string]any{"reasoningEffort": "high"},
					"max":    map[string]any{"reasoningEffort": "max"},
				}
			} else {
				entry["variants"] = map[string]any{
					"none":   map[string]any{"reasoningEffort": "none"},
					"low":    map[string]any{"disabled": true},
					"medium": map[string]any{"disabled": true},
					"high":   map[string]any{"disabled": true},
				}
			}
		}
		if model.MaxOutputTokens > 0 {
			limit := make(map[string]any)
			if model.ContextLength > 0 {
				limit["context"] = model.ContextLength
			}
			limit["output"] = model.MaxOutputTokens
			entry["limit"] = limit
		}
		models[id] = entry
	}
	return models
}

func openCodeModelSupportsThinkingLevels(model LaunchModel) bool {
	for _, family := range append([]string{model.Details.Family}, model.Details.Families...) {
		if normalizeOpenCodeModelFamily(family) == "gptoss" {
			return true
		}
	}

	return strings.Contains(normalizeOpenCodeModelFamily(model.Name), "gptoss")
}

func normalizeOpenCodeModelFamily(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	return s
}
