package launch

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// F130-L2-4: a server's error body reaches the terminal quoted, on one line.
func TestRound130SyncErrorBodyIsQuoted(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("bad gateway\x1b]52;c;AAAA\a\x1b[2J\nError: forged"))
	}))
	defer srv.Close()
	_, err := CatalogSync(srv.URL)
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.ContainsAny(err.Error(), "\x1b\a\n") {
		t.Errorf("error carries a control sequence or a forged line: %q", err.Error())
	}
}

// C1 controls (U+009B CSI, U+009D OSC) and raw invalid bytes are quoted like the C0 ones.
func TestRound130PrintableCellQuotesC1AndInvalidBytes(t *testing.T) {
	for _, s := range []string{"evil\u009b2J", "evil\u009d52;c;AA", "raw\x9b2J"} {
		leak := func(o string) bool {
			return strings.ContainsAny(o, "\u009b\u009d") || strings.Contains(o, "\x9b") || strings.Contains(o, "\x9d")
		}
		if leak(PrintableCell(s)) || leak(printableName(s)) {
			t.Errorf("%q leaks a C1 control: cell=%q name=%q", s, PrintableCell(s), printableName(s))
		}
	}
}

// F131-L2-5: a mirror's userinfo key echoed in a body is redacted.
func TestRound131UserinfoKeyIsRedactedFromBodies(t *testing.T) {
	got := redactURLUserinfo("https://mirrorkey-8f2a91c0d3e4@mirror.example/api.json", "invalid key mirrorkey-8f2a91c0d3e4 auth=Basic bWlycm9ya2V5LThmMmE5MWMwZDNlNDo=")
	if strings.Contains(got, "mirrorkey-8f2a91c0d3e4") {
		t.Errorf("key survived: %q", got)
	}
}

// F131-L2-4: the router error prints quoted.
func TestRound131RouterErrorIsPrintable(t *testing.T) {
	e := &oaicaRouterError{Status: 500, Host: "http://127.0.0.1:1", Body: "oops\x1b]52;c;ZXZpbA==\a\x1b[2J"}
	if strings.ContainsAny(e.Error(), "\x1b\a") {
		t.Errorf("router error carries a control sequence: %q", e.Error())
	}
}

func TestRound131SyncBodyDoesNotEchoTheMirrorKey(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("invalid key mirrorkey-8f2a91c0d3e4"))
	}))
	defer srv.Close()
	u := strings.Replace(srv.URL, "http://", "http://mirrorkey-8f2a91c0d3e4@", 1)
	_, err := CatalogSync(u)
	if err == nil || strings.Contains(err.Error(), "mirrorkey-8f2a91c0d3e4") {
		t.Errorf("error echoes the mirror key: %v", err)
	}
}
