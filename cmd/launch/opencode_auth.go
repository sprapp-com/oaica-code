package launch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

// opencodeAuthEntry is one provider credential in opencode's auth.json
// (~/.local/share/opencode/auth.json — XDG_DATA_HOME/opencode on Linux,
// same tree macOS/Windows use via os.UserHomeDir()/.local/share). Verified
// against opencode's real on-disk format: a flat map of provider id to
// {"type":"api","key":"..."}.
type opencodeAuthEntry struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// OpencodeAuthPath returns the path to opencode's auth.json.
func OpencodeAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "opencode", "auth.json"), nil
}

func readOpencodeAuth(path string) (map[string]opencodeAuthEntry, error) {
	auth := make(map[string]opencodeAuthEntry)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return auth, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return nil, fmt.Errorf("failed to parse opencode auth.json: %w", err)
	}
	return auth, nil
}

// OpencodeKnownProviders lists provider ids already present in opencode's
// auth.json, sorted, for offering as signin choices alongside "add a new one".
func OpencodeKnownProviders() []string {
	path, err := OpencodeAuthPath()
	if err != nil {
		return nil
	}
	auth, err := readOpencodeAuth(path)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(auth))
	for name := range auth {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// SaveOpencodeAPIKey writes/overwrites one provider's API key in opencode's
// auth.json, preserving every other provider entry untouched.
func SaveOpencodeAPIKey(provider, key string) error {
	path, err := OpencodeAuthPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	auth, err := readOpencodeAuth(path)
	if err != nil {
		return err
	}
	auth[provider] = opencodeAuthEntry{Type: "api", Key: key}

	data, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteWithBackup(path, data, "opencode")
}
