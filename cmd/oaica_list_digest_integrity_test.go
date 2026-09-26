package cmd

// oaica_list_digest_integrity_test.go — `oaica list` and `oaica ps` panicked on
// a model whose ID was shorter than 12 characters (2026-09-26 audit).
//
// Both tables render the ID column by slicing the server's digest to 12
// characters. The digest is not ours — it arrives as JSON from whatever
// OLLAMA_HOST points at, so its length is asserted by the peer, not by us. An
// upstream that reports a short digest, an empty digest (its own ID column in
// the same row already carries the "not reported" case with "-"), or a shim
// that speaks the routes without the field at all, slices out of range and
// takes the whole CLI down with a Go stack trace instead of printing a table:
//
//	$ oaica list
//	panic: runtime error: slice bounds out of range [:12] with length 0
//
// A malformed peer is exactly the case a management command exists to survive,
// so the ID is truncated only when it is long enough to truncate.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
	"github.com/spf13/cobra"
)

// oaicaListTable runs one of the two list handlers against a stub server that
// answers with the given JSON body on the given route.
func oaicaListTable(t *testing.T, route string, body string, handler func(cmd *cobra.Command, args []string) error) (string, error) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != route {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OLLAMA_HOST", srv.URL)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	hErr := handler(cmd, nil)
	w.Close()
	out, _ := io.ReadAll(r)
	os.Stdout = oldStdout
	return string(out), hErr
}

// A digest the peer reports as empty, or shorter than the 12 we slice, must
// print a row rather than panic.
func TestAListRowSurvivesADigestShorterThanTheSlice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		digest string
		want   string
	}{
		{"empty", "", "-"},
		{"one char", "a", "a"},
		{"eleven", "abcdefghijk", "abcdefghijk"},
		{"exactly twelve", "abcdefghijkl", "abcdefghijkl"},
		{"longer", "abcdefghijklmnop", "abcdefghijkl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(api.ListResponse{Models: []api.ListModelResponse{{
				Name:   "kat-awq",
				Digest: tc.digest,
				Size:   1024,
			}}})
			if err != nil {
				t.Fatal(err)
			}

			out, err := oaicaListTable(t, "/api/tags", string(body), ListHandler)
			if err != nil {
				t.Fatalf("ListHandler: %v", err)
			}
			if !strings.Contains(out, "kat-awq") {
				t.Errorf("the row for kat-awq is missing:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("digest %q is printed as an ID column without %q:\n%s", tc.digest, tc.want, out)
			}
			for _, bad := range []string{"panic", "slice bounds"} {
				if strings.Contains(out, bad) {
					t.Errorf("the CLI printed %q instead of a table:\n%s", bad, out)
				}
			}
		})
	}
}

// The running table slices the same field from the same kind of peer.
func TestARunningRowSurvivesADigestShorterThanTheSlice(t *testing.T) {
	body, err := json.Marshal(api.ProcessResponse{Models: []api.ProcessModelResponse{{
		Name:          "kat-awq",
		Digest:        "",
		Size:          1024,
		SizeVRAM:      512,
		ContextLength: 4096,
	}}})
	if err != nil {
		t.Fatal(err)
	}

	out, err := oaicaListTable(t, "/api/ps", string(body), ListRunningHandler)
	if err != nil {
		t.Fatalf("ListRunningHandler: %v", err)
	}
	if !strings.Contains(out, "kat-awq") {
		t.Errorf("the row for kat-awq is missing:\n%s", out)
	}
}

// And the source no longer slices a digest it did not length-check. Pinned at
// the source because the same two lines are the whole finding.
func TestNoListTableSlicesAnUncheckedDigest(t *testing.T) {
	src := cmdGoSource(t)
	if strings.Contains(src, "m.Digest[:12]") {
		t.Error("cmd/cmd.go slices the server's digest to 12 characters without checking its length — a peer that reports a short or empty digest panics the CLI; render it through oaicaShortDigest instead")
	}
}
