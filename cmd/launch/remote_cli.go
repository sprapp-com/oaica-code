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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	// VersionSet reports that the caller actually passed --api-version, which
	// is what separates "set the version to this" from "I did not mention it".
	// Without it, replacing an existing entry cleared Version, and clearing
	// Version MOVES THE ENDPOINT: a v4 remote re-added to rotate its key
	// silently began resolving to ".../v1/chat/completions" (404), with a
	// confirmation line that did not say so. Every other flag here describes
	// the entry's value; this one also describes the absence of an intent.
	VersionSet bool
	// KeySet reports that the caller passed --api-key or --api-key-env at all,
	// including passing one empty. An ABSENT credential flag preserves what the
	// entry already had; an explicitly empty one clears it.
	//
	// The usual "absent means cleared" rule is wrong here for the same reason
	// it is wrong for Version, only worse: the credential's default is not a
	// setting, it is a missing key. `oaica remote add box --base-url https://new`
	// — a repoint with nothing to say about credentials — deleted a stored
	// secret that existed nowhere else, and the confirmation line named only
	// the new URL (2026-09-26 audit). Clearing is a typed intent precisely
	// because it cannot be undone from anything oaica holds.
	KeySet bool
	// WireSet reports that the caller passed --wire, and ToolFormatSet that it
	// passed --tool-format. Both follow Version's rule rather than the usual
	// "absent means cleared" one, because neither default is neutral:
	//
	//   - Wire's default (openai) MOVES THE ENDPOINT. A row for a vendor that
	//     speaks Anthropic natively 404s on <base>/chat/completions (the
	//     2026-09-25 z.ai failure), so `oaica remote add zai --base-url <new>`
	//     — a repoint with nothing to say about the wire — silently turned a
	//     working passthrough row into a broken translated one, with a
	//     confirmation line that did not mention it.
	//   - ToolFormat's default is INFERRED from the wire, so clearing it
	//     reverts a deliberate setting to whatever the wire implies: a model
	//     pinned to "freeform" or "none" because it cannot hold a tool_use
	//     loop went back to "tool_calls", which is precisely the pairing the
	//     capability gate exists to refuse.
	//
	// Both are typeable, so the reset is available — it just has to be meant
	// (`--wire openai`, `--tool-format tool_calls`).
	WireSet       bool
	ToolFormatSet bool
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
	// Members this struct does not model (`schema_note`, a hand-added label) are
	// re-attached rather than deleted by the rewrite — see store_document.go.
	b, err := storeDocumentMergeValue(f, path)
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

