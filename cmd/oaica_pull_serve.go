package cmd

// oaica_pull_serve.go — ollama-style `oaica pull <model>` / `oaica serve
// <model>` for true local self-hosting, distinct from `oaica run` (which
// stays a thin client to api.oaica.com — unchanged). Talks to the router's
// /v1/manifest and /v1/pull endpoints (prism-api-router/src/index.ts),
// which stream the GGUF directly through the Worker rather than issuing a
// presigned URL — same reasoning here: the license key is checked once at
// pull time, nothing to leak or outlive that check.
//
// The old Ollama-native pullCmd (PullHandler, ollama.com registry protocol)
// is dead in this fork — it required a local Ollama server (checkServerHeartbeat)
// this thin client doesn't run. This file replaces it.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
	"github.com/ollama/ollama/cmd/internal/httpbody"
	"github.com/ollama/ollama/cmd/launch"
	"github.com/spf13/cobra"
)

// oaicaLicenseKeyPath mirrors oaicaAPIKeyPath's pattern but for the
// self-host distribution license (a separate credential from OAICA_API_KEY
// — that one authorizes cloud chat calls, this one authorizes downloading
// raw weights). Distinguishing them matters: a leaked chat API key should
// never double as a weights-download credential.
// hfHostAcceptsToken reports whether a manifest's HuggingFace URL is a
// huggingface.co host, the only host this machine's HF token may be sent to.
// HFURL is data from the router's manifest, so the token is a credential that
// must not travel to whatever host that data happens to name; anything else
// is fetched anonymously (the repos are public, so this costs speed, not
// correctness).
//
// The SCHEME is checked too, and that is not belt-and-braces: "https" in a
// URL is what makes the token unreadable on the wire. A manifest naming
// http://huggingface.co/... passed the host allowlist and got the token
// attached as a bearer over plaintext — anyone on the path (a coffee-shop
// AP, a transparent proxy, the LAN) reads a private-repo credential. HF
// serves everything over TLS, so refusing anything that is not https costs
// nothing but the throttle.
func hfHostAcceptsToken(rawURL string) bool {
	return hfURLIsTrusted(rawURL)
}

// oaicaHFToken opportunistically finds a HuggingFace token for faster HF
// pull downloads (public repo, so this is a speed optimization only, never
// required for correctness) — checks HF_TOKEN first, then the standard
// location the official `hf`/`huggingface-cli` tools write to, so a user
// who's already logged in via those tools gets the speedup for free.
func oaicaHFToken() string {
	if t := strings.TrimSpace(os.Getenv("HF_TOKEN")); t != "" {
		return t
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".huggingface", "token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func oaicaLicenseKeyPath() (string, error) {
	dir, err := oaicaConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "license_key"), nil
}

// oaicaLicenseKey returns the weights-distribution licence key: OAICA_LICENSE_KEY
// first, then the file this host saved. Same precedence as launch's
// requireLicenseLive, so one machine cannot be running two different keys.
//
// This value is NOT validated here, deliberately, and that is not a gap the
// audit's sibling finding (R7) should close: the entitlement decision for
// these bytes belongs to the router, which both checks the key and hands back
// the manifest's decrypt_key only after that check passes (see
// oaicaFetchManifest). A client-side "is this key valid" test would either
// duplicate the server's answer or, worse, be the thing that decides — a
// licence check the client can be talked out of is not one. launch's stricter
// env-anchor rule exists because that path must decide LOCALLY whether to
// start a session; here the server decides and a bad key fails visibly with
// license_required / license_invalid.
func oaicaLicenseKey() string {
	if k := strings.TrimSpace(os.Getenv("OAICA_LICENSE_KEY")); k != "" {
		return k
	}
	path, err := oaicaLicenseKeyPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// oaicaModelsDir defaults to ~/.oaica/models but honors OAICA_MODELS_DIR —
// GGUFs are tens of GB, a home partition is often too small (real report:
// a user ran out of disk on their home fs). Override to point pulls at a
// bigger disk without a symlink workaround.
func oaicaModelsDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("OAICA_MODELS_DIR")); d != "" {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
		return d, nil
	}
	dir, err := oaicaConfigDir()
	if err != nil {
		return "", err
	}
	modelsDir := filepath.Join(dir, "models")
	if err := os.MkdirAll(modelsDir, 0o700); err != nil {
		return "", err
	}
	return modelsDir, nil
}

func oaicaModelPath(model string) (string, error) {
	if err := validateModelName(model); err != nil {
		return "", err
	}
	dir, err := oaicaModelsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, model+".gguf"), nil
}

// oaicaLocalServersPath is the registry `oaica serve` writes an entry to on
// startup and removes on clean exit — how `oaica launch`'s picker and
// per-model host resolution find out a local server exists, without the
// user having to set OAICA_HOST by hand. See launch/oaica_models.go's
// oaicaLocalServerEntries/oaicaResolveHostForModel, the readers of this
// file (cmd package can't import them the other way — this file just
// writes raw JSON, format is the contract, not a shared Go type).
func oaicaLocalServersPath() (string, error) {
	dir, err := oaicaConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "local_servers.json"), nil
}

