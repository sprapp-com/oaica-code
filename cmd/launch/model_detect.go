package launch

// model_detect.go — local model-file detection, the Ollama-scans-its-
// models-directory behavior for our formats: walk the user's model
// directories for *.pqm (prism-engine) and *.gguf (llama.cpp) files and
// register anything found into ~/.oaica/models.json so `oaica model list`
// and launch-flag suggestions know about them without a manual
// `oaica model add` per file.
//
// The scan is deliberately conservative: it never overwrites config the
// user (or the catalog) already declared. An existing entry only gains a
// ModelPath if it didn't have one, and a brand-new file becomes an entry
// with just id/engine/quant/path/context-window-left-zero — the same
// "register the fact, learn the rest later" stance model_manifest.go's
// header takes.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// localScanDirs returns the directories the auto-scan walks, in priority
// order. OAICA_MODELS_DIR (colon-separated, like PATH) comes first, then
// ~/.oaica/models — the directory `oaica pull`-style fetches land in.
func localScanDirs() []string {
	var dirs []string
	if env := os.Getenv("OAICA_MODELS_DIR"); env != "" {
		for _, d := range filepath.SplitList(env) {
			if d != "" {
				dirs = append(dirs, d)
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".oaica", "models"))
	}
	return dirs
}

// localModelFileExt maps a model-file extension to the engine that
// serves it. prism-engine reads .pqm (Prism Quantized Model), llama.cpp
// reads .gguf.
func engineForExt(ext string) (ModelEngine, bool) {
	switch strings.ToLower(ext) {
	case ".pqm":
		return EnginePrism, true
	case ".gguf":
		return EngineLlamaCPP, true
	}
	return "", false
}

// quantFromFilename extracts a quantization label from common filename
// conventions (gguf: model.q4_k_m.gguf; pqm: model.pqm.bf16 / -awq-).
var quantFromFilename = regexp.MustCompile(`(?i)(?:[._-]|^)(q[2-8](?:_[0-9]+)?(?:_k(?:_(?:s|m|l|xl|xxl|xxxl))?)?|fp8|int8|nvfp4|awq|ternary(?:-1\.58bit)?)(?:[._-]|$)`)

// quantSubmatch returns capture group 1 of the quant regex — the tag
// itself, without the delimiters the pattern requires around it.
func quantSubmatch(name string) string {
	if m := quantFromFilename.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

// localScanReport is what ModelScan returns for the caller to print.
type localScanReport struct {
	Dirs     []string
	Added    []string
	Updated  []string // existing entry that gained a ModelPath
	Ignored  []string // file already registered with that path
	Invalid  []string // id: reason
}

// ModelScan walks dirs (empty = localScanDirs) and upserts every .pqm /
// .gguf file found into the manifest as Source=="local-scan".
func ModelScan(dirs []string) (localScanReport, error) {
	if len(dirs) == 0 {
		dirs = localScanDirs()
	}
	report := localScanReport{Dirs: dirs}

	m, err := loadModelManifest()
	if err != nil {
		return report, err
	}

	type found struct{ id, raw, path, ext string }
	var files []found
	seen := map[string]bool{}
	for _, dir := range dirs {
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			continue // missing dir is normal (nothing fetched yet), not an error
		}
		err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable subtree: skip, don't abort the scan
			}
			if d.IsDir() {
				return nil
			}
			ext := filepath.Ext(path)
			if _, ok := engineForExt(ext); !ok {
				return nil
			}
			abs, aerr := filepath.Abs(path)
			if aerr == nil {
				path = abs
			}
			id := sanitizeModelID(strings.TrimSuffix(filepath.Base(path), ext))
			if id == "" {
				return nil
			}
			if !seen[id] {
				seen[id] = true
				files = append(files, found{id: id, raw: filepath.Base(path), path: path, ext: ext})
			}
			return nil
		})
		if err != nil {
			return report, fmt.Errorf("scan %s: %w", dir, err)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].id < files[j].id })

	for _, f := range files {
		engine, _ := engineForExt(f.ext)
		e := ModelManifestEntry{
			ID:     f.id,
			Engine: engine,
			// Quant comes from the raw filename, not the sanitized id —
			// sanitize strips the quant tag, which is exactly what we want
			// to capture here.
			Quant:     quantSubmatch(f.raw),
			ModelPath: f.path,
			Source:    "local-scan",
		}
		if err := validateModelManifestEntry(e); err != nil {
			report.Invalid = append(report.Invalid, fmt.Sprintf("%s: %v", f.id, err))
			continue
		}
		prev, existed := m.Get(f.id)
		switch {
		case !existed:
			m.Put(e)
			report.Added = append(report.Added, f.id)
		case prev.ModelPath == "" && prev.Source != "sync":
			// known but pathless (hand-declared): fill in the path, keep config
			prev.ModelPath = f.path
			if prev.Quant == "" {
				prev.Quant = e.Quant
			}
			m.Put(prev)
			report.Updated = append(report.Updated, f.id)
		case prev.ModelPath == f.path:
			report.Ignored = append(report.Ignored, f.id)
		default:
			// same id, different path: two files claim the id. Keep the
			// existing entry; the scan never silently repoints a model.
			report.Ignored = append(report.Ignored, f.id+" (path differs: "+f.path+")")
		}
	}

	if len(report.Added) > 0 || len(report.Updated) > 0 {
		if err := m.save(); err != nil {
			return report, err
		}
	}
	return report, nil
}

// sanitizeModelID makes a filename safe as a manifest id: spaces and
// slashes to dashes, trimmed. Returns "" for nothing-left names.
func sanitizeModelID(name string) string {
	s := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			return r
		default:
			return '-'
		}
	}, strings.TrimSpace(name))
	s = strings.Trim(s, "-._")
	// Drop a trailing quant tag ("tiny.q4_k_m" → "tiny"): the quant lives
	// in the Quant field; same model in two quants must share one id so
	// path-conflict logic can keep them distinct-by-path.
	if loc := quantFromFilename.FindStringIndex(s); loc != nil && loc[1] == len(s) {
		s = strings.Trim(s[:loc[0]], "-._")
	}
	return s
}