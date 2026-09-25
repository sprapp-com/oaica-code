package launch

// doctor_report.go — `oaica doctor --report`: a support bundle a user can
// paste into a ticket without leaking credentials.
//
// Two things make it safe rather than merely careful:
//
//  1. Every value that is a credential anywhere in this client (the
//     OAICA_API_KEY environment variable, every remote's key from
//     remotes.json, every key in auth.json) is collected first, and paths are
//     reported as present/absent plus their file mode — never their contents.
//  2. The rendered text is scanned against those values *before* it is
//     printed. If one appears, the report is not printed at all and the
//     command fails: a redaction-by-intention bug must not become a leaked
//     key in someone's support ticket.

import (
	"fmt"
	"io"
	"net/url"
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
	for _, env := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		add(env, os.Getenv(env))
	}
	if remotes, err := loadUserRemotes(); err == nil {
		for _, r := range remotes {
			add("the remotes.json key for "+r.Name, r.key())
			// A key can also arrive as URL userinfo (https://KEY@host/v1),
			// which key() never sees because the transport, not this client,
			// turns it into the Authorization header.
			if u, perr := url.Parse(strings.TrimSpace(r.BaseURL)); perr == nil && u.User != nil {
				add("the key embedded in the base_url of "+r.Name, u.User.Username())
				if pw, has := u.User.Password(); has {
					add("the key embedded in the base_url of "+r.Name, pw)
				}
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
	return secrets
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
	if fi.IsDir() {
		fmt.Fprintf(w, "  directory         %s\n", path)
		return
	}
	note := ""
	if sensitive {
		mode := fi.Mode().Perm()
		note = fmt.Sprintf("  mode %04o", mode)
		// 0o077 = any group/other bit: the file is readable beyond its owner.
		if mode&0o077 != 0 {
			note += "  <-- readable by other users"
		}
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
		describeFile(&b, filepath.Join(home, ".oaica", "auth.json"), true)
		describeFile(&b, filepath.Join(home, ".oaica", "remotes.json"), true)
		describeFile(&b, filepath.Join(home, ".oaica", "config.json"), false)
		describeFile(&b, filepath.Join(home, ".oaica", "cache"), false)
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
