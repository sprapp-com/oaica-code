package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ollama/ollama/cmd/config"
	"github.com/ollama/ollama/cmd/internal/fileutil"
	"golang.org/x/term"
)

const (
	claudeDesktopIntegrationName = "claude-desktop"
	claudeDesktopProfileName     = "Ollama"
	claudeDesktopProfileID       = "00000000-0000-4000-8000-000000000114"
	claudeDesktopGatewayBaseURL  = "https://ollama.com"
	claudeDesktopAPIKeyURL       = "https://ollama.com/settings/keys"
	claudeDesktopModelLabel      = "Ollama Cloud"
	claudeDesktopUnsupported     = "Claude Desktop is no longer supported. Existing installations can be restored with 'oaica launch claude-desktop --restore'."
	claudeDesktopSuccessMessage  = "Claude Desktop profile changed to Ollama Cloud."
	claudeDesktopRestoreMessage  = "To restore the usual Claude profile, run: oaica launch claude-desktop --restore"
	claudeDesktopRestoredMessage = "Claude Desktop restored to the usual Claude profile."
)

var (
	claudeDesktopGOOS           = runtime.GOOS
	claudeDesktopUserHome       = os.UserHomeDir
	claudeDesktopStat           = os.Stat
	claudeDesktopOpenApp        = defaultClaudeDesktopOpenApp
	claudeDesktopOpenAppPath    = defaultClaudeDesktopOpenAppPath
	claudeDesktopQuitApp        = defaultClaudeDesktopQuitApp
	claudeDesktopIsRunning      = defaultClaudeDesktopIsRunning
	claudeDesktopRunningAppPath = defaultClaudeDesktopRunningAppPath
	claudeDesktopGlob           = filepath.Glob
	claudeDesktopSleep          = time.Sleep
	claudeDesktopHTTPClient     = http.DefaultClient
	claudeDesktopPromptAPIKey   = promptClaudeDesktopAPIKey
	claudeDesktopValidateAPIKey = validateClaudeDesktopAPIKey
)

// ClaudeDesktop configures and launches Claude Desktop in third-party
// inference mode using Ollama Cloud as the gateway.
type ClaudeDesktop struct{}

func (c *ClaudeDesktop) String() string { return "Claude Desktop" }

func (c *ClaudeDesktop) Supported() error { return claudeDesktopSupported() }

func (c *ClaudeDesktop) Paths() []string {
	return nil
}

func (c *ClaudeDesktop) AutodiscoveredModel() string {
	return claudeDesktopModelLabel
}

