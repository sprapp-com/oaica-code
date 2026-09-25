package launch

// doctor_report.go — `oaica doctor --report`: a support bundle a user can
// paste into a ticket without leaking credentials.
//
// Two things make it safe rather than merely careful:
//
//  1. Every value that is a credential anywhere in this client (the
//     OAICA_API_KEY environment variable, every remote's key from
//     remotes.json, every key in auth.json, the key files signin and the
//     licence flow write — ~/.oaica/api_key, license_key, license.json — and
//     the `oaica serve --api-key` values in local_servers.json) is collected
//     first, and paths are reported as present/absent plus their file mode —
//     never their contents.
//  2. The rendered text is scanned against those values *before* it is
//     printed. If one appears, the report is not printed at all and the
//     command fails: a redaction-by-intention bug must not become a leaked
//     key in someone's support ticket.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/ollama/ollama/version"
)

// minSecretLen: values shorter than this are not credentials and matching
// them against arbitrary text produces false positives (a one-character test
// key, an empty string), which would make --report useless.
const minSecretLen = 8

// reportSecret is one credential plus how to name it in an error, so a leak
// can be reported without printing the value.
type reportSecret struct {
	label string
	value string
}

// reportSecrets collects every credential this client holds. Read-only.
func reportSecrets() []reportSecret {
	var secrets []reportSecret
	add := func(label, value string) {
		value = strings.TrimSpace(value)
		if len(value) >= minSecretLen {
			secrets = append(secrets, reportSecret{label: label, value: value})
		}
	}

	add("OAICA_API_KEY", os.Getenv("OAICA_API_KEY"))
	// OAICA_HOST is a base URL configured exactly like a remote's, so it can
	// carry a credential the same two ways (userinfo or a query parameter).
	// The report itself prints only its presence, but the scan's job is to
	// know every credential this client holds — a value that is missing from
	// the list is a value a later print site could add without noticing.
	for _, secret := range baseURLSecrets(os.Getenv("OAICA_HOST")) {
		add("the key embedded in OAICA_HOST", secret)
	}
	for _, env := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "HF_TOKEN"} {
		add(env, os.Getenv(env))
	}
	if remotes, err := loadUserRemotes(); err == nil {
		for _, r := range remotes {
			add("the remotes.json key for "+r.Name, r.key())
			// A key can also arrive as URL userinfo (https://KEY@host/v1),
			// which key() never sees because the transport, not this client,
			// turns it into the Authorization header. userinfoSecret also
			// answers for a URL url.Parse rejects (a pasted passphrase with a
			// space, a bad port) — those cannot be transmitted, but they can
			// still be printed, and a report that prints a typed credential is
			// the false negative this scan exists to prevent.
			// Both places a key can ride in a base URL: userinfo
			// (https://user:password@host/v1 — including a password that
			// contains "/", which url.Parse rejects outright) and the query
			// string ("?api_key=…", "?token=…", a shape some gateways use
			// instead of a header). The report prints the URL it failed on, so
			// either form reaches a pasted ticket. Redaction alone is not
			// enough: the scan is what makes "this report contains no
			// credential values" a claim instead of a hope, and it can only
			// check values it was told about.
			for _, secret := range baseURLSecrets(r.BaseURL) {
				add("the key embedded in the base_url of "+r.Name, secret)
			}
		}
	}
	if store, _, err := loadAuthStore(); err == nil {
		names := make([]string, 0, len(store.Providers))
		for name := range store.Providers {
			names = append(names, name)
		}
		sort.Strings(names) // stable labels for tests and error text
		for _, name := range names {
			add("the auth.json key for "+name, store.Providers[name].Key)
		}
	}
	// The oaica and licence keys live in files the report does not print, but
	// the scan is only as good as the value list: a key that is missing from
	// it is a key the report could print without noticing. `oaica signin`
	// writes ~/.oaica/api_key, `oaica pull`/serve accept ~/.oaica/license_key,
	// and the launch-side licence state is ~/.oaica/license.json
	// (launch/license.go). Three files, three places the same key can sit.
	if home := reportHome(); home != "" {
		add("the key saved by `oaica signin` (~/.oaica/api_key)",
			readSecretFile(filepath.Join(home, ".oaica", "api_key")))
		// `oaica pull` attaches the HuggingFace token (HF_TOKEN, or the file
		// the official `hf` tooling writes) to a weight download, so it is a
		// credential this machine transmits and can therefore also be printed
		// by a report (2026-09-26 audit: it was in neither the secret list nor
		// the egress documentation).
		add("the HuggingFace token in ~/.huggingface/token",
			readSecretFile(filepath.Join(home, ".huggingface", "token")))
		add("the licence key in ~/.oaica/license_key",
			readSecretFile(filepath.Join(home, ".oaica", "license_key")))
		// `oaica serve --api-key K` records K in local_servers.json, so that
		// file is a fourth credential store — one the report used to describe
		// as non-sensitive and never scan for (2026-09-26 audit).
		for _, k := range localServerKeys(home) {
			add("an `oaica serve --api-key` value in ~/.oaica/local_servers.json", k)
		}
	}
	if f, err := loadLicenseFile(); err == nil {
		add("the licence key in ~/.oaica/license.json", f.Key)
	}
	return secrets
}

