package launch

// remote_cli.go — `oaica remote list/add/show/rm` handlers, deliberately the
// same shape as model_manifest_cli.go's `oaica model` verbs (thin functions
// over load/save so cmd/cmd.go stays a one-line RunE per verb).
//
// These read and write the EXISTING ~/.oaica/remotes.json defined in
// user_remotes.go — the on-disk schema is unchanged and built-in providers
// (ollama-cloud, zai, openrouter) keep working. The only thing this adds is a
// supported way to edit the file, which the 0.4.6 fresh-user audit (P1-4)
// flagged as hand-edit-JSON-only.
//
// API keys are NEVER printed: the auth column shows `key` (stored in the
// file), `env:<VAR>` (read from the environment at use time) or `none`.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RemoteAddOptions is the parsed form of `oaica remote add`'s flags.
type RemoteAddOptions struct {
	Name       string
	BaseURL    string
	APIKey     string
	APIKeyEnv  string
	Wire       string
	ToolFormat string
	Version    string
}

// loadUserRemotesFileRaw reads remotes.json WITHOUT merging builtinRemotes():
// only entries that actually live in the file may be rewritten, otherwise a
// `remote add` would silently bake the built-in providers into the file and
// freeze whatever their env vars happened to be.
func loadUserRemotesFileRaw() (userRemotesFile, string, error) {
	path := userRemotesPath()
	if path == "" {
		return userRemotesFile{}, "", fmt.Errorf("cannot locate ~/.oaica/remotes.json (no home directory) — set OAICA_REMOTES_FILE")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return userRemotesFile{}, path, nil // a missing file is normal
		}
		return userRemotesFile{}, path, err
	}
	var f userRemotesFile
	if err := json.Unmarshal(b, &f); err != nil {
		return userRemotesFile{}, path, fmt.Errorf("%s: %w", path, err)
	}
	return f, path, nil
}

func saveUserRemotesFile(f userRemotesFile, path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	// 0o600: this file may hold plaintext bearer tokens (--api-key).
	// Chmod AFTER the write too (2026-09-01 security audit M2): the mode arg
	// only applies at creation — a pre-existing world-readable file (observed
	// 0664 live, plaintext api_key inside) stayed readable by every local
	// user forever.
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

var (
	validRemoteWires       = []string{"openai", "anthropic"}
	validRemoteToolFormats = []string{"tool_calls", "freeform", "xml", "none"}
)

func oneOf(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// RemoteAdd creates or replaces an entry in remotes.json. Returns the entry
// written so the caller can print a confirmation.
func RemoteAdd(opts RemoteAddOptions) (userRemote, error) {
	name := strings.TrimSpace(opts.Name)
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if name == "" {
		return userRemote{}, fmt.Errorf("remote name is required")
	}
	if strings.Contains(name, "/") {
		return userRemote{}, fmt.Errorf("remote name %q must not contain '/' — the picker uses \"<remote>/<model>\"", name)
	}
	if baseURL == "" {
		return userRemote{}, fmt.Errorf("--base-url is required (e.g. --base-url https://api.example.com)")
	}
	if opts.APIKey != "" && opts.APIKeyEnv != "" {
		return userRemote{}, fmt.Errorf("--api-key and --api-key-env are mutually exclusive")
	}
	wire := strings.ToLower(strings.TrimSpace(opts.Wire))
	if wire != "" && !oneOf(wire, validRemoteWires) {
		return userRemote{}, fmt.Errorf("--wire must be one of %s", strings.Join(validRemoteWires, ", "))
	}
	toolFormat := strings.ToLower(strings.TrimSpace(opts.ToolFormat))
	if toolFormat != "" && !oneOf(toolFormat, validRemoteToolFormats) {
		return userRemote{}, fmt.Errorf("--tool-format must be one of %s", strings.Join(validRemoteToolFormats, ", "))
	}

	r := userRemote{
		Name:       name,
		BaseURL:    baseURL,
		APIKey:     strings.TrimSpace(opts.APIKey),
		APIKeyEnv:  strings.TrimSpace(opts.APIKeyEnv),
		Version:    strings.TrimSpace(opts.Version),
		Wire:       wire,
		ToolFormat: toolFormat,
	}

	f, path, err := loadUserRemotesFileRaw()
	if err != nil {
		return userRemote{}, err
	}
	replaced := false
	for i := range f.Remotes {
		if strings.TrimSpace(f.Remotes[i].Name) == name {
			// Preserve fields this CLI does not expose (tool_reliable,
			// force_tools, prices) so `remote add` on an existing entry is
			// an edit, not a silent reset of hand-tuned settings.
			existing := f.Remotes[i]
			r.ToolReliable = existing.ToolReliable
			r.ForceTools = existing.ForceTools
			r.PriceInputPerM = existing.PriceInputPerM
			r.PriceOutputPerM = existing.PriceOutputPerM
			f.Remotes[i] = r
			replaced = true
			break
		}
	}
	if !replaced {
		f.Remotes = append(f.Remotes, r)
	}
	if err := saveUserRemotesFile(f, path); err != nil {
		return userRemote{}, err
	}
	return r, nil
}

// RemoteRemove deletes an entry from remotes.json. Returns whether it existed.
func RemoteRemove(name string) (bool, error) {
	name = strings.TrimSpace(name)
	f, path, err := loadUserRemotesFileRaw()
	if err != nil {
		return false, err
	}
	out := f.Remotes[:0]
	existed := false
	for _, r := range f.Remotes {
		if strings.TrimSpace(r.Name) == name {
			existed = true
			continue
		}
		out = append(out, r)
	}
	if !existed {
		// A built-in provider isn't in the file, so say how to actually hide it.
		for _, b := range builtinRemotes() {
			if b.Name == name {
				return false, fmt.Errorf("%q is a built-in provider, not a remotes.json entry — unset %s to hide it", name, b.APIKeyEnv)
			}
		}
		return false, nil
	}
	f.Remotes = out
	if err := saveUserRemotesFile(f, path); err != nil {
		return false, err
	}
	return true, nil
}

// remoteAuthLabel describes how a remote authenticates without ever revealing
// the secret itself.
func remoteAuthLabel(r userRemote) string {
	if env := strings.TrimSpace(r.APIKeyEnv); env != "" {
		return "env:" + env
	}
	if strings.TrimSpace(r.APIKey) != "" {
		return "key"
	}
	return "none"
}

func sortedRemotes() ([]userRemote, error) {
	remotes, err := loadUserRemotes()
	if err != nil {
		return nil, err
	}
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].Name < remotes[j].Name })
	return remotes, nil
}