func (c *ClaudeDesktop) ConfigureAutodiscovery() error {
	if err := claudeDesktopSupported(); err != nil {
		return err
	}

	targets, err := claudeDesktopTargetPaths()
	if err != nil {
		return err
	}

	key, err := claudeDesktopValidatedAPIKey(context.Background(), claudeDesktopTargetProfilePaths(targets))
	if err != nil {
		return err
	}

	// The restore state is loaded, added to and published as one critical
	// section (updateClaudeDesktopRestoreState), with the Claude Desktop
	// documents' own locked writes nested inside it.
	return updateClaudeDesktopRestoreState(func(restore *claudeDesktopRestoreState) error {
		for _, path := range targets.normalConfigs {
			if err := writeClaudeDesktopDeploymentMode(path, "3p"); err != nil {
				return err
			}
		}
		for _, target := range targets.thirdPartyProfiles {
			if err := writeClaudeDesktopDeploymentMode(target.desktopConfig, "3p"); err != nil {
				return err
			}
			if err := writeClaudeDesktopMeta(target.meta, claudeDesktopProfileID, claudeDesktopProfileName); err != nil {
				return err
			}
			// Before overwriting the profile's credential, remember what was there.
			if err := recordClaudeDesktopGatewayKeyInjection(restore, target.profile, key); err != nil {
				return err
			}
			if err := writeClaudeDesktopGatewayProfile(target.profile, key, true); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *ClaudeDesktop) RestoreHint() string {
	return claudeDesktopRestoreMessage
}

func (c *ClaudeDesktop) ConfigurationSuccessMessage() string {
	return claudeDesktopSuccessMessage + "\n" + claudeDesktopRestoreMessage
}

func (c *ClaudeDesktop) RestoreSuccessMessage() string {
	return claudeDesktopRestoredMessage
}

func (c *ClaudeDesktop) AutodiscoveryConfigured() bool {
	targets, err := claudeDesktopTargetPaths()
	if err != nil {
		return false
	}
	return claudeDesktopTargetsConfigured(targets)
}

func (c *ClaudeDesktop) Onboard() error {
	return config.MarkIntegrationOnboarded(claudeDesktopIntegrationName)
}

func (c *ClaudeDesktop) RequiresInteractiveOnboarding() bool {
	return false
}

func (c *ClaudeDesktop) SkipModelReadiness() bool {
	return true
}

func (c *ClaudeDesktop) Run(_ string, _ []LaunchModel, _ []string) error {
	return errClaudeDesktopUnsupported()
}

func (c *ClaudeDesktop) Restore() error {
	if err := claudeDesktopSupported(); err != nil {
		return err
	}
	targets, err := claudeDesktopTargetPaths()
	if err != nil {
		return err
	}

	// The same critical section as ConfigureAutodiscovery: the state is read
	// under its lock, the profiles' records are consumed inside it, and what is
	// left is published inside the same section.
	if err := updateClaudeDesktopRestoreState(func(restore *claudeDesktopRestoreState) error {
		for _, path := range targets.normalConfigs {
			if err := writeClaudeDesktopDeploymentMode(path, "1p"); err != nil {
				return err
			}
		}
		for _, target := range targets.thirdPartyProfiles {
			if err := writeClaudeDesktopDeploymentMode(target.desktopConfig, "1p"); err != nil {
				return err
			}
			if err := restoreClaudeDesktopMeta(target.meta); err != nil {
				return err
			}
			prior, recorded := restore.Profiles[target.profile]
			if err := restoreClaudeDesktopOllamaProfile(target.profile, prior, recorded); err != nil {
				return err
			}
			delete(restore.Profiles, target.profile)
		}
		return nil
	}); err != nil {
		return err
	}
	return claudeDesktopLaunchOrRestart("Restart Claude Desktop to use the usual Claude profile?")
}

func errClaudeDesktopUnsupported() error {
	return errors.New(claudeDesktopUnsupported)
}

func claudeDesktopSupported() error {
	switch claudeDesktopGOOS {
	case "darwin", "windows":
		return nil
	default:
		return fmt.Errorf("Claude Desktop launch is only supported on macOS and Windows")
	}
}

func claudeDesktopInstalled() bool {
	if claudeDesktopAppPath() != "" {
		return true
	}
	if claudeDesktopGOOS == "windows" && claudeDesktopIsRunning() {
		return true
	}
	for _, dir := range claudeDesktopProfileDirCandidates(false) {
		if _, err := claudeDesktopStat(dir); err == nil {
			return true
		}
	}
	return false
}

func claudeDesktopAppPath() string {
	if claudeDesktopGOOS != "darwin" && claudeDesktopGOOS != "windows" {
		return ""
	}
	for _, path := range claudeDesktopAppCandidates() {
		if _, err := claudeDesktopStat(path); err == nil {
			return path
		}
	}
	return ""
}

func claudeDesktopAppCandidates() []string {
	switch claudeDesktopGOOS {
	case "darwin":
		return claudeDesktopDarwinAppCandidates()
	case "windows":
		return claudeDesktopWindowsAppCandidates()
	default:
		return nil
	}
}

func claudeDesktopDarwinAppCandidates() []string {
	candidates := []string{"/Applications/Claude.app"}
	if home, err := claudeDesktopUserHome(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Applications", "Claude.app"))
	}
	return candidates
}

func claudeDesktopWindowsAppCandidates() []string {
	local, err := claudeDesktopLocalAppData()
	if err != nil {
		return nil
	}

	candidates := []string{
		filepath.Join(local, "Programs", "Claude", "Claude.exe"),
		filepath.Join(local, "Programs", "Claude Desktop", "Claude.exe"),
		filepath.Join(local, "Claude", "Claude.exe"),
		filepath.Join(local, "Claude Nest", "Claude.exe"),
		filepath.Join(local, "Claude Desktop", "Claude.exe"),
		filepath.Join(local, "AnthropicClaude", "Claude.exe"),
	}
	for _, pattern := range []string{
		filepath.Join(local, "AnthropicClaude", "app-*", "Claude.exe"),
		filepath.Join(local, "Programs", "Claude", "app-*", "Claude.exe"),
		filepath.Join(local, "Programs", "Claude Desktop", "app-*", "Claude.exe"),
	} {
		matches, _ := claudeDesktopGlob(pattern)
		candidates = append(candidates, matches...)
	}
	return claudeDesktopDedupePaths(candidates)
}

func claudeDesktopDedupePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		key := strings.ToLower(path)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, path)
	}
	return out
}

