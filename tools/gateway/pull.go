package main

// pull.go — the weights-distribution routes for `oaica pull <model>`:
// GET /v1/manifest/{model}, GET /v1/pull/{model}, GET /v1/catalog.
//
// WHY this file exists: these three routes used to live in the TypeScript
// prism-api-router Worker that fronted api.oaica.com. The cutover moved
// api.oaica.com onto this Go gateway and the Worker was deleted, but the
// routes were never ported — so every `oaica pull` since the cutover hit
// the mux's catch-all and got `{"error":{"type":"not_found","message":
// "unknown route"}}`. Local self-hosting has been broken for users since
// then (P0). The client is cmd/oaica_pull_serve.go; the wire contract
// below is fixed by its oaicaManifest struct and by the two error `type`
// strings it string-matches on ("license_required" / "license_invalid").
//
// WHY hf-source models bypass the gateway for the actual bytes: a GGUF is
// tens of gigabytes. Proxying that through this process would burn our
// egress and CPU to re-serve a blob HuggingFace already hosts for free on
// fast, globally cached infrastructure. So for source=="hf" the manifest
// hands the client the HF URL and the client fetches it directly; the
// gateway only ever sees the small manifest request, which is where the
// license check happens. source=="file" is the fallback for weights we
// cannot publish on HF (or that live only on this box) — there the
// gateway does stream the bytes itself.
//
// WHY license keys are hashed: pull_license_keys stores only the SHA-256
// of each bearer token, exactly like api_keys (gwKey). The config file is
// checked into ops repos and copied between machines; a plaintext
// credential in it would be a distribution license leaked to anyone who
// ever reads a config backup. Hashing also means this file can be shared
// with an operator who must add a key without learning existing ones.
// The license key is deliberately a SEPARATE credential from api_keys: a
// leaked chat API key must never double as a weights-download key.
//
// Abuse/rate limiting: none applied here on purpose. /v1/manifest and
// /v1/catalog are tiny, in-memory, config-only responses (cheaper than the
// already-public /v1/models), and /v1/pull is served from a small, fixed,
// operator-curated set of local files. If public /v1/pull traffic ever
// becomes a bandwidth problem the fix is to move that model to source=hf,
// not to add a limiter here.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// gwPullEntry is one downloadable model in `pull_catalog`.
type gwPullEntry struct {
	// Model is the id users type: `oaica pull <model>`. It need not match
	// any gwModel id — the catalog of weights you can download locally is
	// not the same set as the models this gateway serves over the API.
	Model string `json:"model"`
	// Source is "hf" (client downloads from HFURL itself) or "file" (this
	// gateway streams FilePath).
	Source string `json:"source"`
	// HFURL is the direct resolve/ URL of the GGUF on HuggingFace.
	HFURL string `json:"hf_url,omitempty"`
	// FilePath is an absolute path on this host, source=="file" only.
	FilePath string `json:"file_path,omitempty"`
	// SizeBytes is the size of the blob AS DOWNLOADED — for an encrypted
	// hf blob that is the ciphertext size, not the plaintext GGUF size
	// (the client knows this and does not size-check the hf path).
	SizeBytes int64 `json:"size_bytes"`
	// SHA256 of the downloaded blob, optional ("" => manifest sends null).
	SHA256 string `json:"sha256,omitempty"`
	// LicenseRequired gates both the manifest and /v1/pull behind a
	// pull_license_keys bearer token.
	LicenseRequired bool `json:"license_required"`
	// DecryptKeyHex is the 64-hex-char AES-256 key for an ENCRYPTED hf
	// blob (chunked AES-256-GCM; see decryptChunkedAESGCMStream in
	// cmd/oaica_pull_serve.go). Encryption is what lets a licensed model
	// sit in a public HF repo without leaking the weights: the bytes are
	// free to fetch, the key is not. Only ever emitted after the license
	// check passes; omit entirely for a plain, unencrypted hf blob.
	DecryptKeyHex string `json:"decrypt_key,omitempty"`
	Description   string `json:"description,omitempty"`
}

// gwManifest is the /v1/manifest/{model} response. Field names and
// nullability must match cmd/oaica_pull_serve.go's oaicaManifest exactly —
// its SHA256/HFURL/DecryptKeyHex are *string, so those are pointers here
// and marshal to null when absent rather than being omitted.
type gwManifest struct {
	Model     string  `json:"model"`
	SizeBytes int64   `json:"size_bytes"`
	SHA256    *string `json:"sha256"`
	// PullURL is "/v1/pull/<model>" for source=="file" and "" for hf — the
	// client only reads it when Source != "hf".
	PullURL       string  `json:"pull_url"`
	Source        string  `json:"source"`
	HFURL         *string `json:"hf_url"`
	DecryptKeyHex *string `json:"decrypt_key"`
}

