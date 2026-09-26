package launch

// auth_cli.go — `oaica auth login/list/logout`, the CLI over auth_store.go.
// Same shape as remote_cli.go: each verb is a thin function writing to an
// io.Writer so cmd/cmd.go stays a one-line RunE.
//
// `login` is the supported way to make a paid plan usable. It resolves the
// provider from the SAME catalog the picker uses (provider_catalog.go), so a
// provider or plan added to providers.json — or pulled by `oaica remote
// sync` — gets a login path with no code change here. The endpoint shown in
// the prompt comes from that catalog row, so a user can see what they're
// logging into before pasting a key.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"
)

// knownAuthProvider resolves a provider name against the provider catalog,
// accepting the name case-insensitively. A name the catalog does not carry
// is still allowed (a user's own remotes.json entry is a legitimate target),
// but callers surface whether it was found so the prompt can say so.
func knownAuthProvider(name string) (providerCatalogEntry, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, e := range providerCatalog() {
		if strings.ToLower(e.Name) == want {
			return e, true
		}
	}
	return providerCatalogEntry{}, false
}

// AuthLogin stores an API key for a provider. An empty key reads one from the
// terminal with echo off; a non-terminal stdin without a key is an error
// rather than a hang, so scripts must pass --key explicitly.
func AuthLogin(out io.Writer, provider, key string) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return fmt.Errorf("usage: oaica auth login <provider> — run `oaica auth list` for the providers oaica knows")
	}

	entry, known := knownAuthProvider(provider)
	remotes, rerr := loadUserRemotes()
	if rerr != nil && !known {
		// Not in the catalog, and the remotes cannot be read either: only a
		// configured remote can make this name resolvable.
		return rerr
	}
	// A configured remote's OWN spelling wins, whether or not the catalog
	// knows the name: the read path is a case-SENSITIVE map lookup on
	// remote.Name (auth_store.go via storedAuthKey/user_remotes.go), so
	// anything else is a credential nothing ever looks up. `oaica auth login
	// MyBox` used to store "mybox" — success printed, and the very next launch
	// still demanded a key, with no error anywhere. The catalog branch had the
	// same hole from the other side: a remote added as "ZAI" matched catalog
	// "zai", so the key was stored under "zai" and the remote's own lookup
	// found nothing (2026-09-26 audit, both branches).
	matched := ""
	if rerr == nil {
		for _, r := range remotes {
			if strings.EqualFold(r.Name, provider) {
				matched = r.Name
				break
			}
		}
	}
	switch {
	case matched != "":
		provider = matched
	case known:
		provider = entry.Name
	default:
		return fmt.Errorf("%q is not a provider oaica knows (neither in the provider catalog nor in ~/.oaica/remotes.json). Run `oaica auth list` to see the catalog, or `oaica remote add %s --base-url ...` first", provider, provider)
	}

	if key == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("%s needs a key: pass --key (or run `oaica auth login %s` interactively to enter it hidden)", provider, provider)
		}
		var err error
		key, err = promptAuthKey(out, provider, entry, known)
		if err != nil {
			return err
		}
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("no key entered — %s unchanged", provider)
	}

	label := ""
	if known {
		label = entry.PlanLabel
	}
	// Through updateAuthStore, which holds the store's cross-process lock
	// across load-mutate-save. Eleven concurrent `auth login` calls used to
	// print eleven successes and leave one key (2026-09-26 audit, fourth
	// round).
	path := authStorePath()
	if err := updateAuthStore(out, func(f *authStoreFile) error {
		f.Providers[provider] = authCredential{
			Type:    authCredentialTypeAPIKey,
			Key:     key,
			Label:   label,
			SavedAt: nowUTC(),
		}
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "Logged in to %s (%s). The key is stored in %s, mode 0600.\n", provider, maskKey(key), path)
	if known && entry.APIKeyEnv != "" {
		// keyEnvNamesProse, not the raw field: api_key_env may list two
		// names, and "An A, B set in the environment" names nothing a user
		// can export.
		fmt.Fprintf(out, "An %s set in the environment still takes precedence over it.\n", keyEnvNamesProse(entry.APIKeyEnv))
	}
	return nil
}