type claudeDesktopPaths struct {
	normalConfig  string
	desktopConfig string
	meta          string
	profile       string
}

type claudeDesktopThirdPartyPaths struct {
	desktopConfig string
	meta          string
	profile       string
}

type claudeDesktopTargets struct {
	normalConfigs      []string
	thirdPartyProfiles []claudeDesktopThirdPartyPaths
}

func claudeDesktopConfigPaths() (claudeDesktopPaths, error) {
	switch claudeDesktopGOOS {
	case "darwin":
		return claudeDesktopDarwinConfigPaths()
	case "windows":
		return claudeDesktopWindowsConfigPaths()
	default:
		return claudeDesktopPaths{}, claudeDesktopSupported()
	}
}

func claudeDesktopDarwinConfigPaths() (claudeDesktopPaths, error) {
	normalRoots, thirdPartyRoots, err := claudeDesktopDarwinProfileRoots()
	if err != nil {
		return claudeDesktopPaths{}, err
	}
	normalBase := normalRoots[0]
	thirdPartyBase := thirdPartyRoots[0]
	return claudeDesktopPaths{
		normalConfig:  filepath.Join(normalBase, "claude_desktop_config.json"),
		desktopConfig: filepath.Join(thirdPartyBase, "claude_desktop_config.json"),
		meta:          filepath.Join(thirdPartyBase, "configLibrary", "_meta.json"),
		profile:       filepath.Join(thirdPartyBase, "configLibrary", claudeDesktopProfileID+".json"),
	}, nil
}

func claudeDesktopWindowsConfigPaths() (claudeDesktopPaths, error) {
	normalBase, err := claudeDesktopProfileDir(true)
	if err != nil {
		return claudeDesktopPaths{}, err
	}
	thirdPartyBase, err := claudeDesktopProfileDir(false)
	if err != nil {
		return claudeDesktopPaths{}, err
	}
	return claudeDesktopPaths{
		normalConfig:  filepath.Join(normalBase, "claude_desktop_config.json"),
		desktopConfig: filepath.Join(thirdPartyBase, "claude_desktop_config.json"),
		meta:          filepath.Join(thirdPartyBase, "configLibrary", "_meta.json"),
		profile:       filepath.Join(thirdPartyBase, "configLibrary", claudeDesktopProfileID+".json"),
	}, nil
}

func claudeDesktopProfileDir(normal bool) (string, error) {
	candidates := claudeDesktopProfileDirCandidates(normal)
	if len(candidates) == 0 {
		return "", fmt.Errorf("Claude Desktop profile directory could not be resolved")
	}
	for _, candidate := range candidates {
		if _, err := claudeDesktopStat(candidate); err == nil {
			return candidate, nil
		}
	}
	return candidates[0], nil
}

func claudeDesktopProfileDirCandidates(normal bool) []string {
	if claudeDesktopGOOS != "windows" {
		return nil
	}
	normalRoots, thirdPartyRoots, err := claudeDesktopWindowsProfileRoots()
	if err != nil {
		return nil
	}
	if normal {
		return normalRoots
	}
	return thirdPartyRoots
}

func claudeDesktopDarwinProfileRoots() ([]string, []string, error) {
	home, err := claudeDesktopUserHome()
	if err != nil {
		return nil, nil, err
	}
	base := filepath.Join(home, "Library", "Application Support")
	return []string{filepath.Join(base, "Claude")}, []string{filepath.Join(base, "Claude-3p")}, nil
}

func claudeDesktopWindowsProfileRoots() ([]string, []string, error) {
	local, err := claudeDesktopLocalAppData()
	if err != nil {
		return nil, nil, err
	}
	normalRoots := []string{
		filepath.Join(local, "Claude"),
		filepath.Join(local, "Claude Nest"),
	}
	thirdPartyRoots := []string{
		filepath.Join(local, "Claude-3p"),
		filepath.Join(local, "Claude Nest-3p"),
	}
	return normalRoots, thirdPartyRoots, nil
}

func claudeDesktopTargetPaths() (claudeDesktopTargets, error) {
	var (
		normalRoots     []string
		thirdPartyRoots []string
		err             error
	)

	switch claudeDesktopGOOS {
	case "darwin":
		normalRoots, thirdPartyRoots, err = claudeDesktopDarwinProfileRoots()
	case "windows":
		normalRoots, thirdPartyRoots, err = claudeDesktopWindowsProfileRoots()
	default:
		err = claudeDesktopSupported()
	}
	if err != nil {
		return claudeDesktopTargets{}, err
	}

	return newClaudeDesktopTargets(normalRoots, thirdPartyRoots), nil
}