// WriteRemoteList prints every configured remote as an aligned table to w.
func WriteRemoteList(w io.Writer) error {
	remotes, err := sortedRemotes()
	if err != nil {
		return err
	}
	if len(remotes) == 0 {
		fmt.Fprintf(w, "No remotes configured (%s).\nAdd one with `oaica remote add <name> --base-url https://api.example.com --api-key-env EXAMPLE_API_KEY`\n", userRemotesPath())
		return nil
	}
	fmt.Fprintf(w, "%-16s %-42s %-10s %-12s %s\n", "NAME", "BASE_URL", "WIRE", "TOOL_FORMAT", "AUTH")
	for _, r := range remotes {
		d := r.Descriptor()
		fmt.Fprintf(w, "%-16s %-42s %-10s %-12s %s\n", r.Name, r.BaseURL, d.Wire, d.ToolFormat, remoteAuthLabel(r))
	}
	return nil
}

// RemoteShow returns one remote by name, built-ins included.
func RemoteShow(name string) (userRemote, error) {
	name = strings.TrimSpace(name)
	remotes, err := loadUserRemotes()
	if err != nil {
		return userRemote{}, err
	}
	for _, r := range remotes {
		if r.Name == name {
			return r, nil
		}
	}
	return userRemote{}, fmt.Errorf("no remote named %q in %s — add one with `oaica remote add`", name, userRemotesPath())
}

// WriteRemoteShow prints one remote's full detail to w. The api key is shown
// as `<set>` / `env:<VAR>` / `none`, never its value.
func WriteRemoteShow(w io.Writer, name string) error {
	r, err := RemoteShow(name)
	if err != nil {
		return err
	}
	d := r.Descriptor()
	key := "none"
	if env := strings.TrimSpace(r.APIKeyEnv); env != "" {
		key = "env:" + env
	} else if strings.TrimSpace(r.APIKey) != "" {
		key = "<set>"
	}
	fmt.Fprintf(w, "name:          %s\n", r.Name)
	fmt.Fprintf(w, "base_url:      %s\n", r.BaseURL)
	fmt.Fprintf(w, "version:       %s\n", orDash(r.Version))
	fmt.Fprintf(w, "wire:          %s\n", d.Wire)
	fmt.Fprintf(w, "tool_format:   %s\n", d.ToolFormat)
	fmt.Fprintf(w, "tool_reliable: %t\n", d.ToolReliable)
	fmt.Fprintf(w, "force_tools:   %t\n", r.ForceTools)
	fmt.Fprintf(w, "api_key:       %s\n", key)
	if r.PriceInputPerM > 0 || r.PriceOutputPerM > 0 {
		fmt.Fprintf(w, "price_per_m:   in %s / out %s USD\n", floatOrDash(r.PriceInputPerM), floatOrDash(r.PriceOutputPerM))
	}
	return nil
}
