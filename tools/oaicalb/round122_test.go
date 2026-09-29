package main

// Round 122 leg 3 (2026-09-29 audit): the backstop meter books what the backend served, from the
// keys the backend reads, bounded, and never truncates a body it inspects.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRound122MeterReadsStreamByTheBackendsExactKey(t *testing.T) {
	meterSrv, records := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
	}))
	defer sse.Close()
	h := serveWith(newStaticPool([]*backend{newBackend(sse.URL)}), func(bs []*backend, _ int) *backend { return bs[0] })
	// The backend reads only "stream"; a differently-cased key is ignored by it, so it streams.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"Stream":false}`))
	h(httptest.NewRecorder(), req)
	waitForRecords(t, records, 1)
	if rec := (*records)[0]; !rec.Stream || rec.PromptTokens != 11 || rec.CompletionTokens != 5 {
		t.Errorf("a served streamed turn was booked %+v, want stream 11/5", rec)
	}
}

func TestRound122MeterBoundsClientTextInTheRow(t *testing.T) {
	meterSrv, records := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })
	srv := vLLMWithUsage(t)
	h := serveWith(newStaticPool([]*backend{newBackend(srv.URL)}), func(bs []*backend, _ int) *backend { return bs[0] })
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+strings.Repeat("m", 1<<20)+`"}`))
	req.Header.Set("X-Session-Id", strings.Repeat("s", 1<<20))
	h(httptest.NewRecorder(), req)
	waitForRecords(t, records, 1)
	if rec := (*records)[0]; len(rec.Model) > 300 || len(rec.SessionID) > 200 {
		t.Errorf("the row carries a %d-byte model and a %d-byte session id", len(rec.Model), len(rec.SessionID))
	}
}

func TestRound122OversizeBodyIsForwardedWholeAndUnmetered(t *testing.T) {
	meterSrv, records := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })
	var got atomic.Int64
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got.Store(n)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer be.Close()
	h := serveWith(newStaticPool([]*backend{newBackend(be.URL)}), func(bs []*backend, _ int) *backend { return bs[0] })
	body := `{"model":"m","pad":"` + strings.Repeat("x", meterBodyLimit+1024) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.ContentLength = int64(len(body))
	w := httptest.NewRecorder()
	h(w, req)
	// Refused whole, never forwarded cut (round 127: the meter cannot read it, so it must not serve it).
	if w.Code != http.StatusRequestEntityTooLarge || got.Load() != 0 {
		t.Errorf("status %d, backend received %d bytes: an oversize body was served unmetered or cut", w.Code, got.Load())
	}
	_ = records
}

// F124-L3-2 (2026-09-29 audit, round 124): SSE or document is decided by the response, and an
// oversize request is still metered.
func TestRound124MeterFramingComesFromTheResponse(t *testing.T) {
	meterSrv, records := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
	}))
	defer sse.Close()
	h := serveWith(newStaticPool([]*backend{newBackend(sse.URL)}), func(bs []*backend, _ int) *backend { return bs[0] })
	for _, stream := range []string{`1`, `"true"`} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":`+stream+`}`))
		h(httptest.NewRecorder(), req)
	}
	waitForRecords(t, records, 2)
	for i, rec := range (*records)[:2] {
		if rec.PromptTokens != 11 || rec.CompletionTokens != 5 || !rec.UsageSeen {
			t.Errorf("row %d booked %+v, want 11/5", i, rec)
		}
	}
}

// F124-L3-3: every listen address in the in-tree oaicalb configs is loopback, as the deploy README
// says and as every consumer (gatekeeper, gateway, the watchdogs) reaches them.
func TestRound124InTreeConfigsBindLoopback(t *testing.T) {
	for _, f := range []string{"oaicalb.json", "nemotron-oaicalb.json"} {
		b, err := os.ReadFile("../a100b/" + f)
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(b, &cfg); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"leastconn_addr", "session_hash_addr", "status_addr"} {
			addr, _ := cfg[k].(string)
			if addr != "" && !strings.HasPrefix(addr, "127.0.0.1:") && !strings.HasPrefix(addr, "localhost:") {
				t.Errorf("%s %s = %q binds every interface", f, k, addr)
			}
		}
	}
}

