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
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ollama/ollama/cmd/launch"

	"github.com/spf13/cobra"
)

// oaicaLicenseKeyPath mirrors oaicaAPIKeyPath's pattern but for the
// self-host distribution license (a separate credential from OAICA_API_KEY
// — that one authorizes cloud chat calls, this one authorizes downloading
// raw weights). Distinguishing them matters: a leaked chat API key should
// never double as a weights-download credential.
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
}

func oaicaRegisterLocalServer(model, origin, apiKey string) error {
	path, err := oaicaLocalServersPath()
	if err != nil {
		return err
	}
	entries := oaicaReadLocalServers(path)
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
	return os.WriteFile(path, b, 0o600)
}

func oaicaUnregisterLocalServer(model string) {
	path, err := oaicaLocalServersPath()
	if err != nil {
		return
	}
	entries := oaicaReadLocalServers(path)
	filtered := entries[:0]
	for _, e := range entries {
		if e.Model != model {
			filtered = append(filtered, e)
		}
	}
	b, err := json.MarshalIndent(filtered, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(path, b, 0o600)
}

func oaicaReadLocalServers(path string) []oaicaLocalServerEntry {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []oaicaLocalServerEntry
	if json.Unmarshal(b, &entries) != nil {
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
	req, err := http.NewRequest(http.MethodGet, oaicaHost()+"/v1/manifest/"+model, nil)
	if err != nil {
		return nil, err
	}
	if key := oaicaLicenseKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach %s: %w", oaicaHost(), err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
			if e.Error.Type == "license_required" || e.Error.Type == "license_invalid" {
				return nil, fmt.Errorf("%s\n\nSet a license key: OAICA_LICENSE_KEY=<key> or save one to ~/.oaica/license_key", e.Error.Message)
			}
			return nil, fmt.Errorf("%s", e.Error.Message)
		}
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, oaicaHost())
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
func oaicaPullModel(model string) (string, error) {
	manifest, err := oaicaFetchManifest(model)
	if err != nil {
		return "", err
	}

	destPath, err := oaicaModelPath(model)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(destPath); err == nil && fi.Size() == manifest.SizeBytes {
		fmt.Fprintf(os.Stderr, "%s already downloaded (%s), skipping\n", model, humanBytes(manifest.SizeBytes))
		return destPath, nil
	}

	if manifest.Source == "hf" {
		return oaicaPullFromHF(model, manifest, destPath)
	}

	pullURL := oaicaHost() + manifest.PullURL
	req, err := http.NewRequest(http.MethodGet, pullURL, nil)
	if err != nil {
		return "", err
	}
	if key := oaicaLicenseKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 0} // large file, no overall timeout
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("pull failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("pull failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	tmpPath := destPath + ".partial"
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	fmt.Fprintf(os.Stderr, "pulling %s (%s)...\n", model, humanBytes(manifest.SizeBytes))
	written, err := io.Copy(f, &progressReader{r: resp.Body, total: manifest.SizeBytes, label: model})
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
	if hfToken := oaicaHFToken(); hfToken != "" {
		// The repo is public — a token isn't required for correctness,
		// only speed. HF explicitly warns unauthenticated requests get
		// throttled ("Please set a HF_TOKEN to enable higher rate limits
		// and faster downloads"); opportunistically use one if the user
		// already has the HF CLI configured locally, but don't require it.
		req.Header.Set("Authorization", "Bearer "+hfToken)
	}
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HF download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HF download failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	tmpPath := destPath + ".partial"
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if key == nil {
		// Plaintext blob: stream it straight through, verifying size and (when
		// the manifest supplies one) sha256 — the integrity check GCM's auth
		// tag provides on the encrypted path.
		fmt.Fprintf(os.Stderr, "pulling %s from HuggingFace (%s)...\n", model, humanBytes(manifest.SizeBytes))
		hasher := sha256.New()
		written, err := io.Copy(io.MultiWriter(f, hasher), &progressReader{r: resp.Body, total: manifest.SizeBytes, label: model})
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
	// No exact-size verification here (unlike the R2/fallback path) —
	// manifest.SizeBytes is the ENCRYPTED blob's size, not the decrypted
	// plaintext size (chunked AES-GCM adds ~32 bytes/8MB-chunk overhead),
	// so they'll never match exactly. GCM's authentication tag already
	// catches truncation/corruption per-chunk (decryptChunkedAESGCMStream
	// errors out immediately on a bad tag) — that's the real integrity
	// check here, a byte-count comparison would just be redundant and
	// wrong given the size mismatch.
	written, err := decryptChunkedAESGCMStream(&progressReader{r: resp.Body, total: manifest.SizeBytes, label: model}, f, key)
	if err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("pull/decrypt interrupted: %w", err)
	}
	f.Close()
	fmt.Fprintln(os.Stderr)
	_ = written

	if err := os.Rename(tmpPath, destPath); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "%s saved to %s\n", model, destPath)
	return destPath, nil
}

// decryptChunkedAESGCMStream reads the chunked AES-256-GCM frame format
// (matches the encryption tool used to prepare HF-hosted models):
//   repeated: [4-byte big-endian ciphertext_len][12-byte nonce][ciphertext+16-byte GCM tag]
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
	insecure, _ := cmd.Flags().GetBool("insecure")
	// Binding off-loopback publishes an inference server. Without a key
	// anyone who can reach the port can use (and bill) your GPU, so this
	// is a hard stop rather than a warning — --insecure is the explicit
	// opt-out for trusted private networks.
	if bindHost != "127.0.0.1" && bindHost != "localhost" && apiKey == "" && !insecure {
		return fmt.Errorf("refusing to bind %s without --api-key: that exposes an unauthenticated inference server to the network.\n\nEither set one:\n  oaica serve %s --host %s --api-key \"$(openssl rand -hex 24)\"\n\nor, only on a network you fully trust, pass --insecure", bindHost, model, bindHost)
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
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr
	proc.Stdin = os.Stdin

	proxyErrCh := make(chan error, 1)
	go func() {
		proxyErrCh <- launch.RunNormalizingProxyOn(bindHost, port, internalPort, apiKey)
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
	cleanup := func() { oaicaUnregisterLocalServer(model) }
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
