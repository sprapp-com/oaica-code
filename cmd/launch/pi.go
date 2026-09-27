package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
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

// Pi implements Runner and Editor for Pi (Pi Coding Agent) integration
type Pi struct{}

const (
	piNpmPackage       = "@earendil-works/pi-coding-agent"
	piLegacyNpmPackage = "@mariozechner/pi-coding-agent"
	piWebSearchSource  = "npm:@ollama/pi-web-search"
	piWebSearchPkg     = "@ollama/pi-web-search"
)

func (p *Pi) String() string { return "Pi" }

var npmRegistryBaseURL = "https://registry.npmjs.org"

func (p *Pi) Run(_ string, _ []LaunchModel, args []string) error {
	fmt.Fprintf(os.Stderr, "\n%sPreparing Pi...%s\n", ansiGray, ansiReset)
	if err := ensureNpmInstalled(); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "%sChecking Pi installation...%s\n", ansiGray, ansiReset)
	bin, err := ensurePiInstalled()
	if err != nil {
		return err
	}

	ensurePiWebSearchPackage(bin)

	fmt.Fprintf(os.Stderr, "\n%sLaunching Pi...%s\n\n", ansiGray, ansiReset)

	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func ensureNpmInstalled() error {
	if _, err := exec.LookPath("npm"); err != nil {
		return fmt.Errorf("npm (Node.js) is required to launch pi\n\nInstall it first:\n  https://nodejs.org/\n\nThen re-run:\n  oaica launch pi")
	}
	return nil
}

// confirmPiNpmMutation is the consent gate every npm write in this file passes
// through. ENTERPRISE.md's network table (row 6) tells the user "none of these
// installers run unprompted" — a promise only the final install branch kept:
// the legacy-package migration and the "official package present, `pi` not on
// PATH" reinstall both reached `npm install -g` with no prompt at all, so
// declining a prompt the user was never shown could not keep npm out
// (2026-09-26 audit). The other npm agents (cline, dsh, openclaw) already
// prompt; pi's two extra paths were the whole gap.
//
// A decline returns an error rather than a soft "carry on without the binary":
// `oaica launch pi` cannot run without pi, and falling through would print the
// misleading "pi was installed but the binary was not found on PATH".
func confirmPiNpmMutation(action string) error {
	ok, err := ConfirmPrompt(action + " with npm?")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("pi installation cancelled")
	}
	return nil
}

func ensurePiInstalled() (string, error) {
	if _, err := exec.LookPath("pi"); err == nil {
		install, pkgErr := installedPiPackageInfo()
		if pkgErr != nil {
			fmt.Fprintf(os.Stderr, "%sCould not verify which Pi package is installed: %v%s\n", ansiYellow, pkgErr, ansiReset)
			fmt.Fprintf(os.Stderr, "Pi will still launch. To switch to the official package manually:\n  npm uninstall -g %s\n  npm install -g %s\n\n", piLegacyNpmPackage, piNpmPackage)
			return "pi", nil
		}

		if install.packageName == piLegacyNpmPackage {
			if err := confirmPiNpmMutation("Update Pi"); err != nil {
				return "", err
			}
			fmt.Fprintf(os.Stderr, "%sUpdating Pi...%s\n", ansiGray, ansiReset)
			if err := migrateLegacyPiPackage(install.npmPrefix); err != nil {
				return "", err
			}
			if err := requirePiOnPath(); err != nil {
				return "", err
			}
		}
		return "pi", nil
	}

	if _, err := exec.LookPath("npm"); err != nil {
		return "", fmt.Errorf("pi is not installed and required dependencies are missing\n\nInstall the following first:\n  npm (Node.js): https://nodejs.org/\n\nThen re-run:\n  oaica launch pi")
	}

	install, pkgErr := installedPiPackageInfo()
	if pkgErr == nil && install.packageName == piLegacyNpmPackage {
		if err := confirmPiNpmMutation("Update Pi"); err != nil {
			return "", err
		}
		fmt.Fprintf(os.Stderr, "%sUpdating Pi...%s\n", ansiGray, ansiReset)
		if err := migrateLegacyPiPackage(install.npmPrefix); err != nil {
			return "", err
		}
		if err := requirePiOnPath(); err != nil {
			return "", err
		}
		return "pi", nil
	}
	if pkgErr == nil && install.packageName == piNpmPackage {
		if err := confirmPiNpmMutation("Reinstall Pi"); err != nil {
			return "", err
		}
		fmt.Fprintf(os.Stderr, "%sInstalling Pi...%s\n", ansiGray, ansiReset)
		if err := installPiPackageWithPrefix(install.npmPrefix); err != nil {
			return "", err
		}
		if err := requirePiOnPath(); err != nil {
			return "", err
		}
		return "pi", nil
	}

	if err := confirmPiNpmMutation("Install Pi"); err != nil {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "\nInstalling Pi...\n")
	if err := installPiPackage(); err != nil {
		return "", err
	}

	if err := requirePiOnPath(); err != nil {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "%sPi installed successfully%s\n\n", ansiGreen, ansiReset)
	return "pi", nil
}

