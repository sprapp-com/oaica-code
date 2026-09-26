package httpbody

// httpbody_test.go — the bound itself, pinned: the memory a body can cost must
// not be chosen by whoever sends it (2026-09-26 audit).

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// countingReader reports how much of the body the reader actually consumed: a
// reader that drains the whole thing before checking its length has already
// paid the memory cost it is supposed to prevent.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func TestReadCappedRefusesABodyOverTheLimit(t *testing.T) {
	const max = 1024
	src := &countingReader{r: strings.NewReader(strings.Repeat("x", 4*int(max)))}

	got, err := ReadCapped(src, max, "the test body")
	if err == nil {
		t.Fatalf("a %d-byte body was accepted under a %d-byte limit", 4*max, max)
	}
	if got != nil {
		t.Errorf("an over-limit body was returned anyway (%d bytes) — a caller that parses it is parsing half a body", len(got))
	}
	if !strings.Contains(err.Error(), "the test body") || !strings.Contains(err.Error(), "1024") {
		t.Errorf("the error does not name which read hit the bound and at what size: %v", err)
	}
	if src.n > max+1 {
		t.Errorf("the reader consumed %d bytes of a body it refused at %d — the limit must stop the read, not just reject the result", src.n, max)
	}
}

// The boundary: exactly the cap is a body oaica reads, one byte more is not.
func TestReadCappedAtTheBoundary(t *testing.T) {
	const max = 512

	got, err := ReadCapped(bytes.NewReader(bytes.Repeat([]byte("y"), max)), max, "exact")
	if err != nil {
		t.Fatalf("a body of exactly the limit was refused: %v", err)
	}
	if len(got) != max {
		t.Errorf("read %d bytes, want %d", len(got), max)
	}

	if _, err := ReadCapped(bytes.NewReader(bytes.Repeat([]byte("y"), max+1)), max, "one over"); err == nil {
		t.Error("a body one byte over the limit was accepted")
	}
}

// A diagnostic read must never replace the failure it describes, and must
// never come back as a whole oversized body.
func TestReadCappedOrEmptyTruncatesAndKeepsGoing(t *testing.T) {
	const max = 64
	got := ReadCappedOrEmpty(strings.NewReader(strings.Repeat("z", 500)), max, "the upstream error")
	if len(got) <= max {
		t.Fatalf("the truncation marker is missing: %q", got)
	}
	if !bytes.HasPrefix(got, bytes.Repeat([]byte("z"), max)) {
		t.Error("the diagnostic did not keep the beginning of the body, which is the part that says what went wrong")
	}
	if !strings.Contains(string(got), "truncated at 64 bytes") {
		t.Errorf("a truncated diagnostic must say so: %q", got)
	}

	// An exact-length body is not "truncated", and an error mid-read still
	// yields whatever was read rather than nothing.
	exact := ReadCappedOrEmpty(strings.NewReader(strings.Repeat("z", max)), max, "exact")
	if len(exact) != max {
		t.Errorf("an exact-length diagnostic was altered: %d bytes", len(exact))
	}
	if out := ReadCappedOrEmpty(errReader{}, max, "err"); len(out) != 0 {
		t.Errorf("a body that failed to read returned %q", out)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