// gwCatalogEntry is one row of the public /v1/catalog listing. It
// deliberately omits hf_url/file_path/decrypt_key: the catalog answers
// "what can I pull", not "where are the bytes" — that second question is
// the manifest's job, and is where the license gate lives.
type gwCatalogEntry struct {
	Model           string `json:"model"`
	SizeBytes       int64  `json:"size_bytes"`
	Description     string `json:"description,omitempty"`
	LicenseRequired bool   `json:"license_required"`
}

// validatePullConfig is called from loadConfig so a malformed catalog is
// refused at load/reload time rather than surfacing as a broken pull for a
// user hours later.
func validatePullConfig(cfg gwConfig) error {
	seen := map[string]bool{}
	for i, e := range cfg.PullCatalog {
		if strings.TrimSpace(e.Model) == "" {
			return fmt.Errorf("pull_catalog[%d]: model is required", i)
		}
		if seen[e.Model] {
			return fmt.Errorf("pull_catalog[%d]: duplicate model %q", i, e.Model)
		}
		seen[e.Model] = true
		switch e.Source {
		case "hf":
			if strings.TrimSpace(e.HFURL) == "" {
				return fmt.Errorf("pull_catalog[%d] (%s): source=hf requires hf_url", i, e.Model)
			}
		case "file":
			if strings.TrimSpace(e.FilePath) == "" {
				return fmt.Errorf("pull_catalog[%d] (%s): source=file requires file_path", i, e.Model)
			}
		default:
			return fmt.Errorf("pull_catalog[%d] (%s): source must be \"hf\" or \"file\" (got %q)", i, e.Model, e.Source)
		}
		if e.SizeBytes <= 0 {
			return fmt.Errorf("pull_catalog[%d] (%s): size_bytes must be > 0", i, e.Model)
		}
		if e.SHA256 != "" {
			if len(e.SHA256) != 64 {
				return fmt.Errorf("pull_catalog[%d] (%s): sha256 must be 64 hex chars (got %d)", i, e.Model, len(e.SHA256))
			}
			if _, err := hex.DecodeString(e.SHA256); err != nil {
				return fmt.Errorf("pull_catalog[%d] (%s): sha256 is not hex: %w", i, e.Model, err)
			}
		}
		if e.DecryptKeyHex != "" {
			// The client rejects anything that is not a 32-byte AES-256 key.
			if len(e.DecryptKeyHex) != 64 {
				return fmt.Errorf("pull_catalog[%d] (%s): decrypt_key must be 64 hex chars (AES-256)", i, e.Model)
			}
			if _, err := hex.DecodeString(e.DecryptKeyHex); err != nil {
				return fmt.Errorf("pull_catalog[%d] (%s): decrypt_key is not hex: %w", i, e.Model, err)
			}
		}
	}
	for i, k := range cfg.PullLicenseKeys {
		if len(k.SHA256) != 64 {
			return fmt.Errorf("pull_license_keys[%d]: sha256 must be 64 hex chars (got %d)", i, len(k.SHA256))
		}
		if _, err := hex.DecodeString(k.SHA256); err != nil {
			return fmt.Errorf("pull_license_keys[%d]: sha256 is not hex: %w", i, err)
		}
	}
	return nil
}

// pullEntry looks up a catalog entry under the config lock.
func (g *gateway) pullEntry(model string) (gwPullEntry, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, e := range g.cfg.PullCatalog {
		if e.Model == model {
			return e, true
		}
	}
	return gwPullEntry{}, false
}

// licenseLabel mirrors keyLabel but against pull_license_keys — same
// constant-time digest compare so timing does not leak which stored key a
// guess was closest to. Returns ("", false) when no usable Bearer header
// was sent at all, so the caller can distinguish "no license" from
// "wrong license" (the client renders different advice for each).
func (g *gateway) licenseLabel(r *http.Request) (label string, presentedKey bool) {
	auth := r.Header.Get("Authorization")
	key := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if key == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(key))
	presented := []byte(hex.EncodeToString(sum[:]))
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, k := range g.cfg.PullLicenseKeys {
		if subtle.ConstantTimeCompare(presented, []byte(k.SHA256)) == 1 {
			label = k.Label
		}
	}
	return label, true
}

// checkPullLicense returns true when the request may have the weights.
// On false it has already written the 401 body. The `type` strings are
// load-bearing: cmd/oaica_pull_serve.go matches on them verbatim to tell
// the user to set OAICA_LICENSE_KEY.
func (g *gateway) checkPullLicense(w http.ResponseWriter, r *http.Request, e gwPullEntry) bool {
	if !e.LicenseRequired {
		return true
	}
	label, presented := g.licenseLabel(r)
	if !presented {
		writePullErr(w, http.StatusUnauthorized, "license_required",
			fmt.Sprintf("model %q requires a license key; send Authorization: Bearer <key>", e.Model))
		return false
	}
	if label == "" {
		writePullErr(w, http.StatusUnauthorized, "license_invalid",
			fmt.Sprintf("the license key presented is not valid for %q", e.Model))
		return false
	}
	return true
}

