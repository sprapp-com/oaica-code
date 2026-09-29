package launch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
	"github.com/ollama/ollama/envconfig"
)

const defaultGatewayPort = 18789

// The two fields openclawEditConfig sets on the ollama provider besides its
// address. They are written on every write, so they are part of what a write
// LEAVES, and the declaration has to read them: a config the user (or another
// tool) repointed at the OpenAI wire, or gave a key of their own, read as
// current and the launch skipped the write that would have set them back
// (2026-09-27 audit, round 29, A-F2). Named once, so the writer and the
// declaration cannot drift.
const (
	openclawProviderAPI    = "ollama"
	openclawProviderAPIKey = "ollama-local"
)

// openclawFreshInstall is set to true when ensureOpenclawInstalled performs an install
var openclawFreshInstall bool

var openclawCanInstallDaemon = canInstallDaemon

type Openclaw struct{}

func (c *Openclaw) String() string { return "OpenClaw" }

// openclawRemoteRefusal is the one refusal message for a user remote. Run
// refuses the primary this way and Edit refuses any member of the selection
// this way, so the two cannot drift into telling the user different things
// about the same limitation.
func openclawRemoteRefusal(model, remote string) error {
	return fmt.Errorf("OpenClaw does not yet support user remotes (%q → %q); it relies on the Ollama native API, which is not served for user remotes. Use an OpenAI-compatible integration instead: opencode, codex, hermes, cline, droid, or kimi.", model, remote)
}

func (c *Openclaw) Run(model string, _ []LaunchModel, args []string) error {
	// OpenClaw drives the Ollama native API (/api/chat) through the daemon,
	// which the thin-client fork does not serve for user remotes — refuse early
	// with a clear message instead of a confusing daemon 404.
	if ep, ok := resolveLaunchTargetEndpoint(model); ok {
		return openclawRemoteRefusal(model, ep.Name)
	}

	bin, err := ensureOpenclawInstalled()
	if err != nil {
		return err
	}

	firstLaunch := !c.onboarded()

	if firstLaunch {
		fmt.Fprintf(os.Stderr, "\n%sSecurity%s\n\n", ansiBold, ansiReset)
		fmt.Fprintf(os.Stderr, "  OpenClaw can read files and run actions when tools are enabled.\n")
		fmt.Fprintf(os.Stderr, "  A bad prompt can trick it into doing unsafe things.\n\n")
		fmt.Fprintf(os.Stderr, "%s  Learn more: https://docs.openclaw.ai/gateway/security%s\n\n", ansiGray, ansiReset)

		ok, err := ConfirmPrompt("I understand the risks. Continue?")
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		// Ensure the latest version is installed before onboarding so we get
		// the newest wizard flags (e.g. --auth-choice ollama).
		if !openclawFreshInstall {
			update := exec.Command(bin, "update")
			update.Env = openclawInstallEnv()
			update.Stdout = os.Stdout
			update.Stderr = os.Stderr
			_ = update.Run() // best-effort; continue even if update fails
		}

		fmt.Fprintf(os.Stderr, "\n%sSetting up OpenClaw with Ollama...%s\n", ansiGreen, ansiReset)
		fmt.Fprintf(os.Stderr, "%s  Model: %s%s\n\n", ansiGray, model, ansiReset)

		onboardArgs := []string{
			"onboard",
			"--non-interactive",
			"--accept-risk",
			"--auth-choice", "ollama",
			"--custom-base-url", envconfig.ConnectableHost().String(),
			"--custom-model-id", model,
			// Launch owns the first real gateway startup immediately after onboarding,
			// so don't let OpenClaw fail the whole first-run flow on a transient
			// daemon health probe.
			"--skip-health",
			"--skip-channels",
			"--skip-skills",
		}
		if openclawCanInstallDaemon() {
			onboardArgs = append(onboardArgs, "--install-daemon")
		}
		cmd := exec.Command(bin, onboardArgs...)
		cmd.Env = openclawInstallEnv()
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return windowsHint(fmt.Errorf("openclaw onboarding failed: %w\n\nTry running: openclaw onboard", err))
		}

		patchDeviceScopes()
	}

	configureOllamaWebSearch()

	// When extra args are passed through, run exactly what the user asked for
	// after setup and skip the built-in gateway+TUI convenience flow.
	if len(args) > 0 {
		cleanup := func() {}
		if shouldEnsureGatewayForArgs(args) {
			cleanupFn, _, _, err := c.ensureGatewayReady(bin)
			if err != nil {
				return windowsHint(err)
			}
			if cleanupFn != nil {
				cleanup = cleanupFn
			}
		}
		defer cleanup()

		cmd := exec.Command(bin, args...)
		cmd.Env = openclawEnv()
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := runChild(cmd); err != nil {
			return windowsHint(err)
		}
		return nil
	}

	if err := c.runChannelSetupPreflight(bin); err != nil {
		return err
	}
	// Keep local pairing scopes up to date before the gateway lifecycle
	// (restart/start) regardless of channel preflight branch behavior.
	patchDeviceScopes()

	fmt.Fprintf(os.Stderr, "\n%sStarting your assistant — this may take a moment...%s\n\n", ansiGray, ansiReset)

	cleanup, token, port, err := c.ensureGatewayReady(bin)
	if err != nil {
		return windowsHint(err)
	}
	defer cleanup()

	printOpenclawReady(bin, token, port, firstLaunch)

	tuiArgs := []string{"tui"}
	if firstLaunch {
		tuiArgs = append(tuiArgs, "--message", "Wake up, my friend!")
	}
	tui := exec.Command(bin, tuiArgs...)
	tui.Env = openclawEnv()
	tui.Stdin = os.Stdin
	tui.Stdout = os.Stdout
	tui.Stderr = os.Stderr
	if err := runChild(tui); err != nil {
		return windowsHint(err)
	}

	return nil
}

func shouldEnsureGatewayForArgs(args []string) bool {
	return len(args) > 0 && args[0] == "tui"
}

