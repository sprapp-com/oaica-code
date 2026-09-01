package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A dead upstream must be labelled, not advertised healthy. UpstreamProbes
// is a package-level cache — reset between tests.
func resetProbes(t *testing.T) {
	t.Helper()
	upstreamProbes.Lock()
	upstreamProbes.m = nil
	upstreamProbes.Unlock()
}

func TestModelsHandler_HealthAnnotation(t *testing.T) {
	resetProbes(t)
	var upUp int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&upUp) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: up.URL, ListenAddr: ":0", LedgerPath: t.TempDir() + "/l.jsonl",
		APIKeys: []gwKey{{SHA256: keyHash("sk-test"), Label: "x"}},
		Models:  []gwModel{{ID: "m1", UpstreamID: "m1"}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	g.modelsHandler(w, req)
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("models body: %v", err)
	}
	if len(body.Data) != 1 || body.Data[0]["status"] != nil {
		t.Fatalf("healthy model should omit status: %v", body.Data)
	}

	atomic.StoreInt32(&upUp, 1)
	resetProbes(t)
	w = httptest.NewRecorder()
	g.modelsHandler(w, req)
	json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Data) != 1 || body.Data[0]["status"] != "unhealthy" {
		t.Fatalf("dead upstream must be labelled unhealthy, got: %v", body.Data)
	}
}