func newClaudeDesktopTargets(normalRoots, thirdPartyRoots []string) claudeDesktopTargets {
	targets := claudeDesktopTargets{}
	for _, root := range claudeDesktopDedupePaths(normalRoots) {
		targets.normalConfigs = append(targets.normalConfigs, filepath.Join(root, "claude_desktop_config.json"))
	}
	for _, root := range claudeDesktopDedupePaths(thirdPartyRoots) {
		targets.thirdPartyProfiles = append(targets.thirdPartyProfiles, claudeDesktopThirdPartyPaths{
			desktopConfig: filepath.Join(root, "claude_desktop_config.json"),
			meta:          filepath.Join(root, "configLibrary", "_meta.json"),
			profile:       filepath.Join(root, "configLibrary", claudeDesktopProfileID+".json"),
		})
	}
	return targets
}

func claudeDesktopTargetProfilePaths(targets claudeDesktopTargets) []string {
	paths := make([]string, 0, len(targets.thirdPartyProfiles))
	for _, target := range targets.thirdPartyProfiles {
		paths = append(paths, target.profile)
	}
	return paths
}

func claudeDesktopLocalAppData() (string, error) {
	if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
		return local, nil
	}
	if home := strings.TrimSpace(os.Getenv("USERPROFILE")); home != "" {
		return filepath.Join(home, "AppData", "Local"), nil
	}
	home, err := claudeDesktopUserHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "AppData", "Local"), nil
}

type claudeDesktopAPIKeySource int

const (
	claudeDesktopAPIKeySourceNone claudeDesktopAPIKeySource = iota
	claudeDesktopAPIKeySourceEnv
	claudeDesktopAPIKeySourceProfile
)

func claudeDesktopValidatedAPIKey(ctx context.Context, profilePaths []string) (string, error) {
	key, source, err := claudeDesktopAPIKey(profilePaths)
	if err != nil {
		return "", err
	}
	if err := claudeDesktopValidateAPIKey(ctx, key); err == nil {
		return key, nil
	} else if source != claudeDesktopAPIKeySourceProfile || !canPromptClaudeDesktopAPIKey() {
		return "", err
	}
	return promptValidClaudeDesktopAPIKey(ctx)
}

func claudeDesktopAPIKey(profilePaths []string) (string, claudeDesktopAPIKeySource, error) {
	if key := strings.TrimSpace(os.Getenv("OLLAMA_API_KEY")); key != "" {
		return key, claudeDesktopAPIKeySourceEnv, nil
	}
	for _, profilePath := range profilePaths {
		if key := readClaudeDesktopGatewayAPIKey(profilePath); key != "" {
			return key, claudeDesktopAPIKeySourceProfile, nil
		}
	}
	key, err := promptClaudeDesktopAPIKeyValue()
	return key, claudeDesktopAPIKeySourceNone, err
}

func canPromptClaudeDesktopAPIKey() bool {
	return isInteractiveSession() && !currentLaunchConfirmPolicy.requireYesMessage
}

func promptValidClaudeDesktopAPIKey(ctx context.Context) (string, error) {
	key, err := promptClaudeDesktopAPIKeyValue()
	if err != nil {
		return "", err
	}
	if err := claudeDesktopValidateAPIKey(ctx, key); err != nil {
		return "", err
	}
	return key, nil
}

func promptClaudeDesktopAPIKeyValue() (string, error) {
	if !canPromptClaudeDesktopAPIKey() {
		return "", missingClaudeDesktopAPIKeyError()
	}
	key, err := claudeDesktopPromptAPIKey()
	if err != nil {
		return "", err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", missingClaudeDesktopAPIKeyError()
	}
	return key, nil
}

func missingClaudeDesktopAPIKeyError() error {
	return fmt.Errorf("OLLAMA_API_KEY is required for Claude Desktop. Create an API key at %s, then re-run with OLLAMA_API_KEY set", claudeDesktopAPIKeyURL)
}

func promptClaudeDesktopAPIKey() (string, error) {
	fmt.Fprint(os.Stderr, claudeDesktopAPIKeyPrompt())
	key, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(key), nil
}

func claudeDesktopAPIKeyPrompt() string {
	return fmt.Sprintf("Create an Ollama API key at %s\nEnter Ollama API key (input hidden): ", claudeDesktopAPIKeyURL)
}

