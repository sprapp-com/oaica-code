package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// r107Gw starts the whole gateway mux (both surfaces) over one upstream.
func r107Gw(t *testing.T, up *httptest.Server, keys []gwKey) (*httptest.Server, string) {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	g := &gateway{}
	if keys == nil {
		keys = []gwKey{{SHA256: keyHash("sk"), Label: "k"}}
	}
	if err := g.apply(gwConfig{UpstreamAddr: up.URL, ListenAddr: ":0", LedgerPath: ledger, APIKeys: keys,
		Models: []gwModel{{ID: "kat-awq", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"}}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	t.Cleanup(srv.Close)
	return srv, ledger
}

func r107Post(t *testing.T, srv *httptest.Server, path, body string, cancelAfter time.Duration) (int, http.Header, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if cancelAfter > 0 {
		time.AfterFunc(cancelAfter, cancel)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1, nil, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}