func requirePiOnPath() error {
	if _, err := exec.LookPath("pi"); err != nil {
		return fmt.Errorf("pi was installed but the binary was not found on PATH\n\nYou may need to restart your shell")
	}
	return nil
}

func installPiPackage() error {
	return installPiPackageWithPrefix("")
}

func installPiPackageWithPrefix(prefix string) error {
	if err := runQuietCommand("npm", npmArgs(prefix, "install", "-g", piNpmPackage+"@latest")...); err != nil {
		return fmt.Errorf("failed to install pi: %w", err)
	}
	return nil
}

func migrateLegacyPiPackage(prefix string) error {
	if err := installPiPackageForced(prefix); err != nil {
		return err
	}

	installed, err := npmPackageInstalledWithPrefix(piNpmPackage, prefix)
	if err != nil {
		return fmt.Errorf("failed to verify official pi package: %w", err)
	}
	if !installed {
		return fmt.Errorf("failed to verify official pi package")
	}

	if err := uninstallLegacyPiPackageWithPrefix(prefix); err != nil {
		return err
	}
	return installPiPackageWithPrefix(prefix)
}

func installPiPackageForced(prefix string) error {
	if err := runQuietCommand("npm", npmArgs(prefix, "install", "-g", piNpmPackage+"@latest", "--force")...); err != nil {
		return fmt.Errorf("failed to install pi: %w", err)
	}
	return nil
}

func uninstallLegacyPiPackageWithPrefix(prefix string) error {
	if err := runQuietCommand("npm", npmArgs(prefix, "uninstall", "-g", piLegacyNpmPackage)...); err != nil {
		return fmt.Errorf("failed to remove legacy pi package: %w", err)
	}
	return nil
}

func runQuietCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, msg)
}

type piPackageInstall struct {
	packageName string
	npmPrefix   string
}

func installedPiPackageInfo() (piPackageInstall, error) {
	if _, err := exec.LookPath("npm"); err != nil {
		return piPackageInstall{}, err
	}

	if bin, err := exec.LookPath("pi"); err == nil {
		install, err := piPackageInstallFromBinary(bin)
		if err == nil && install.packageName != "" {
			return install, nil
		}
	}

	installed, err := npmPackageInstalled(piLegacyNpmPackage)
	if err != nil {
		return piPackageInstall{}, err
	}
	if installed {
		return piPackageInstall{packageName: piLegacyNpmPackage}, nil
	}

	installed, err = npmPackageInstalled(piNpmPackage)
	if err != nil {
		return piPackageInstall{}, err
	}
	if installed {
		return piPackageInstall{packageName: piNpmPackage}, nil
	}

	return piPackageInstall{}, nil
}