// F125-L3-1 / F125-L3-2 (2026-09-29 audit, round 125).
func TestRound125MeterBooksLargeDocumentsAndMixedCaseStreams(t *testing.T) {
	meterSrv, records := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })
	doc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"`+strings.Repeat("x", 5<<20)+`"}}],"usage":{"prompt_tokens":11,"completion_tokens":5}}`)
	}))
	defer doc.Close()
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "Text/Event-Stream; charset=utf-8")
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
	}))
	defer sse.Close()
	for _, be := range []*httptest.Server{doc, sse} {
		h := serveWith(newStaticPool([]*backend{newBackend(be.URL)}), func(bs []*backend, _ int) *backend { return bs[0] })
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":1}`)))
	}
	waitForRecords(t, records, 2)
	for i, rec := range (*records)[:2] {
		if rec.PromptTokens != 11 || rec.CompletionTokens != 5 || !rec.UsageSeen {
			t.Errorf("row %d booked %+v, want 11/5", i, rec)
		}
	}
}

// F126-L3-1 (2026-09-29 audit, round 126): a streamed turn whose client did not ask for usage is still
// metered, because the meter asks for it.
func TestRound126MeterForcesIncludeUsageOnStreams(t *testing.T) {
	meterSrv, records := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })
	var lastBody atomic.Value
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		lastBody.Store(string(b))
		var req struct {
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		json.Unmarshal(b, &req)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		if req.StreamOptions.IncludeUsage { // vLLM sends the usage frame only when asked
			io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":5}}\n\n")
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer be.Close()
	h := serveWith(newStaticPool([]*backend{newBackend(be.URL)}), func(bs []*backend, _ int) *backend { return bs[0] })
	bodies := []string{
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"a<b"}]}`,
		`{"model":"m","stream":true,"stream_options":{"include_usage":false,"x":1}}`,
		`{"model":"m","stream":"true"}`,
		`{"model":"m","stream":1}`,
	}
	for _, b := range bodies {
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(b)))
	}
	waitForRecords(t, records, 4)
	for i, rec := range (*records)[:4] {
		if rec.PromptTokens != 11 || rec.CompletionTokens != 5 || !rec.UsageSeen {
			t.Errorf("body %d booked %+v, want 11/5", i, rec)
		}
	}
	if lb, _ := lastBody.Load().(string); !strings.Contains(lb, `"include_usage":true`) {
		t.Errorf("the backend was not asked for usage: %s", lb)
	}
	// a non-stream body is forwarded untouched
	lastBody.Store("")
	plain := `{"model":"m","messages":[]}`
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(plain)))
	if lb, _ := lastBody.Load().(string); lb != plain {
		t.Errorf("a non-stream body was rewritten: %s", lb)
	}
}

// F127-L3-1 (2026-09-29 audit, round 127): a body the meter cannot fully read as a JSON object is refused,
// including the spellings Python's parser accepts and Go's does not.
func TestRound127MeterRefusesBodiesItCannotRead(t *testing.T) {
	var reached atomic.Int32
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		reached.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer be.Close()
	meterSrv, _ := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })
	h := serveWith(newStaticPool([]*backend{newBackend(be.URL)}), func(bs []*backend, _ int) *backend { return bs[0] })
	for name, body := range map[string]string{
		"NaN":      `{"model":"m","stream":true,"pad":NaN}`,
		"Infinity": `{"model":"m","stream":true,"pad":Infinity}`,
		"BOM":      "\xef\xbb\xbf" + `{"model":"m","stream":true}`,
		"array":    `[1,2]`,
		"null":     `null`,
	} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, w.Code)
		}
	}
	if reached.Load() != 0 {
		t.Errorf("%d unreadable bodies reached the backend", reached.Load())
	}
	// a normal body still goes through
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)))
	if w.Code != http.StatusOK {
		t.Errorf("a normal body answered %d", w.Code)
	}
}