func (c *Openclaw) ensureGatewayReady(bin string) (func(), string, int, error) {
	token, port := c.gatewayInfo()
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// If the gateway is already running (e.g. via the daemon), restart it
	// so it picks up any config changes (model, provider, etc.).
	if portOpen(addr) {
		restart := exec.Command(bin, "daemon", "restart")
		restart.Env = openclawEnv()
		if err := restart.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "%s  Warning: daemon restart failed: %v%s\n", ansiYellow, err, ansiReset)
		}
		if !waitForPort(addr, 10*time.Second) {
			fmt.Fprintf(os.Stderr, "%s  Warning: gateway did not come back after restart%s\n", ansiYellow, ansiReset)
		}
	}

	// If the daemon is installed but not currently listening, try to bring it
	// up before falling back to a foreground child process.
	if openclawCanInstallDaemon() && !portOpen(addr) {
		start := exec.Command(bin, "daemon", "start")
		start.Env = openclawEnv()
		if err := start.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "%s  Warning: daemon start failed: %v%s\n", ansiYellow, err, ansiReset)
		} else if waitForPort(addr, 10*time.Second) {
			fmt.Fprintf(os.Stderr, "%sStarting gateway...%s\n", ansiGray, ansiReset)
			return func() {}, token, port, nil
		}
	}

	cleanup := func() {}

	// If the gateway still isn't running, start it as a background child process.
	if !portOpen(addr) {
		gw := exec.Command(bin, "gateway", "run", "--force")
		gw.Env = openclawEnv()
		if err := gw.Start(); err != nil {
			return nil, "", 0, fmt.Errorf("failed to start gateway: %w", err)
		}
		cleanup = func() {
			if gw.Process != nil {
				_ = gw.Process.Kill()
				_ = gw.Wait()
			}
		}
	}

	fmt.Fprintf(os.Stderr, "%sStarting gateway...%s\n", ansiGray, ansiReset)
	if !waitForPort(addr, 30*time.Second) {
		cleanup()
		return nil, "", 0, fmt.Errorf("gateway did not start on %s", addr)
	}

	return cleanup, token, port, nil
}

// runChannelSetupPreflight prompts users to connect a messaging channel before
// starting the built-in gateway+TUI flow. In interactive sessions, it loops
// until a channel is configured, unless the user chooses "Set up later".
func (c *Openclaw) runChannelSetupPreflight(bin string) error {
	if !isInteractiveSession() {
		return nil
	}
	// --yes is headless; channel setup spawns an interactive picker we can't
	// auto-answer, so skip it. Users can run `openclaw channels add` later.
	if currentLaunchConfirmPolicy.yes {
		return nil
	}

	for {
		if c.channelsConfigured() {
			return nil
		}

		fmt.Fprintf(os.Stderr, "\nYour assistant can message you on WhatsApp, Telegram, Discord, and more.\n\n")
		ok, err := ConfirmPromptWithOptions("Connect a channel (messaging app) now?", ConfirmOptions{
			YesLabel: "Yes",
			NoLabel:  "Set up later",
		})
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		cmd := exec.Command(bin, "channels", "add")
		cmd.Env = openclawEnv()
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return windowsHint(fmt.Errorf("openclaw channel setup failed: %w\n\nTry running: %s channels add", err, bin))
		}
	}
}

// channelsConfigured reports whether local OpenClaw config contains at least
// one meaningfully configured channel entry.
func (c *Openclaw) channelsConfigured() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	for _, path := range []string{
		filepath.Join(home, ".openclaw", "openclaw.json"),
		filepath.Join(home, ".clawdbot", "clawdbot.json"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var cfg map[string]any
		if json.Unmarshal(data, &cfg) != nil {
			continue
		}

		channels, _ := cfg["channels"].(map[string]any)
		if channels == nil {
			return false
		}

		for key, value := range channels {
			if key == "defaults" || key == "modelByChannel" {
				continue
			}
			entry, ok := value.(map[string]any)
			if !ok {
				continue
			}
			for entryKey := range entry {
				if entryKey != "enabled" {
					return true
				}
			}
		}
		return false
	}

	return false
}

// gatewayInfo reads the gateway auth token and port from the OpenClaw config.
func (c *Openclaw) gatewayInfo() (token string, port int) {
	port = defaultGatewayPort
	home, err := os.UserHomeDir()
	if err != nil {
		return "", port
	}

	for _, path := range []string{
		filepath.Join(home, ".openclaw", "openclaw.json"),
		filepath.Join(home, ".clawdbot", "clawdbot.json"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var config map[string]any
		if json.Unmarshal(data, &config) != nil {
			continue
		}
		gw, _ := config["gateway"].(map[string]any)
		if p, ok := gw["port"].(float64); ok && p > 0 {
			port = int(p)
		}
		auth, _ := gw["auth"].(map[string]any)
		if t, _ := auth["token"].(string); t != "" {
			token = t
		}
		return token, port
	}
	return "", port
}

func printOpenclawReady(bin, token string, port int, firstLaunch bool) {
	u := fmt.Sprintf("http://127.0.0.1:%d", port)
	if token != "" {
		u += "/#token=" + url.QueryEscape(token)
	}

	fmt.Fprintf(os.Stderr, "\n%s✓ OpenClaw is running%s\n\n", ansiGreen, ansiReset)
	fmt.Fprintf(os.Stderr, "  Open the Web UI:\n")
	fmt.Fprintf(os.Stderr, "    %s\n\n", hyperlink(u, u))

	if firstLaunch {
		fmt.Fprintf(os.Stderr, "%s  Quick start:%s\n", ansiBold, ansiReset)
		fmt.Fprintf(os.Stderr, "%s    /help             see all commands%s\n", ansiGray, ansiReset)
		fmt.Fprintf(os.Stderr, "%s    %s skills                         browse and install skills%s\n\n", ansiGray, bin, ansiReset)
		fmt.Fprintf(os.Stderr, "%s  The OpenClaw gateway is running in the background.%s\n", ansiYellow, ansiReset)
		fmt.Fprintf(os.Stderr, "%s  Stop it with: %s gateway stop%s\n\n", ansiYellow, bin, ansiReset)
	}
}

// openclawEnv returns the environment every OpenClaw child process (the
// gateway, the TUI, `channels add`, the npm install) runs in: the launcher's
// own environment minus every credential oaica hands out or reads, so openclaw
// talks to the Ollama gateway with none of the user's keys in reach.
//
// The scrubbed set is DERIVED, not enumerated (2026-09-26 audit, round 13).
// It used to be a hard-coded list of eight provider variables, which missed
// the two the Claude Code path is careful about: the launcher's own router
// token (OAICA_API_KEY was declared as a leg's TokenEnv in tier_routing.go)
// and every configured remote's api_key_env (the auditor's repro kept
// ZAI_API_KEY while tierPlan.childEnv stripped it). The set now comes from
// tierPlan.credentialEnvNames() — the one function that decides which
// variables a launched child must not inherit — applied through
// scrubCredentialEnv, the same removal childEnv performs. Adding a remote, or
// a name to a remote's api_key_env, changes what openclaw scrubs with no edit
// here.
//
// The eight provider names are kept as well, on purpose: they are a policy
// about OPENCLAW specifically (a shell OPENAI_API_KEY must not turn openclaw
// into a direct OpenAI client), which no tier plan knows about. The catalog
// only lists a provider while its key variable is set, so the derived set
// cannot promise those names on its own.
func openclawEnv() []string {
	clear := map[string]bool{
		"ANTHROPIC_API_KEY":     true,
		"ANTHROPIC_OAUTH_TOKEN": true,
		"OPENAI_API_KEY":        true,
		"GEMINI_API_KEY":        true,
		"MISTRAL_API_KEY":       true,
		"GROQ_API_KEY":          true,
		"XAI_API_KEY":           true,
		"OPENROUTER_API_KEY":    true,
	}
	var env []string
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		if !clear[key] {
			env = append(env, e)
		}
	}
	// scrubCredentialEnv REMOVES the entries (a blanked name still tells the
	// child the variable is there), and leaves everything else — PATH, the
	// user's own tooling — untouched. Applied after the loop above so the two
	// rules read as one list.
	env = scrubCredentialEnv(env, openclawCredentialEnvNames())
	if _, ok := os.LookupEnv("OPENCLAW_PLUGIN_STAGE_DIR"); !ok {
		if dir := openclawPluginStageDir(); dir != "" {
			env = append(env, "OPENCLAW_PLUGIN_STAGE_DIR="+dir)
		}
	}
	return env
}