// AuthLoginVia delegates a login to the tool that owns the credential
// instead of prompting for a key oaica would store itself: `oaica auth login
// --via opencode zai-coding-plan` runs opencode's own `auth login`, so the
// secret lands in the store opencode already reads and there is never a second
// copy to keep in sync. It exists because reusing those logins is the whole
// point (auth_external.go) — a provider opencode already knows how to
// authenticate needs no oaica-specific flow.
//
// The argv comes from the source's own declaration (externalLoginArgv), the
// same one `oaica auth list` prints, so the command a user is told to run and
// the command this executes can never drift.
func AuthLoginVia(out io.Writer, provider, via string) error {
	provider = strings.TrimSpace(provider)
	via = strings.TrimSpace(via)
	if provider == "" {
		return fmt.Errorf("usage: oaica auth login --via %s <provider>", via)
	}
	argv, ok := externalLoginArgv(via, provider)
	if !ok {
		return fmt.Errorf("unknown credential source %q — supported: %s", via, strings.Join(externalAuthSourceNames(), ", "))
	}
	bin := argv[0]
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("%s is not installed (or not on PATH), so its stored login cannot be created — install it, or run `oaica auth login %s` to store a key with oaica instead", bin, provider)
	}
	fmt.Fprintf(out, "Delegating to %s — the credential stays in that tool's own store; oaica reads it, never copies it.\n", strings.Join(argv, " "))
	cmd := exec.Command(path, argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", strings.Join(argv, " "), err)
	}
	// Prove the credential is actually readable through the path oaica will
	// use, rather than trusting that the delegated command did what we think.
	if key := externalAuthKey(via, provider); key != "" {
		fmt.Fprintf(out, "%s now authenticates via %s (%s).\n", provider, via, maskKey(key))
		return nil
	}
	if cred, ok := externalAuthFor(via, provider); ok && cred.Reason != "" {
		return fmt.Errorf("%s is still not usable: %s", provider, cred.Reason)
	}
	return fmt.Errorf("%s finished but no usable credential for %q is readable in %s's store — check the provider id it expects", strings.Join(argv, " "), provider, via)
}

func promptAuthKey(out io.Writer, provider string, entry providerCatalogEntry, known bool) (string, error) {
	if known {
		// The endpoint oaica will actually call, not the catalog's base_url
		// prefix — a v4 row's BaseURL stops a segment short, so the URL shown
		// beside "paste your key for this provider" 404s.
		fmt.Fprintf(out, "%s — %s\n", provider, entry.EndpointBase())
		if entry.PlanLabel != "" {
			fmt.Fprintf(out, "Plan: %s\n", entry.PlanLabel)
		}
		if entry.KeyURL != "" {
			fmt.Fprintf(out, "Get a key at %s\n", hyperlink(entry.KeyURL, entry.KeyURL))
		}
	} else {
		fmt.Fprintf(out, "%s (from ~/.oaica/remotes.json)\n", provider)
	}
	if known && entry.APIKeyEnv != "" {
		fmt.Fprintf(out, "Enter %s API key (input hidden; or set %s and skip this): ", provider, keyEnvNamesProse(entry.APIKeyEnv))
	} else {
		fmt.Fprintf(out, "Enter %s API key (input hidden): ", provider)
	}
	key, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(out)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(key)), nil
}

// AuthLogout removes a provider's stored credential. It does not touch
// remotes.json or the environment — a key supplied by either keeps working,
// which is stated rather than left implicit.
func AuthLogout(out io.Writer, provider string) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return fmt.Errorf("usage: oaica auth logout <provider>")
	}
	// Resolved inside the lock: a login landing between this read and the
	// delete would otherwise be removed by a logout that never saw it.
	found := false
	if err := updateAuthStore(out, func(f *authStoreFile) error {
		if _, ok := f.Providers[provider]; !ok {
			// Case-insensitive fallback: `oaica auth logout ZAI` should work.
			for name := range f.Providers {
				if strings.EqualFold(name, provider) {
					provider = name
					ok = true
					break
				}
			}
			if !ok {
				return nil
			}
		}
		found = true
		delete(f.Providers, provider)
		return nil
	}); err != nil {
		return err
	}
	if !found {
		fmt.Fprintf(out, "No stored credential for %s — nothing to remove.\n", provider)
		return nil
	}
	fmt.Fprintf(out, "Removed the stored key for %s.\n", provider)
	if entry, known := knownAuthProvider(provider); known {
		if entry.APIKeyEnv != "" {
			fmt.Fprintf(out, "If %s is set in this shell, %s keeps working without it.\n", keyEnvNamesProse(entry.APIKeyEnv), provider)
		}
		// auth_via: another agent CLI's own login is a SECOND source, and the
		// env warning above does not cover it. Without this line a user who
		// logged in and logged out reads a confirmation, sees the provider
		// come back "ready" in `oaica auth list` and in the picker, and has no
		// way to tell that a store they never touched is what still
		// authenticates it (2026-09-26 audit, fifth round). The credential is
		// not ours to delete — the tool that owns the store removes it.
		if entry.AuthVia != "" {
			if cred, ok := externalAuthFor(entry.AuthVia, provider); ok && cred.Key != "" {
				fmt.Fprintf(out, "%s keeps working: %s's own login still holds a credential for it, and `oaica auth logout` removed only oaica's copy.\n",
					provider, cred.Source)
			}
		}
	}
	return nil
}

