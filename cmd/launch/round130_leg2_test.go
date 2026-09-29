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
