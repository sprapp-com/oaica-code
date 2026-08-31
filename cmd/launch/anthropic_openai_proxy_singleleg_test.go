package launch

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSingleLegProxy5xxSignalsNoPanic(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer upstream.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	table := proxyRouteTable{Default: proxyRoute{BaseURL: upstream.URL, UpstreamModel: "m"}}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("PANIC in proxy: %v", r)
			}
		}()
		RunAnthropicOpenAIProxyRoutes(ln, table)
	}()
	time.Sleep(50 * time.Millisecond)
	body, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 16, "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	for i := 0; i < 3; i++ {
		resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func TestSingleLegProxyDeadUpstreamNoPanic(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	table := proxyRouteTable{Default: proxyRoute{BaseURL: "http://127.0.0.1:1", UpstreamModel: "m"}}
	go RunAnthropicOpenAIProxyRoutes(ln, table)
	time.Sleep(50 * time.Millisecond)
	body, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 16, "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(body))
	t.Logf("resp err=%v", err)
	if resp != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