func piPackageInstallFromBinary(bin string) (piPackageInstall, error) {
	realPath, err := filepath.EvalSymlinks(bin)
	if err != nil {
		realPath = bin
	}

	dir := filepath.Dir(realPath)
	for {
		packageJSON := filepath.Join(dir, "package.json")
		data, err := os.ReadFile(packageJSON)
		if err == nil {
			var payload struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(data, &payload) == nil && (payload.Name == piLegacyNpmPackage || payload.Name == piNpmPackage) {
				return piPackageInstall{packageName: payload.Name, npmPrefix: npmPrefixForPackageRoot(dir)}, nil
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return piPackageInstall{}, nil
}

func npmPrefixForPackageRoot(packageRoot string) string {
	return npmPrefixForPackageRootForGOOS(filepath.Clean(packageRoot), runtime.GOOS, string(filepath.Separator))
}

func npmPrefixForPackageRootForGOOS(packageRoot, goos, separator string) string {
	packageRoot = strings.TrimRight(packageRoot, separator)
	nodeModules := separator + "node_modules" + separator
	idx := strings.LastIndex(packageRoot, nodeModules)
	if idx == -1 {
		return ""
	}

	rootDir := packageRoot[:idx]
	if pathBaseForSeparator(rootDir, separator) == "lib" {
		// Unix npm global root is <prefix>/lib/node_modules.
		return pathDirForSeparator(rootDir, separator)
	}
	if goos == "windows" {
		// Windows npm global root is usually <prefix>\node_modules.
		return rootDir
	}
	return ""
}

func pathBaseForSeparator(path, separator string) string {
	path = strings.TrimRight(path, separator)
	idx := strings.LastIndex(path, separator)
	if idx == -1 {
		return path
	}
	return path[idx+len(separator):]
}

func pathDirForSeparator(path, separator string) string {
	path = strings.TrimRight(path, separator)
	idx := strings.LastIndex(path, separator)
	if idx == -1 {
		return ""
	}
	if idx == 0 {
		return separator
	}
	return path[:idx]
}

func npmPackageInstalled(pkg string) (bool, error) {
	return npmPackageInstalledWithPrefix(pkg, "")
}

func npmPackageInstalledWithPrefix(pkg, prefix string) (bool, error) {
	cmd := exec.Command("npm", npmArgs(prefix, "ls", "-g", pkg, "--depth=0", "--json")...)
	out, err := cmd.Output()

	var payload struct {
		Dependencies map[string]json.RawMessage `json:"dependencies"`
	}

	if parseErr := json.Unmarshal(out, &payload); parseErr == nil {
		_, ok := payload.Dependencies[pkg]
		if ok {
			return true, nil
		}
		return false, nil
	}

	if err == nil {
		return false, nil
	}

	if exitErr, ok := err.(*exec.ExitError); ok {
		msg := strings.TrimSpace(string(exitErr.Stderr))
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		if msg == "" {
			return false, err
		}
		return false, fmt.Errorf("%w: %s", err, msg)
	}

	return false, err
}

func npmArgs(prefix string, args ...string) []string {
	if prefix == "" {
		return args
	}
	return append([]string{"--prefix", prefix}, args...)
}

func ensurePiWebSearchPackage(bin string) {
	if !shouldManageOllamaWebSearch() {
		fmt.Fprintf(os.Stderr, "%sCloud is disabled; skipping %s setup.%s\n", ansiGray, piWebSearchPkg, ansiReset)
		return
	}
	// PI_OFFLINE means "do not talk to the npm registry from here". It used to
	// gate only the version check, so the install path still reached npm (and,
	// when the package was missing, installed it) with offline mode set
	// (2026-09-26 audit).
	if piOfflineModeEnabled() {
		fmt.Fprintf(os.Stderr, "%sPI_OFFLINE is set; skipping %s setup.%s\n", ansiGray, piWebSearchPkg, ansiReset)
		return
	}

	fmt.Fprintf(os.Stderr, "%sChecking Pi web search package...%s\n", ansiGray, ansiReset)

	pkg, err := piPackageInfo(bin, piWebSearchSource)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s  Warning: could not check %s installation: %v%s\n", ansiYellow, piWebSearchPkg, err, ansiReset)
		return
	}

	if !pkg.installed {
		// Ask first, like every other integration (claude.go, kimi.go,
		// hermes.go, muse.go all prompt before installing). This one ran the
		// install unprompted, so `oaica launch pi` fetched and installed a
		// third-party npm package the user never agreed to (2026-09-26 audit).
		ok, err := ConfirmPrompt(fmt.Sprintf("Pi web search (%s) is not installed. Install with npm?", piWebSearchSource))
		if err != nil || !ok {
			fmt.Fprintf(os.Stderr, "%s  Skipping %s — install it yourself with `%s install %s` if you want it.%s\n",
				ansiGray, piWebSearchPkg, bin, piWebSearchSource, ansiReset)
			return
		}
		fmt.Fprintf(os.Stderr, "%sInstalling %s...%s\n", ansiGray, piWebSearchPkg, ansiReset)
		cmd := exec.Command(bin, "install", piWebSearchSource)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "%s  Warning: could not install %s: %v%s\n", ansiYellow, piWebSearchPkg, err, ansiReset)
			return
		}

		fmt.Fprintf(os.Stderr, "%s  ✓ Installed %s%s\n", ansiGreen, piWebSearchPkg, ansiReset)
		return
	}

	updateAvailable, err := piWebSearchUpdateAvailable(pkg.installedPath)
	if err != nil || !updateAvailable {
		return
	}

	// The update is asked for too: it is still an npm fetch and a rewrite of
	// the user's installed package, and a declined prompt must leave the
	// package alone.
	ok, err := ConfirmPrompt(fmt.Sprintf("A newer %s is available. Update it now?", piWebSearchSource))
	if err != nil || !ok {
		fmt.Fprintf(os.Stderr, "%s  Keeping the installed %s — update it yourself with `%s update %s`.%s\n",
			ansiGray, piWebSearchPkg, bin, piWebSearchSource, ansiReset)
		return
	}
	fmt.Fprintf(os.Stderr, "%sUpdating %s...%s\n", ansiGray, piWebSearchPkg, ansiReset)
	cmd := exec.Command(bin, "update", piWebSearchSource)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s  Warning: could not update %s: %v%s\n", ansiYellow, piWebSearchPkg, err, ansiReset)
		return
	}

	fmt.Fprintf(os.Stderr, "%s  ✓ Updated %s%s\n", ansiGreen, piWebSearchPkg, ansiReset)
}

func shouldManageOllamaWebSearch() bool {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return true
	}

	disabled, known := cloudStatusDisabled(context.Background(), client)
	if known && disabled {
		return false
	}
	return true
}

type piPackageListEntry struct {
	installed     bool
	installedPath string
}

func piPackageInfo(bin, source string) (piPackageListEntry, error) {
	cmd := exec.Command(bin, "list")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return piPackageListEntry{}, err
		}
		return piPackageListEntry{}, fmt.Errorf("%w: %s", err, msg)
	}

	lines := strings.Split(string(out), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, source) {
			return piPackageListEntry{installed: true, installedPath: piPackageListInstalledPath(lines[i+1:])}, nil
		}
	}

	return piPackageListEntry{}, nil
}

func piPackageListInstalledPath(lines []string) string {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "npm:") || strings.HasPrefix(trimmed, "git:") || strings.HasSuffix(trimmed, ":") {
			return ""
		}
		if filepath.IsAbs(trimmed) {
			return trimmed
		}
		return ""
	}
	return ""
}

