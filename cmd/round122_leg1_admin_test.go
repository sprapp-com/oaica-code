package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRound122AdminKeyEchoedInError(t *testing.T) {
	const admin = "adm-live-PROBEADMIN-77aa"
	const license = "lic-live-PROBELICENSE-88bb"
	echo := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>upstream rejected: " + r.Header.Get("Authorization") + "</html>"))
	}
	srv := httptest.NewServer(http.HandlerFunc(echo))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OAICA_HOST", srv.URL)
	t.Setenv("OAICA_API_KEY", "")
	t.Setenv("OAICA_ADMIN_KEY", admin)
	t.Setenv("OAICA_LICENSE_KEY", license)

	_, errList := oaicaAuthList()
	errLogin := oaicaAuthLogin("p", "https://x.example", "", "")
	errLogout := oaicaAuthLogout("p")
	_, errManifest := oaicaFetchManifest("m")
	for name, e := range map[string]error{"router list": errList, "router login": errLogin, "router logout": errLogout} {
		t.Logf("%s: %v", name, e)
		if strings.Contains(e.Error(), admin) {
			t.Errorf("RED: %s printed OAICA_ADMIN_KEY", name)
		}
	}
	t.Logf("pull manifest (licence-bearing, same echo): %v", errManifest)
}