// writePullErr writes the {"error":{"message","type"}} shape the pull
// client parses. It does NOT reuse writeErr: that helper also sets a
// "code" field equal to the type, and these three routes are a contract
// with an already-shipped client — keeping the body minimal avoids any
// chance of a future writeErr change breaking pulls in the field.
func writePullErr(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	})
}

// pathModel extracts the {model} suffix. Written by hand rather than via
// r.PathValue so the handlers work with a plain mux prefix registration
// too, and so a trailing slash or empty id is rejected uniformly.
func pathModel(r *http.Request, prefix string) string {
	return strings.TrimSpace(strings.TrimPrefix(r.URL.Path, prefix))
}

// manifestHandler serves GET /v1/manifest/{model}.
//
// NOT metered and NOT ledgered, and it does not consult api_keys: pulling
// weights is not an inference request, it consumes no upstream tokens, and
// the ledger/meterhub pipeline is denominated in tokens and dollars-per-
// token. Writing a zero-token row per pull would pollute usage reporting
// and billing for no gain.
func (g *gateway) manifestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writePullErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	model := pathModel(r, "/v1/manifest/")
	e, ok := g.pullEntry(model)
	if !ok {
		writePullErr(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("unknown model %q; see https://oaica.com for the catalog", model))
		return
	}
	if !g.checkPullLicense(w, r, e) {
		return
	}

	m := gwManifest{
		Model:     e.Model,
		SizeBytes: e.SizeBytes,
		Source:    e.Source,
	}
	if e.SHA256 != "" {
		s := e.SHA256
		m.SHA256 = &s
	}
	switch e.Source {
	case "hf":
		u := e.HFURL
		m.HFURL = &u
		// pull_url stays "" — the client goes straight to HF for the bytes.
		if e.DecryptKeyHex != "" {
			// Only reached once the license gate above passed. This is the
			// single moment the key leaves the server, and it is why the
			// gate lives on the manifest rather than on the byte stream.
			k := e.DecryptKeyHex
			m.DecryptKeyHex = &k
		}
	case "file":
		m.PullURL = "/v1/pull/" + e.Model
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(m)
}

// pullHandler serves GET /v1/pull/{model} for source=="file" models,
// streaming the GGUF off local disk. Uses http.ServeContent so Range
// requests work: a pull is a multi-gigabyte download over a link that may
// drop, and resuming from a byte offset beats restarting from zero.
// Also not metered/ledgered — see manifestHandler.
func (g *gateway) pullHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writePullErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	model := pathModel(r, "/v1/pull/")
	e, ok := g.pullEntry(model)
	if !ok {
		writePullErr(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("unknown model %q; see https://oaica.com for the catalog", model))
		return
	}
	if e.Source != "file" {
		// hf models are fetched straight from HuggingFace; the client never
		// calls this path for them. 404 rather than a redirect so a
		// mis-wired client fails loudly instead of silently working.
		writePullErr(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("model %q is not served by this endpoint; use /v1/manifest/%s", model, model))
		return
	}
	if !g.checkPullLicense(w, r, e) {
		return
	}

	f, err := os.Open(e.FilePath)
	if err != nil {
		// The path is operator-configured, so a failure here is our bug or
		// a missing/unmounted disk, not user input. Don't echo the path.
		writePullErr(w, http.StatusInternalServerError, "pull_unavailable",
			fmt.Sprintf("weights for %q are not available on this host", model))
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		writePullErr(w, http.StatusInternalServerError, "pull_unavailable",
			fmt.Sprintf("weights for %q are not available on this host", model))
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	// ServeContent sets Content-Length (or Content-Range for a Range
	// request) and handles 206/416 for us.
	http.ServeContent(w, r, e.Model+".gguf", fi.ModTime(), f)
}

// catalogHandler serves GET /v1/catalog: the public "what can I pull"
// listing for the CLI and the docs site. Public even for
// license_required models — knowing a licensed model exists is how a user
// learns to ask for a license; the gate is on the manifest, which is what
// actually reveals the location and any decryption key.
func (g *gateway) catalogHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writePullErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	g.mu.RLock()
	entries := g.cfg.PullCatalog
	g.mu.RUnlock()

	out := make([]gwCatalogEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, gwCatalogEntry{
			Model:           e.Model,
			SizeBytes:       e.SizeBytes,
			Description:     e.Description,
			LicenseRequired: e.LicenseRequired,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	// A bare JSON array: the consumers are `oaica`'s own CLI and the docs
	// site, and there is no pagination or metadata to hang off an envelope.
	json.NewEncoder(w).Encode(out)
}
