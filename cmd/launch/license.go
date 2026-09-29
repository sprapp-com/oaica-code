package launch

// license.go — one-off paid activation for the `launch` command family
// (2026-09-04). oaica-code is MIT-licensed (LICENSE, root of the repo),
// which permits anyone to build it from source for free — this gate is NOT
// DRM and cannot stop a determined source build. Its job is narrower: make
// the CONVENIENT path (a prebuilt binary someone downloaded) require a
// $20 one-off purchase (Stripe Checkout, with Stripe Tax), the same way most
// paid CLI tools built on permissive-licensed code work. See docs/LICENSING.md.
//
// Validation is against the oaica-saas licence API (oaica-saas/api/src/license.ts),
// whose request/response shapes follow the Lemon Squeezy License API this file
// used before the move to Stripe:
//   - POST /license/activate  — one-time, from `oaica activate <key>`,
//     binds the key to an "instance" (this machine) and stores the
//     returned instance_id. The server enforces the key's activation
//     limit itself; oaica-code does not re-implement seat counting.
//   - POST /license/validate  — periodic re-check (every
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
	"crypto/sha256"
	"encoding/hex"
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

// licenseServerAPI is the base URL of the oaica-saas licence API. Var-indirected so tests can point it at an
// httptest server; a `-tags devtest` build also reads OAICA_LICENSE_API (license_api_dev.go), a release build
// never does — redirecting the gate to a server of one's own is exactly the bypass the devtest doctrine forbids.
var licenseServerAPI = "https://oaica-saas-api.okx.workers.dev/license"

// oaicaPurchaseURL is printed in the "not activated" error — the page that starts Stripe Checkout for the
// licence (oaica-saas web/buy.html). It is the one place to update when the site moves.
const oaicaPurchaseURL = "https://oaica-saas-api.okx.workers.dev/buy.html"

// devTestLicenseKeys holds keys that activate and revalidate entirely locally,
// with no licence-server network call — for verifying the launch/license flow on
// a machine without spending a real purchase (e.g. a one-off install test on a
// public/cybercafe PC).
//
// It is EMPTY in a normal build. The list is populated by
// license_devkey_dev.go, which is compiled only under `-tags devtest`, because
// a key compiled into the shipped artifact is a key every user of that
// artifact can type: `oaica activate <the published key>` produced a perpetual
// licence with no network call and no purchase, so the paywall on the prebuilt
// binary — the thing the licence gate exists to be — was open to anyone who
// read the repository. The gate is a convenience paywall rather than DRM, and
// anyone building from source can bypass it; that is not a reason to also hand
// the bypass to every user of the binary (2026-09-26 audit, fifth round).
//
// Keeping it a variable rather than a const also keeps the MECHANISM testable
// in a release-shaped build: a test registers its own throwaway key here, and
// the behaviour of all three honouring paths is exercised without the tag.
var devTestLicenseKeys []string

// devTestKeyBuildTagged reports whether those keys came from the `devtest`
// build tag rather than being compiled into this build unconditionally. A test
// uses it to tell "the tag is doing its job" from "the key is back in the
// shipped binary".
var devTestKeyBuildTagged bool

// isTestLicenseKey reports whether key is a locally-honoured dev/test key.
//
// A predicate and not a string comparison against a sentinel: a release build
// that zeroed a constant would compare every stored and injected key against
// "", and a licence file holding `"key": ""` — or an empty OAICA_LICENSE_KEY
// that some upstream layer turned into a set-but-empty variable — would have
// been read as the dev key and let straight through the gate.
func isTestLicenseKey(key string) bool {
	for _, d := range devTestLicenseKeys {
		if d != "" && key == d {
			return true
		}
	}
	return false
}

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

