package launch

// F128-L2-1 (2026-09-29 audit, round 128): once the store and product are configured, a key issued for
// another Lemon Squeezy store's product does not open the gate — by activation or by the env anchor.

import (
	"net/http"
	"testing"
)

func TestRound128ForeignStoreKeyIsRefusedOnceTheProductIsConfigured(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	oldS, oldP := oaicaLemonStoreID, oaicaLemonProductID
	oaicaLemonStoreID, oaicaLemonProductID = 111, 222
	t.Cleanup(func() { oaicaLemonStoreID, oaicaLemonProductID = oldS, oldP })
	stubLemonSqueezy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		store, product := "999999", "424242"
		if r.FormValue("license_key") == "OURS" {
			store, product = "111", "222"
		}
		switch r.URL.Path {
		case "/activate":
			w.Write([]byte(`{"activated":true,"error":null,"license_key":{"status":"active"},"instance":{"id":"inst-1","name":"x"},"meta":{"store_id":` + store + `,"product_id":` + product + `}}`))
		case "/validate":
			w.Write([]byte(`{"valid":true,"error":null,"license_key":{"status":"active"},"instance":{"id":"inst-1"},"meta":{"store_id":` + store + `,"product_id":` + product + `}}`))
		}
	})
	if f, err := activateLicenseLive("FOREIGN", ""); err == nil {
		t.Errorf("a foreign store's key activated: %+v", f)
	}
	t.Setenv("OAICA_LICENSE_KEY", "FOREIGN")
	if err := requireLicenseLive(nil, nil); err == nil {
		t.Errorf("a foreign store's key in OAICA_LICENSE_KEY passed the launch gate")
	}
	if _, err := activateLicenseLive("OURS", ""); err != nil {
		t.Errorf("a key for this product was refused: %v", err)
	}
}
