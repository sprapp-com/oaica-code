//go:build devtest

package launch

import (
	"os"
	"strings"
)

// license_api_dev.go — lets a `-tags devtest` build point the licence gate at a local oaica-saas
// (OAICA_LICENSE_API=http://localhost:4965/license) for an end-to-end Stripe test-mode run. A release build has
// no such switch: a variable that redirects the gate to a server of the user's own is a bypass.

func init() {
	if v := strings.TrimSpace(os.Getenv("OAICA_LICENSE_API")); v != "" {
		licenseServerAPI = strings.TrimRight(v, "/")
	}
}
