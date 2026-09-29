package main

// Round 124 leg 3 (2026-09-29 audit): the incremental argument scanner answers exactly as the
// predicates it replaced, for every fragmentation of every text, including Unicode whitespace and
// fragments that cut a rune in two (F124-L3-1); the upstream hop does not HTML-escape (F124-L3-4).

import (
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRound124ArgScanMatchesThePredicatesItReplaced(t *testing.T) {
	alphabet := []string{" ", "\t", "\n", " ", "\v", "\f", "\u0085", " ", "{", "}", "[", "]", "\"", "\\", "a", ":", "1", ",", "é", "模", "\\\"", "{\"a\":1}", "{\"b\":[1,2]}", "\"x}\""}
	r := rand.New(rand.NewSource(124))
	for it := 0; it < 60000; it++ {
		var b strings.Builder
		for n := r.Intn(9); n > 0; n-- {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		full := b.String()
		tb := &toolBlock{}
		// fragment at random byte offsets, INCLUDING inside a rune
		for pos := 0; pos < len(full); {
			next := pos + 1 + r.Intn(3)
			if next > len(full) {
				next = len(full)
			}
			tb.args.WriteString(full[pos:next])
			pos = next
			cur := tb.args.String()
			if got, want := tb.argsFinished(), argsAreFinished(cur); got != want {
				t.Fatalf("argsFinished(%q) = %v, want %v", cur, got, want)
			}
			if got, want := tb.finishedObject(), finishedObjectArgs(cur); got != want {
				t.Fatalf("finishedObject(%q) = %v, want %v", cur, got, want)
			}
			d := alphabet[r.Intn(len(alphabet))]
			if got, want := tb.argsExtend(d), callArgsExtend(cur, d); got != want {
				t.Fatalf("argsExtend(%q, %q) = %v, want %v", cur, d, got, want)
			}
		}
	}
}

func TestRound124UpstreamHopIsNotHTMLEscaped(t *testing.T) {
	var got atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got.Store(n)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer up.Close()
	srv, _ := r107Gw(t, up, []gwKey{{SHA256: keyHash("sk"), Label: "k"}})
	body := `{"model":"kat-awq","messages":[{"role":"user","content":"` + strings.Repeat("<", 400<<10) + `"}]}`
	if code, _, rb := r107Post(t, srv, "/v1/chat/completions", body, 0); code != 200 {
		t.Fatalf("%d %.100s", code, rb)
	}
	if g := got.Load(); g > int64(len(body))*2 {
		t.Errorf("the upstream received %d bytes for a %d-byte client body: '<' was escaped to six bytes", g, len(body))
	}
}