func piWebSearchUpdateAvailable(installedPath string) (bool, error) {
	if piOfflineModeEnabled() || installedPath == "" {
		return false, nil
	}

	installedVersion, err := npmInstalledPackageVersion(installedPath)
	if err != nil || installedVersion == "" {
		return false, err
	}

	latestVersion, err := npmLatestPackageVersion(piWebSearchPkg)
	if err != nil || latestVersion == "" {
		return false, err
	}

	return latestVersion != installedVersion, nil
}

func piOfflineModeEnabled() bool {
	value := os.Getenv("PI_OFFLINE")
	return value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
}

func npmInstalledPackageVersion(installedPath string) (string, error) {
	data, err := os.ReadFile(filepath.Join(installedPath, "package.json"))
	if err != nil {
		return "", err
	}

	var payload struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", err
	}
	return payload.Version, nil
}

func npmLatestPackageVersion(pkg string) (string, error) {
	client := http.Client{Timeout: 10 * time.Second}
	requestURL := strings.TrimRight(npmRegistryBaseURL, "/") + "/" + url.PathEscape(pkg) + "/latest"
	resp, err := client.Get(requestURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("npm registry returned %s", resp.Status)
	}

	var payload struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	return payload.Version, nil
}

func (p *Pi) Paths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	var paths []string
	modelsPath := filepath.Join(home, ".pi", "agent", "models.json")
	if _, err := os.Stat(modelsPath); err == nil {
		paths = append(paths, modelsPath)
	}
	settingsPath := filepath.Join(home, ".pi", "agent", "settings.json")
	if _, err := os.Stat(settingsPath); err == nil {
		paths = append(paths, settingsPath)
	}
	return paths
}

// readPiJSONDocument reads one of Pi's JSON documents, of which oaica models
// only a few members. A document that cannot be read is returned as an error
// rather than treated as an empty one: the caller writes its map back whole,
// so an unparseable file would come back as the handful of keys oaica knows.
// Numbers decode as json.Number, which survives a value past 2^53 intact.
func readPiJSONDocument(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	// decodeJSONObject also covers the document that IS `null`: it decodes
	// into a nil map, which every caller below writes into (2026-09-27 audit,
	// round 19).
	doc, err := decodeJSONObject(data)
	if err != nil {
		return nil, fmt.Errorf("refusing to update %s: it is not valid JSON (%v) — oaica models only part of that file, so rewriting what it cannot read would delete the rest of your configuration", path, err)
	}
	return doc, nil
}

// piRejectMixedEndpoints refuses a selection Pi cannot express, before any of
// it is written.
//
// Pi's models.json gives oaica one provider slot and every model this package
// registers in it is served from that slot's single baseUrl with its single
// apiKey (piProviderBaseURL/piProviderKey). A selection that spans endpoints
// therefore declared the other endpoint's model under a URL that does not
// serve it and a credential that is not its own: a local model registered in a
// provider pointed at a third-party API, or a second remote's model declared
// under the first remote's URL and key — the launch then fails at the far end
// with a model-not-found the user cannot act on (2026-09-26 audit, round 16).
//
// Refusing is the answer the round-10 repoint and muse's settings both take,
// and the reason it is refusal rather than a second provider block is that the
// rest of this file reads ONE slot: Pi.Models() and piPickerNameFor answer
// from providers["ollama"] and its baseUrl, so a block oaica added beside it
// would be invisible to the picker — the model would look absent from the live
// config and be re-registered on every launch.
func piRejectMixedEndpoints(models []LaunchModel) error {
	sawDaemon := false
	remoteName, remoteModel := "", ""
	for _, m := range models {
		ep, ok := resolveRemoteEndpoint(m.Name)
		if !ok {
			sawDaemon = true
			continue
		}
		switch {
		case remoteName == "":
			remoteName, remoteModel = ep.Name, m.Name
		case ep.Name != remoteName:
			return fmt.Errorf("pi cannot launch the remote models %q and %q in one selection: Pi serves every oaica-registered model from a single provider endpoint and credential, so one of them would be declared under the other's endpoint and fail with model-not-found. Launch them in separate `oaica launch pi` runs", remoteModel, m.Name)
		}
	}
	if sawDaemon && remoteName != "" {
		return fmt.Errorf("pi cannot launch the remote model %q and a local model in one selection: Pi serves every oaica-registered model from a single provider endpoint and credential, so the local model would be declared under %s's endpoint and fail with model-not-found. Launch them in separate `oaica launch pi` runs", remoteModel, remoteName)
	}
	return nil
}