// licenseRecordAge returns how long ago a validation stamp was, and whether
// that stamp is usable as evidence at all.
//
// A stamp in the FUTURE is not usable: `time.Since` returns a negative
// duration, and BOTH freshness tests in this file are `age < window`, so a
// negative age satisfied the revalidate TTL and the offline grace alike. The
// consequence was a permanent offline bypass with no purchase anywhere in it —
// writing `{"validated_at":"2126-01-01T00:00:00Z"}` into ~/.oaica/license.json
// (or into the OAICA_LICENSE_KEY anchor, which is the same record shape and the
// same comparison) made any string pass the gate forever, with no network call
// and no activation. A zero stamp is equally unusable: it certifies no
// validation happened, which is exactly what the grace window must not be
// anchored to. The file is one a user, a restore, or a skewed clock can write,
// so "the clock says this was validated recently" is only meaningful when the
// clock and the record agree — 2026-09-26 audit, seventh round.
func licenseRecordAge(validatedAt, now time.Time) (time.Duration, bool) {
	if validatedAt.IsZero() {
		return 0, false
	}
	age := now.Sub(validatedAt)
	if age < 0 {
		return 0, false
	}
	return age, true
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

// licenseResponse covers the fields both /activate and
// /validate return that this file actually reads; the server's
// response may carry more — ignored.
type licenseResponse struct {
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
	// Meta names the product the key was issued for; a response for any other product is not this gate's
	// (2026-09-29 audit, round 128, F128-L2-1; the Stripe licence server states `product` explicitly).
	Meta struct {
		Product string `json:"product"`
	} `json:"meta"`
}

// licenseProduct is the product name our licence server states in meta.product.
const licenseProduct = "oaica-code"

// licenseIssuedForUs reports whether a response is for this product. A response that states no product is
// accepted (the host is ours, over TLS, and the shape is the older one); one that states ANOTHER product is not.
func licenseIssuedForUs(r licenseResponse) bool {
	return r.Meta.Product == "" || r.Meta.Product == licenseProduct
}

func callLicenseAPI(path string, form url.Values) (licenseResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), licenseHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, licenseServerAPI+path, strings.NewReader(form.Encode()))
	if err != nil {
		return licenseResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return licenseResponse{}, err
	}
	defer resp.Body.Close()
	var parsed licenseResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return licenseResponse{}, fmt.Errorf("bad response from license server: %w", err)
	}
	if resp.StatusCode != http.StatusOK && parsed.Error == "" {
		return parsed, fmt.Errorf("license server: HTTP %d", resp.StatusCode)
	}
	if (parsed.Activated || parsed.Valid) && !licenseIssuedForUs(parsed) {
		parsed.Activated, parsed.Valid = false, false
		parsed.Error = "this key is not an oaica-code license"
	}
	return parsed, nil
}