// AuthList prints every provider oaica knows how to authenticate, marking
// which ones are actually usable right now and where the credential comes
// from. Providers are listed whether or not a credential exists — the point
// is to answer "what can I log into", and a list that only shows what is
// already configured cannot answer that.
//
// Keys are masked, never printed: this output is meant to be pasteable into
// a bug report.
func AuthList(out io.Writer) error {
	f, _, err := loadAuthStore()
	if err != nil {
		if errors.Is(err, errAuthStoreUnparseable) {
			// The write path moves such a file aside and starts a new store
			// (updateAuthStore), and that is not something this read-only
			// command may do on its own — but it is the answer to "what now",
			// and leaving it unsaid makes the file look bricked.
			return fmt.Errorf("%w\nNo credential can be read from it until it is rewritten: `oaica auth login <provider> --key …` keeps it as <file>.unreadable-<timestamp> and starts a new store, or repair the file by hand.", err)
		}
		return err
	}
	stored := map[string]bool{}
	for name, c := range f.Providers {
		stored[name] = c.Type == authCredentialTypeAPIKey && strings.TrimSpace(c.Key) != ""
	}

	entries := providerCatalog()
	width := len("PROVIDER")
	for _, e := range entries {
		if len(e.Name) > width {
			width = len(e.Name)
		}
	}
	// CREDENTIAL is wide enough for the longest actionable value it can hold
	// ("run: opencode auth login minimax-coding-plan"), so the PLAN column
	// stays aligned instead of wrapping into a paragraph.
	const credWidth = 46
	fmt.Fprintf(out, "%-*s  %-9s  %-*s  %s\n", width, "PROVIDER", "STATUS", credWidth, "CREDENTIAL", "PLAN")
	// Reasons a present-but-unusable external credential could not be used
	// (an expired OAuth token): printed under the table so the CREDENTIAL
	// column can stay narrow and the user learns WHY, not just "needs key".
	var notes []string
	for _, e := range entries {
		status, cred := "not set", "-"
		external, hasExternal := externalAuthFor(e.AuthVia, e.Name)
		externalCmd := externalLoginArgvString(e.AuthVia, e.Name)
		switch {
		case keyEnvNameSet(e.APIKeyEnv) != "":
			// Name the variable that IS set: api_key_env can list two, and
			// printing the raw comma-joined string as if it were a variable
			// name told the user to export something that does not exist —
			// while the gate above (raw os.Getenv) reported the row as missing
			// a key that was in fact exported.
			status, cred = "ready", "env:"+keyEnvNameSet(e.APIKeyEnv)
		case stored[e.Name]:
			status, cred = "ready", "stored"
		case hasExternal && external.Key != "":
			// Another agent CLI's own login, reused (auth_external.go) — no
			// oaica copy exists and none is needed.
			status, cred = "ready", external.Source
		case hasExternal && external.Reason != "":
			status, cred = "needs key", "run: "+externalCmd
			notes = append(notes, fmt.Sprintf("%s: %s", e.Name, external.Reason))
		case e.AuthVia != "" && externalStoreExists(e.AuthVia):
			// Declared as reusable, that tool is installed, but it has no entry
			// for this provider yet: point at the tool's own login, which is
			// the whole point of auth_via — one login serves both.
			status, cred = "needs key", "run: "+externalCmd
		case e.APIKeyEnv != "":
			status, cred = "needs key", "run: oaica auth login "+e.Name
		}
		plan := e.PlanLabel
		if plan == "" {
			plan = "-"
		}
		fmt.Fprintf(out, "%-*s  %-9s  %-*s  %s\n", width, e.Name, status, credWidth, cred, plan)
	}
	if len(notes) > 0 {
		fmt.Fprintln(out, "\nA stored login oaica could not use:")
		for _, n := range notes {
			fmt.Fprintf(out, "  %s\n", n)
		}
	}

	// Store-only entries (a user remote the catalog does not carry) would
	// otherwise be invisible, and `auth logout` on them would look like a
	// no-op.
	var extra []string
	for name := range f.Providers {
		if _, known := knownAuthProvider(name); !known {
			extra = append(extra, name)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		fmt.Fprintln(out, "\nStored credentials outside the catalog:")
		for _, name := range extra {
			fmt.Fprintf(out, "  %s  %s\n", name, maskKey(f.Providers[name].Key))
		}
	}

	fmt.Fprintln(out, "\n`oaica auth login <provider>` stores a key in ~/.oaica/auth.json (0600).")
	fmt.Fprintln(out, "An env var named in the CREDENTIAL column still wins over a stored key.")
	return nil
}

// nowUTC is a seam for tests that need deterministic saved_at values.
var nowUTC = func() time.Time { return time.Now().UTC() }