func (p *Pi) Edit(models []LaunchModel) error {
	if len(models) == 0 {
		return nil
	}

	// The selection as a whole has to be expressible in Pi's one provider slot
	// before any of it is written (piRejectMixedEndpoints).
	if err := piRejectMixedEndpoints(models); err != nil {
		return err
	}

	// Pi's primary model is configured via Edit, not passed to Run, so the
	// capability gate applies here. --force-tools is not threaded through Edit.
	if err := gateOpenAITools(models[0].Name, false); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	configPath := filepath.Join(home, ".pi", "agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}
	settingsPath := filepath.Join(home, ".pi", "agent", "settings.json")

	// The whole load-mutate-save of BOTH documents runs under their locks, one
	// lock per file (foreignStoreLockBase: the documents are Pi's, so the lock
	// is oaica's and no stray .lock appears in ~/.pi). Pi has one provider slot
	// and oaica rewrites the whole of both documents from one snapshot, so two
	// Edits that overlap each read the same snapshot, each writes its own
	// launch's models, and the rename that lands last publishes a document with
	// the other launch's changes deleted while both launches report success.
	// The other writer an entry can be lost to here is Pi itself — it owns
	// these files and oaica models only part of each one (2026-09-26 audit,
	// twelfth round).
	//
	// The reads below are INSIDE the locks, which is what makes them loads of
	// the newest documents rather than of the ones this process saw on the way
	// in. The two locks are taken in this order everywhere, so two oaica
	// processes cannot deadlock on them.
	return fileutil.WithFileLock(foreignStoreLockBase(configPath), func() error {
		return fileutil.WithFileLock(foreignStoreLockBase(settingsPath), func() error {
			return piEditDocuments(configPath, settingsPath, models)
		})
	})
}

// piEditDocuments is Pi.Edit's load → mutate → save, run by Edit with both of
// Pi's documents locked.
func piEditDocuments(configPath, settingsPath string, models []LaunchModel) error {
	config, err := readPiJSONDocument(configPath)
	if err != nil {
		return err
	}

	// Both documents are read before either is written: a refusal on the
	// second must not leave the first already rewritten, so the pair either
	// updates together or the files stay exactly as the user wrote them.
	settings, err := readPiJSONDocument(settingsPath)
	if err != nil {
		return err
	}

	providers, ok := config["providers"].(map[string]any)
	if !ok {
		providers = make(map[string]any)
	}

	ollama, ok := providers["ollama"].(map[string]any)
	if !ok {
		ollama = map[string]any{
			"baseUrl": piProviderBaseURL(models),
			"api":     piProviderAPI,
			"apiKey":  piProviderKey(models),
		}
	} else {
		// Pi has ONE provider slot, and every model this package registers is
		// served from its baseUrl with its apiKey — so the slot has to match
		// the models of THIS launch. It used to be written once and reused
		// thereafter: after a remote-backed launch the slot kept that remote's
		// URL and token, so a later local-model launch registered the daemon's
		// model in a provider pointed at someone else's API, with that
		// credential, and the run failed at the far end (2026-09-26 audit,
		// tenth round).
		//
		// A user who hand-configured this slot for their own endpoint is told,
		// not silently rewritten: leaving it alone still adds this launch's
		// models to the slot's list, so a launch that cannot repoint says so.
		//
		// Only a slot THIS package wrote is repointed. piProviderAPI alone does
		// NOT show that: "openai-completions" is Pi's OWN value for every
		// OpenAI-compatible provider, so a slot the user configured for their
		// own server carries it too — the api value alone repointed such a slot
		// at oaica's endpoint and wrote oaica's credential over theirs. What the
		// writer emits is the pair: this api value AND a base URL oaica writes
		// (the daemon's /v1, or a configured remote's base). An endpoint that
		// cannot be shown to be one of those is the user's — their endpoint,
		// their key, their choice — so it is left exactly as they wrote it
		// (2026-09-27 audit, round 21). TestPiEdit's "preserving ollama provider
		// settings" case pins the same contract for a foreign api value.
		slotAPI, _ := ollama["api"].(string)
		slotBase, _ := ollama["baseUrl"].(string)
		if slotAPI == piProviderAPI && piEndpointWasOurs(slotBase) {
			wantBase, wantKey := piProviderBaseURL(models), piProviderKey(models)
			if oldBase, _ := ollama["baseUrl"].(string); strings.TrimRight(oldBase, "/") != strings.TrimRight(wantBase, "/") {
				fmt.Fprintf(noticeWriter(), "%s  Warning: repointing Pi's ollama provider from %s to %s — Pi serves every oaica-registered model from this one provider, and this launch's models resolve there%s\n",
					ansiYellow, piPrintableBaseURL(oldBase), piPrintableBaseURL(wantBase), ansiReset)
			}
			ollama["baseUrl"] = wantBase
			ollama["apiKey"] = wantKey
		} else if slotAPI == piProviderAPI {
			// Not ours to move, but written in this package's own shape: name
			// the endpoint left in place, so a launch whose models landed in a
			// provider pointed somewhere else is visible rather than silent.
			fmt.Fprintf(noticeWriter(), "%s  Warning: Pi's ollama provider is set to %s, which this launch did not configure — leaving your endpoint and key as they are, and adding this launch's models to that provider%s\n",
				ansiYellow, piPrintableBaseURL(slotBase), ansiReset)
		}
	}

	existingModels, ok := ollama["models"].([]any)
	if !ok {
		existingModels = make([]any, 0)
	}

	// Build set of selected models to track which need to be added. Keyed by
	// the id these entries are STORED under (piModelIDFor — the bare upstream
	// id for a user-remote model), not by the picker name: an entry on disk
	// carries the stored id, so a set keyed on picker names never matched a
	// remote model's entry, and every launch dropped and rebuilt it from
	// scratch — losing every member oaica does not model (2026-09-26 audit,
	// thirteenth round).
	selectedSet := make(map[string]bool, len(models))
	for _, m := range models {
		selectedSet[piModelIDFor(m)] = true
	}

	// Build new models list:
	// 1. Keep user-managed models (no _launch marker) - untouched
	// 2. Keep ollama-managed models (_launch marker) that are still selected,
	//    except stale cloud entries that should be rebuilt below
	// 3. Add new ollama-managed models
	var newModels []any
	for _, m := range existingModels {
		if modelObj, ok := m.(map[string]any); ok {
			if id, ok := modelObj["id"].(string); ok {
				// User-managed model (no _launch marker) - always preserve
				if !isPiOllamaModel(modelObj) {
					newModels = append(newModels, m)
				} else if selectedSet[id] {
					// Rebuild stale managed cloud entries so createConfig refreshes
					// the whole entry instead of patching it in place.
					if !hasContextWindow(modelObj) {
						if _, ok := lookupCloudModelLimit(id); ok {
							continue
						}
					}
					newModels = append(newModels, m)
					selectedSet[id] = false
				}
			}
		}
	}

	// Add newly selected models that weren't already in the list
	for _, model := range models {
		if id := piModelIDFor(model); selectedSet[id] {
			selectedSet[id] = false
			newModels = append(newModels, createConfig(model))
		}
	}

	ollama["models"] = newModels
	providers["ollama"] = ollama
	config["providers"] = providers

	configData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := fileutil.WriteWithBackup(configPath, configData, "pi"); err != nil {
		return err
	}

	// Update settings.json with default provider and model
	settings["defaultProvider"] = piDefaultProvider
	settings["defaultModel"] = piModelIDFor(models[0])

	settingsData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteWithBackup(settingsPath, settingsData, "pi")
}

func (p *Pi) Models() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	configPath := filepath.Join(home, ".pi", "agent", "models.json")
	config, err := fileutil.ReadJSON(configPath)
	if err != nil {
		return nil
	}

	providers, _ := config["providers"].(map[string]any)
	ollama, _ := providers["ollama"].(map[string]any)
	models, _ := ollama["models"].([]any)
	baseURL, _ := ollama["baseUrl"].(string)

	// Report the picker names — the names the launcher saves and compares the
	// live config against — not the stored ids verbatim (piPickerNameFor).
	var result []string
	for _, m := range models {
		if modelObj, ok := m.(map[string]any); ok {
			if id, ok := modelObj["id"].(string); ok {
				// Only entries this package wrote are translated: an entry
				// without the _launch marker is Pi's or the user's, and
				// prefacing its id with a remote name would report a model
				// nothing saved.
				if isPiOllamaModel(modelObj) {
					id = piPickerNameFor(id, baseURL)
				}
				result = append(result, id)
			}
		}
	}
	slices.Sort(result)
	return result
}