func readClaudeDesktopGatewayAPIKey(path string) string {
	cfg, err := readClaudeDesktopJSON(path)
	if err != nil {
		return ""
	}
	key, _ := cfg["inferenceGatewayApiKey"].(string)
	return strings.TrimSpace(key)
}

func validateClaudeDesktopAPIKey(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if claudeDesktopAPIKeyHasInvalidHeaderChars(key) {
		return claudeDesktopAPIKeyVerificationError()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeDesktopGatewayBaseURL+"/v1/models", nil)
	if err != nil {
		return claudeDesktopAPIKeyVerificationError()
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	resp, err := claudeDesktopHTTPClient.Do(req)
	if err != nil {
		return claudeDesktopAPIKeyVerificationError()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("Ollama API key was rejected; create a valid key at %s", claudeDesktopAPIKeyURL)
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	default:
		return fmt.Errorf("could not verify Ollama API key; ollama.com returned status %d, try again later", resp.StatusCode)
	}
}

func claudeDesktopAPIKeyHasInvalidHeaderChars(key string) bool {
	return strings.ContainsFunc(key, func(r rune) bool {
		return r < ' ' || r == 0x7f
	})
}

func claudeDesktopAPIKeyVerificationError() error {
	return fmt.Errorf("could not verify Ollama API key; copy a key from %s and try again", claudeDesktopAPIKeyURL)
}

// withClaudeDesktopConfigLock runs one Claude Desktop JSON document's
// read-modify-write under that document's lock.
//
// Every writer below reads the document with readClaudeDesktopJSONAllowMissing,
// sets the keys it owns and publishes the whole document back — a lost update
// waiting to happen, because two `oaica launch claude-desktop` commands whose
// writers overlap each publish a snapshot taken before the other's keys landed
// and the publish that renames last decides the file while both report success.
// The lock therefore has to cover the read, not just the publish, and it has to
// be the SAME lock for every function that touches the document — write and
// restore both — which is why it is derived from the path here rather than held
// per function.
//
// Claude Desktop's config directory is not oaica's (oaica writes two profiles
// into a tree Claude Desktop owns and syncs), so the lock is keyed under
// ~/.oaica/locks (foreignStoreLockBase) rather than dropped beside the
// document. ConfigureAutodiscovery and Restore take the restore-state lock
// first and these inner ones second — the order is the same in both entry
// points, so two commands cannot deadlock against each other (2026-09-26 audit,
// thirteenth round).
func withClaudeDesktopConfigLock(path string, fn func() error) error {
	return fileutil.WithFileLock(foreignStoreLockBase(path), fn)
}

func writeClaudeDesktopDeploymentMode(path, mode string) error {
	return withClaudeDesktopConfigLock(path, func() error {
		cfg, err := readClaudeDesktopJSONAllowMissing(path)
		if err != nil {
			return fmt.Errorf("parse Claude Desktop config: %w", err)
		}
		cfg["deploymentMode"] = mode
		return writeClaudeDesktopJSON(path, cfg)
	})
}

func writeClaudeDesktopMeta(path, id, name string) error {
	return withClaudeDesktopConfigLock(path, func() error {
		meta, err := readClaudeDesktopJSONAllowMissing(path)
		if err != nil {
			return fmt.Errorf("parse Claude Desktop config metadata: %w", err)
		}

		meta["appliedId"] = id
		entries := make([]any, 0)
		for _, entry := range claudeDesktopAnySlice(meta["entries"]) {
			entryMap, _ := entry.(map[string]any)
			if entryMap == nil {
				entries = append(entries, entry)
				continue
			}
			if entryID, _ := entryMap["id"].(string); entryID == id {
				continue
			}
			entries = append(entries, entryMap)
		}
		entries = append(entries, map[string]any{
			"id":   id,
			"name": name,
		})
		meta["entries"] = entries
		return writeClaudeDesktopJSON(path, meta)
	})
}

func writeClaudeDesktopGatewayProfile(path string, apiKey string, forceChooser bool) error {
	return withClaudeDesktopConfigLock(path, func() error {
		cfg, err := readClaudeDesktopJSONAllowMissing(path)
		if err != nil {
			return fmt.Errorf("parse Claude Desktop Ollama profile: %w", err)
		}
		cfg["inferenceProvider"] = "gateway"
		cfg["inferenceGatewayBaseUrl"] = claudeDesktopGatewayBaseURL
		cfg["inferenceGatewayApiKey"] = apiKey
		cfg["inferenceGatewayAuthScheme"] = "bearer"
		delete(cfg, "inferenceModels")
		cfg["disableDeploymentModeChooser"] = forceChooser
		return writeClaudeDesktopJSON(path, cfg)
	})
}

func restoreClaudeDesktopMeta(path string) error {
	return withClaudeDesktopConfigLock(path, func() error {
		meta, err := readClaudeDesktopJSONAllowMissing(path)
		if err != nil {
			return fmt.Errorf("parse Claude Desktop config metadata: %w", err)
		}
		if len(meta) == 0 {
			return nil
		}

		changed := false
		if appliedID, _ := meta["appliedId"].(string); appliedID == claudeDesktopProfileID {
			delete(meta, "appliedId")
			changed = true
		}

		entries := claudeDesktopAnySlice(meta["entries"])
		if entries != nil {
			filtered := make([]any, 0, len(entries))
			for _, entry := range entries {
				entryMap, _ := entry.(map[string]any)
				if entryID, _ := entryMap["id"].(string); entryID == claudeDesktopProfileID {
					changed = true
					continue
				}
				filtered = append(filtered, entry)
			}
			meta["entries"] = filtered
		}

		if !changed {
			return nil
		}
		return writeClaudeDesktopJSON(path, meta)
	})
}

func restoreClaudeDesktopOllamaProfile(path string, prior claudeDesktopProfileRestoreState, recorded bool) error {
	return withClaudeDesktopConfigLock(path, func() error {
		cfg, err := readClaudeDesktopJSONAllowMissing(path)
		if err != nil {
			return fmt.Errorf("parse Claude Desktop Ollama profile: %w", err)
		}
		if len(cfg) == 0 {
			return nil
		}
		cfg["disableDeploymentModeChooser"] = false
		delete(cfg, "inferenceProvider")
		delete(cfg, "inferenceGatewayBaseUrl")
		delete(cfg, "inferenceGatewayAuthScheme")
		delete(cfg, "inferenceModels")
		// The gateway credential is a secret oaica wrote, so restore removes it —
		// but only where oaica wrote one. A profile oaica never put a key into
		// (no record) keeps the key its owner configured; a profile whose own key
		// oaica replaced gets that value back; a profile whose own key oaica
		// reused keeps it where it is (2026-09-26 audit).
		switch {
		case !recorded || prior.HadKey && prior.Replaced == "":
		case prior.HadKey:
			cfg["inferenceGatewayApiKey"] = prior.Replaced
		default:
			delete(cfg, "inferenceGatewayApiKey")
		}
		return writeClaudeDesktopJSON(path, cfg)
	})
}

// claudeDesktopRestoreState records what each Ollama third-party profile held
// before oaica wrote its gateway credential into it, so `--restore` can take
// back exactly what oaica added. Without it restore has only the file to go on,
// where a key oaica injected and a key the user configured themselves look
// identical — leaving a secret oaica wrote on disk, or deleting the user's
// (2026-09-26 audit). It is the same shape as the Codex app's restore state,
// for the same reason; the file is oaica's own, so nothing oaica keeps here is
// read by Claude Desktop.
type claudeDesktopRestoreState struct {
	// Profiles is keyed by profile path: macOS has one third-party profile
	// root and Windows two, and they do not have to hold the same credential.
	Profiles map[string]claudeDesktopProfileRestoreState `json:"profiles,omitempty"`
}

type claudeDesktopProfileRestoreState struct {
	// HadKey is whether the profile already held a gateway API key before
	// oaica configured it.
	HadKey bool `json:"had_key"`
	// Replaced is the user's key that oaica overwrote, recorded only when it
	// differed from the one oaica wrote: restoring a key that was never lost
	// would copy the user's credential into a second file for nothing. Empty
	// with HadKey true means the key still in the profile is the user's own.
	Replaced string `json:"replaced,omitempty"`
}

func claudeDesktopRestoreStatePath() string {
	home, err := claudeDesktopUserHome()
	if err != nil {
		return filepath.Join(os.TempDir(), "ollama-claude-desktop-restore.json")
	}
	return filepath.Join(home, ".ollama", "launch", "claude-desktop-restore.json")
}

func loadClaudeDesktopRestoreState() (claudeDesktopRestoreState, error) {
	state := claudeDesktopRestoreState{}
	data, err := os.ReadFile(claudeDesktopRestoreStatePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return claudeDesktopRestoreState{}, fmt.Errorf("parse Claude Desktop restore state: %w", err)
	}
	return state, nil
}

// updateClaudeDesktopRestoreState runs a load → mutate → publish of oaica's own
// restore-state file under that file's lock, with the load INSIDE it.
//
// Both entry points that use the state (ConfigureAutodiscovery and Restore)
// need the whole cycle to be one critical section: what each of them records is
// a before-snapshot of a Claude Desktop profile it is about to overwrite, and a
// state loaded before the lock is a snapshot that does not hold the other
// command's records — publishing it deletes them, which costs the user the
// credential the deleted record existed to protect. The file is oaica's own
// (unlike the Claude Desktop documents nested inside this cycle), so the lock
// goes beside it, as the other oaica-owned stores do. The nesting order is
// fixed at restore-state-first in both entry points, so two commands cannot
// deadlock against each other (2026-09-26 audit, thirteenth round).
func updateClaudeDesktopRestoreState(mutate func(*claudeDesktopRestoreState) error) error {
	path := claudeDesktopRestoreStatePath()
	return fileutil.WithFileLock(path, func() error {
		state, err := loadClaudeDesktopRestoreState()
		if err != nil {
			return err
		}
		if err := mutate(&state); err != nil {
			return err
		}
		return writeClaudeDesktopRestoreState(state)
	})
}

func writeClaudeDesktopRestoreState(state claudeDesktopRestoreState) error {
	path := claudeDesktopRestoreStatePath()
	if len(state.Profiles) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fileutil.WriteWithBackup(path, data)
}

// recordClaudeDesktopGatewayKeyInjection saves the credential a profile holds
// before oaica overwrites it. A record already present is left alone: after the
// first configure the key in that file is oaica's, and recording it as the
// user's would make restore keep oaica's secret — the bug this exists to fix.
func recordClaudeDesktopGatewayKeyInjection(state *claudeDesktopRestoreState, profilePath, injectedKey string) error {
	if _, ok := state.Profiles[profilePath]; ok {
		return nil
	}
	cfg, err := readClaudeDesktopJSONAllowMissing(profilePath)
	if err != nil {
		return fmt.Errorf("parse Claude Desktop Ollama profile: %w", err)
	}
	existing, _ := cfg["inferenceGatewayApiKey"].(string)
	existing = strings.TrimSpace(existing)
	prior := claudeDesktopProfileRestoreState{HadKey: existing != ""}
	if existing != "" && existing != strings.TrimSpace(injectedKey) {
		prior.Replaced = existing
	}
	if state.Profiles == nil {
		state.Profiles = make(map[string]claudeDesktopProfileRestoreState)
	}
	state.Profiles[profilePath] = prior
	return nil
}

func readClaudeDesktopAppliedID(path string) string {
	meta, err := readClaudeDesktopJSON(path)
	if err != nil {
		return ""
	}
	applied, _ := meta["appliedId"].(string)
	return applied
}

func readClaudeDesktopDeploymentMode(path string) string {
	cfg, err := readClaudeDesktopJSON(path)
	if err != nil {
		return ""
	}
	mode, _ := cfg["deploymentMode"].(string)
	return mode
}

func claudeDesktopTargetsConfigured(targets claudeDesktopTargets) bool {
	if len(targets.normalConfigs) == 0 || len(targets.thirdPartyProfiles) == 0 {
		return false
	}
	for _, path := range targets.normalConfigs {
		if readClaudeDesktopDeploymentMode(path) != "3p" {
			return false
		}
	}
	for _, target := range targets.thirdPartyProfiles {
		if readClaudeDesktopDeploymentMode(target.desktopConfig) != "3p" {
			return false
		}
		if !claudeDesktopThirdPartyProfileConfigured(target) {
			return false
		}
	}
	return true
}

func claudeDesktopThirdPartyProfileConfigured(target claudeDesktopThirdPartyPaths) bool {
	if readClaudeDesktopAppliedID(target.meta) != claudeDesktopProfileID {
		return false
	}

	cfg, err := readClaudeDesktopJSON(target.profile)
	if err != nil {
		return false
	}
	if s, _ := cfg["inferenceProvider"].(string); s != "gateway" {
		return false
	}
	if s, _ := cfg["inferenceGatewayBaseUrl"].(string); strings.TrimRight(s, "/") != claudeDesktopGatewayBaseURL {
		return false
	}
	if s, _ := cfg["inferenceGatewayApiKey"].(string); strings.TrimSpace(s) == "" {
		return false
	}
	return true
}

func readClaudeDesktopJSONAllowMissing(path string) (map[string]any, error) {
	cfg, err := readClaudeDesktopJSON(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	return cfg, err
}

func readClaudeDesktopJSON(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	// UseNumber: the caller writes this document back whole, so every number
	// in it has to survive decoding exactly. Through float64 an integer above
	// 2^53 changes value and 1.0 becomes 1 — oaica edited a file it was only
	// asked to add a profile to (2026-09-26 audit).
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

func writeClaudeDesktopJSON(path string, cfg any) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fileutil.WriteWithBackup(path, data)
}

func claudeDesktopAnySlice(value any) []any {
	switch v := value.(type) {
	case []any:
		return v
	case nil:
		return nil
	default:
		return nil
	}
}

func claudeDesktopLaunchOrRestart(prompt string) error {
	if !claudeDesktopIsRunning() {
		return claudeDesktopOpenApp()
	}
	restartAppPath := ""
	if claudeDesktopGOOS == "windows" {
		restartAppPath = claudeDesktopRunningAppPath()
	}

	restart, err := ConfirmPrompt(prompt)
	if err != nil {
		return err
	}
	if !restart {
		fmt.Fprintln(os.Stderr, "\nQuit and reopen Claude Desktop when you're ready for the profile change to take effect.")
		return nil
	}

	if err := claudeDesktopQuitApp(); err != nil {
		return fmt.Errorf("quit Claude Desktop: %w", err)
	}
	if err := waitForClaudeDesktopExit(30 * time.Second); err != nil {
		return err
	}
	if restartAppPath != "" {
		return claudeDesktopOpenAppPath(restartAppPath)
	}
	return claudeDesktopOpenApp()
}

func waitForClaudeDesktopExit(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !claudeDesktopIsRunning() {
			return nil
		}
		claudeDesktopSleep(200 * time.Millisecond)
	}
	return fmt.Errorf("Claude Desktop did not quit; quit it manually and re-run the command")
}

func defaultClaudeDesktopIsRunning() bool {
	switch claudeDesktopGOOS {
	case "darwin":
		out, err := exec.Command("pgrep", "-f", "Claude.app/Contents/MacOS/Claude").Output()
		return err == nil && strings.TrimSpace(string(out)) != ""
	case "windows":
		out, err := exec.Command("powershell.exe", "-NoProfile", "-Command", `(Get-Process claude -ErrorAction SilentlyContinue | Where-Object { $_.MainWindowHandle -ne 0 } | Select-Object -First 1).Id`).Output()
		return err == nil && strings.TrimSpace(string(out)) != ""
	default:
		return false
	}
}

func defaultClaudeDesktopOpenApp() error {
	switch claudeDesktopGOOS {
	case "windows":
		if path := claudeDesktopAppPath(); path != "" {
			return claudeDesktopOpenAppPath(path)
		}
		if path := claudeDesktopRunningAppPath(); path != "" {
			return claudeDesktopOpenAppPath(path)
		}
		return fmt.Errorf("Claude Desktop executable was not found; open Claude Desktop manually once and re-run 'oaica launch claude-desktop --restore'")
	case "darwin":
		return openClaudeDesktopDarwin()
	default:
		return claudeDesktopSupported()
	}
}

func defaultClaudeDesktopOpenAppPath(path string) error {
	switch claudeDesktopGOOS {
	case "windows":
		return exec.Command("powershell.exe", "-NoProfile", "-Command", "Start-Process -FilePath "+quotePowerShellString(path)).Run()
	case "darwin":
		return openClaudeDesktopDarwin()
	default:
		return claudeDesktopSupported()
	}
}

func openClaudeDesktopDarwin() error {
	cmd := exec.Command("open", "-a", "Claude")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func defaultClaudeDesktopRunningAppPath() string {
	if claudeDesktopGOOS != "windows" {
		return ""
	}
	script := `(Get-Process claude -ErrorAction SilentlyContinue | Where-Object { $_.MainWindowHandle -ne 0 -and $_.Path } | Select-Object -First 1 -ExpandProperty Path)`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-Command", script).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func defaultClaudeDesktopQuitApp() error {
	if claudeDesktopGOOS == "windows" {
		script := `Get-Process claude -ErrorAction SilentlyContinue | Where-Object { $_.MainWindowHandle -ne 0 } | ForEach-Object { [void]$_.CloseMainWindow() }`
		return exec.Command("powershell.exe", "-NoProfile", "-Command", script).Run()
	}
	return exec.Command("osascript", "-e", `tell application "Claude" to quit`).Run()
}

func quotePowerShellString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