// updateUserRemotesFile is the ONLY writer path for remotes.json: it holds
// the store's cross-process lock across load → mutate → save, so two
// concurrent `oaica remote add` (a fleet-wiring script, two terminals) cannot
// each read the same snapshot and lose one another's entry — eight concurrent
// adds used to leave one remote, each printing "added"
// (2026-09-26 audit, fourth round). Read-only callers keep using
// loadUserRemotesFileRaw directly.
func updateUserRemotesFile(mutate func(*userRemotesFile) error) (string, error) {
	path := userRemotesPath()
	if path == "" {
		return "", fmt.Errorf("cannot locate ~/.oaica/remotes.json (no home directory) — set OAICA_REMOTES_FILE")
	}
	err := fileutil.WithFileLock(path, func() error {
		f, _, err := loadUserRemotesFileRaw()
		if err != nil {
			return err
		}
		snapshot := storeDocumentSnapshot(f)
		if err := mutate(&f); err != nil {
			return err
		}
		if !storeDocumentChanged(snapshot, f) {
			// Nothing to say. Writing anyway re-serialises a partial view of the
			// document over the file — which is how `remote rm <typo>`, a
			// command that removed nothing and reported it, deleted every
			// hand-added member (2026-09-26 audit).
			return nil
		}
		return saveUserRemotesFile(f, path)
	})
	if err != nil {
		return path, err
	}
	return path, nil
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

// validateRemoteBaseURL refuses a --base-url that cannot work, at the point the
// user typed it (2026-09-26 audit, fourth round). Every value used to be
// accepted verbatim: `remote add --base-url garbage` printed "added" and the
// remote then failed at request time deep in the proxy ("unsupported protocol
// scheme" from the appended "/chat/completions"), naming neither the remote nor
// the flag — and a value carrying a newline forges a line in every listing that
// prints it (`oaica remote list`, the picker, doctor output).
func validateRemoteBaseURL(baseURL string) error {
	// Every message below is printed to the terminal by main.go's
	// cobra.CheckErr, and --base-url can itself carry the key as userinfo. The
	// success line two functions over redacts for exactly that reason; these
	// refusals did not, so a mistyped line put a live credential in the
	// terminal, the CI log and the scrollback (2026-09-26 audit, ninth round).
	// Redacted once here rather than at six call sites, so a seventh branch
	// cannot be added without it.
	shown := redactBaseURL(baseURL)
	for _, r := range baseURL {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("--base-url %q contains a control character — it is echoed by `oaica remote list` and in launch messages, so an embedded newline forges an extra line of output", shown)
		}
		if r == ' ' {
			return fmt.Errorf("--base-url %q contains a space", shown)
		}
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		// url.Parse's own text re-states the whole URL, so the wrap needs the
		// error redacted too, not just the value.
		return fmt.Errorf("--base-url %q is not a URL: %w (e.g. --base-url https://api.example.com)", shown, redactErr(err))
	}
	// A bare host ("garbage", "not/absolute", "api.example.com") parses without
	// error and with an empty scheme, which is exactly the value that reaches
	// http.NewRequest as "<base>/chat/completions" and fails there.
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("--base-url %q: the scheme must be http or https (got %q) — oaica appends /chat/completions and /models to this value, so anything else fails at request time with an error that names neither the remote nor this flag", shown, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("--base-url %q has no host (e.g. --base-url https://api.example.com)", shown)
	}
	// A query or fragment is not part of the endpoint oaica talks to: the base
	// is joined with "/v1/chat/completions" and "/v1/models", so a query
	// SWALLOWS that path and every request goes somewhere the user never
	// intended, with a doctor line showing the mangled URL and a failure that
	// names neither this flag nor the cause (2026-09-26 audit).
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("--base-url %q carries a query string or fragment — oaica appends /v1/chat/completions to this value, so anything after \"?\" or \"#\" truncates the path and every request goes to the wrong URL. Put the endpoint itself here (e.g. --base-url https://api.example.com/v1); a credential the endpoint needs in the query is not supported — use --api-key", shown)
	}
	// url.Parse accepts any digits as a port; the failure surfaces much later as
	// "address 99999999: invalid port" from the transport.
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("--base-url %q has port %q, which is not a usable TCP port (1-65535)", shown, p)
		}
	}
	return nil
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
	// A control character in a name forges output wherever the name is printed
	// as a table cell or a field line: `remote list` grew an extra, entirely
	// fabricated row and `remote show` an extra `base_url:`-looking line, both
	// of which a reader takes as real. Only "/" was rejected here, so
	// `remote add $'mine\nzai-coding-plan/base_url: evil'` was accepted
	// (2026-09-26 audit, ninth round). The printers quote such a name too —
	// remotes.json is hand-editable, so add-time validation cannot be the only
	// line of defence.
	if strings.ContainsFunc(name, isControlRune) {
		return userRemote{}, fmt.Errorf("remote name %q contains a control character (a newline, tab or escape) — the name is printed in `oaica remote list`/`show` and would forge lines there", name)
	}
	if baseURL == "" {
		return userRemote{}, fmt.Errorf("--base-url is required (e.g. --base-url https://api.example.com)")
	}
	if err := validateRemoteBaseURL(baseURL); err != nil {
		return userRemote{}, err
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

	// The whole replace-or-append runs inside the store's lock, so a
	// concurrent `remote add` for another name cannot be lost under ours.
	if _, err := updateUserRemotesFile(func(f *userRemotesFile) error {
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
				//
				// Version HAS a flag and is still preserved when that flag was not
				// passed (VersionSet). The usual "absent means cleared" rule exists
				// so a field can be reset to its default by omitting it — but for
				// Version the default is not neutral, it is a different endpoint,
				// so the reset has to be typed (`--api-version v1`).
				existing := f.Remotes[i]
				if !opts.VersionSet {
					r.Version = existing.Version
				}
				// Wire and ToolFormat keep the same rule (see WireSet): their
				// defaults are not neutral either, and an edit that mentions
				// neither flag used to move the endpoint or re-open a tool loop
				// the row had deliberately closed.
				if !opts.WireSet {
					r.Wire = existing.Wire
				}
				if !opts.ToolFormatSet {
					r.ToolFormat = existing.ToolFormat
				}
				// Wire and ToolFormat keep the same rule (see WireSet): their
				// defaults are not neutral either, and an edit that mentions
				// neither flag used to move the endpoint or re-open a tool loop
				// the row had deliberately closed.
				// The credential is preserved unless the caller said something
				// about it (KeySet) — see RemoteAddOptions.KeySet. Passing one
				// of the two flags replaces BOTH fields, because the pair is
				// mutually exclusive by this command's own rule: switching a row
				// to --api-key-env has to drop the literal it was using.
				if !opts.KeySet {
					r.APIKey = existing.APIKey
					r.APIKeyEnv = existing.APIKeyEnv
				}
				r.ToolReliable = existing.ToolReliable
				r.ForceTools = existing.ForceTools
				r.PriceInputPerM = existing.PriceInputPerM
				r.PriceOutputPerM = existing.PriceOutputPerM
				r.RoutePolicy = existing.RoutePolicy
				r.Weight = existing.Weight
				r.AuthVia = existing.AuthVia
				// ModelsPath: the vendor's declared model-list URL, set by
				// hand in remotes.json because no flag writes it. Dropping it
				// sent the picker at <base>/models instead, which for the
				// vendor that needs it returns the wrong list or a 404, so
				// its models disappeared from the picker (2026-09-26 audit).
				r.ModelsPath = existing.ModelsPath
				f.Remotes[i] = r
				replaced = true
				break
			}
		}
		if !replaced {
			f.Remotes = append(f.Remotes, r)
		}
		return nil
	}); err != nil {
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
	missing := false
	path, err := updateUserRemotesFile(func(f *userRemotesFile) error {
		for i := range f.Remotes {
			if strings.TrimSpace(f.Remotes[i].Name) == name {
				f.Remotes[i].APIKey = key
				return nil
			}
		}
		missing = true
		return nil
	})
	if err != nil {
		return err
	}
	if missing {
		return fmt.Errorf("remote %q is not in %s — cannot save its API key", name, path)
	}
	return nil
}