// piStoredOwnedIDs is the id of every entry under Pi's ollama provider that
// this package wrote (isPiOllamaModel), in file order. Those are the entries
// piEditDocuments owns and rebuilds, so they are the ones a write of a
// selection leaves: an entry with no _launch marker is the user's and is
// carried over untouched.
// piStoredProvider is what Pi's ollama provider slot holds: the entries this
// package wrote, and the endpoint and credential the writer sets on the slot
// itself.
type piStoredProvider struct {
	ids     []string
	api     string
	baseURL string
	apiKey  string
	// present is false when the file holds no ollama provider at all, which is
	// not the same as one with empty fields: a missing slot is one the writer
	// creates, so nothing there can be what a write would leave.
	present bool
}

func readPiStoredProvider(home string) piStoredProvider {
	var slot piStoredProvider
	config, err := fileutil.ReadJSON(filepath.Join(home, ".pi", "agent", "models.json"))
	if err != nil {
		return slot
	}
	providers, _ := config["providers"].(map[string]any)
	ollama, ok := providers["ollama"].(map[string]any)
	if !ok {
		return slot
	}
	slot.present = true
	slot.api, _ = ollama["api"].(string)
	slot.baseURL, _ = ollama["baseUrl"].(string)
	slot.apiKey, _ = ollama["apiKey"].(string)
	models, _ := ollama["models"].([]any)
	for _, m := range models {
		modelObj, ok := m.(map[string]any)
		if !ok || !isPiOllamaModel(modelObj) {
			continue
		}
		if id, ok := modelObj["id"].(string); ok {
			slot.ids = append(slot.ids, id)
		}
	}
	return slot
}

func piStoredOwnedIDs(home string) []string { return readPiStoredProvider(home).ids }