// openclawCredentialEnvNames is openclawEnv's derived half: every variable
// oaica itself could have read a real upstream credential from.
//
// It is computed through credentialEnvNames rather than by re-walking the
// configuration, so the two launch paths cannot drift: the router leg
// contributes the launcher's credential, and every configured remote (including
// the built-ins loadUserRemotes merges in) contributes its api_key_env — both
// names of a comma-joined spec included, which is what credentialEnvNames
// splits for the Claude Code path too.
//
// Built here, per call, rather than taken from a launch's plan: openclawEnv
// runs on every child process this integration spawns, several of them on
// paths that never resolve a model, and this is a file read either way.
func openclawCredentialEnvNames() []string {
	plan := tierPlan{
		// The router leg. The name is the one tier_routing.go declares as that
		// leg's TokenEnv; openclaw_env_credential_integrity_test.go pins the
		// two together by resolving the router through the real code.
		Primary: launchEndpoint{
			Source:         sourceRouter,
			RemoteEndpoint: RemoteEndpoint{Name: "oaica", TokenEnv: "OAICA_API_KEY"},
		},
	}
	remotes, err := loadUserRemotes()
	if err != nil {
		// A remotes.json that will not parse still must not leak the launcher's own
		// credential (the router leg above is already named) NOR the built-in
		// providers': every other reader of the store falls back to them on this
		// error and they stay launchable, so one typo or a truncated write left
		// their real keys in the environment of every child (2026-09-29 audit,
		// round 111, F111-L2-1).
		remotes = builtinRemotes()
	}
	for _, r := range remotes {
		// APIKeyEnv is the row's raw (possibly comma-joined) spec, which is
		// what credentialEnvNames splits; TokenEnv would add only the one name
		// keyEnvName happened to pick. No model, no base URL: nothing here is
		// served, this plan exists for its credential names.
		plan.Routes.Fallbacks = append(plan.Routes.Fallbacks, routeFor(launchEndpoint{
			Source:         sourceUserRemote,
			RemoteEndpoint: RemoteEndpoint{Name: r.Name, APIKeyEnv: r.APIKeyEnv},
		}))
	}
	return plan.credentialEnvNames()
}

func openclawInstallEnv() []string {
	env := openclawEnv()
	if _, ok := os.LookupEnv("OPENCLAW_EAGER_BUNDLED_PLUGIN_DEPS"); !ok {
		env = append(env, "OPENCLAW_EAGER_BUNDLED_PLUGIN_DEPS=1")
	}
	return env
}

func openclawPluginStageDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".openclaw", "plugin-runtime-deps")
}

// portOpen checks if a TCP port is currently accepting connections.
func portOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func waitForPort(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func windowsHint(err error) error {
	if runtime.GOOS != "windows" {
		return err
	}
	return fmt.Errorf("%w\n\n"+
		"OpenClaw runs best on WSL2.\n"+
		"Quick setup: wsl --install\n"+
		"Guide: https://docs.openclaw.ai/windows", err)
}

// onboarded checks if OpenClaw onboarding wizard was completed
// by looking for the wizard.lastRunAt marker in the config
func (c *Openclaw) onboarded() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	configPath := filepath.Join(home, ".openclaw", "openclaw.json")
	legacyPath := filepath.Join(home, ".clawdbot", "clawdbot.json")

	config := make(map[string]any)
	if data, err := os.ReadFile(configPath); err == nil {
		_ = json.Unmarshal(data, &config)
	} else if data, err := os.ReadFile(legacyPath); err == nil {
		_ = json.Unmarshal(data, &config)
	} else {
		return false
	}

	// Check for wizard.lastRunAt marker (set when onboarding completes)
	wizard, _ := config["wizard"].(map[string]any)
	if wizard == nil {
		return false
	}
	lastRunAt, _ := wizard["lastRunAt"].(string)
	return lastRunAt != ""
}

// patchDeviceScopes upgrades the local CLI device's paired operator scopes so
// newer gateway auth baselines (approvedScopes) allow launch+TUI reconnects
// without forcing an interactive re-pair. Only patches the local device,
// not remote ones. Best-effort: silently returns on any error.
func patchDeviceScopes() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".openclaw", "devices", "paired.json")
	// The gateway's pairing record, rewritten whole from a snapshot: the read of
	// the device list and the publish of the patched copy take the store's lock,
	// so two oaica launches cannot drop each other's pairing entries. Note this
	// only orders oaica against oaica — the gateway itself writes this file
	// without knowing about the lock. Best-effort, as the rest of this
	// integration is.
	_ = fileutil.WithFileLock(foreignStoreLockBase(path), func() error {
		patchDeviceScopesLocked(home, path)
		return nil
	})
}

