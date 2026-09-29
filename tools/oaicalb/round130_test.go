package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// F130-L3-1 (2026-09-29 audit, round 130): a client that hangs up mid-stream still leaves a meter row, marked
// aborted.
func TestRound130DisconnectStillReportsTheTurn(t *testing.T) {
	hub, records := fakeMeterHub(t)
	metered = newMeterHub(hub.URL, "tok", "r")
	defer func() { metered = nil }()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 200; i++ {
			if _, err := fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t%d\"}}]}\n\n", i); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	defer up.Close()
	lb := httptest.NewServer(serveWith(newStaticPool([]*backend{newBackend(up.URL)}), leastConnPick))
	defer lb.Close()
	resp, err := http.Post(lb.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	for n := 0; n < 10; {
		l, err := br.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(l, "data:") {
			n++
		}
	}
	resp.Body.Close()
	waitForRecords(t, records, 1)
	if !(*records)[0].Aborted {
		t.Errorf("the row of a stream the client dropped is not marked aborted: %+v", (*records)[0])
	}
}

// F130-L3-2: generation routes the meter does not read are refused, not served unmetered.
func TestRound130UnmeteredGenerationRoutesAreRefused(t *testing.T) {
	hub, records := fakeMeterHub(t)
	metered = newMeterHub(hub.URL, "tok", "r")
	defer func() { metered = nil }()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`)
	}))
	defer up.Close()
	lb := httptest.NewServer(sessionHandler(newStaticPool([]*backend{newBackend(up.URL)}), 0))
	defer lb.Close()
	for _, p := range []string{"/invocations", "/v1/chat/completions/batch", "/generative_scoring", "/inference/v1/generate"} {
		resp, err := http.Post(lb.URL+p, "application/json", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s -> %d, want 404", p, resp.StatusCode)
		}
	}
	_ = records
}
