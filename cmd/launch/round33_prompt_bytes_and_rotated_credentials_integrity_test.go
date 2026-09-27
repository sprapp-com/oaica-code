package launch

// round33_prompt_bytes_and_rotated_credentials_integrity_test.go — a prompt
// measured by its transport encoding on the client proxy, and a credential a
// reader does not read (2026-09-27 audit, round 33, A-F1/F2/F3).
//
//   - A-F1: the client proxy's context-fit clamp measured the prompt as
//     len(request body)/4. An inline image is a data URI — base64 of the whole
//     file, four thirds of its size, inside those bytes — so a 1 MB screenshot
//     was billed as ~350k prompt tokens on a 262144-token leg that accepts
//     images, and the clamp refused the turn LOCALLY with the "prompt is too
//     long" wording Claude Code pattern-matches to its compaction recovery
//     path. Round 32 fixed exactly this in tools/gateway (`messagesBytes` /
//     `promptPayloadBytes`); the proxy that `oaica launch claude` serves never
//     got it, and its calibration could not rescue the turn either: a live
//     sample scales by the same inflated byte count, and the recorded ratio of
//     an image body (~0.0002 tok/byte) is discarded by calibMinRatio, so the
//     session never calibrates at all.
//   - A-F2/A-F3: OMP's and Hermes' writers store the remote's token
//     (provider["apiKey"], model.api_key) and neither drift term reads the
//     VALUE back — OMP checks only that it is non-empty, Hermes never looks.
//     After the remote's key is rotated the store still reads as current, the
//     managed launch skips its rewrite, and the child starts against the
//     remote with the revoked bearer. Nothing else supplies the credential:
//     both `Run`s pass the environment through untouched. Round 32 closed this
//     exact shape for droid.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestTheClientProxyChargesAnImageAsAnImageNotItsBase64 is A-F1.
func TestTheClientProxyChargesAnImageAsAnImageNotItsBase64(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var mu sync.Mutex
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "kat-awq",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "a cat"},
			}},
			"usage": map[string]any{"prompt_tokens": 312, "completion_tokens": 2, "total_tokens": 314},
		})
	}))
	defer upstream.Close()

	proxyURL := startCalibProxy(t, upstream.URL, "sess-image")

	imageBody := func(imageBytes int) []byte {
		t.Helper()
		img := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x7f}, imageBytes))
		body, err := json.Marshal(map[string]any{
			"model":      "kat-awq",
			"max_tokens": 4096,
			"messages": []map[string]any{{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "what is in this screenshot?"},
					{"type": "image", "source": map[string]any{
						"type": "base64", "media_type": "image/png", "data": img,
					}},
				},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}

	// How the clamp measures a body: an image is one image, not its base64.
	small := imageBody(64 << 10)
	large := imageBody(2 << 20)
	if got, want := clientPromptBytes(large), clientPromptBytes(small); got != want {
		t.Errorf("clientPromptBytes(2MB image) = %d, clientPromptBytes(64KB image) = %d: the proxy is measuring the prompt as the raw body, so a screenshot is billed to the context-fit clamp as its own encoded size (~%d tokens)", got, want, len(large)/4)
	}

	// The documented path: Claude Code pastes / Read-tool screenshots.
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(large))
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a 1MB+ inline image was refused locally: status=%d body=%s; want 200 — the image is a supported path (openAIMessage.MarshalJSON exists so it reaches the upstream) and the real prompt is a few hundred tokens", resp.StatusCode, strings.TrimSpace(string(rb)))
	}

	// The upstream must have seen the image itself, so this is not passing by
	// the proxy dropping the attachment.
	mu.Lock()
	if len(bodies) == 0 {
		mu.Unlock()
		t.Fatal("the upstream was never called with the image turn")
	}
	seen := string(bodies[len(bodies)-1])
	mu.Unlock()
	if !strings.Contains(seen, `"image_url"`) {
		t.Errorf("the upstream request carries no image part: the turn was let through without the screenshot the client sent")
	}
}

// TestAClientProxyImageTurnIsMeasuredTheSameWayWhenCalibrated: the calibrated
// half — a live sample must not be scaled by the image's base64 either.
func TestAClientProxyImageTurnIsMeasuredTheSameWayWhenCalibrated(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// Report the ratio a text session really shows, so the sample is
		// recorded and the session is calibrated.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "kat-awq",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"},
			}},
			"usage": map[string]any{"prompt_tokens": len(b) / 4, "completion_tokens": 2, "total_tokens": len(b)/4 + 2},
		})
	}))
	defer upstream.Close()

	proxyURL := startCalibProxy(t, upstream.URL, "sess-image-calibrated")

	small, _ := json.Marshal(map[string]any{
		"model": "kat-awq", "max_tokens": 1024,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(small))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup turn failed: %d", resp.StatusCode)
	}

	img := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x7f}, 1<<20))
	body, _ := json.Marshal(map[string]any{
		"model": "kat-awq", "max_tokens": 4096,
		"messages": []map[string]any{{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "what is in this screenshot?"},
			{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": img,
			}},
		}}},
	})
	resp2, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("a calibrated session still refused the image turn: status=%d body=%s; the estimate is scaled by a byte count that includes the base64", resp2.StatusCode, strings.TrimSpace(string(rb)))
	}
}