func patchDeviceScopesLocked(home, path string) {
	deviceID := readLocalDeviceID(home)
	if deviceID == "" {
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var devices map[string]map[string]any
	// json.Number, so the token timestamps and ids in the gateway's record
	// come back as the text they went in as rather than a rounded float64.
	{
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&devices); err != nil {
			return
		}
	}

	dev, ok := devices[deviceID]
	if !ok || dev == nil {
		// A member that is JSON `null` EXISTS as a key and reads as a nil map,
		// and patchScopes writes into the map it is handed: without the nil
		// check this best-effort helper — the one that is supposed to return
		// quietly on every failure — took the launch down with "assignment to
		// entry in nil map" (2026-09-27 audit, round 20).
		return
	}

	required := []string{
		"operator.read",
		"operator.admin",
		"operator.approvals",
		"operator.pairing",
	}

	changed := patchScopes(dev, "scopes", required)
	if patchScopes(dev, "approvedScopes", required) {
		changed = true
	}
	if tokens, ok := dev["tokens"].(map[string]any); ok {
		for role, tok := range tokens {
			if tokenMap, ok := tok.(map[string]any); ok {
				if !isOperatorToken(role, tokenMap) {
					continue
				}
				if patchScopes(tokenMap, "scopes", required) {
					changed = true
				}
			}
		}
	}

	if !changed {
		return
	}

	out, err := json.MarshalIndent(devices, "", "  ")
	if err != nil {
		return
	}
	// A backup, like every other write to this daemon's files: this function
	// has no way to report a bad write, so it must not be the one write that
	// leaves no copy behind.
	_ = fileutil.WriteWithBackup(path, out, "openclaw")
}

// readLocalDeviceID reads the local device ID from openclaw's identity file.
func readLocalDeviceID(home string) string {
	data, err := os.ReadFile(filepath.Join(home, ".openclaw", "identity", "device-auth.json"))
	if err != nil {
		return ""
	}
	var auth map[string]any
	if err := json.Unmarshal(data, &auth); err != nil {
		return ""
	}
	id, _ := auth["deviceId"].(string)
	return id
}

// patchScopes ensures obj[key] contains all required scopes. Returns true if
// any scopes were added.
func patchScopes(obj map[string]any, key string, required []string) bool {
	existing, _ := obj[key].([]any)
	have := make(map[string]bool, len(existing))
	for _, s := range existing {
		if str, ok := s.(string); ok {
			have[str] = true
		}
	}
	added := false
	for _, s := range required {
		if !have[s] {
			existing = append(existing, s)
			added = true
		}
	}
	if added {
		obj[key] = existing
	}
	return added
}

func isOperatorToken(tokenRole string, token map[string]any) bool {
	if strings.EqualFold(strings.TrimSpace(tokenRole), "operator") {
		return true
	}
	role, _ := token["role"].(string)
	return strings.EqualFold(strings.TrimSpace(role), "operator")
}

// canInstallDaemon reports whether the openclaw daemon can be installed as a
// background service. Returns false on Linux when systemd is absent (e.g.
// containers) so that --install-daemon is omitted and the gateway is started
// as a foreground child process instead. Returns true in all other cases.
func canInstallDaemon() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	// /run/systemd/system exists as a directory when systemd is the init system.
	// This is absent in most containers.
	fi, err := os.Stat("/run/systemd/system")
	if err != nil || !fi.IsDir() {
		return false
	}
	// Even when systemd is the init system, user services require a user
	// manager instance. XDG_RUNTIME_DIR being set is a prerequisite.
	return os.Getenv("XDG_RUNTIME_DIR") != ""
}

func ensureOpenclawInstalled() (string, error) {
	if _, err := exec.LookPath("openclaw"); err == nil {
		return "openclaw", nil
	}
	if _, err := exec.LookPath("clawdbot"); err == nil {
		return "clawdbot", nil
	}

	_, npmErr := exec.LookPath("npm")
	_, gitErr := exec.LookPath("git")
	if npmErr != nil || gitErr != nil {
		var missing []string
		if npmErr != nil {
			missing = append(missing, "npm (Node.js): https://nodejs.org/")
		}
		if gitErr != nil {
			missing = append(missing, "git: https://git-scm.com/")
		}
		return "", fmt.Errorf("OpenClaw is not installed and required dependencies are missing\n\nInstall the following first:\n  %s\n\nThen re-run:\n  oaica launch openclaw", strings.Join(missing, "\n  "))
	}

	ok, err := ConfirmPrompt("OpenClaw is not installed. Install with npm?")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("openclaw installation cancelled")
	}

	fmt.Fprintf(os.Stderr, "\nInstalling OpenClaw...\n")
	cmd := exec.Command("npm", "install", "-g", "openclaw@latest")
	cmd.Env = openclawInstallEnv()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to install openclaw: %w", err)
	}

	if _, err := exec.LookPath("openclaw"); err != nil {
		return "", fmt.Errorf("openclaw was installed but the binary was not found on PATH\n\nYou may need to restart your shell")
	}

	fmt.Fprintf(os.Stderr, "%sOpenClaw installed successfully%s\n\n", ansiGreen, ansiReset)
	openclawFreshInstall = true
	return "openclaw", nil
}

func (c *Openclaw) Paths() []string {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".openclaw", "openclaw.json")
	if _, err := os.Stat(p); err == nil {
		return []string{p}
	}
	legacy := filepath.Join(home, ".clawdbot", "clawdbot.json")
	if _, err := os.Stat(legacy); err == nil {
		return []string{legacy}
	}
	return nil
}

