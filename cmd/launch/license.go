package launch

// license.go — one-off paid activation for the `launch` command family
// (2026-09-04). oaica-code is MIT-licensed (LICENSE, root of the repo),
// which permits anyone to build it from source for free — this gate is NOT
// DRM and cannot stop a determined source build. Its job is narrower: make
// the CONVENIENT path (a prebuilt binary someone downloaded) require a
// $20 one-off Lemon Squeezy purchase, the same way most paid CLI tools
// built on permissive-licensed code work. See docs/LICENSING.md.
//
// Validation is against Lemon Squeezy's public License API
// (https://docs.lemonsqueezy.com/help/licensing/license-api):
//   - POST /v1/licenses/activate  — one-time, from `oaica activate <key>`,
//     binds the key to an "instance" (this machine) and stores the
//     returned instance_id. Lemon Squeezy enforces the product's
//     activation limit itself; oaica-code does not re-implement seat
//     counting.
//   - POST /v1/licenses/validate  — periodic re-check (every
//     licenseRevalidateTTL) that the key is still valid (not refunded or
//     manually revoked). Cheap, side-effect-free, safe to call often.
//
// Offline tolerance: a machine that activated successfully once must not
// get locked out by a flaky network on every subsequent launch. A cached
// license younger than licenseRevalidateTTL skips the network call
// entirely; one older than that but younger than licenseOfflineGrace still
// launches (with a one-line stderr notice) while a fresh validate is
// attempted in the background-equivalent (best-effort, synchronous but
// short-timeout) — only past the full grace window does an unreachable
// license server actually block a previously-activated install.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// lemonSqueezyLicenseAPI is the base URL for Lemon Squeezy's License API.
// Var-indirected so tests can point it at an httptest server.
var lemonSqueezyLicenseAPI = "https://api.lemonsqueezy.com/v1/licenses"

// oaicaPurchaseURL is printed in the "not activated" error — the Lemon
// Squeezy product checkout page. Replace with the real product URL once
// the Lemon Squeezy product is created; kept as a named const so it is
// the one place to update.
const oaicaPurchaseURL = "https://oaica.lemonsqueezy.com/buy/oaica-code"

// testLicenseKey is a dev/test-only key that activates and revalidates
// entirely locally, with no Lemon Squeezy network call — for verifying the
// launch/license flow on a machine without spending a real purchase (e.g.
// a one-off install test on a public/cybercafe PC). Not a secret: it is
// visible in source, matching this project's own stated stance (see the
// package doc comment above) that the license gate is a convenience paywall
// on the prebuilt binary, not DRM. Anyone building from source could add
// their own bypass anyway; this just gives the maintainer one without
// touching real Lemon Squeezy activation state.
const testLicenseKey = "OAICA-TEST-DEV-FREE"

const (
	// licenseRevalidateTTL: how long a successful validate is trusted
	// before the next launch re-checks live. Short enough that a revoked/
	// refunded license stops working within a reasonable window; long
	// enough that ordinary use never waits on network.
	licenseRevalidateTTL = 7 * 24 * time.Hour
	// licenseOfflineGrace: how long a PREVIOUSLY-validated license keeps
	// working with zero network reachability before launch actually
	// blocks. Generous — a paying user on a plane or a flaky connection
	// must not get locked out of a tool they paid for.
	licenseOfflineGrace = 30 * 24 * time.Hour
	// licenseHTTPTimeout is short: this runs on every launch once the
	// revalidate TTL has expired, same reasoning as context_window_remote.go's
	// probeTimeout — it must not add seconds of startup latency to the
	// common (already-cached) case, and even the uncached case shouldn't
	// hang.
	licenseHTTPTimeout = 5 * time.Second
)

type licenseFile struct {
	Key          string    `json:"key"`
	InstanceID   string    `json:"instance_id"`
	InstanceName string    `json:"instance_name"`
	ActivatedAt  time.Time `json:"activated_at"`
	// ValidatedAt is the last time a live /validate call succeeded. Empty
	// (zero value) only right after activation, before the first
	// requireLicense call — Activate() sets it too so a freshly-activated
	// install never re-validates on its very first launch.
	ValidatedAt time.Time `json:"validated_at"`
}

func licenseFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "license.json"), nil
}

func loadLicenseFile() (licenseFile, error) {
	path, err := licenseFilePath()
	if err != nil {
		return licenseFile{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return licenseFile{}, err
	}
	var f licenseFile
	if err := json.Unmarshal(b, &f); err != nil {
		return licenseFile{}, err
	}
	return f, nil
}

func saveLicenseFile(f licenseFile) error {
	path, err := licenseFilePath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, b)
}