// DeclaresSelection reports whether Pi's store already holds what a write of
// models would leave.
//
// The comparison is a set, and Pi is the one integration where that is the
// honest reading. Its fallback (sameModelSelection) compares index by index,
// and the list it compared is in no particular order on either side: the picker
// hands back the models last-checked-first, and piEditDocuments keeps the
// entries already in the file where they are and appends the new ones. So a
// two-model launch agreed only when the save order happened to be sorted, and
// every other launch read its own write as drift, re-resolved the inventory and
// re-ran the configure step forever (2026-09-27 audit, round 30, A-F1). Pi's
// primary is settings.defaultModel, not the order of this list, so the order
// carries nothing the declaration needs to check.
//
// Only the entries this package wrote are compared, because the writer
// preserves the rest (an entry without the _launch marker is the user's own
// model in that provider slot) — they are not a selection it failed to remove.
func (p *Pi) DeclaresSelection(models []LaunchModel) bool {
	if len(models) == 0 {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	seen := make(map[string]bool, len(models))
	want := make([]string, 0, len(models))
	for _, m := range models {
		id := piModelIDFor(m)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		want = append(want, id)
	}
	held := piStoredOwnedIDs(home)
	if len(held) != len(want) {
		return false
	}
	slices.Sort(want)
	slices.Sort(held)
	if !sameStoreStrings(held, want) {
		return false
	}
	// The slot's address and credential are part of what a write leaves: the
	// writer sets both whenever the slot is one it wrote (an api value of its
	// own AND a base URL piEndpointWasOurs recognises). Reading the ids alone
	// answered "current" for a slot a write would move — the daemon has since
	// changed address, or the slot still carries an earlier remote launch's
	// token — so the launch skipped the write and Pi kept dialling the old
	// endpoint with a credential that does not belong to it (2026-09-27 audit,
	// round 31; the same field the round-22 warning is about, and the read
	// cline's store key has carried since round 29).
	slot := readPiStoredProvider(home)
	if !slot.present {
		return false
	}
	if slot.api == piProviderAPI && piEndpointWasOurs(slot.baseURL) {
		if strings.TrimRight(slot.baseURL, "/") != strings.TrimRight(piProviderBaseURL(models), "/") {
			return false
		}
		if slot.apiKey != piProviderKey(models) {
			return false
		}
	}
	// Pi's write publishes TWO documents, and this declaration reads one: the
	// provider and model settings.json names are set from the same selection,
	// and Pi.Run hands the model it is given to nothing but the capability gate
	// — so that file is what Pi actually opens on. Reading only models.json
	// left a file naming another provider or model (Pi's own TUI writes one)
	// reading as current, and the launch reported success while Pi started
	// something else (2026-09-27 audit, round 32, A-F2).
	return piSettingsDeclare(models[0], home)
}

// piDefaultProvider is the provider name Pi's settings name for the slot this
// package writes.
const piDefaultProvider = "ollama"

// piSettingsDeclare reports whether settings.json already names the provider
// and model a write of model would leave there. An unreadable or silent file is
// not a declaration: "cannot tell" has to mean "write".
func piSettingsDeclare(model LaunchModel, home string) bool {
	settings, err := fileutil.ReadJSON(filepath.Join(home, ".pi", "agent", "settings.json"))
	if err != nil {
		return false
	}
	provider, _ := settings["defaultProvider"].(string)
	id, _ := settings["defaultModel"].(string)
	return provider == piDefaultProvider && id == piModelIDFor(model)
}

// piPickerNameFor is the inverse of piModelIDFor for an entry on disk: the
// picker name that entry was written for.
//
// Pi keeps ONE provider with a single base URL (piProviderBaseURL), so the
// provider's base URL says which endpoint the entries came from: on a
// user-remote endpoint a stored id is the bare upstream id (piModelIDFor wrote
// it), and on the daemon's own /v1 it is already the picker name. Reporting a
// bare id verbatim answers with a name the launcher never saved, so
// liveConfigMatches is false on every run and each launch rewrites the config
// it just read (2026-09-26 audit, thirteenth round — the same fix droid and
// hermes carry).
func piPickerNameFor(id, providerBaseURL string) string {
	if id == "" || providerBaseURL == "" {
		return id
	}
	want := strings.TrimRight(providerBaseURL, "/")
	if want == strings.TrimRight(piDaemonProviderBaseURL(), "/") {
		// The daemon's endpoint: every entry here is a picker name already.
		return id
	}
	remotes, err := loadUserRemotes()
	if err != nil {
		// Same rule findUserRemoteForModel uses: a corrupt store must not take
		// the built-in providers with it.
		remotes = builtinRemotes()
	}
	for _, r := range remotes {
		if strings.TrimRight(r.openAIBase(), "/") != want {
			continue
		}
		// Only a name that resolves back to the same model is that model's
		// picker name; anything else is left as it was found.
		candidate := r.Name + "/" + id
		if ep, ok := resolveRemoteEndpoint(candidate); ok && ep.UpstreamModel == id {
			return candidate
		}
	}
	return id
}

// piDaemonProviderBaseURL is the base URL piProviderBaseURL falls back to: the
// local daemon's OpenAI-compatible endpoint.
func piDaemonProviderBaseURL() string { return envconfig.ConnectableHost().String() + "/v1" }

// isPiOllamaModel reports whether a model config entry is managed by oaica launch
func isPiOllamaModel(cfg map[string]any) bool {
	if v, ok := cfg["_launch"].(bool); ok && v {
		return true
	}
	return false
}

// hasContextWindow reports whether a model entry already carries a context
// window, which is the marker Pi.Edit reads as "this entry is one createConfig
// wrote, keep it as-is rather than rebuild it".
//
// The document this reads is the one readPiJSONDocument decoded, and that
// decoder sets UseNumber — so the number arrives as json.Number and the
// concrete cases below are unreachable from it. The guard used to switch on
// float64/int/int64 alone (correct for the plain decode it was written
// against), which after UseNumber landed (64640c6c, 2026-09-26) answered false
// for every entry on disk: a _launch cloud row carrying a window was rebuilt
// from scratch on every launch, so createConfig's handful of members replaced
// it and everything oaica does not model in that entry — the window the router
// reported among them — was deleted (2026-09-26 audit, twelfth round). The
// concrete cases are kept so the helper is right whichever decoder fed it, the
// same shape as toFloat64 in anthropic_openai_proxy.go. A value that does not
// parse, or does not exceed zero, states no window.
func hasContextWindow(cfg map[string]any) bool {
	switch v := cfg["contextWindow"].(type) {
	case json.Number:
		n, err := v.Float64()
		return err == nil && n > 0
	case float64:
		return v > 0
	case float32:
		return v > 0
	case int:
		return v > 0
	case int64:
		return v > 0
	default:
		return false
	}
}

// piModelIDFor is the model id Pi should use: the daemon-side id for an
// ollama-cloud catalogue row (LaunchModel.Upstream), the bare upstream id for a
// user-remote model, otherwise the picker name.
func piModelIDFor(model LaunchModel) string {
	return launchModelWriteID(model)
}

// piProviderAPI is the provider API value this package writes when it creates
// Pi's ollama slot, and the marker that the slot is ours to repoint (see
// Pi.Edit).
const piProviderAPI = "openai-completions"

// piPrintableBaseURL is a base URL as printed in a notice: doctor's rule (see
// redactBaseURL), because a provider base URL is a shape this project allows to
// carry a credential in its userinfo and this line goes to the terminal.
func piPrintableBaseURL(baseURL string) string { return redactBaseURL(baseURL) }

// piEndpointWasOurs reports whether a provider base URL is one this package
// writes into Pi's single provider slot: the daemon's /v1, the daemon's
// documented default addresses, or a configured remote's base. It is the
// endpoint half of the ownership test — see Pi.Edit for why the api value
// cannot carry that meaning on its own.
//
// An empty base URL is NOT ours: this package always writes a base URL, so an
// empty one means the user removed it, and a slot that cannot be shown to be
// ours is preserved rather than repointed. A store that cannot be read falls
// back to the built-in remotes, the rule findUserRemoteForModel uses — a
// corrupt store must not take a remote's own endpoint away from it.
func piEndpointWasOurs(baseURL string) bool {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return false
	}
	if base == strings.TrimRight(piDaemonProviderBaseURL(), "/") {
		return true
	}
	// The daemon's DOCUMENTED DEFAULT address: an earlier oaica wrote it even
	// when OLLAMA_HOST has since moved this machine's daemon elsewhere. That
	// default is the whole set this predicate can recognise — a value an earlier
	// oaica wrote under a custom OLLAMA_HOST is that host's address, which this
	// launch cannot derive from anything it holds, and taking a LAN address as
	// ours on a suffix alone would rewrite the user's own provider on that host.
	// Such a value is treated as the user's: the warning below says so and
	// leaves the endpoint and key alone (2026-09-27 audit, round 22).
	for _, host := range []string{"http://127.0.0.1:11434", "http://localhost:11434", "http://[::1]:11434"} {
		if base == host || base == host+"/v1" {
			return true
		}
	}
	remotes, err := loadUserRemotes()
	if err != nil {
		remotes = builtinRemotes()
	}
	for _, r := range remotes {
		if strings.TrimRight(r.openAIBase(), "/") == base || strings.TrimRight(remoteBaseURL(r), "/") == base {
			return true
		}
	}
	return false
}