func (c *Openclaw) Edit(models []LaunchModel) error {
	if len(models) == 0 {
		return nil
	}

	// Run refuses a user remote, but Edit runs first and used to write the
	// whole config regardless: openclawEditConfig replaced
	// models.providers.ollama.models with the selection and set
	// agents.defaults.model.primary to the picker name, then Run refused and
	// no launch happened — leaving OpenClaw configured for a model the daemon
	// cannot serve, its primary pointing at a namespaced name that resolves
	// nowhere. The refusal belongs where the user can still act on it, before
	// the file is touched (2026-09-26 audit, round 16; same shape as
	// museRejectNonDaemonModels).
	for _, m := range models {
		if ep, ok := resolveRemoteEndpoint(m.Name); ok {
			return openclawRemoteRefusal(m.Name, ep.Name)
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	configPath := filepath.Join(home, ".openclaw", "openclaw.json")
	legacyPath := filepath.Join(home, ".clawdbot", "clawdbot.json")

	// openclaw.json is OpenClaw's document and oaica models one part of it: the
	// read below, the merge, and the whole-document write have to run under the
	// same lock every other oaica writer of this file takes
	// (configureOllamaWebSearch, and a concurrent `oaica launch openclaw`), or
	// two commands each publish a snapshot taken before the other's entry and
	// the rename that lands last deletes it while both report success
	// (2026-09-26 audit, twelfth round). The lock is keyed off the file oaica
	// publishes to, not off the legacy path it may have read from: that is the
	// path every writer of this store resolves to, and the lock's identity is
	// what makes two processes meet.
	if err := fileutil.WithFileLock(foreignStoreLockBase(configPath), func() error {
		return openclawEditConfig(configPath, legacyPath, models)
	}); err != nil {
		return err
	}

	// Clear any per-session model overrides so the new primary takes effect
	// immediately rather than being shadowed by a cached modelOverride. Its own
	// file, so its own lock — taken after this one is released, to keep a single
	// lock order (config, then session state) between any two oaica commands.
	clearSessionModelOverride(openclawPrimaryModel(models[0]))
	return nil
}

func openclawEditConfig(configPath, legacyPath string, models []LaunchModel) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}

	// Read into map[string]any to preserve unknown fields
	config := make(map[string]any)
	readPath := configPath
	data, err := os.ReadFile(configPath)
	if err != nil {
		readPath = legacyPath
		data, err = os.ReadFile(legacyPath)
	}
	if err == nil && len(bytes.TrimSpace(data)) > 0 {
		// A decode failure must STOP the write, not be treated as "empty". The
		// file belongs to OpenClaw and oaica models only a part of it, so
		// writing over a document it could not read deletes everything it
		// could not parse — the gateway token, channels, plugins, the wizard
		// marker (whose loss makes the next launch re-run onboarding over a
		// config the user already had). A partially-decoded map is worse than
		// an empty one: Go inserts keys as it goes, so what gets written back
		// is the half of the file that happened to come before the syntax
		// error (2026-09-26 audit). Same stance as muse.go, and as
		// configureOllamaWebSearch below.
		// decodeJSONObject, not a bare Decode: a document that IS `null`
		// decodes into a nil map, and the write of models.providers below then
		// panics with it (2026-09-27 audit, round 20 — the same guard the
		// web-search writer of this file already took in round 19).
		doc, derr := decodeJSONObject(data)
		if derr != nil {
			return fmt.Errorf("refusing to update %s: it is not valid JSON (%v) — oaica models only part of that file, so rewriting what it cannot read would delete the rest of your OpenClaw configuration", readPath, derr)
		}
		config = doc
	}

	// Navigate/create: models.providers.ollama (preserving other providers)
	modelsSection, _ := config["models"].(map[string]any)
	if modelsSection == nil {
		modelsSection = make(map[string]any)
	}
	providers, _ := modelsSection["providers"].(map[string]any)
	if providers == nil {
		providers = make(map[string]any)
	}
	ollama, _ := providers["ollama"].(map[string]any)
	if ollama == nil {
		ollama = make(map[string]any)
	}

	ollama["baseUrl"] = envconfig.ConnectableHost().String()
	// needed to register provider
	ollama["apiKey"] = openclawProviderAPIKey
	ollama["api"] = openclawProviderAPI

	// Build map of existing models to preserve user customizations. Keyed by the
	// entry's "id", which openclawModelID writes — the same value the merge and
	// the "written" set below use, so a row is matched to its own previous
	// entry rather than to one whose picker label happened to match.
	existingModels, _ := ollama["models"].([]any)
	existingByID := make(map[string]map[string]any)
	for _, m := range existingModels {
		if entry, ok := m.(map[string]any); ok {
			if id, ok := entry["id"].(string); ok {
				existingByID[id] = entry
			}
		}
	}

	var newModels []any
	written := make(map[string]bool, len(models))
	// openclawStoredRows, not the selection as it came: the provider's list is
	// keyed by id, so two rows naming one id used to publish two entries
	// OpenClaw cannot tell apart (2026-09-27 audit, round 27, F6). The same
	// helper answers DeclaresSelection, so the list written and the list
	// compared against are built by one rule.
	for _, m := range openclawStoredRows(models) {
		id := openclawModelID(m)
		entry, _ := openclawModelConfig(m)
		// Merge existing fields (user customizations)
		if existing, ok := existingByID[id]; ok {
			for k, v := range existing {
				if _, isNew := entry[k]; !isNew {
					entry[k] = v
				}
			}
		}
		written[id] = true
		newModels = append(newModels, entry)
	}
	// Rows this launch did not write stay in the file, unless they are rows a
	// previous launch wrote: the provider's list is oaica's to replace (a pinned
	// contract — a deselected model must leave), but it is also where a user's
	// own local models live, and publishing the selection alone deleted every
	// one of them on an otherwise ordinary launch (2026-09-27 audit, round 21).
	// openclawModelConfig always writes a "cost" object; an entry without one
	// was not written by oaica, so it is carried over as it stands. An entry
	// with no id cannot be matched to a launched model and is kept whole — it
	// used to be dropped by the same test that skips an id this launch wrote,
	// which deleted rows the comment here promised to keep (2026-09-27 audit,
	// round 22).
	for _, raw := range existingModels {
		entry, ok := raw.(map[string]any)
		if !ok {
			newModels = append(newModels, raw)
			continue
		}
		id, _ := entry["id"].(string)
		if id == "" {
			newModels = append(newModels, entry)
			continue
		}
		if written[id] {
			continue
		}
		if _, ours := entry["cost"]; ours {
			continue
		}
		newModels = append(newModels, entry)
	}
	ollama["models"] = newModels

	providers["ollama"] = ollama
	modelsSection["providers"] = providers
	config["models"] = modelsSection

	// Update agents.defaults.model.primary (preserving other agent settings)
	agents, _ := config["agents"].(map[string]any)
	if agents == nil {
		agents = make(map[string]any)
	}
	defaults, _ := agents["defaults"].(map[string]any)
	if defaults == nil {
		defaults = make(map[string]any)
	}
	modelConfig, _ := defaults["model"].(map[string]any)
	if modelConfig == nil {
		modelConfig = make(map[string]any)
	}
	modelConfig["primary"] = openclawPrimaryModel(models[0])
	defaults["model"] = modelConfig
	agents["defaults"] = defaults
	config["agents"] = agents

	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := fileutil.WriteWithBackup(configPath, out, "openclaw"); err != nil {
		return err
	}
	return nil
}

