//go:build devtest

package launch

// license_devkey_dev.go — the dev/test licence key, compiled in ONLY under
// `-tags devtest`.
//
// Build with the tag to verify the launch/licence flow on a machine with no
// purchase (a one-off install test on a public/cybercafe PC, a CI smoke test of
// `oaica activate`):
//
//	go build -tags devtest -o /tmp/oaica-dev ./cmd/oaica
//	/tmp/oaica-dev activate OAICA-TEST-DEV-FREE
//
// Without the tag — every release artifact — the key is not in the binary at
// all, so it cannot be typed, and `isTestLicenseKey` is false for every input
// (see license.go for why the list was moved out of the default build).
//
// The key is not a secret and never was; the point is that a key published in
// the source must not work in the artifact users pay for.

func init() {
	devTestKeyBuildTagged = true
	devTestLicenseKeys = append(devTestLicenseKeys, "OAICA-TEST-DEV-FREE")
}
