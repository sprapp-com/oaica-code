package cmd

// updatecheck_firstcheck_integrity_test.go — the update notice named the
// version the CACHE held before this run's fetch, not the one the fetch just
// returned. On a machine whose cache is empty the local value is "", so the
// check reported "nothing to say" and stored the answer — the notice appeared
// only on the NEXT command, naming a version the client had by then already
// been behind for a whole invocation. A fresh install is exactly the machine
// that most needs to be told, and it is the one that always takes this path
// (2026-09-26 audit).
//
// The same one-command delay hit a stale cache: 0.5.45 cached, 0.5.47
// upstream, one run refreshed the cache and said nothing.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTheCheckAnnouncesTheVersionItJustFetched(t *testing.T) {
	setUpdateCheckHome(t)
	setUpdateCheckVersion(t, "0.5.46")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("version=0.5.47\ncommit=deadbeef\n"))
	}))
	defer srv.Close()
	oldURL := updateCheckURLForTest(srv.URL)
	defer oldURL()

	var notice bytes.Buffer
	oldOut := updateNoticeOut
	updateNoticeOut = &notice
	defer func() { updateNoticeOut = oldOut }()

	checkForUpdate()

	if !strings.Contains(notice.String(), "0.5.47") {
		t.Errorf("a fresh machine (empty cache) was told nothing: notice = %q — the check compared against the version the cache held BEFORE its own fetch, so the user hears about an update only on the next command", notice.String())
	}
	if !strings.Contains(notice.String(), "0.5.46") {
		t.Errorf("notice = %q, want it to name the installed version", notice.String())
	}

	// The control: the notice is announced once, not on every command.
	var second bytes.Buffer
	updateNoticeOut = &second
	checkForUpdate()
	if second.Len() != 0 {
		t.Errorf("the same update was announced twice: %q", second.String())
	}
}
