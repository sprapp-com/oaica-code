package cmd

// oaica_pull_serve.go — ollama-style `oaica pull <model>` / `oaica serve
// <model>` for true local self-hosting, distinct from `oaica run` (which
// stays a thin client to api.sprapp.com — unchanged). Talks to the router's
// /v1/manifest and /v1/pull endpoints (prism-api-router/src/index.ts),
// which stream the GGUF directly through the Worker rather than issuing a
// presigned URL — same reasoning here: the license key is checked once at
// pull time, nothing to leak or outlive that check.
//
// The old Ollama-native pullCmd (PullHandler, ollama.com registry protocol)
// is dead in this fork — it required a local Ollama server (checkServerHeartbeat)
// this thin client doesn't run. This file replaces it.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// oaicaLicenseKeyPath mirrors oaicaAPIKeyPath's pattern but for the
// self-host distribution license (a separate credential from OAICA_API_KEY
// — that one authorizes cloud chat calls, this one authorizes downloading
// raw weights). Distinguishing them matters: a leaked chat API key should
// never double as a weights-download credential.
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

func oaicaModelsDir() (string, error) {
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

type oaicaManifest struct {
	Model      string  `json:"model"`
	SizeBytes  int64   `json:"size_bytes"`
	SHA256     *string `json:"sha256"`
	PullURL    string  `json:"pull_url"`
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
	_, err := oaicaPullModel(args[0])
	return err
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
	threads := runtime.NumCPU()

	serveArgs := []string{
		"-m", modelPath,
		"-ngl", "999",
		"-c", strconv.Itoa(ctxSize),
		"-t", strconv.Itoa(threads),
		"-fa", "on",
		"-ctk", "q8_0", "-ctv", "q8_0",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
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

	fmt.Fprintf(os.Stderr, "starting %s locally on 127.0.0.1:%d (ctx=%d, threads=%d, moe=%s)...\n", model, port, ctxSize, threads, moeMode)
	fmt.Fprintf(os.Stderr, "%s\n", llamaServer+" "+strings.Join(serveArgs, " "))

	proc := exec.Command(llamaServer, serveArgs...)
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr
	proc.Stdin = os.Stdin

	fmt.Fprintf(os.Stderr, "\nOnce healthy, point oaica at it in another shell:\n  export OAICA_HOST=http://127.0.0.1:%d\n  oaica launch claude\n\n", port)

	return proc.Run()
}
