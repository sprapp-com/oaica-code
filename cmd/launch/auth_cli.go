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
	if !known {
		// Not in the catalog: only worth accepting if the name actually
		// matches a configured remote, otherwise this is a typo that would
		// silently write an entry nothing ever reads.
		remotes, err := loadUserRemotes()
		if err != nil {
			return err
		}
		found := false
		for _, r := range remotes {
			if strings.EqualFold(r.Name, provider) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%q is not a provider oaica knows (neither in the provider catalog nor in ~/.oaica/remotes.json). Run `oaica auth list` to see the catalog, or `oaica remote add %s --base-url ...` first", provider, provider)
		}
		provider = strings.ToLower(provider)
	} else {
		provider = entry.Name
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

	f, path, err := loadAuthStore()
	if err != nil {
		return err
	}
	label := ""
	if known {
		label = entry.PlanLabel
	}
	f.Providers[provider] = authCredential{
		Type:    authCredentialTypeAPIKey,
		Key:     key,
		Label:   label,
		SavedAt: nowUTC(),
	}
	if err := saveAuthStore(f, path); err != nil {
		return err
	}
	fmt.Fprintf(out, "Logged in to %s (%s). The key is stored in %s, mode 0600.\n", provider, maskKey(key), path)
	if known && entry.APIKeyEnv != "" {
		fmt.Fprintf(out, "An %s set in the environment still takes precedence over it.\n", entry.APIKeyEnv)
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
		fmt.Fprintf(out, "%s — %s\n", provider, entry.BaseURL)
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
		fmt.Fprintf(out, "Enter %s API key (input hidden; or set %s and skip this): ", provider, entry.APIKeyEnv)
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
	f, path, err := loadAuthStore()
	if err != nil {
		return err
	}
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
			fmt.Fprintf(out, "No stored credential for %s — nothing to remove.\n", provider)
			return nil
		}
	}
	delete(f.Providers, provider)
	if err := saveAuthStore(f, path); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed the stored key for %s.\n", provider)
	if entry, known := knownAuthProvider(provider); known && entry.APIKeyEnv != "" {
		fmt.Fprintf(out, "If %s is set in this shell, %s keeps working without it.\n", entry.APIKeyEnv, provider)
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
		case e.APIKeyEnv != "" && strings.TrimSpace(os.Getenv(e.APIKeyEnv)) != "":
			status, cred = "ready", "env:"+e.APIKeyEnv
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
