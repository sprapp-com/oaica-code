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

	"github.com/ollama/ollama/cmd/internal/fileutil"
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
	// Through the atomic writer, NOT os.WriteFile (2026-09-26 audit): an
	// in-place O_TRUNC write makes the truncation window visible to every
	// other oaica process, and a crash inside it leaves a zero-byte file —
	// which loadUserRemotes reports as a parse error, losing every remote
	// and every inline api_key. WriteFileAtomic also re-asserts the mode on
	// the target (2026-09-01 security audit M2: a pre-existing 0664 file
	// with a plaintext key inside stayed world-readable forever).
	return fileutil.WriteFileAtomic(path, append(b, '\n'), 0o600)
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
			// Preserve every field this command has NO FLAG FOR, so
			// `remote add` on an existing entry is an edit, not a silent
			// reset. The rule is exact: a field with a flag is whatever you
			// passed (absent means cleared), a field without one is left
			// alone — anything else silently destroys a setting the user
			// cannot type back (2026-09-26 audit).
			//
			// route_policy, weight and auth_via had no flag at all, and each
			// one changes routing: weight 0 drops the leg from a weighted
			// split entirely, a lost route_policy reverts remote-only to
			// local-first, a lost auth_via re-prompts for a credential
			// another tool already owns.
			existing := f.Remotes[i]
			r.ToolReliable = existing.ToolReliable
			r.ForceTools = existing.ForceTools
			r.PriceInputPerM = existing.PriceInputPerM
			r.PriceOutputPerM = existing.PriceOutputPerM
			r.RoutePolicy = existing.RoutePolicy
			r.Weight = existing.Weight
			r.AuthVia = existing.AuthVia
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

// savePromptedRemoteKey stores a key the user just typed for a remote that
// already exists, and changes NOTHING else on the entry.
//
// It exists instead of a RemoteAdd call because RemoteAdd cannot express
// "this credential, keep the rest": it refuses APIKey and APIKeyEnv together
// (mutually exclusive by design), so persisting a typed key to a row that
// declares api_key_env necessarily DELETED the indirection and baked the
// secret into remotes.json — in a file the user had deliberately kept the
// secret out of, right after a prompt that had just told them to set that
// variable instead (2026-09-26 audit). Leaving the row's other fields alone
// is the same contract as RemoteAdd's flagless-field rule: a write that
// knows about one field must not silently reset the other eleven.
func savePromptedRemoteKey(name, key string) error {
	name = strings.TrimSpace(name)
	f, path, err := loadUserRemotesFileRaw()
	if err != nil {
		return err
	}
	for i := range f.Remotes {
		if strings.TrimSpace(f.Remotes[i].Name) == name {
			f.Remotes[i].APIKey = key
			return saveUserRemotesFile(f, path)
		}
	}
	return fmt.Errorf("remote %q is not in %s — cannot save its API key", name, path)
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
		// redactBaseURL: a remote configured as https://key@host/v1 carries
		// the credential in the URL's userinfo, and list output lands in
		// terminals, shell history and tickets. Same fix as doctor's.
		fmt.Fprintf(w, "%-16s %-42s %-10s %-12s %s\n", r.Name, redactBaseURL(r.BaseURL), d.Wire, d.ToolFormat, remoteAuthLabel(r))
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
// as `<set>` / `env:<VAR>` / `none`, never its value — and a key embedded in
// base_url's userinfo prints as REDACTED, so the field is never a second,
// unredacted copy of the secret.
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
	fmt.Fprintf(w, "base_url:      %s\n", redactBaseURL(r.BaseURL))
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