type oaicaLocalServerEntry struct {
	Model     string `json:"model"`
	Origin    string `json:"origin"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
	// APIKey is the --api-key the server's normalizing proxy requires (empty
	// for loopback-only servers). The launcher's translation proxy sends it
	// as the bearer for "<model>:local" launches; the registry file is 0600.
	APIKey string `json:"api_key,omitempty"`
	// extra holds every field of a row this struct does not model, so a rewrite
	// of the registry hands them back rather than deleting them. Every writer
	// here replaces the file whole (register, unregister, drop), so a field a
	// user or a future build added was gone after the next `oaica serve`
	// (2026-09-27 audit, round 27, B-F). Same stance as the integration stores:
	// what a writer did not write is not its to delete.
	extra map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON reads a row and keeps whatever it does not model.
func (e *oaicaLocalServerEntry) UnmarshalJSON(b []byte) error {
	type plain oaicaLocalServerEntry
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*e = oaicaLocalServerEntry(p)
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for _, k := range []string{"model", "origin", "pid", "started_at", "api_key"} {
		delete(all, k)
	}
	if len(all) > 0 {
		e.extra = all
	}
	return nil
}

// MarshalJSON writes a row as the fields this struct models, then the fields it
// carried from the file, in a stable order.
func (e oaicaLocalServerEntry) MarshalJSON() ([]byte, error) {
	type plain oaicaLocalServerEntry
	base, err := json.Marshal(plain(e))
	if err != nil || len(e.extra) == 0 {
		return base, err
	}
	keys := make([]string, 0, len(e.extra))
	for k := range e.extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := bytes.TrimRight(base, "}")
	for _, k := range keys {
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		out = append(out, ',')
		out = append(out, kb...)
		out = append(out, ':')
		out = append(out, e.extra[k]...)
	}
	return append(out, '}'), nil
}

func oaicaRegisterLocalServer(model, origin, apiKey string) error {
	path, err := oaicaLocalServersPath()
	if err != nil {
		return err
	}
	// The lock covers the whole load-mutate-save, not just the write: its
	// writers are separate `oaica serve` processes registering themselves at
	// startup, and an atomic rename only stops a READER from seeing half a
	// file — two servers starting at once both read the same snapshot and the
	// one that renames last wins with the other's entry gone, while both
	// report a clean start (2026-09-26 audit).
	return fileutil.WithFileLock(path, func() error {
		// A registry that cannot be READ is not an empty one: this write replaces
		// the file whole, so treating an unreadable document as "no entries"
		// deleted every other server's row — and the --api-key recorded with it,
		// which is the credential the launcher's translation proxy sends for a
		// "<model>:local" launch. One corrupt byte anywhere in the file was
		// enough, and the servers it described are still running (2026-09-27
		// audit, round 25). Refused instead: the file is the user's to move
		// aside, and registration is best-effort at its call site, so the serve
		// itself still starts.
		entries, err := oaicaReadLocalServersStrict(path)
		if err != nil {
			return fmt.Errorf("not registering %s: %w — writing the registry would drop every server already recorded in it, so it was left as it is; move it aside to start a fresh one", model, err)
		}
		filtered := entries[:0]
		for _, e := range entries {
			if e.Model != model {
				filtered = append(filtered, e)
			}
		}
		filtered = append(filtered, oaicaLocalServerEntry{
			Model:     model,
			Origin:    origin,
			PID:       os.Getpid(),
			StartedAt: time.Now().UTC().Format(time.RFC3339),
			APIKey:    apiKey,
		})
		b, err := json.MarshalIndent(filtered, "", "  ")
		if err != nil {
			return err
		}
		// Atomic, not os.WriteFile (2026-09-26 audit): every `oaica serve`
		// registers itself here and every `oaica serve --stop` unregisters, so
		// two servers starting at once wrote through one live path. The file
		// carries each server's --api-key, and it is read by a DIFFERENT command
		// (`oaica pull` / the picker) than the one that writes it.
		return fileutil.WriteFileAtomic(path, b, 0o600)
	})
}

// oaicaUnregisterLocalServerAt removes exactly the entry THIS process
// registered: model, origin and pid all have to match. A teardown that knows
// only the model name cannot tell its own entry from another live instance's
// — `oaica serve bonsai` and `oaica serve bonsai --port 30002` both write a
// "bonsai" entry (the second replaces the first), and the first one's exit
// then deleted the entry the still-running second had written, so the only
// source for a "bonsai:local" row lost the server that was up
// (2026-09-26 audit).
func oaicaUnregisterLocalServerAt(model, origin string) {
	pid := os.Getpid()
	oaicaDropLocalServers(func(e oaicaLocalServerEntry) bool {
		return e.Model == model && e.Origin == origin && e.PID == pid
	}, false)
}

// oaicaUnregisterLocalServer removes the OLDEST registry entry for model. It
// is for a caller that knows nothing about the instance beyond the model name
// (a future `serve --stop`, and the registry's own tests); a serve process
// tearing itself down has an origin and a pid and must use
// oaicaUnregisterLocalServerAt instead.
func oaicaUnregisterLocalServer(model string) {
	oaicaDropLocalServers(func(e oaicaLocalServerEntry) bool { return e.Model == model }, true)
}

// oaicaDropLocalServers rewrites local_servers.json without the entries match
// selects — the first only, when first is true. Nothing is written when
// nothing matched, so a teardown whose entry was already replaced (or never
// registered) cannot disturb the live servers' entries.
//
// Under the same lock as the registration path: this is the same
// load-mutate-save over the same file, and a teardown racing a startup would
// otherwise drop the OTHER server's entry (2026-09-26 audit).
func oaicaDropLocalServers(match func(oaicaLocalServerEntry) bool, first bool) {
	path, err := oaicaLocalServersPath()
	if err != nil {
		return
	}
	_ = fileutil.WithFileLock(path, func() error {
		// Nothing is written when the file cannot be read: there is no entry to
		// match, and a rewrite from an empty snapshot would drop the live
		// servers' rows (the same rule oaicaRegisterLocalServer states).
		entries, err := oaicaReadLocalServersStrict(path)
		if err != nil {
			return nil
		}
		filtered := entries[:0]
		dropped := false
		for _, e := range entries {
			if match(e) && (!first || !dropped) {
				dropped = true
				continue
			}
			filtered = append(filtered, e)
		}
		if !dropped {
			return nil
		}
		b, err := json.MarshalIndent(filtered, "", "  ")
		if err != nil {
			return err
		}
		// Best-effort (this is a teardown path), but still atomic: a partial
		// write here would strand the OTHER running servers' entries
		// (2026-09-26 audit).
		return fileutil.WriteFileAtomic(path, b, 0o600)
	})
}

// oaicaReadLocalServersStrict reads the registry, keeping "the file is not
// there" apart from "the file cannot be read". An absent registry is an empty
// one — the first `oaica serve` on a box creates it. A present-but-unreadable
// one is NOT empty: its entries (and the --api-keys in them) are unknown, and a
// caller that rewrites the file from this answer destroys them, which is what
// the nil-returning reader made oaicaRegisterLocalServer do (2026-09-27 audit,
// round 25).
func oaicaReadLocalServersStrict(path string) ([]oaicaLocalServerEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var entries []oaicaLocalServerEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return entries, nil
}

// oaicaReadLocalServers is the lenient form, for readers that only list (the
// registry's tests). An unreadable file answers with no entries; a caller that
// WRITES the file back must use oaicaReadLocalServersStrict, so an unreadable
// document is refused instead of replaced.
func oaicaReadLocalServers(path string) []oaicaLocalServerEntry {
	entries, err := oaicaReadLocalServersStrict(path)
	if err != nil {
		return nil
	}
	return entries
}

type oaicaManifest struct {
	Model     string  `json:"model"`
	SizeBytes int64   `json:"size_bytes"`
	SHA256    *string `json:"sha256"`
	PullURL   string  `json:"pull_url"`
	// Source, HFURL, DecryptKeyHex are set when the router routes this
	// pull through the encrypted-HuggingFace fallback instead of R2/a
	// bitdeer-style plain fallback (see checkPullAuth's doc in the router
	// — HF public repos give free, reliable, unlimited-bandwidth hosting;
	// encryption is what keeps the weights non-usable without a valid
	// license even though the HF repo itself is public). When Source ==
	// "hf", download directly from HFURL (bypassing the router entirely
	// for the actual bytes — no reason to pay Worker CPU/egress just to
	// proxy what's already a public, free-to-fetch blob) and decrypt
	// locally with DecryptKeyHex using the chunked AES-256-GCM format
	// (see decryptChunkedAESGCM) — the key is only ever handed out AFTER
	// the license check that gated this manifest request passes.
	Source        string  `json:"source"`
	HFURL         *string `json:"hf_url"`
	DecryptKeyHex *string `json:"decrypt_key"`
}

func oaicaFetchManifest(model string) (*oaicaManifest, error) {
	// The name is checked BEFORE the request, not after it. The guard for this
	// shape already existed (validateModelName refuses separators, "." and
	// ".."), but the pull path only reached it through oaicaModelPath, which
	// runs once the manifest has been fetched — so an argument that was never a
	// model name was put on the wire first, on the one request that carries the
	// distribution licence as a bearer (2026-09-26 audit).
	if err := validateModelName(model); err != nil {
		return nil, err
	}
	req, err := launch.NewRedactedRequest(http.MethodGet, oaicaManifestURL(model), nil)
	if err != nil {
		return nil, err
	}
	if key := oaicaLicenseKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	// Redirects refused: this request carries the distribution licence as a
	// bearer, and net/http's rule for re-sending it compares hostnames only —
	// a redirect to the same name on another port or a plaintext scheme keeps
	// the credential. The router's manifest endpoint answers directly.
	client := pullHTTPClient(noRedirects, 15*time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, launch.RedactError(fmt.Errorf("couldn't reach %s: %w", launch.RedactBaseURL(oaicaHost()), err))
	}
	defer resp.Body.Close()
	// A manifest is small; its size is the router's choice, not oaica's
	// (2026-09-26 audit).
	body, _ := httpbody.ReadCapped(resp.Body, 8<<20, "the model manifest")
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
			// Bounded and redacted: this is the request that carries the
			// distribution licence as a bearer, so an upstream that echoes the
			// Authorization header put the key in the error — and the message
			// itself was unbounded, up to the 8 MiB body cap, while the same
			// file's other readers cap at 4 KiB (2026-09-26 audit, sixteenth
			// round).
			msg := launch.PrintableCell(truncateForError([]byte(oaicaDiagnosis(e.Error.Message))))
			if e.Error.Type == "license_required" || e.Error.Type == "license_invalid" {
				return nil, fmt.Errorf("%s\n\nSet a license key: OAICA_LICENSE_KEY=<key> or save one to ~/.oaica/license_key", msg)
			}
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, launch.RedactBaseURL(oaicaHost()))
	}
	var m oaicaManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("bad manifest response: %w", err)
	}
	return &m, nil
}

// oaicaPullModel downloads model's GGUF into ~/.oaica/models/, streaming
// with progress. Returns the local file path on success. Re-downloads are
// NOT resumed (no Range support wired up yet) — deleting a partial file
// and re-running is the current recovery path for an interrupted pull.
// oaicaPullURL is the byte-stream URL for a manifest's pull_url, refused
// unless it addresses the router itself.
//
// pull_url is DATA from the router's manifest, and oaicaHost()+pull_url is a
// concatenation, not a URL join: a value that is not a path can carry its own
// authority, and in the RFC-3986 userinfo form everything after the LAST "@"
// is the authority — so "router@attacker/v1/pull/m" resolved to the attacker,
// and the manifest chose both the host that received the distribution license
// as a bearer and the bytes that were installed as the model file. The
// sibling manifest field hf_url is refused unless it is https on
// huggingface.co, for exactly this reason (hfHostAcceptsToken); this is the
// same rule for the same kind of data, and it costs nothing: the router's own
// pull_url is always a path ("/v1/pull/<model>", tools/gateway/pull.go).
func oaicaPullURL(host, pullURL string) (string, error) {
	pullURL = strings.TrimSpace(pullURL)
	// "/v1/pull/x", not "//evil/x" (protocol-relative: another host), not
	// "?k=v" (swallows the router's own path), not a bare host.
	if !strings.HasPrefix(pullURL, "/") || strings.HasPrefix(pullURL, "//") {
		return "", fmt.Errorf("router sent a pull_url that is not a path: %q", pullURL)
	}
	base, err := url.Parse(strings.TrimSpace(host))
	if err != nil || base.Host == "" {
		return "", fmt.Errorf("cannot resolve the router host: %w", err)
	}
	full, err := url.Parse(base.String() + pullURL)
	if err != nil {
		return "", fmt.Errorf("router sent an unusable pull_url: %w", err)
	}
	// The authority of the composed URL must be the router's, and the
	// credential it rides in (if any) the router's own.
	if full.Scheme != base.Scheme || full.Host != base.Host {
		return "", fmt.Errorf("router sent a pull_url that points at %s, not at the router", full.Host)
	}
	return base.String() + pullURL, nil
}

// oaicaManifestURL is the router's manifest endpoint for model, with the name
// as ONE percent-escaped path segment. A model name is user input and a URL is
// not a concatenation: unescaped, a "?" in the name ended the path and gave the
// router a query it never had (everything after it stops being a model), a "#"
// cut the request short at a fragment, and a separator addressed a different
// endpoint on the same router. validateModelName refuses the separators and
// runs first; this is what makes the rest of the name inert.
func oaicaManifestURL(model string) string {
	return oaicaHost() + "/v1/manifest/" + url.PathEscape(model)
}

// manifestDigest is the digest a manifest states for its model file, or "" when
// it states none (the two call sites that compare digests read it through this
// so they cannot disagree about which field is the truth).
func manifestDigest(m *oaicaManifest) string {
	if m == nil || m.SHA256 == nil {
		return ""
	}
	return strings.TrimSpace(*m.SHA256)
}

// fileSHA256Hex digests a file on disk, for the "is what is installed the
// artifact the manifest describes" check. Streamed, not read into memory: a
// model file is gigabytes (2026-09-26 audit).
func fileSHA256Hex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func oaicaPullModel(model string) (string, error) {
	manifest, err := oaicaFetchManifest(model)
	if err != nil {
		return "", err
	}

	destPath, err := oaicaModelPath(model)
	if err != nil {
		return "", err
	}
	// An encrypted model is compared by the digest it was installed from, not by size or by hashing the
	// plaintext against the ciphertext's digest: those never matched, so every pull re-downloaded it.
	if manifest.Source == "hf" && manifest.DecryptKeyHex != nil {
		if want := manifestDigest(manifest); want != "" {
			if rec, rerr := os.ReadFile(destPath + encryptedSourceDigestSuffix); rerr == nil && strings.EqualFold(strings.TrimSpace(string(rec)), want) {
				if _, serr := os.Stat(destPath); serr == nil {
					fmt.Fprintf(os.Stderr, "%s already downloaded, skipping\n", model)
					return destPath, nil
				}
			}
		}
	}
	if fi, err := os.Stat(destPath); err == nil && fi.Size() == manifest.SizeBytes {
		// The length is not the check. A file of the right size and the wrong
		// content — a download that was truncated and padded, a corrupted
		// disk, any other writer — used to be accepted here and skipped
		// forever, so the command succeeded, the model was broken, and
		// re-running the pull never repaired it because this shortcut is what
		// runs first. When the manifest states a digest, that digest decides
		// (2026-09-26 audit). A digest oaica cannot compute — an unreadable
		// file — is not a match either: re-download.
		if want := manifestDigest(manifest); want != "" {
			if got, herr := fileSHA256Hex(destPath); herr == nil && strings.EqualFold(got, want) {
				fmt.Fprintf(os.Stderr, "%s already downloaded (%s), skipping\n", model, humanBytes(manifest.SizeBytes))
				return destPath, nil
			}
			fmt.Fprintf(os.Stderr, "%s is on disk at the declared size but does not match the manifest's sha256 — re-downloading\n", model)
		} else {
			fmt.Fprintf(os.Stderr, "%s already downloaded (%s), skipping\n", model, humanBytes(manifest.SizeBytes))
			return destPath, nil
		}
	}

	if manifest.Source == "hf" {
		return oaicaPullFromHF(model, manifest, destPath)
	}

	pullURL, err := oaicaPullURL(oaicaHost(), manifest.PullURL)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodGet, pullURL, nil)
	if err != nil {
		return "", err
	}
	// The distribution licence, NOT the router API key: these bytes are the
	// paid artifact, and the entitlement for them is the licence. oaicaAuthorize
	// (and so OAICA_API_KEY and the OAICA_HOST userinfo form) is deliberately
	// not attached here — a machine whose only credential is the userinfo key
	// gets a visible 401 from the router rather than a silently different
	// entitlement path (2026-09-26 audit, B-aside: documented, not changed).
	if key := oaicaLicenseKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := pullHTTPClient(noRedirects, 0) // large file, no overall timeout; bounded by size + stall guard instead
	resp, err := client.Do(req)
	if err != nil {
		return "", launch.RedactError(fmt.Errorf("pull failed: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("pull failed: HTTP %d: %s", resp.StatusCode, readPullErrorBody(resp.Body))
	}

	// One temp file per pull: two concurrent pulls of a model shared `<dest>.partial`, each truncating the other's
	// bytes, and the winner installed a file whose hash it had checked on the wire, not on disk (2026-09-29 audit,
	// round 130, F130-L2-1).
	f, stopWatch, err := createPullTemp(destPath)
	if err != nil {
		return "", err
	}
	tmpPath := f.Name()
	defer stopWatch()
	defer f.Close()

	fmt.Fprintf(os.Stderr, "pulling %s (%s)...\n", model, humanBytes(manifest.SizeBytes))
	src := io.Reader(newStallGuard(resp.Body, pullStallTimeout))
	if manifest.SizeBytes > 0 {
		src = newCappedReader(src, manifest.SizeBytes)
	}
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, hasher), &progressReader{r: src, total: manifest.SizeBytes, label: model})
	if err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("pull interrupted: %w", err)
	}
	f.Close()
	fmt.Fprintln(os.Stderr)

	if manifest.SizeBytes > 0 && written != manifest.SizeBytes {
		os.Remove(tmpPath)
		return "", fmt.Errorf("pull incomplete: got %d bytes, expected %d", written, manifest.SizeBytes)
	}
	// The HF plaintext path checks this digest; so must this one. Without it a
	// manifest's size is the only thing between the router and the installed
	// model, and size_bytes: 0 removes even that.
	if want := manifestDigest(manifest); want != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, want) {
			os.Remove(tmpPath)
			return "", fmt.Errorf("sha256 mismatch: got %s, expected %s", got, want)
		}
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "%s saved to %s\n", model, destPath)
	return destPath, nil
}

// oaicaPullFromHF downloads directly from a public HuggingFace repo
// (manifest.HFURL) — completely bypassing our router for the actual
// bytes, only the earlier /v1/manifest call went through the router (for
// the license check + decrypt key). Free, reliable HF bandwidth instead
// of our own infra; the file is encrypted at rest so being on a public
// repo doesn't leak the weights — only a valid license gets the key.
func oaicaPullFromHF(model string, manifest *oaicaManifest, destPath string) (string, error) {
	if manifest.HFURL == nil {
		return "", fmt.Errorf("router said source=hf but didn't include hf_url")
	}
	// The allowlist hfHostAcceptsToken applies to the TOKEN, applied to the
	// request itself: hf_url chooses the host, so without this the router could
	// name loopback, the LAN, or a metadata endpoint and this command would
	// fetch it and install the body as the model.
	if !hfURLIsTrusted(*manifest.HFURL) {
		return "", fmt.Errorf("router sent an hf_url that is not an https HuggingFace URL; refusing to fetch it")
	}
	// decrypt_key is optional: the gateway catalog serves PUBLIC, plaintext
	// GGUFs with decrypt_key: null (shipping a key for a plaintext blob would
	// be meaningless). Only licensed/encrypted models carry one.
	var key []byte
	if manifest.DecryptKeyHex != nil {
		var err error
		key, err = hex.DecodeString(*manifest.DecryptKeyHex)
		if err != nil || len(key) != 32 {
			return "", fmt.Errorf("bad decrypt key from router")
		}
	}

	req, err := http.NewRequest(http.MethodGet, *manifest.HFURL, nil)
	if err != nil {
		return "", err
	}
	if hfToken := oaicaHFToken(); hfToken != "" && hfHostAcceptsToken(*manifest.HFURL) {
		// The repo is public — a token isn't required for correctness,
		// only speed. HF explicitly warns unauthenticated requests get
		// throttled ("Please set a HF_TOKEN to enable higher rate limits
		// and faster downloads"); opportunistically use one if the user
		// already has the HF CLI configured locally, but don't require it.
		//
		// hfHostAcceptsToken is the guard that makes this safe: HFURL comes
		// from the endpoint's manifest, so without it a router (or anything
		// that can answer as one) could name any host it liked and receive
		// this machine's HuggingFace token as a bearer on the next pull.
		req.Header.Set("Authorization", "Bearer "+hfToken)
	}
	client := pullHTTPClient(trustedHostRedirects(hfURLIsTrusted), 0)
	resp, err := client.Do(req)
	if err != nil {
		return "", launch.RedactError(fmt.Errorf("HF download failed: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HF download failed: HTTP %d: %s", resp.StatusCode, readPullErrorBody(resp.Body))
	}

	// One temp file per pull: two concurrent pulls of a model shared `<dest>.partial`, each truncating the other's
	// bytes, and the winner installed a file whose hash it had checked on the wire, not on disk (2026-09-29 audit,
	// round 130, F130-L2-1).
	f, stopWatch, err := createPullTemp(destPath)
	if err != nil {
		return "", err
	}
	tmpPath := f.Name()
	defer stopWatch()
	defer f.Close()

	body := io.Reader(newStallGuard(resp.Body, pullStallTimeout))

	if key == nil {
		// Plaintext blob: stream it straight through, verifying size and (when
		// the manifest supplies one) sha256 — the integrity check GCM's auth
		// tag provides on the encrypted path.
		fmt.Fprintf(os.Stderr, "pulling %s from HuggingFace (%s)...\n", model, humanBytes(manifest.SizeBytes))
		hasher := sha256.New()
		src := body
		if manifest.SizeBytes > 0 {
			src = newCappedReader(src, manifest.SizeBytes)
		}
		written, err := io.Copy(io.MultiWriter(f, hasher), &progressReader{r: src, total: manifest.SizeBytes, label: model})
		if err != nil {
			os.Remove(tmpPath)
			return "", fmt.Errorf("pull interrupted: %w", err)
		}
		f.Close()
		fmt.Fprintln(os.Stderr)

		if manifest.SizeBytes > 0 && written != manifest.SizeBytes {
			os.Remove(tmpPath)
			return "", fmt.Errorf("pull incomplete: got %d bytes, expected %d", written, manifest.SizeBytes)
		}
		if manifest.SHA256 != nil && *manifest.SHA256 != "" {
			got := hex.EncodeToString(hasher.Sum(nil))
			if !strings.EqualFold(got, strings.TrimSpace(*manifest.SHA256)) {
				os.Remove(tmpPath)
				return "", fmt.Errorf("sha256 mismatch: got %s, expected %s", got, *manifest.SHA256)
			}
		}
		if err := os.Rename(tmpPath, destPath); err != nil {
			return "", err
		}
		fmt.Fprintf(os.Stderr, "%s saved to %s\n", model, destPath)
		return destPath, nil
	}

	fmt.Fprintf(os.Stderr, "pulling %s from HuggingFace (%s encrypted, decrypting as it streams)...\n", model, humanBytes(manifest.SizeBytes))
	// The plaintext size is not comparable to manifest.SizeBytes — that is the
	// ENCRYPTED blob's size (chunked AES-GCM adds ~32 bytes/chunk), so the
	// check belongs on the CIPHERTEXT the frame reader consumed: GCM
	// authenticates each chunk, but a blob that stops exactly on a chunk
	// boundary decrypts cleanly and is not complete (2026-09-26 audit).
	// Counting what came off the wire is what tells the two apart.
	// The manifest's sha256 describes the blob AS DOWNLOADED (tools/gateway/pull.go), i.e. the
	// ciphertext; the plaintext digest is accepted too, so a manifest stated either way installs. GCM
	// authenticates each chunk alone with no index, so reordered chunks decrypted cleanly and installed;
	// the plaintext and router arms already checked their digest (2026-09-29 audit, round 127, F127-L1-6).
	ctHasher, plainHasher := sha256.New(), sha256.New()
	ct := &countingReader{r: io.TeeReader(body, ctHasher)}
	if manifest.SizeBytes > 0 {
		ct.r = newCappedReader(ct.r, manifest.SizeBytes)
	}
	written, err := decryptChunkedAESGCMStream(&progressReader{r: ct, total: manifest.SizeBytes, label: model}, io.MultiWriter(f, plainHasher), key)
	if err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("pull/decrypt interrupted: %w", err)
	}
	f.Close()
	fmt.Fprintln(os.Stderr)

	if manifest.SizeBytes > 0 && ct.n != manifest.SizeBytes {
		os.Remove(tmpPath)
		return "", fmt.Errorf("pull incomplete: got %d encrypted bytes, expected %d — the transfer stopped early", ct.n, manifest.SizeBytes)
	}
	_ = written

	if want := manifestDigest(manifest); want != "" {
		gotCT, gotPlain := hex.EncodeToString(ctHasher.Sum(nil)), hex.EncodeToString(plainHasher.Sum(nil))
		if !strings.EqualFold(gotCT, want) && !strings.EqualFold(gotPlain, want) {
			os.Remove(tmpPath)
			return "", fmt.Errorf("sha256 mismatch: got %s (downloaded) / %s (decrypted), expected %s", gotCT, gotPlain, want)
		}
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return "", err
	}
	// So the next pull can tell this model is the one the manifest names without re-downloading it.
	if want := manifestDigest(manifest); want != "" {
		_ = os.WriteFile(destPath+encryptedSourceDigestSuffix, []byte(want+"\n"), 0o600)
	}
	fmt.Fprintf(os.Stderr, "%s saved to %s\n", model, destPath)
	return destPath, nil
}

// encryptedSourceDigestSuffix names the record of which manifest digest an encrypted model was installed
// from: its size and digest describe the ciphertext, which the installed plaintext file cannot be compared to.
const encryptedSourceDigestSuffix = ".src-sha256"

// decryptChunkedAESGCMStream reads the chunked AES-256-GCM frame format
// (matches the encryption tool used to prepare HF-hosted models):
//
//	repeated: [4-byte big-endian ciphertext_len][12-byte nonce][ciphertext+16-byte GCM tag]
//
// Decrypts one chunk at a time (8MB plaintext each) so memory use stays
// flat regardless of file size — never buffers the whole (multi-GB) file.
func decryptChunkedAESGCMStream(r io.Reader, w io.Writer, key []byte) (int64, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return 0, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return 0, err
	}

	var written int64
	lenBuf := make([]byte, 4)
	nonceBuf := make([]byte, gcm.NonceSize())
	for {
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			if err == io.EOF {
				break
			}
			return written, fmt.Errorf("reading chunk length: %w", err)
		}
		ctLen := binary.BigEndian.Uint32(lenBuf)
		// Bound BEFORE allocating: this is a 4-byte field the sender writes,
		// and make([]byte, ctLen) on 0xFFFFFFFF is a 4 GiB allocation from a
		// 17-byte response — a fatal OOM the sender chooses (2026-09-26 audit).
		if ctLen == 0 || ctLen > maxEncryptedChunkBytes || ctLen < uint32(gcm.Overhead()) {
			return written, fmt.Errorf("chunk length %d is out of range (max %d)", ctLen, maxEncryptedChunkBytes)
		}
		if _, err := io.ReadFull(r, nonceBuf); err != nil {
			return written, fmt.Errorf("reading nonce: %w", err)
		}
		ct := make([]byte, ctLen)
		if _, err := io.ReadFull(r, ct); err != nil {
			return written, fmt.Errorf("reading ciphertext: %w", err)
		}
		pt, err := gcm.Open(ct[:0], nonceBuf, ct, nil)
		if err != nil {
			return written, fmt.Errorf("decrypt failed (wrong key or corrupted download): %w", err)
		}
		n, err := w.Write(pt)
		if err != nil {
			return written, err
		}
		written += int64(n)
	}
	return written, nil
}

type progressReader struct {
	r       io.Reader
	total   int64
	read    int64
	label   string
	lastPct int
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.read += int64(n)
	if p.total > 0 {
		pct := int(p.read * 100 / p.total)
		if pct != p.lastPct && pct%2 == 0 {
			fmt.Fprintf(os.Stderr, "\r%s: %d%% (%s / %s)", p.label, pct, humanBytes(p.read), humanBytes(p.total))
			p.lastPct = pct
		}
	}
	return n, err
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// PullHandler replaces Ollama's registry-pull implementation (dead in this
// fork — it required a local Ollama server via checkServerHeartbeat, which
// pullCmd's PreRunE has already been changed away from below).
func PullHandler(cmd *cobra.Command, args []string) error {
	model := args[0]
	destPath, err := oaicaPullModel(model)
	if err != nil {
		return err
	}
	autoPopulateModelManifest(model, destPath)
	// A pulled GGUF does nothing on its own — name the one command that turns
	// it into something usable, so the next step isn't a docs lookup.
	fmt.Fprintf(os.Stderr, "serve it with: oaica serve %s\n", model)
	return nil
}

// autoPopulateModelManifest writes a minimal ~/.oaica/models.json entry
// after a successful pull, so `oaica model list`/`oaica plan set` have
// something to work with without the user hand-typing flags via
// `oaica model add`.
//
// Deliberately partial: the router's pull-time manifest
// (oaicaFetchManifest's oaicaManifest type) carries model id, size, and a
// checksum — not architecture, quantization, or real context window. Those
// need either a router schema addition (out of scope here — the router is
// a separate service) or the user filling them in by hand afterward. What
// IS known reliably: this pulled model is a GGUF served via `oaica serve`
// through llama-server (EngineLlamaCPP), and its ModelPath.
//
// Never overwrites an existing entry: if the user already ran
// `oaica model add` with real arch/quant/context (or a previous pull
// already created one), a bare re-pull must not blow that away with a
// blanker record.
func autoPopulateModelManifest(model, modelPath string) {
	if existing, err := launch.ModelShow(model); err == nil {
		_ = existing // already present — leave it untouched, whatever detail it has
		return
	}
	e, err := launch.ModelAdd(launch.ModelAddOptions{
		ID:        model,
		Engine:    string(launch.EngineLlamaCPP),
		ModelPath: modelPath,
		Notes:     "auto-populated by `oaica pull` — arch/quant/context-window unknown until set with `oaica model add --engine llama.cpp --context-window N ...` (the router's pull manifest doesn't carry that metadata yet)",
	})
	if err != nil {
		// Never fail the pull over manifest bookkeeping — the weights are
		// already on disk and `oaica serve` doesn't need a manifest entry
		// to run. Surface it as a warning only.
		fmt.Fprintf(os.Stderr, "warning: failed to record %s in the model manifest: %v\n", model, err)
		return
	}
	fmt.Fprintf(os.Stderr, "recorded %s in the model manifest (%s) — edit with `oaica model add %s --context-window N ...` to fill in the rest\n", e.ID, mustModelManifestPath(), e.ID)
}

// mustModelManifestPath returns the manifest path for the message above,
// falling back to a fixed relative description if HOME can't be resolved
// (never fatal — this only feeds a user-facing hint string).
func mustModelManifestPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/.oaica/models.json"
	}
	return filepath.Join(home, ".oaica", "models.json")
}

// findLlamaServer locates a llama-server binary: $OAICA_LLAMA_SERVER env
// var first (explicit override), then PATH. Does NOT attempt to build or
// download one — that's a real build toolchain + CUDA arch decision the
// user needs to make themselves (see the RTX 4060 -cmoe build recipe).
func findLlamaServer() (string, error) {
	if p := strings.TrimSpace(os.Getenv("OAICA_LLAMA_SERVER")); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("llama-server"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf(`llama-server binary not found on PATH.

Build it (CUDA example, adjust CMAKE_CUDA_ARCHITECTURES for your GPU):
  git clone https://github.com/ggml-org/llama.cpp && cd llama.cpp
  cmake -B build -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=89
  cmake --build build --config Release -j$(nproc)

Then either put it on PATH or set OAICA_LLAMA_SERVER=/path/to/llama-server`)
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// ServeHandler spawns a local llama-server against a pulled model, using
// -cmoe (CPU-RAM MoE expert offload) by default — every i-compact-tier
// model we ship is a MoE checkpoint that needs it to fit consumer VRAM;
// harmless no-op flag on dense models. Prints the OAICA_HOST export the
// user needs so `oaica launch`/`oaica run` route to it instead of the
// cloud router.
//
// --ncmoe N overrides -cmoe with llama-server's -ncmoe (keep the MoE
// experts of only the FIRST N layers on CPU, rest fully on GPU). Measured
// on an RTX 4060 laptop (8GB VRAM) with kat-coder-i-compact (40 layers,
// 16.5GB Q4_K_M): the relationship is NOT monotonic — full CPU offload
// (-cmoe, equivalent to N=40) and near-full (N=34) both beat moderate
// mixing (N=30, N=36), and N=20 OOM'd. N=34 measured ~25-6x faster than
// -cmoe depending on prompt-cache warmth. Real cause: every CPU/GPU layer
// boundary crossing costs a host<->device transfer per token; minimizing
// the NUMBER of boundary crossings (few GPU-resident layers, all
// contiguous at the end) beats maximizing GPU-resident layer COUNT once
// you're VRAM-constrained enough that "mostly GPU" isn't achievable
// anyway. This is model/hardware-specific — no safe universal default,
// hence a flag + this note rather than silently overriding -cmoe.
func ServeHandler(cmd *cobra.Command, args []string) error {
	model := args[0]

	bindHost, _ := cmd.Flags().GetString("host")
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}
	apiKey, _ := cmd.Flags().GetString("api-key")
	// The key can come from the environment: on the command line it sits in /proc/<pid>/cmdline,
	// readable by every local user, for the server's whole life (2026-09-29 audit, round 123,
	// F123-L1-5).
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("OAICA_SERVE_API_KEY"))
	}
	insecure, _ := cmd.Flags().GetBool("insecure")
	// Binding off-loopback publishes an inference server. Without a key
	// anyone who can reach the port can use (and bill) your GPU, so this
	// is a hard stop rather than a warning — --insecure is the explicit
	// opt-out for trusted private networks.
	if bindHost != "127.0.0.1" && bindHost != "localhost" && apiKey == "" && !insecure {
		return fmt.Errorf("refusing to bind %s without --api-key: that exposes an unauthenticated inference server to the network.\n\nEither set one (the environment keeps it out of the process list):\n  OAICA_SERVE_API_KEY=\"$(openssl rand -hex 24)\" oaica serve %s --host %s\n\nor, only on a network you fully trust, pass --insecure", bindHost, model, bindHost)
	}

	modelPath, err := oaicaModelPath(model)
	if err != nil {
		return err
	}
	if _, err := os.Stat(modelPath); err != nil {
		return fmt.Errorf("%s not found locally — run `oaica pull %s` first", model, model)
	}

	llamaServer, err := findLlamaServer()
	if err != nil {
		return err
	}

	port, _ := cmd.Flags().GetInt("port")
	if port == 0 {
		port, err = freePort()
		if err != nil {
			return err
		}
	}
	ctxSize, _ := cmd.Flags().GetInt("ctx-size")
	if ctxSize == 0 {
		ctxSize = 8192
	}
	noCmoe, _ := cmd.Flags().GetBool("no-cmoe")
	ncmoe, _ := cmd.Flags().GetInt("ncmoe")
	threads, _ := cmd.Flags().GetInt("threads")
	if threads == 0 {
		// Default to PHYSICAL cores, not runtime.NumCPU() (logical/SMT
		// count). Measured on the 6-core/12-thread 4060 laptop: -t 12
		// (all SMT threads) was WORSE than -t 6 (38.7 vs ~51 tok/s) for
		// this CPU-offloaded-MoE workload — it's memory-bandwidth-bound,
		// not compute-bound, so hyperthreads compete for the same cache/
		// bandwidth without adding real throughput. Go's runtime.NumCPU()
		// has no portable physical-core query, so approximate with /2 —
		// wrong on non-SMT hardware (halves real capacity there) but
		// right on the common consumer laptop/desktop case this command
		// targets. --threads overrides for anyone who profiles their own
		// box and finds a different optimum.
		threads = runtime.NumCPU() / 2
		if threads < 1 {
			threads = 1
		}
	}

	// llama-server binds an INTERNAL port; RunLocalNormalizingProxy sits in
	// front of it on `port` (the one printed to the user / used as
	// OAICA_HOST). Two real bugs this fixes vs talking to llama-server
	// directly:
	//  1. Claude Code's /v1/messages requests crash llama-server's strict
	//     Jinja chat template ("System message must be at the beginning")
	//     — the same bug prism-api-router/src/index.ts's extractModelName
	//     fixes for cloud requests, ported here since the router isn't in
	//     the loop for local self-host. See local_proxy.go.
	//  2. -a <model> sets llama-server's own /v1/models `id` to the
	//     friendly name instead of the full GGUF file path — without it,
	//     `oaica launch`'s readiness check (which does GET /v1/models and
	//     looks for the requested name) reports "model not found" even
	//     though the server is healthy and serving.
	internalPort, err := freePort()
	if err != nil {
		return err
	}

	serveArgs := []string{
		"-m", modelPath,
		"-a", model,
		"-ngl", "999",
		"-c", strconv.Itoa(ctxSize),
		"-t", strconv.Itoa(threads),
		"-fa", "on",
		"-ctk", "q8_0", "-ctv", "q8_0",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(internalPort),
	}
	moeMode := "cmoe"
	switch {
	case ncmoe > 0:
		serveArgs = append(serveArgs, "-ncmoe", strconv.Itoa(ncmoe))
		moeMode = fmt.Sprintf("ncmoe=%d", ncmoe)
	case !noCmoe:
		serveArgs = append(serveArgs, "-cmoe")
	default:
		moeMode = "off"
	}

	auth := "no auth (loopback)"
	if apiKey != "" {
		auth = "bearer-token auth"
	} else if bindHost != "127.0.0.1" && bindHost != "localhost" {
		auth = "NO AUTH — exposed to the network"
	}
	fmt.Fprintf(os.Stderr, "starting %s on %s:%d (ctx=%d, threads=%d, moe=%s, %s)...\n", model, bindHost, port, ctxSize, threads, moeMode, auth)
	fmt.Fprintf(os.Stderr, "%s\n", llamaServer+" "+strings.Join(serveArgs, " "))

	proc := exec.Command(llamaServer, serveArgs...)
	// llama-server's own port is on loopback for every local user to see in `ps`: it gets a random
	// per-launch key of its own, in the environment and never in argv, and only the proxy holds it
	// (2026-09-29 audit, round 126, F126-L1-2).
	var kb [24]byte
	if _, err := rand.Read(kb[:]); err != nil {
		return fmt.Errorf("could not make a backend key: %w", err)
	}
	backendKey := hex.EncodeToString(kb[:])
	proc.Env = append(os.Environ(), "LLAMA_API_KEY="+backendKey)
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr
	proc.Stdin = os.Stdin

	proxyErrCh := make(chan error, 1)
	go func() {
		proxyErrCh <- launch.RunNormalizingProxyOnKeyed(bindHost, port, internalPort, apiKey, backendKey)
	}()

	// Registers this model in ~/.oaica/local_servers.json so `oaica launch`'s
	// picker and per-model host resolution pick it up automatically — no
	// manual OAICA_HOST needed. Unregistered on any exit path (signal or
	// process death) so a stale/dead entry doesn't linger and get offered
	// as a live option. Health-checked again at read time regardless (see
	// launch/oaica_models.go) as a second line of defense against staleness.
	// Always register loopback for `oaica launch` discovery on this box,
	// even when also listening on 0.0.0.0 — the local CLI has no reason to
	// route back in via the external address.
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := oaicaRegisterLocalServer(model, origin, apiKey); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to register local server (picker won't auto-discover it): %v\n", err)
	}
	// Its OWN entry, matched by origin and pid — the model name alone is not
	// enough to identify this instance (2026-09-26 audit).
	cleanup := func() { oaicaUnregisterLocalServerAt(model, origin) }
	defer cleanup()

	// Calling Process.Kill()/Signal() after the process has already
	// exited/been Wait()'d is a known Go stdlib gotcha that can panic
	// rather than just return an error (observed here: llama-server OOMing
	// and exiting near-instantly races with the proxy failing to bind,
	// both select branches firing close together). Never let a
	// best-effort cleanup kill crash the process instead of exiting
	// cleanly.
	safeKill := func() {
		defer func() { recover() }()
		if proc.Process != nil {
			proc.Process.Kill()
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		safeKill()
		os.Exit(130)
	}()

	fmt.Fprintf(os.Stderr, "\nAuto-discovered by `oaica launch` — no OAICA_HOST needed. To point manually anyway:\n  export OAICA_HOST=http://127.0.0.1:%d\n  oaica launch claude\n\n", port)

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- proc.Run() }()

	select {
	case err := <-runErrCh:
		return err
	case err := <-proxyErrCh:
		safeKill()
		return fmt.Errorf("local proxy failed: %w", err)
	}
}

// createPullTemp makes this pull's own temp file next to destPath. Ctrl-C or SIGTERM during the copy removes it
// (every interrupted pull used to leave a multi-GB file), and stale siblings from crashed pulls are swept first:
// an active pull rewrites its file continuously, so only one untouched for a while is abandoned (2026-09-29
// audit, round 131, F131-L2-1).
func createPullTemp(destPath string) (*os.File, func(), error) {
	dir, base := filepath.Dir(destPath), filepath.Base(destPath)
	if old, _ := filepath.Glob(filepath.Join(dir, base+".partial*")); len(old) > 0 {
		for _, p := range old {
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() && time.Since(fi.ModTime()) > pullStaleTempAge {
				os.Remove(p)
			}
		}
	}
	f, err := os.CreateTemp(dir, base+".partial-*")
	if err != nil {
		return nil, func() {}, err
	}
	name := f.Name()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			f.Close()
			os.Remove(name)
			os.Exit(130)
		case <-done:
		}
	}()
	return f, func() { signal.Stop(sigCh); close(done) }, nil
}

// pullStaleTempAge: a temp file untouched this long belongs to a pull that is gone.
var pullStaleTempAge = 10 * time.Minute
