package launch

// catalog_sync.go — `oaica model catalog sync`: fetch the models.dev payload,
// check it against the contract, and either adopt it or refuse it and keep the
// last good copy.
//
// Nothing auto-fetches. A launch reads whatever is cached and works offline;
// an explicit command is the only thing that talks to the network. That is a
// deliberate divergence from opencode, which treats the catalog as live
// infrastructure with a 5-minute refresh and a cross-process file lock — oaica
// is a one-shot CLI with no resident process to host a background refresher,
// so paying a 5 MB fetch on a timer would only tax launches that are already
// network-bound.
//
// Mechanics are model_sync.go's: ETag revalidation, a last-good fallback when
// the network is down, and file:// for air-gapped hosts and tests.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultCatalogSyncURL = "https://models.dev/api.json"

func catalogCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "cache", "catalog", "modelsdev.json"), nil
}

// CatalogSyncReport is what CatalogSync returns for the caller to print.
type CatalogSyncReport struct {
	URL       string
	Providers int
	Models    int
	FromCache bool
	Unchanged bool
	Refused   bool
}

// CatalogSync fetches the payload at url (empty = defaultCatalogSyncURL),
// validates it, and adopts it only if it passes. A contract failure is
// returned as an error naming the field, with the cached catalog left
// untouched — the refusal is the entire safety story while the drift tooling
// (Plan B) does not exist yet.
func CatalogSync(url string) (CatalogSyncReport, error) {
	if strings.TrimSpace(url) == "" {
		url = defaultCatalogSyncURL
	}

	cachePath, err := catalogCachePath()
	if err != nil {
		return CatalogSyncReport{URL: url}, err
	}

	var etag string
	if b, rerr := os.ReadFile(cachePath + ".etag"); rerr == nil {
		etag = strings.TrimSpace(string(b))
	}

	body, newEtag, fromCache, err := fetchCatalogBody(url, etag, cachePath)
	if err != nil {
		return CatalogSyncReport{URL: url}, err
	}

	// Byte-identical to what we already hold: nothing to validate, nothing to
	// write. (The ETag path usually catches this; file:// has no ETag.)
	if prev, rerr := os.ReadFile(cachePath); rerr == nil && sameBytes(prev, body) {
		f, _ := parseModelsDevCatalog(body)
		pn, mn := f.counts()
		return CatalogSyncReport{URL: url, Providers: pn, Models: mn, Unchanged: true}, nil
	}

	// The contract check runs on the RAW body and before the parse, because a
	// field retyped upstream (limit.context arriving as a string) fails
	// json.Unmarshal outright — and the only thing that can then name the
	// FIELD is the raw tree. Reporting "not valid JSON" there would refuse the
	// payload correctly but tell a human nothing about what moved.
	if fails := validateModelsDevContractRaw(body); len(fails) > 0 {
		lines := make([]string, 0, len(fails))
		for i, fl := range fails {
			if i == 5 {
				lines = append(lines, fmt.Sprintf("... and %d more", len(fails)-i))
				break
			}
			lines = append(lines, fl.String())
		}
		return CatalogSyncReport{URL: url, Refused: true},
			fmt.Errorf("refused: %s does not match the fields oaica reads:\n  %s\n(cached catalog left in place)",
				url, strings.Join(lines, "\n  "))
	}

	// Backstop for a shape the contract does not constrain (a field we read but
	// do not validate, or a nested type the raw walk does not reach): an
	// unreadable body is never cached.
	f, perr := parseModelsDevCatalog(body)
	if perr != nil {
		return CatalogSyncReport{URL: url, Refused: true},
			fmt.Errorf("refused: %s is not readable as the models.dev catalog: %w (cached catalog left in place)", url, perr)
	}

	if !fromCache {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
			return CatalogSyncReport{URL: url}, err
		}
		if err := os.WriteFile(cachePath, body, 0o600); err != nil {
			return CatalogSyncReport{URL: url}, err
		}
		if newEtag != "" {
			_ = os.WriteFile(cachePath+".etag", []byte(newEtag), 0o600)
		}
	}

	pn, mn := f.counts()
	return CatalogSyncReport{URL: url, Providers: pn, Models: mn, FromCache: fromCache}, nil
}

// loadModelsDevCatalog returns the cached catalog and whether one exists. It
// never fetches: the picker calls this on every build, offline or not.
func loadModelsDevCatalog() (modelsDevFile, bool) {
	path, err := catalogCachePath()
	if err != nil {
		return modelsDevFile{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return modelsDevFile{}, false
	}
	f, err := parseModelsDevCatalog(b)
	if err != nil {
		return modelsDevFile{}, false
	}
	return f, true
}

// catalogAgeDays reports how stale the cached payload is, or 0 when it is
// missing or unreadable. The picker shows this past 30 days so staleness is
// visible rather than silent.
func catalogAgeDays(now time.Time) int {
	path, err := catalogCachePath()
	if err != nil {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	days := int(now.Sub(fi.ModTime()).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

func sameBytes(a, b []byte) bool {
	ha, hb := sha256.Sum256(a), sha256.Sum256(b)
	return hex.EncodeToString(ha[:]) == hex.EncodeToString(hb[:])
}

// fetchCatalogBody is the generic ETag fetch-and-cache shared with
// provider_sync.go. file:// reads the path directly and carries no ETag;
// a transport error falls back to whatever is already cached.
func fetchCatalogBody(url, etag, cachePath string) (body []byte, newEtag string, fromCache bool, err error) {
	if strings.HasPrefix(url, "file://") {
		b, rerr := os.ReadFile(strings.TrimPrefix(url, "file://"))
		return b, "", false, rerr
	}

	// A --url may carry a mirror's credential as userinfo
	// (https://KEY@mirror/api.json) — that is how a private mirror
	// authenticates. The request keeps it; nothing printed or wrapped may:
	// every message below uses the redacted form (redact.go, and the same rule
	// provider_sync.go states for the identical input).
	display := redactBaseURL(url)
	req, err := NewRedactedRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		if b, rerr := os.ReadFile(cachePath); rerr == nil {
			return b, etag, true, nil
		}
		return nil, "", false, fmt.Errorf("couldn't reach %s: %w", display, redactErr(err))
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		b, rerr := os.ReadFile(cachePath)
		if rerr != nil {
			return nil, "", false, fmt.Errorf("%s returned 304 but no cache exists", display)
		}
		return b, etag, true, nil
	case resp.StatusCode != http.StatusOK:
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", false, fmt.Errorf("%s: HTTP %d: %s", display, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	// 16 MiB cap: the live payload is ~5 MB, and the cap exists so a hostile or
	// broken response cannot exhaust memory.
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", false, err
	}
	return b, resp.Header.Get("ETag"), false, nil
}