// localServerKeys returns every api_key recorded in
// ~/.oaica/local_servers.json, the file `oaica serve` writes so `oaica launch`
// can find a running local server. Only ever called to feed the leak scan,
// which compares values and never prints them; a malformed or absent file
// yields no keys, which is why the caller does not treat an error as fatal.
func localServerKeys(home string) []string {
	b, err := os.ReadFile(filepath.Join(home, ".oaica", "local_servers.json"))
	if err != nil {
		return nil
	}
	var entries []struct {
		APIKey string `json:"api_key"`
	}
	if json.Unmarshal(b, &entries) != nil {
		return nil
	}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, e.APIKey)
	}
	return keys
}

// readSecretFile returns a file's trimmed contents, or "" for any error —
// missing, unreadable, a directory. Only ever called to feed the leak scan,
// which compares values and never prints them.
func readSecretFile(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// scanReportForSecrets returns the labels of any secrets whose value appears
// in report, sorted. Values are never returned or printed.
func scanReportForSecrets(report string, secrets []reportSecret) []string {
	var found []string
	for _, s := range secrets {
		if strings.Contains(report, s.value) {
			found = append(found, s.label)
		}
	}
	sort.Strings(found)
	return found
}

// describeFile prints a path's presence and, when it holds credentials, its
// permission bits — a world-readable auth.json is worth knowing about, and
// the mode says so without revealing anything in the file.
func describeFile(w io.Writer, path string, sensitive bool) {
	if path == "" {
		fmt.Fprintln(w, "  (no path — home directory unavailable)")
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(w, "  absent            %s\n", path)
			return
		}
		fmt.Fprintf(w, "  unreadable (%v)  %s\n", err, path)
		return
	}
	// The bits are printed for directories too. docs/ENTERPRISE.md's checklist
	// item 4 sends a reviewer to `ls -la ~/.oaica/` to confirm the 0700/0600
	// layout — and most of that layout is the DIRECTORY the credential files
	// sit in (a 0755 ~/.oaica makes them reachable whatever their own mode
	// says). The directory branch returned before the mode note, so the
	// bundle could not answer the one question the layout check asks
	// (2026-09-26 audit).
	note := ""
	if sensitive {
		mode := fi.Mode().Perm()
		note = fmt.Sprintf("  mode %04o", mode)
		// 0o077 = any group/other bit: the path is reachable beyond its owner.
		if mode&0o077 != 0 {
			note += "  <-- readable by other users"
		}
	}
	if fi.IsDir() {
		fmt.Fprintf(w, "  directory%s  %s\n", note, path)
		return
	}
	fmt.Fprintf(w, "  present%s  %s\n", note, path)
}