// clearSessionModelOverride removes per-session model overrides from the main
// agent session so the global primary model takes effect on the next TUI launch.
func clearSessionModelOverride(primary string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".openclaw", "agents", "main", "sessions", "sessions.json")
	// Session state is a document of OpenClaw's that oaica rewrites whole, so the
	// read and the publish belong under the same lock as any other oaica writer
	// of it — without one, a session record another command had just updated is
	// dropped by the publish of a snapshot taken before it (2026-09-26 audit).
	// Best-effort, like every write in this integration: a lock that cannot be
	// placed means no write, not a failed launch.
	_ = fileutil.WithFileLock(foreignStoreLockBase(path), func() error {
		clearSessionModelOverrideLocked(path, primary)
		return nil
	})
}

func clearSessionModelOverrideLocked(path, primary string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var sessions map[string]map[string]any
	// json.Number, as in patchDeviceScopes: session state carries timestamps.
	{
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&sessions); err != nil {
			return
		}
	}
	changed := false
	for _, sess := range sessions {
		if override, _ := sess["modelOverride"].(string); override != "" && override != primary {
			delete(sess, "modelOverride")
			delete(sess, "providerOverride")
			// This branch is the function's whole reason to exist, and it used
			// to leave `changed` false: with a session whose "model" already
			// equalled the new primary (so the branch below does not co-fire)
			// the deletion was discarded at `if !changed { return }` and the
			// stale override survived on disk, shadowing the primary the user
			// had just chosen on the next TUI launch (2026-09-26 audit, sixth
			// round).
			changed = true
		}
		if model, _ := sess["model"].(string); model != "" && model != primary {
			sess["model"] = primary
			changed = true
		}
	}
	if !changed {
		return
	}
	out, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return
	}
	_ = fileutil.WriteWithBackup(path, out, "openclaw")
}

// configureOllamaWebSearch keeps launch-managed OpenClaw installs on the
// bundled Ollama web_search provider. Older launch builds installed an
// external openclaw-web-search plugin that added custom ollama_web_search and
// ollama_web_fetch tools. Current OpenClaw versions ship Ollama web_search as
// the bundled "ollama" plugin instead, so we migrate stale config and ensure
// fresh installs select the bundled provider.
func configureOllamaWebSearch() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	configPath := filepath.Join(home, ".openclaw", "openclaw.json")
	// The same file Openclaw.Edit rewrites, and the same read-modify-publish
	// shape: this rewrites whole sections of OpenClaw's config from a snapshot,
	// so it takes the same lock, or the two commands publish over each other
	// (2026-09-26 audit, twelfth round). Best-effort: if the lock cannot be
	// placed, nothing is written and the launch continues.
	_ = fileutil.WithFileLock(foreignStoreLockBase(configPath), func() error {
		configureOllamaWebSearchLocked(configPath)
		return nil
	})
}

func configureOllamaWebSearchLocked(configPath string) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}
	var config map[string]any
	// UseNumber: the document is rewritten wholesale below, and decoding into
	// map[string]any turns every number into a float64 — a config holding an
	// integer larger than 2^53 came back with a different value, silently
	// changed by a command that meant to touch one plugin entry
	// (2026-09-26 audit).
	// decodeJSONObject, not a bare Decode: a document that IS `null` decodes
	// into a nil map, and this function finishes by writing config["plugins"]
	// and config["tools"] into it (2026-09-27 audit, round 19).
	doc, derr := decodeJSONObject(data)
	if derr != nil {
		return
	}
	config = doc

	stalePluginConfigured := false

	plugins, _ := config["plugins"].(map[string]any)
	if plugins == nil {
		plugins = make(map[string]any)
	}
	entries, _ := plugins["entries"].(map[string]any)
	if entries == nil {
		entries = make(map[string]any)
	}
	tools, _ := config["tools"].(map[string]any)
	if tools == nil {
		tools = make(map[string]any)
	}
	web, _ := tools["web"].(map[string]any)
	if web == nil {
		web = make(map[string]any)
	}
	search, _ := web["search"].(map[string]any)
	if search == nil {
		search = make(map[string]any)
	}
	fetch, _ := web["fetch"].(map[string]any)
	if fetch == nil {
		fetch = make(map[string]any)
	}

	alsoAllow, _ := tools["alsoAllow"].([]any)
	var filteredAlsoAllow []any
	for _, v := range alsoAllow {
		s, ok := v.(string)
		if !ok {
			filteredAlsoAllow = append(filteredAlsoAllow, v)
			continue
		}
		if s == "ollama_web_search" || s == "ollama_web_fetch" {
			stalePluginConfigured = true
			continue
		}
		filteredAlsoAllow = append(filteredAlsoAllow, v)
	}
	if len(filteredAlsoAllow) > 0 {
		tools["alsoAllow"] = filteredAlsoAllow
	} else {
		delete(tools, "alsoAllow")
	}

	if _, ok := entries["openclaw-web-search"]; ok {
		delete(entries, "openclaw-web-search")
		stalePluginConfigured = true
	}
	ollamaEntry, _ := entries["ollama"].(map[string]any)
	if ollamaEntry == nil {
		ollamaEntry = make(map[string]any)
	}
	// "enabled" is written only where it is missing, or where an earlier launch
	// left the stale plugin shape behind and this is the repair. The entry is a
	// plugin OpenClaw itself knows, and a value already there is one the user
	// set: re-enabling it on every launch silently undid the choice
	// (2026-09-27 audit, round 21, F12).
	// "enabled" is written only where it is missing, or where an earlier launch
	// left the stale plugin shape behind and this is the repair. The entry is a
	// plugin OpenClaw itself knows, and a value already there is one the user
	// set: re-enabling it on every launch silently undid the choice
	// (2026-09-27 audit, round 21, F12).
	if _, exists := ollamaEntry["enabled"]; !exists || stalePluginConfigured {
		ollamaEntry["enabled"] = true
	}
	entries["ollama"] = ollamaEntry
	plugins["entries"] = entries

	if allow, ok := plugins["allow"].([]any); ok {
		var nextAllow []any
		hasOllama := false
		for _, v := range allow {
			s, ok := v.(string)
			if ok && s == "openclaw-web-search" {
				stalePluginConfigured = true
				continue
			}
			if ok && s == "ollama" {
				hasOllama = true
			}
			nextAllow = append(nextAllow, v)
		}
		if !hasOllama {
			nextAllow = append(nextAllow, "ollama")
		}
		plugins["allow"] = nextAllow
	}

	if installs, ok := plugins["installs"].(map[string]any); ok {
		if _, exists := installs["openclaw-web-search"]; exists {
			delete(installs, "openclaw-web-search")
			stalePluginConfigured = true
		}
		if len(installs) > 0 {
			plugins["installs"] = installs
		} else {
			delete(plugins, "installs")
		}
	}

	if stalePluginConfigured || search["provider"] == nil {
		search["provider"] = "ollama"
	}
	if stalePluginConfigured {
		fetch["enabled"] = true
	}
	// As above: a "search" tool the user turned off stays off. What this
	// integration owns is the PROVIDER behind it, not whether search runs at
	// all — and a launch that flips the switch back on every time makes the
	// user's setting unchangeable (2026-09-27 audit, round 21, F12).
	// As above: a "search" tool the user turned off stays off. What this
	// integration owns is the PROVIDER behind it, not whether search runs at
	// all — and a launch that flips the switch back on every time makes the
	// user's setting unchangeable (2026-09-27 audit, round 21, F12).
	if stalePluginConfigured {
		search["enabled"] = true
	} else if _, exists := search["enabled"]; !exists {
		search["enabled"] = true
	}
	web["search"] = search
	if len(fetch) > 0 {
		web["fetch"] = fetch
	}
	tools["web"] = web
	config["plugins"] = plugins
	config["tools"] = tools

	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return
	}
	// Nothing to say? Then say nothing. This ran on every launch and rewrote
	// the file unconditionally, replacing the inode and moving the mtime of a
	// config the OpenClaw daemon owns — a rewrite nobody asked for, and one
	// whose only effect was to make the daemon's own concurrent edit losable
	// (2026-09-26 audit). The comparison is on the marshalled document, so a
	// config that is merely formatted differently is still left alone.
	if before, err := json.Marshal(configOnDisk(data)); err == nil {
		if after, err := json.Marshal(config); err == nil && bytes.Equal(before, after) {
			return
		}
	}
	// WriteWithBackup, like every other write in this integration
	// (Openclaw.Edit): this is another tool's config, and the one write here
	// that kept no copy was the one that ran on every launch (2026-09-26
	// audit). It also skips the write outright when the bytes are identical.
	_ = fileutil.WriteWithBackup(configPath, append(out, '\n'), "openclaw")
}