// lemonSqueezyLicenseResponse covers the fields both /activate and
// /validate return that this file actually reads. Lemon Squeezy's
// response carries more (customer/order/product metadata) — ignored.
type lemonSqueezyLicenseResponse struct {
	Activated bool   `json:"activated"`
	Valid     bool   `json:"valid"`
	Error     string `json:"error"`
	Instance  *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"instance"`
	LicenseKey struct {
		Status string `json:"status"`
	} `json:"license_key"`
}

func callLemonSqueezyLicenseAPI(path string, form url.Values) (lemonSqueezyLicenseResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), licenseHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lemonSqueezyLicenseAPI+path, strings.NewReader(form.Encode()))
	if err != nil {
		return lemonSqueezyLicenseResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return lemonSqueezyLicenseResponse{}, err
	}
	defer resp.Body.Close()
	var parsed lemonSqueezyLicenseResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return lemonSqueezyLicenseResponse{}, fmt.Errorf("bad response from license server: %w", err)
	}
	if resp.StatusCode != http.StatusOK && parsed.Error == "" {
		return parsed, fmt.Errorf("license server: HTTP %d", resp.StatusCode)
	}
	return parsed, nil
}

// activateLicenseLive binds key to this machine via Lemon Squeezy's
// /activate endpoint and persists the returned instance_id. instanceName
// defaults to the hostname when empty — a human-readable label Lemon
// Squeezy's dashboard shows per activation, not used for any local logic.
func activateLicenseLive(key, instanceName string) (licenseFile, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return licenseFile{}, errors.New("empty license key")
	}
	if key == testLicenseKey {
		now := time.Now()
		return licenseFile{Key: key, ActivatedAt: now, ValidatedAt: now, InstanceName: "test", InstanceID: "test"}, nil
	}
	if instanceName == "" {
		instanceName, _ = os.Hostname()
		if instanceName == "" {
			instanceName = "unknown-host"
		}
	}
	form := url.Values{"license_key": {key}, "instance_name": {instanceName}}
	resp, err := callLemonSqueezyLicenseAPI("/activate", form)
	if err != nil {
		return licenseFile{}, fmt.Errorf("could not reach license server: %w", err)
	}
	if !resp.Activated {
		msg := resp.Error
		if msg == "" {
			msg = "activation rejected (already at the seat limit for this key?)"
		}
		return licenseFile{}, fmt.Errorf("license activation failed: %s", msg)
	}
	now := time.Now()
	f := licenseFile{Key: key, ActivatedAt: now, ValidatedAt: now, InstanceName: instanceName}
	if resp.Instance != nil {
		f.InstanceID = resp.Instance.ID
	}
	return f, nil
}

// validateLicenseLive re-checks a previously activated key+instance is
// still valid (not refunded/revoked) — never mutates activation state.
func validateLicenseLive(key, instanceID string) (bool, error) {
	form := url.Values{"license_key": {key}}
	if instanceID != "" {
		form.Set("instance_id", instanceID)
	}
	resp, err := callLemonSqueezyLicenseAPI("/validate", form)
	if err != nil {
		return false, err
	}
	return resp.Valid, nil
}

// requireLicenseFn is swappable so tests (and any future free/OSS build
// tag) can skip the real network-backed gate. requireLicenseLive is the
// production default — see composeLaunchPrecondition in cmd.go for how
// this is wired alongside oaicaEnsureSignedIn without changing LaunchCmd's
// signature or any existing test call site.
var requireLicenseFn = requireLicenseLive

// RequireLicense is the exported entry point cmd.go composes into
// LaunchCmd's single injected PreRunE precondition (alongside
// oaicaEnsureSignedIn) — see composeLaunchPrecondition there. Kept as a
// thin wrapper around the swappable requireLicenseFn so package launch's
// own tests can still stub the var directly without an import cycle.
func RequireLicense(cmd *cobra.Command, args []string) error {
	return requireLicenseFn(cmd, args)
}

func requireLicenseLive(cmd *cobra.Command, args []string) error {
	f, err := loadLicenseFile()
	if err != nil {
		// No license.json at all — never activated on this machine.
		return fmt.Errorf(
			"oaica-code needs a one-time license — get one at %s, then run `oaica activate <key>`",
			oaicaPurchaseURL,
		)
	}

	if f.Key == testLicenseKey {
		return nil // dev/test key — never revalidates over the network
	}

	age := time.Since(f.ValidatedAt)
	if age < licenseRevalidateTTL {
		return nil // cached, still fresh — no network call on the common path
	}

	valid, verr := validateLicenseLive(f.Key, f.InstanceID)
	if verr == nil {
		if !valid {
			return fmt.Errorf(
				"license %s is no longer valid (refunded or revoked) — get a new one at %s",
				redactLicenseKey(f.Key), oaicaPurchaseURL,
			)
		}
		f.ValidatedAt = time.Now()
		_ = saveLicenseFile(f) // best-effort; a write failure just re-checks next launch
		return nil
	}

	// Network/server error, not an explicit invalid answer: fall back to
	// the offline grace window rather than punishing a paying user for a
	// bad connection. Past the grace window this is a hard block.
	if age < licenseOfflineGrace {
		fmt.Fprintf(os.Stderr, "warning: could not reach the license server (%v) — running on a cached license, %s remaining before this must reconnect\n",
			verr, (licenseOfflineGrace - age).Round(time.Hour))
		return nil
	}
	return fmt.Errorf(
		"license could not be re-validated (%v) and the %s offline grace period has expired — reconnect to the internet to continue",
		verr, licenseOfflineGrace,
	)
}

// redactLicenseKey shows enough of a key for the user to recognize it in
// an error message without echoing the whole secret back to a terminal
// that might be recorded/shared.
func redactLicenseKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "…" + key[len(key)-4:]
}

// ActivateCmd is `oaica activate <key>` — the one-time step after
// purchase. Separate top-level command (not under `launch`) so it reads
// naturally: buy, activate, then launch works from then on.
func ActivateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "activate <license-key>",
		Short: "Activate your oaica-code license on this machine",
		Long: fmt.Sprintf(`Activate a one-time oaica-code license purchased at:

  %s

Binds the key to this machine (Lemon Squeezy's activation-limit rules
apply — most keys allow a small number of machines). Run once per
machine; after that, 'oaica launch ...' works without any extra step.`, oaicaPurchaseURL),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := strings.TrimSpace(args[0])
			f, err := activateLicenseLive(key, "")
			if err != nil {
				return err
			}
			if err := saveLicenseFile(f); err != nil {
				return fmt.Errorf("activated, but failed to save the license locally: %w", err)
			}
			fmt.Printf("License activated on %q. `oaica launch` is ready to use.\n", f.InstanceName)
			return nil
		},
	}
}