// buildDoctorReport renders the report. It does not print; runDoctorReport
// scans the result first.
func buildDoctorReport() (string, bool) {
	var b strings.Builder

	fmt.Fprintf(&b, "oaica doctor --report\n")
	fmt.Fprintf(&b, "version:  %s\n", version.Version)
	fmt.Fprintf(&b, "platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "home:     %s\n", reportHome())

	fmt.Fprintf(&b, "\nconfiguration files (contents are never included):\n")
	home := reportHome()
	if home != "" {
		// The directory itself first: a 0755 ~/.oaica makes every file
		// below reachable whatever its own mode says, so the 0700/0600
		// layout check this list exists to answer is decided HERE and the
		// report was silent on it (2026-09-26 audit).
		describeFile(&b, filepath.Join(home, ".oaica"), true)
		describeFile(&b, filepath.Join(home, ".oaica", "auth.json"), true)
		describeFile(&b, filepath.Join(home, ".oaica", "remotes.json"), true)
		describeFile(&b, filepath.Join(home, ".oaica", "api_key"), true)
		describeFile(&b, filepath.Join(home, ".oaica", "license_key"), true)
		describeFile(&b, filepath.Join(home, ".oaica", "license.json"), true)
		// sensitive=true: config.json is where OAICA_HOST lives, and that
		// value may carry a key in its userinfo (see SplitUserinfoCredential)
		// — the same shape the file's own mode decides whether other users
		// can read, which is exactly what the bits are printed for
		// (2026-09-26 audit).
		describeFile(&b, filepath.Join(home, ".oaica", "config.json"), true)
		describeFile(&b, filepath.Join(home, ".oaica", "local_servers.json"), true)
		// sensitive=true so the directory's own bits print (see describeFile):
		// the cache holds the picker's fetched rows, not credentials, but a
		// world-writable cache dir is still a layout fact the 0700/0600 check
		// asks about and the report was silent on (2026-09-26 audit).
		describeFile(&b, filepath.Join(home, ".oaica", "cache"), true)
	} else {
		fmt.Fprintln(&b, "  (no home directory — cannot list configuration files)")
	}

	fmt.Fprintf(&b, "\ncredentials (presence only, values never printed):\n")
	for _, env := range []string{"OAICA_API_KEY", "OAICA_HOST", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		if strings.TrimSpace(os.Getenv(env)) == "" {
			fmt.Fprintf(&b, "  %-18s unset\n", env)
		} else {
			fmt.Fprintf(&b, "  %-18s set\n", env)
		}
	}
	if store, _, err := loadAuthStore(); err == nil && len(store.Providers) > 0 {
		names := make([]string, 0, len(store.Providers))
		for name := range store.Providers {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "  auth.json          %d provider(s): %s\n", len(names), strings.Join(names, ", "))
	} else {
		fmt.Fprintf(&b, "  auth.json          no stored providers\n")
	}

	fmt.Fprintf(&b, "\nchecks:\n")
	failed := doctorChecks(&b)

	fmt.Fprintf(&b, "\nthe report contains no credential values; that is enforced by\n")
	fmt.Fprintf(&b, "scanning this text against every key before printing it.\n")
	return b.String(), failed
}

// runDoctorReport prints the report, or fails without printing anything when
// a credential value would have appeared in it.
func runDoctorReport(w io.Writer) error {
	report, failed := buildDoctorReport()

	if leaked := scanReportForSecrets(report, reportSecrets()); len(leaked) > 0 {
		// Deliberately does not print the report or the value: this is a bug
		// in the report, and the user's next step is to send us the error
		// line, not their key.
		return fmt.Errorf("refusing to print the support report: it would contain %s "+
			"(values withheld). This is a bug in `oaica doctor --report` — please report it.",
			strings.Join(leaked, ", "))
	}

	if _, err := io.WriteString(w, report); err != nil {
		return err
	}
	if failed {
		return fmt.Errorf("doctor found failures (exit 1 for scripts)")
	}
	return nil
}

// reportHome is the home directory as reported, or "" when it cannot be
// determined.
func reportHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