// activateLicenseLive binds key to this machine via the
// licence server's /activate endpoint and persists the returned instance_id. instanceName
// defaults to the hostname when empty — a human-readable label the
// server's dashboard shows per activation, not used for any local logic.
func activateLicenseLive(key, instanceName string) (licenseFile, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return licenseFile{}, errors.New("empty license key")
	}
	if isTestLicenseKey(key) {
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
	resp, err := callLicenseAPI("/activate", form)
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
	resp, err := callLicenseAPI("/validate", form)
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
	// OAICA_LICENSE_KEY is the documented alternative to `oaica activate` —
	// README names it twice ("or `export OAICA_LICENSE_KEY=...`" and the
	// env-var table's "License key for gated models") — and until 2026-09-26
	// only `pull`/`serve` read it, so a deployment that injects the key from
	// a secret manager instead of running an interactive activation was told
	// to go buy a licence it already had (2026-09-26 audit).
	//
	// It is read FIRST, as pull/serve read it, and it wins over a stored
	// activation: a stale ~/.oaica/license.json on the same machine used to
	// shadow the injected key entirely, so the same variable meant two
	// different things depending on which command you ran (2026-09-26 audit,
	// second round).
	if key := strings.TrimSpace(os.Getenv("OAICA_LICENSE_KEY")); key != "" {
		return requireLicenseFromEnv(key)
	}
	f, err := loadLicenseFile()
	if err != nil {
		return fmt.Errorf(
			"oaica-code needs a one-time license — get one at %s, then run `oaica activate <key>`",
			oaicaPurchaseURL,
		)
	}

	if isTestLicenseKey(f.Key) {
		return nil // dev/test key — never revalidates over the network
	}

	age, ageOK := licenseRecordAge(f.ValidatedAt, time.Now())
	if ageOK && age < licenseRevalidateTTL {
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
	if ageOK && age < licenseOfflineGrace {
		fmt.Fprintf(os.Stderr, "warning: could not reach the license server (%v) — running on a cached license, %s remaining before this must reconnect\n",
			verr, (licenseOfflineGrace - age).Round(time.Hour))
		return nil
	}
	return fmt.Errorf(
		"license could not be re-validated (%v) and the %s offline grace period has expired — reconnect to the internet to continue",
		verr, licenseOfflineGrace,
	)
}

// requireLicenseFromEnv is the OAICA_LICENSE_KEY path. It has to match the
// stored-key path on BOTH properties that path has: a cached validation
// younger than the revalidation TTL costs no network call, and an unreachable
// licence server is bounded by the offline grace window rather than being a
// free pass forever.
//
// What it does NOT do is write the key. The anchor it persists holds a SHA-256
// of the key and the time it was last validated, never the secret itself, so a
// key injected from a secret manager stays in the secret manager.
func requireLicenseFromEnv(key string) error {
	if isTestLicenseKey(key) {
		return nil // dev/test key — never revalidates over the network
	}

	sum := sha256Hex(key)
	anchor, _ := loadEnvLicenseAnchor()
	// Same freshness rule as the stored path, and for the same reason: this
	// anchor is the same record shape under the same comparison, so a future
	// validated_at here was a bypass too (see licenseRecordAge).
	anchorAge, anchorOK := time.Duration(0), false
	if anchor != nil && anchor.KeySHA256 == sum {
		anchorAge, anchorOK = licenseRecordAge(anchor.ValidatedAt, time.Now())
	}
	if anchorOK && anchorAge < licenseRevalidateTTL {
		return nil // validated recently — no network call, same as a stored key
	}

	valid, verr := validateLicenseLive(key, "")
	if verr == nil {
		if !valid {
			return fmt.Errorf(
				"license %s from OAICA_LICENSE_KEY is no longer valid (refunded or revoked) — get a new one at %s",
				redactLicenseKey(key), oaicaPurchaseURL,
			)
		}
		_ = saveEnvLicenseAnchor(envLicenseAnchor{KeySHA256: sum, ValidatedAt: time.Now()})
		return nil
	}

	// Unreachable server. The stored path allows this for
	// licenseOfflineGrace only, anchored to its last successful validation —
	// an env key with no such anchor would otherwise work forever without
	// ever contacting the licence server, which is not "the same as a stored
	// key", it is a different licence mode (2026-09-26 audit).
	if anchorOK && anchorAge < licenseOfflineGrace {
		fmt.Fprintf(os.Stderr, "warning: could not reach the license server (%v) — running on the OAICA_LICENSE_KEY activation cached %s ago, %s remaining before this must reconnect\n",
			verr, anchorAge.Round(time.Hour), (licenseOfflineGrace - anchorAge).Round(time.Hour))
		return nil
	}
	return fmt.Errorf(
		"the license key in OAICA_LICENSE_KEY could not be validated (%v) and no successful validation of it is on record — reconnect to the internet, or run `oaica activate <key>` once",
		verr,
	)
}

// envLicenseAnchor records that a key from OAICA_LICENSE_KEY was validated at
// ValidatedAt. It stores a digest, not the key: the secret stays wherever the
// deployment injected it from.
type envLicenseAnchor struct {
	KeySHA256   string    `json:"key_sha256"`
	ValidatedAt time.Time `json:"validated_at"`
}

func envLicenseAnchorPath() (string, error) {
	path, err := licenseFilePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "license_env.json"), nil
}

func loadEnvLicenseAnchor() (*envLicenseAnchor, error) {
	path, err := envLicenseAnchorPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a envLicenseAnchor
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func saveEnvLicenseAnchor(a envLicenseAnchor) error {
	path, err := envLicenseAnchorPath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, b)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// redactMinLen is the shortest key that can afford a fingerprint. The
// "first four and last four" shape is a fixed eight characters, so on
// anything much shorter it stops being a fingerprint and becomes the secret:
// a nine-character key came back as eight of its nine characters, printed
// into an error message that ends up in a log, a screenshot or a support
// ticket. Real keys (`oaica-lic-` plus 32 hex characters) are far above
// this; a short one is a mis-paste, and for a mis-paste the length alone is
// what the user needs.
const redactMinLen = 20

// redactLicenseKey shows enough of a key for the user to recognize it in
// an error message without echoing the whole secret back to a terminal
// that might be recorded/shared. A key too short to carry a fingerprint is
// shown as its length and nothing else.
func redactLicenseKey(key string) string {
	if key == "" {
		return "****"
	}
	if len(key) < redactMinLen {
		return fmt.Sprintf("****(%d chars)", len(key))
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

Binds the key to this machine (the key's activation limit
applies — most keys allow a small number of machines). Run once per
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