// TestAnOMPStoreHoldingAReplacedRemoteTokenIsDrift is A-F2.
func TestAnOMPStoreHoldingAReplacedRemoteTokenIsDrift(t *testing.T) {
	const remoteBase = "http://10.9.9.9:8088"

	seed := func(t *testing.T, liveToken, storedToken string) {
		t.Helper()
		home := t.TempDir()
		setOMPTestHome(t, home)
		writeRemotes(t, `{"remotes":[{"name":"box","base_url":"`+remoteBase+`","api_key":"`+liveToken+`"}]}`)
		modelsPath := filepath.Join(home, ".omp", "agent", "models.yml")
		if err := os.MkdirAll(filepath.Dir(modelsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		// The shape ensureOMPProvider writes for a user remote: the wire is
		// chat completions, the credential is apiKey, and the model is the
		// upstream id the remote serves.
		cfg := "providers:\n" +
			"  ollama:\n" +
			"    baseUrl: " + remoteBase + "/v1\n" +
			"    api: openai-completions\n" +
			"    auth: apiKey\n" +
			"    apiKey: " + storedToken + "\n" +
			"    models:\n" +
			"      - id: big-model\n"
		if err := os.WriteFile(modelsPath, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Control: the token the remote issues now, in the store, reads as current.
	seed(t, "live-token", "live-token")
	if got := (&OMP{}).CurrentModel(); got != "box/big-model" {
		t.Fatalf("control: CurrentModel = %q, want box/big-model (the fixture must be the shape the writer leaves, or this test cannot show the credential is unread)", got)
	}

	// The documented path: the user re-keys the box (oaica remote add ... --key).
	seed(t, "rotated-token", "revoked-token")
	if got := (&OMP{}).CurrentModel(); got != "" {
		t.Errorf("OMP's store holds a credential the remote has replaced (apiKey: revoked-token, live: rotated-token) and still reports %q as current: the launch skips the rewrite and OMP starts against the remote with the dead bearer, 401ing every request with no message", got)
	}
}

// TestAHermesConfigHoldingAReplacedRemoteTokenIsDrift is A-F3.
func TestAHermesConfigHoldingAReplacedRemoteTokenIsDrift(t *testing.T) {
	const remoteBase = "http://10.9.9.9:8088"

	seed := func(t *testing.T, liveToken string) (string, string) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HERMES_HOME", home)
		writeRemotes(t, `{"remotes":[{"name":"box","base_url":"`+remoteBase+`","api_key":"`+liveToken+`"}]}`)
		configPath := filepath.Join(home, "config.yaml")
		if err := writeHermesConfig(configPath, "box/big-model", []string{"box/big-model"}); err != nil {
			t.Fatalf("writeHermesConfig: %v", err)
		}
		return home, configPath
	}

	// Control: the writer's own output reads as current.
	_, configPath := seed(t, "live-token")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "api_key: live-token") {
		t.Fatalf("control: the writer did not store the live remote token:\n%s", data)
	}
	if got := (&Hermes{}).CurrentModel(); got != "box/big-model" {
		t.Fatalf("control: CurrentModel = %q, want box/big-model", got)
	}
	// ... but writing over the key the writer wrote is drift: the token is a
	// field the writer sets, so a store holding another one is not what a
	// write would leave.
	stale := strings.Replace(string(data), "api_key: live-token", "api_key: revoked-token", 1)
	if stale == string(data) {
		t.Fatal("control: the fixture has no api_key to rotate")
	}
	if err := os.WriteFile(configPath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (&Hermes{}).CurrentModel(); got != "" {
		t.Errorf("Hermes' config holds a credential the remote has replaced (api_key: revoked-token, live: live-token) and still reports %q as current: the managed launch skips writeHermesConfig and Hermes starts with the dead bearer", got)
	}

	// The same rotation on a fresh session, through the real writer, to prove
	// the drift is not an artefact of the hand-edit above.
	_, configPath2 := seed(t, "rotated-token")
	data2, err := os.ReadFile(configPath2)
	if err != nil {
		t.Fatal(err)
	}
	stale2 := strings.Replace(string(data2), "api_key: rotated-token", "api_key: revoked-token", 1)
	if err := os.WriteFile(configPath2, []byte(stale2), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (&Hermes{}).CurrentModel(); got != "" {
		t.Errorf("Hermes' config holds a revoked credential (api_key: revoked-token, live: rotated-token) and still reports %q as current", got)
	}
}
