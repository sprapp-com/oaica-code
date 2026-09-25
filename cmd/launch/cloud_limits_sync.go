package launch

// cloud_limits_sync.go — `oaica model cloud-limits sync`: pull the hosted
// cloud-alias limits catalog, same ETag/offline-fallback shape as
// provider_sync.go and model_sync.go.

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

const defaultCloudLimitsSyncURL = "https://raw.githubusercontent.com/sprapp-com/oaica-code/main/cmd/launch/cloud_limits/cloud_limits.json"

// CloudLimitsSyncReport is what CloudLimitsSync returns for the caller to print.
type CloudLimitsSyncReport struct {
	URL       string
	Count     int
	FromCache bool
}

// CloudLimitsSync fetches the catalog at url (empty = default) and writes
// it to ~/.oaica/cache/cloud_limits/cloud_limits.json, where
// cloudLimitsFromCatalog() picks it up as an override on the embedded
// default.
func CloudLimitsSync(url string) (CloudLimitsSyncReport, error) {
	if strings.TrimSpace(url) == "" {
		url = defaultCloudLimitsSyncURL
	}

	cachePath, err := cloudLimitsCatalogCachePath()
	if err != nil {
		return CloudLimitsSyncReport{URL: url}, err
	}

	var etag string
	if b, rerr := os.ReadFile(cachePath + ".etag"); rerr == nil {
		etag = strings.TrimSpace(string(b))
	}

	body, newEtag, fromCache, err := fetchCloudLimitsBody(url, etag, cachePath)
	if err != nil {
		return CloudLimitsSyncReport{URL: url}, err
	}

	var f cloudLimitsCatalogFile
	_ = json.Unmarshal(body, &f)

	if !fromCache {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
			return CloudLimitsSyncReport{URL: url}, err
		}
		if err := os.WriteFile(cachePath, body, 0o600); err != nil {
			return CloudLimitsSyncReport{URL: url}, err
		}
		if newEtag != "" {
			_ = os.WriteFile(cachePath+".etag", []byte(newEtag), 0o600)
		}
	}

	return CloudLimitsSyncReport{URL: url, Count: len(f.Limits), FromCache: fromCache}, nil
}

func fetchCloudLimitsBody(url, etag, cachePath string) (body []byte, newEtag string, fromCache bool, err error) {
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
		if b, rerr := os.ReadFile(cachePath); rerr == nil {
			return b, etag, true, nil
		}
		return nil, "", false, fmt.Errorf("couldn't reach %s: %w", url, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
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