// piProviderBaseURL is the base URL Pi's single provider should use: the first
// user-remote model's endpoint, otherwise the daemon's /v1.
func piProviderBaseURL(models []LaunchModel) string {
	for _, m := range models {
		if ep, ok := resolveRemoteEndpoint(m.Name); ok {
			return strings.TrimRight(ep.BaseURL, "/")
		}
	}
	return envconfig.ConnectableHost().String() + "/v1"
}

// piProviderKey is the API key Pi's single provider should use: the first
// user-remote model's token, otherwise "ollama".
func piProviderKey(models []LaunchModel) string {
	for _, m := range models {
		if ep, ok := resolveRemoteEndpoint(m.Name); ok {
			return ep.Token
		}
	}
	return "ollama"
}

// createConfig builds Pi model config with capability detection.
func createConfig(model LaunchModel) map[string]any {
	cfg := map[string]any{
		"id":      piModelIDFor(model),
		"_launch": true,
	}

	// Set input types based on vision capability
	if model.HasCapability("vision") {
		cfg["input"] = []string{"text", "image"}
	} else {
		cfg["input"] = []string{"text"}
	}

	// Set reasoning based on thinking capability
	if model.HasCapability("thinking") {
		cfg["reasoning"] = true
	}

	if model.ContextLength > 0 {
		cfg["contextWindow"] = model.ContextLength
	}

	return cfg
}
