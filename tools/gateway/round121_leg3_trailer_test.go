package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type trailerBody struct {
	r   io.Reader
	req *http.Request
}

func (b *trailerBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		b.req.Trailer.Set("X-Gatekeeper-Tier", "internal")
		b.req.Trailer.Set("X-Oaica-Metered", "0")
	}
	return n, err
}

func TestRound121RequestTrailers(t *testing.T) {
	got := map[string]string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		got["te"] = strings.Join(r.TransferEncoding, ",")
		for k, v := range r.Trailer {
			got["trailer:"+k] = strings.Join(v, ",")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer up.Close()
	srv, _ := r107Gw(t, up, nil)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", nil)
	req.Body = io.NopCloser(&trailerBody{r: strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`), req: req})
	req.ContentLength = -1
	req.Trailer = http.Header{"X-Gatekeeper-Tier": nil, "X-Oaica-Metered": nil}
	req.Header.Set("Authorization", "Bearer sk")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	t.Logf("upstream saw %v", got)
	for k := range got {
		if strings.HasPrefix(k, "trailer:") {
			t.Errorf("client trailer reached upstream: %s=%s", k, got[k])
		}
	}
}
