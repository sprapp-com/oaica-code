package launch

// provider_sync.go — `oaica remote sync`: pull the hosted provider
// catalog so a new provider, a new billing plan on an existing one, or an
// endpoint fix reaches every user with one command, same shape as
// model_sync.go's `oaica model sync`.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

const defaultProviderSyncURL = "https://raw.githubusercontent.com/sprapp-com/oaica-code/main/cmd/launch/providers/providers.json"

// ProviderSyncReport is what ProviderSync returns for the caller to print.
type ProviderSyncReport struct {
	URL       string
	Count     int
	FromCache bool
}

// ProviderSync fetches the catalog at url (empty = defaultProviderSyncURL)
// and writes it to ~/.oaica/cache/providers/providers.json, where
// providerCatalog() picks it up as an override layer on top of the
// embedded default. ETag-cached; offline falls back to the last good copy.
func ProviderSync(url string) (ProviderSyncReport, error) {
	if strings.TrimSpace(url) == "" {
		url = defaultProviderSyncURL
	}
	// display is the URL as the caller may print it. The request must keep any
	// mirror credential in the userinfo (`--url https://KEY@mirror/…` is how a
	// private mirror authenticates), but the report line `oaica provider sync`
	// echoes — and every error string — must not, exactly as ModelSync's report
	// already promises for the identical input (2026-09-26 audit).
	display := redactBaseURL(url)

	cachePath, err := providerCatalogCachePath()
	if err != nil {
		return ProviderSyncReport{URL: display}, err
	}

	etag := loadCatalogETag(cachePath+".etag", display, defaultProviderSyncURL)

	body, newEtag, fromCache, err := fetchProviderCatalogBody(url, etag)
	if err != nil {
		return ProviderSyncReport{URL: display}, err
	}

	// Parsed before it is written, and a body that does not parse is never
	// written: the synced cache overrides the embedded default, so caching one
	// bad response (a captive-portal login page, a truncated transfer, a
	// proxy's HTML error) emptied the provider catalogue for every later run
	// with the count reported as 0 (2026-09-26 audit, third round).
	f, ferr := parseProviderCatalogFileChecked(body)
	if ferr != nil {
		if fromCache {
			return ProviderSyncReport{URL: display}, fmt.Errorf("the cached provider catalog at %s is not readable as one (%v) — remove that file and run this again while online", cachePath, ferr)
		}
		return ProviderSyncReport{URL: display}, fmt.Errorf("%s returned a body that is not a provider catalog (%v) — the cached copy was left untouched", display, ferr)
	}
	if !fromCache {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
			return ProviderSyncReport{URL: display}, err
		}
		if err := fileutil.WriteFileAtomic(cachePath, body, 0o600); err != nil {
			return ProviderSyncReport{URL: display}, err
		}
		saveCatalogETag(cachePath+".etag", display, newEtag)
	}

	return ProviderSyncReport{URL: display, Count: len(f.Providers), FromCache: fromCache}, nil
}

func fetchProviderCatalogBody(url, etag string) (body []byte, newEtag string, fromCache bool, err error) {
	if strings.HasPrefix(url, "file://") {
		b, rerr := os.ReadFile(strings.TrimPrefix(url, "file://"))
		return b, "", false, rerr
	}

	// The --url may carry the mirror's credential as userinfo
	// (https://KEY@mirror/catalog.json). The request keeps it — that is how
	// the mirror authenticates — but nothing written down or printed may:
	// every message below, and the request builder's own parse error
	// (NewRedactedRequest), use the redacted form. docs/ENTERPRISE.md names
	// the catalog caches as a place a credential must never reach.
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
		if cachePath, perr := providerCatalogCachePath(); perr == nil {
			if b, rerr := os.ReadFile(cachePath); rerr == nil {
				return b, etag, true, nil
			}
		}
		return nil, "", false, fmt.Errorf("couldn't reach %s: %w", display, redactErr(err))
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		cachePath, perr := providerCatalogCachePath()
		if perr != nil {
			return nil, "", false, perr
		}
		b, rerr := os.ReadFile(cachePath)
		if rerr != nil {
			return nil, "", false, fmt.Errorf("%s returned 304 but no cache exists", display)
		}
		return b, etag, true, nil
	case resp.StatusCode != http.StatusOK:
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", false, fmt.Errorf("%s: HTTP %d: %s", display, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", false, err
	}
	return b, resp.Header.Get("ETag"), false, nil
}

func parseProviderCatalogFile(b []byte) providerCatalogFile {
	f, _ := parseProviderCatalogFileChecked(b)
	return f
}

// parseProviderCatalogFileChecked is parseProviderCatalogFile for the sync
// path, which must not cache a body it could not read.
// parseProviderCatalogFileChecked decodes a catalog body, refusing a document
// that is not shaped like one.
//
// "It parses as JSON" is not the question. `{"message":"Not Found"}` — a proxy
// or CDN answering 200 for a path that does not exist — decodes cleanly into
// "this catalog has zero providers", as do `{}`, `null` and any unrelated
// object. Each was written over the previous cache and reported as
// `synced 0 provider(s) ... (fresh)` with exit 0; because the cache wins over
// the embedded default, every provider it does not mention then disappeared
// from `oaica remote list` and the picker, and the next run offline re-read the
// garbage and called it a success again (2026-09-26 audit, sixth round).
//
// A document with no "providers" MEMBER is not an empty catalog, and
// "providers": null is not an empty LIST. This is parseModelCatalog's rule
// (model_sync.go), which its two siblings never got.
func parseProviderCatalogFileChecked(b []byte) (providerCatalogFile, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil {
		return providerCatalogFile{}, err
	}
	if _, ok := members["providers"]; !ok {
		return providerCatalogFile{}, fmt.Errorf(`the document has no "providers" member, so it is not a provider catalog (refusing to read it as an empty one)`)
	}
	var f providerCatalogFile
	if err := json.Unmarshal(b, &f); err != nil {
		return providerCatalogFile{}, err
	}
	if f.Providers == nil {
		return providerCatalogFile{}, fmt.Errorf(`"providers" is null, not a provider list`)
	}
	return f, nil
}
