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

	cachePath, err := providerCatalogCachePath()
	if err != nil {
		return ProviderSyncReport{URL: url}, err
	}

	var etag string
	if b, rerr := os.ReadFile(cachePath + ".etag"); rerr == nil {
		etag = strings.TrimSpace(string(b))
	}

	body, newEtag, fromCache, err := fetchProviderCatalogBody(url, etag)
	if err != nil {
		return ProviderSyncReport{URL: url}, err
	}

	f := parseProviderCatalogFile(body)
	if !fromCache {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
			return ProviderSyncReport{URL: url}, err
		}
		if err := os.WriteFile(cachePath, body, 0o600); err != nil {
			return ProviderSyncReport{URL: url}, err
		}
		if newEtag != "" {
			_ = os.WriteFile(cachePath+".etag", []byte(newEtag), 0o600)
		}
	}

	return ProviderSyncReport{URL: url, Count: len(f.Providers), FromCache: fromCache}, nil
}

func fetchProviderCatalogBody(url, etag string) (body []byte, newEtag string, fromCache bool, err error) {
	if strings.HasPrefix(url, "file://") {
		b, rerr := os.ReadFile(strings.TrimPrefix(url, "file://"))
		return b, "", false, rerr
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
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
		return nil, "", false, fmt.Errorf("couldn't reach %s: %w", url, err)
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
			return nil, "", false, fmt.Errorf("%s returned 304 but no cache exists", url)
		}
		return b, etag, true, nil
	case resp.StatusCode != http.StatusOK:
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", false, fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", false, err
	}
	return b, resp.Header.Get("ETag"), false, nil
}

func parseProviderCatalogFile(b []byte) providerCatalogFile {
	var f providerCatalogFile
	_ = json.Unmarshal(b, &f)
	return f
}