// configOnDisk re-reads a config document into the same shape the caller
// mutates, so the two can be compared semantically (numbers as json.Number on
// both sides; a decode failure yields an empty map, which never compares equal
// to a real config and so never suppresses a write it cannot verify).
func configOnDisk(data []byte) map[string]any {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&m) != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// openclawModelID is the id OpenClaw's ollama provider serves a row as: the
// daemon-side id of an ollama-cloud catalogue row (LaunchModel.Upstream —
// "gpt-oss:cloud"), whose picker label names the LOCAL model of that name
// instead. It is also the key this file uses to merge the user's own fields
// back into the row and to recognise the rows a previous launch wrote (see
// openclawEditConfig), so the entry's "id" and that key have to be one value
// (2026-09-27 audit, round 25).
func openclawModelID(model LaunchModel) string { return launchModelWriteID(model) }

// openclawPrimaryModel is the string OpenClaw's agents.defaults.model.primary
// holds for a row: the provider's own namespace, then the id the provider
// DECLARES it as. It is one function because the two places that need it — the
// config writer and the session-override clear — read from the same session
// state, whose "model" field holds this exact spelling (see the
// "ollama/old-model" fixtures in openclaw_test.go). Writing the entry as the
// Upstream id while pointing primary at the picker label would name a model its
// own provider list no longer declares: "ollama/gpt-oss" for an entry declared
// "gpt-oss:cloud" (2026-09-27 audit, round 26).
func openclawPrimaryModel(model LaunchModel) string { return "ollama/" + openclawModelID(model) }

// openclawModelConfig builds an OpenClaw model config entry with capability detection.
// The second return value indicates whether the model is a cloud (remote) model.
//
// The entry's "id" is the id the provider serves the row AS (openclawModelID);
// "name" stays the picker label, which is only a display string to OpenClaw.
func openclawModelConfig(model LaunchModel) (map[string]any, bool) {
	entry := map[string]any{
		"id":    openclawModelID(model),
		"name":  model.Name,
		"input": []any{"text"},
		"cost": map[string]any{
			"input":      0,
			"output":     0,
			"cacheRead":  0,
			"cacheWrite": 0,
		},
	}

	// Set input types based on vision capability
	if model.HasCapability("vision") {
		entry["input"] = []any{"text", "image"}
	}

	// Set reasoning based on thinking capability
	if model.HasCapability("thinking") {
		entry["reasoning"] = true
	}

	if model.ContextLength > 0 {
		entry["contextWindow"] = model.ContextLength
	}
	if model.MaxOutputTokens > 0 {
		entry["maxTokens"] = model.MaxOutputTokens
	}

	return entry, model.Remote || isCloudModelName(model.Name)
}

func (c *Openclaw) Models() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	config, err := openclawReadConfig(home)
	if err != nil {
		return nil
	}
	return openclawProviderModelIDs(config)
}

// openclawReadConfig reads whichever config OpenClaw is using: its own, or the
// legacy path an older install left behind. One reader, so the two callers that
// ask what the file holds (Models and DeclaresSelection) cannot read different
// files.
func openclawReadConfig(home string) (map[string]any, error) {
	config, err := fileutil.ReadJSON(filepath.Join(home, ".openclaw", "openclaw.json"))
	if err != nil {
		config, err = fileutil.ReadJSON(filepath.Join(home, ".clawdbot", "clawdbot.json"))
		if err != nil {
			return nil, err
		}
	}
	return config, nil
}

