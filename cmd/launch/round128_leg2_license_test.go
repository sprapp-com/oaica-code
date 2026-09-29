package launch

// F128-L2-1 (2026-09-29 audit, round 128; reshaped for the Stripe licence server, round 132): a key the server
// answers for ANOTHER product does not open the gate — by activation or by the env anchor.

import (
	"net/http"
	"testing"
)

func TestRound128ForeignProductKeyIsRefused(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		product := "someone-elses-product"
		if r.FormValue("license_key") == "OURS" {
			product = "oaica-code"
		}
		switch r.URL.Path {
		case "/activate":
			w.Write([]byte(`{"activated":true,"error":null,"license_key":{"status":"active"},"instance":{"id":"inst-1","name":"x"},"meta":{"product":"` + product + `"}}`))
		case "/validate":
			w.Write([]byte(`{"valid":true,"error":null,"license_key":{"status":"active"},"instance":{"id":"inst-1"},"meta":{"product":"` + product + `"}}`))
		}
	})
	if f, err := activateLicenseLive("FOREIGN", ""); err == nil {
		t.Errorf("a foreign product's key activated: %+v", f)
	}
	t.Setenv("OAICA_LICENSE_KEY", "FOREIGN")
	if err := requireLicenseLive(nil, nil); err == nil {
		t.Errorf("a foreign product's key in OAICA_LICENSE_KEY passed the launch gate")
	}
	if _, err := activateLicenseLive("OURS", ""); err != nil {
		t.Errorf("a key for this product was refused: %v", err)
	}
}
