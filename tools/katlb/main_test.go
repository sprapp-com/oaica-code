package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// brokenVLLM reproduces the 2026-08-25 outage exactly: the HTTP server is up
// and GET /v1/models is 200, but every chat completion is a 400 (tokenizer
// had no chat_template). The old GET probe reports this backend UP; the
// chat probe must report it DOWN.
func brokenVLLM(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(200)
			io.WriteString(w, `{"object":"list","data":[{"id":"kat-awq"}]}`)
		case "/v1/chat/completions":
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"default chat template is no longer allowed"}}`)
		default:
			w.WriteHeader(404)
		}
	}))
}

// healthyVLLM answers both probes 200 and records the chat body it got.
func healthyVLLM(t *testing.T, got *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(200)
		case "/v1/chat/completions":
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			json.Unmarshal(b, &m)
			*got = m
			w.WriteHeader(200)
			io.WriteString(w, `{"choices":[{"message":{"content":"x"}}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestProbe_GETMissesChat400_ChatProbeCatchesIt(t *testing.T) {
	srv := brokenVLLM(t)
	defer srv.Close()
	b := newBackend(srv.URL)
	client := &http.Client{Timeout: 2 * time.Second}

	if !b.probeOnce(client, "/v1/models", "") {
		t.Fatal("GET probe should report the broken backend UP (that is the bug being fixed)")
	}
	if b.probeOnce(client, "/v1/models", "kat-awq") {
		t.Fatal("chat probe must report a backend that 400s every completion as DOWN")
	}
}

func TestProbe_ChatSends1TokenForServedModel(t *testing.T) {
	var got map[string]any
	srv := healthyVLLM(t, &got)
	defer srv.Close()
	b := newBackend(srv.URL)
	if !b.probeOnce(&http.Client{Timeout: 2 * time.Second}, "/v1/models", "kat-awq") {
		t.Fatal("chat probe against a healthy backend must be UP")
	}
	if got["model"] != "kat-awq" {
		t.Errorf("probe model = %v, want kat-awq (must match --served-model-name)", got["model"])
	}
	if got["max_tokens"] != float64(1) {
		t.Errorf("probe max_tokens = %v, want 1 (probe must be cheap)", got["max_tokens"])
	}
}

func TestHealthCheck_ChatProbeFlipsBackendDown(t *testing.T) {
	srv := brokenVLLM(t)
	defer srv.Close()
	b := newBackend(srv.URL) // starts healthy=true
	go b.healthCheck("/v1/models", "kat-awq", 2*time.Second)
	// 2 consecutive failures at a 3s cadence -> DOWN by ~4s; allow slack.
	deadline := time.Now().Add(9 * time.Second)
	for time.Now().Before(deadline) {
		if !b.healthy.Load() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("backend that 400s chat stayed UP under the chat probe")
}

func TestLoadConfig_BadJSONReturnsErrorNotExit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(p, []byte(`{"backends": [`), 0o600)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("malformed config must return an error (previously log.Fatalf'd)")
	}
}

func TestLoadConfig_ProbeDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"backends":["http://127.0.0.1:1"],"probe_model":"kat-awq"}`), 0o600)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProbeModel != "kat-awq" || cfg.ProbeTimeoutSec != 10 {
		t.Errorf("probe config = %+v, want model kat-awq timeout 10", cfg)
	}
}