// openclawProviderModelIDs reads the ids of the entries under OpenClaw's ollama
// provider, in file order — the ids the writer puts there (openclawModelID).
func openclawProviderModelIDs(config map[string]any) []string {
	modelsSection, _ := config["models"].(map[string]any)
	providers, _ := modelsSection["providers"].(map[string]any)
	ollama, _ := providers["ollama"].(map[string]any)
	modelList, _ := ollama["models"].([]any)

	var result []string
	for _, m := range modelList {
		if entry, ok := m.(map[string]any); ok {
			if id, ok := entry["id"].(string); ok {
				result = append(result, id)
			}
		}
	}
	return result
}

// openclawProviderBaseURL reads models.providers.ollama.baseUrl — the address
// OpenClaw dials for this provider. It is the value openclawEditConfig writes
// from ConnectableHost(), and the one the declaration has to compare against:
// the ids, the primary and the session file say WHICH models the app offers,
// never WHERE it reaches them.
func openclawProviderBaseURL(config map[string]any) string {
	modelsSection, _ := config["models"].(map[string]any)
	providers, _ := modelsSection["providers"].(map[string]any)
	ollama, _ := providers["ollama"].(map[string]any)
	baseURL, _ := ollama["baseUrl"].(string)
	return baseURL
}

// openclawProviderIsOurs reports whether the ollama provider block carries the
// two fields openclawEditConfig writes beside baseUrl: the wire ("api") and the
// placeholder key ("apiKey"). Both are set on every write, so a block holding
// anything else — a user's own key, another wire — is not a block this launch
// would leave, and the launch has to write. Read from the same constants the
// writer sets, so a rename cannot silently make one of them drift.
func openclawProviderIsOurs(config map[string]any) bool {
	modelsSection, _ := config["models"].(map[string]any)
	providers, _ := modelsSection["providers"].(map[string]any)
	ollama, _ := providers["ollama"].(map[string]any)
	api, _ := ollama["api"].(string)
	apiKey, _ := ollama["apiKey"].(string)
	return api == openclawProviderAPI && apiKey == openclawProviderAPIKey
}

// openclawStoredRows is the selection as a write leaves it at the head of
// OpenClaw's provider: the rows in order, each id once. Two rows that name the
// same backend — a catalogue row and the daemon row of the id it is served as —
// used to be written twice, which the provider's own list, keyed by id, cannot
// tell apart (2026-09-27 audit, round 27, F6). The writer and
// DeclaresSelection both build their list from this, so what gets published
// and what counts as already published are one rule.
func openclawStoredRows(models []LaunchModel) []LaunchModel {
	seen := make(map[string]bool, len(models))
	rows := make([]LaunchModel, 0, len(models))
	for _, m := range models {
		id := openclawModelID(m)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		rows = append(rows, m)
	}
	return rows
}

// openclawStoredIDs is openclawStoredRows' ids, in the same order.
func openclawStoredIDs(models []LaunchModel) []string {
	rows := openclawStoredRows(models)
	ids := make([]string, 0, len(rows))
	for _, m := range rows {
		ids = append(ids, openclawModelID(m))
	}
	return ids
}

// openclawPrimaryIs reports whether the default agent's primary is this string.
func openclawPrimaryIs(config map[string]any, primary string) bool {
	agents, _ := config["agents"].(map[string]any)
	defaults, _ := agents["defaults"].(map[string]any)
	modelConfig, _ := defaults["model"].(map[string]any)
	got, _ := modelConfig["primary"].(string)
	return got == primary
}

// openclawSessionsAgree reports whether every session in OpenClaw's main-agent
// session state already names the primary this launch would write. It is the
// same document, read by the same rule, that clearSessionModelOverride
// rewrites, so a stale session model or a stale override is drift here exactly
// when Edit would fix it: a session left naming another model shadows the
// primary on the next TUI launch, which is the state F1 was about — a store
// that "declares the selection" while the app still runs something else
// (2026-09-27 audit, round 27).
func openclawSessionsAgree(home, primary string) bool {
	path := filepath.Join(home, ".openclaw", "agents", "main", "sessions", "sessions.json")
	data, err := os.ReadFile(path)
	if err != nil {
		// No session state at all: nothing shadows the primary.
		return os.IsNotExist(err)
	}
	var sessions map[string]map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&sessions); err != nil {
		// Session state that cannot be read is not state that agrees: say so,
		// so the launch writes what it can rather than leaving a file it
		// could not read as the source of truth.
		return false
	}
	for _, sess := range sessions {
		if override, _ := sess["modelOverride"].(string); override != "" && override != primary {
			return false
		}
		if model, _ := sess["model"].(string); model != "" && model != primary {
			return false
		}
	}
	return true
}

// DeclaresSelection reports whether OpenClaw's config already holds what a
// write of models would leave: the provider's model list begins with exactly
// the ids the write would publish — rows after them are the user's own models,
// which the writer carries over (openclawEditConfig) — and the two fields the
// same write owns, agents.defaults.model.primary and the session state that
// shadows it, already name the first of them.
//
// The list alone was read as the answer, and it is not the store: the primary
// is a second field of the same write, so a config left pointing at a model
// this launch did not choose read as current, Edit was skipped, and the app
// kept running that model (2026-09-27 audit, round 27, F1). The list is read as
// a PREFIX and not as an equality for the mirror-image reason: the rows after
// the selection are the user's own models, which are not drift.
func (c *Openclaw) DeclaresSelection(models []LaunchModel) bool {
	want := openclawStoredIDs(models)
	if len(want) == 0 {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	config, err := openclawReadConfig(home)
	if err != nil {
		return false
	}
	// The provider's address is part of what the store says: OpenClaw dials
	// baseUrl, and a config left on a since-moved daemon (OLLAMA_HOST is read
	// live, so moving the daemon is the documented case) is a store that does
	// not hold this selection. Without this the declaration answered true, the
	// launch skipped its rewrite, and the app kept talking to the old endpoint
	// (2026-09-27 audit, round 28, F1). Compared against the writer's own
	// source, so the two cannot drift.
	if openclawProviderBaseURL(config) != envconfig.ConnectableHost().String() {
		return false
	}
	// The provider block's other two fields, for the same reason: the write sets
	// them on every launch, so a block holding a user's key or another wire is
	// one the write would change (2026-09-27 audit, round 29, A-F2).
	if !openclawProviderIsOurs(config) {
		return false
	}
	if !declaresPrefix(openclawProviderModelIDs(config), want) {
		return false
	}
	primary := openclawPrimaryModel(models[0])
	if !openclawPrimaryIs(config, primary) {
		return false
	}
	return openclawSessionsAgree(home, primary)
}
