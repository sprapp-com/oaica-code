package launch

// remote_skipped_row_visibility_integrity_test.go — a remotes.json row with an
// empty name or base_url vanished with no warning anywhere (2026-09-26 audit,
// tenth round, finding 23).
//
// loadUserRemotes dropped such a row with a bare `continue`: a user who
// hand-edited remotes.json into that state saw a remote simply not exist. The
// picker, `remote list` and `doctor` all omitted it, and nothing on any of them
// said the file had a row it could not use — the one case where the user most
// needs to be told which file to fix. The row genuinely cannot be used (there is
// no host to talk to, and no name for "<remote>/<model>" to resolve against), so
// it stays skipped; the skip is what has to be visible.

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// The skipped row is reported with the file and the reason, and the valid row
// beside it still loads.
func TestASkippedRemoteRowIsReportedWithItsReason(t *testing.T) {
	path := withTempRemotesFile(t)

	body := `{"remotes":[
	  {"name":"","base_url":"https://nameless.example.com"},
	  {"name":"broken","base_url":""},
	  {"name":"good","base_url":"https://api.example.com","api_key":"sk-good"}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var list bytes.Buffer
	out := captureStderr(t, func() {
		if err := WriteRemoteList(&list); err != nil {
			t.Errorf("WriteRemoteList: %v", err)
		}
	})

	if !strings.Contains(list.String(), "good") {
		t.Errorf("the valid row in the same file did not load:\n%s", list.String())
	}
	if strings.Contains(list.String(), "broken") || strings.Contains(list.String(), "nameless.example.com") {
		t.Errorf("a row with no name or no base_url was offered anyway:\n%s", list.String())
	}

	for _, want := range []string{path, "row 1", "no name", "row 2", "broken", "no base_url"} {
		if !strings.Contains(out, want) {
			t.Errorf("the skip report is missing %q — a dropped row must name the file to fix and why the row was dropped:\n%s", want, out)
		}
	}

	// The launch path loads the same store through launchSweepRemotes, and a
	// user who never runs `remote list` must still be told.
	sweepOut := captureStderr(t, func() {
		if _, errs := launchSweepRemotes(); len(errs) != 0 {
			t.Errorf("a file with one unusable row was reported as unreadable as a whole: %v", errs)
		}
	})
	if !strings.Contains(sweepOut, "row 2") || !strings.Contains(sweepOut, path) {
		t.Errorf("the launch sweep did not report the skipped row or the file:\n%s", sweepOut)
	}
}

// Control: a healthy store warns nothing, so the tests above cannot be passed
// by warning about every load.
func TestAHealthyRemotesFileWarnsNothingAboutSkips(t *testing.T) {
	withTempRemotesFile(t)
	if _, err := RemoteAdd(RemoteAddOptions{Name: "mine", BaseURL: "https://api.example.com"}); err != nil {
		t.Fatalf("add: %v", err)
	}

	var list bytes.Buffer
	out := captureStderr(t, func() {
		if err := WriteRemoteList(&list); err != nil {
			t.Errorf("WriteRemoteList: %v", err)
		}
	})
	if strings.Contains(out, "Warning") || strings.Contains(out, "skipped") {
		t.Errorf("a healthy remotes.json produced a skip report:\n%s", out)
	}
	if !strings.Contains(list.String(), "mine") {
		t.Errorf("the healthy row did not list:\n%s", list.String())
	}
}