// RemoteRemove deletes an entry from remotes.json. Returns whether it existed.
func RemoteRemove(name string) (bool, error) {
	name = strings.TrimSpace(name)
	existed := false
	if _, err := updateUserRemotesFile(func(f *userRemotesFile) error {
		out := f.Remotes[:0]
		for _, r := range f.Remotes {
			if strings.TrimSpace(r.Name) == name {
				existed = true
				continue
			}
			out = append(out, r)
		}
		if existed {
			f.Remotes = out
		}
		return nil
	}); err != nil {
		return false, err
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
	return true, nil
}

// isControlRune reports whether r is a control character — the class that, in
// a printed name, moves the cursor or starts a new line.
//
// ASCII is not the class. U+2028 and U+2029 are line separators to most
// consumers, U+0085 is NEL, U+202A–U+202E and U+2066–U+2069 are the bidi
// overrides that move the cursor and reorder what follows, and the
// zero-width/invisible forms (U+200B–U+200F, U+FEFF) hide or reorder text in a
// terminal. An ASCII-only test let all of those into `remote add` and into
// printableName, reaching the same forged-row output the ASCII class was added
// to stop (2026-09-26 audit, tenth round).
func isControlRune(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f:
		return true
	case r == 0x85: // NEL
		return true
	case r >= 0x2028 && r <= 0x2029: // LINE / PARAGRAPH SEPARATOR
		return true
	case r >= 0x200b && r <= 0x200f: // ZWSP, ZWNJ, ZWJ, LRM, RLM
		return true
	case r >= 0x202a && r <= 0x202e: // LRE, RLE, PDF, LRO, RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI, RLI, FSI, PDI
		return true
	case r == 0xfeff: // BOM
		return true
	}
	return false
}

// printableName renders a remote name for a one-line report: an ordinary name
// as-is, one carrying control characters in Go-quoted form. remotes.json is
// edited by hand and written by older versions, so a name that reached the file
// before add-time validation existed still must not forge rows in
// `oaica remote list` or a second field line in `remote show` (2026-09-26
// audit, ninth round).
func printableName(name string) string {
	if strings.ContainsFunc(name, isControlRune) {
		return strconv.Quote(name)
	}
	return name
}

// remoteAuthLabel describes how a remote authenticates without ever revealing
// the secret itself. It defers to userRemote.authSource so the listing and
// `remote show` agree with key() about where the credential actually comes
// from — a stored login or a reused one read "none" here while the proxy
// authenticated with it (2026-09-26 audit, ninth round).
func remoteAuthLabel(r userRemote) string { return r.authSource() }

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
		fmt.Fprintf(w, "%-16s %-42s %-10s %-12s %s\n", printableName(r.Name), redactBaseURL(r.BaseURL), d.Wire, d.ToolFormat, remoteAuthLabel(r))
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
	// From the same resolution order the proxy uses (see authSource): this
	// field answered "none" for a remote whose credential came from a stored
	// login, from `auth_via`, or from base_url's userinfo (2026-09-26 audit).
	key := authSourceProse(r.authSource())
	fmt.Fprintf(w, "name:          %s\n", printableName(r.Name))
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
