package launch

// model_manifest_cli.go — `oaica model add/list/rm/show` handlers, thin
// wrappers over model_manifest.go's load/save so cmd/cmd.go's cobra
// wiring stays a one-line RunE per verb (same shape as PullHandler/
// ServeHandler in cmd/oaica_pull_serve.go).

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ModelAddOptions is the parsed form of `oaica model add`'s flags.
type ModelAddOptions struct {
	ID                     string
	Engine                 string
	Arch                   string
	Quant                  string
	ContextWindow          int
	DefaultMaxOutputTokens int
	GPUMemGB               float64
	RAMGB                  float64
	LaunchFlags            []string
	ModelPath              string
	Notes                  string
}

// ModelAdd creates or replaces a manifest entry. Returns the entry that
// was written so callers can print a confirmation.
func ModelAdd(opts ModelAddOptions) (ModelManifestEntry, error) {
	e := ModelManifestEntry{
		ID:                     strings.TrimSpace(opts.ID),
		Engine:                 ModelEngine(strings.TrimSpace(opts.Engine)),
		Arch:                   opts.Arch,
		Quant:                  opts.Quant,
		ContextWindow:          opts.ContextWindow,
		DefaultMaxOutputTokens: opts.DefaultMaxOutputTokens,
		GPUMemGB:               opts.GPUMemGB,
		RAMGB:                  opts.RAMGB,
		LaunchFlags:            opts.LaunchFlags,
		ModelPath:              opts.ModelPath,
		Notes:                  opts.Notes,
	}
	if err := validateModelManifestEntry(e); err != nil {
		return ModelManifestEntry{}, err
	}
	m, err := loadModelManifest()
	if err != nil {
		return ModelManifestEntry{}, err
	}
	m.Put(e)
	if err := m.save(); err != nil {
		return ModelManifestEntry{}, err
	}
	return e, nil
}

// ModelRemove deletes a manifest entry by ID. Returns whether it existed.
func ModelRemove(id string) (bool, error) {
	m, err := loadModelManifest()
	if err != nil {
		return false, err
	}
	existed := m.Remove(id)
	if existed {
		if err := m.save(); err != nil {
			return false, err
		}
	}
	return existed, nil
}

// ModelShow returns one entry, or an error naming the manifest path if not found.
func ModelShow(id string) (ModelManifestEntry, error) {
	m, err := loadModelManifest()
	if err != nil {
		return ModelManifestEntry{}, err
	}
	e, ok := m.Get(id)
	if !ok {
		path, _ := modelManifestPath()
		return ModelManifestEntry{}, fmt.Errorf("no manifest entry for %q in %s — add one with `oaica model add`", id, path)
	}
	return e, nil
}

// WriteModelList prints every manifest entry as an aligned table to w.
func WriteModelList(w io.Writer) error {
	m, err := loadModelManifest()
	if err != nil {
		return err
	}
	ids := m.SortedIDs()
	if len(ids) == 0 {
		path, _ := modelManifestPath()
		fmt.Fprintf(w, "No models in the manifest (%s).\nAdd one with `oaica model add <id> --engine vllm --context-window 262144 ...`\n", path)
		return nil
	}
	fmt.Fprintf(w, "%-24s %-13s %-10s %-12s %s\n", "ID", "ENGINE", "QUANT", "CONTEXT", "NOTES")
	for _, id := range ids {
		e := m.Models[id]
		ctx := "-"
		if e.ContextWindow > 0 {
			ctx = strconv.Itoa(e.ContextWindow)
		}
		notes := e.Notes
		if len(notes) > 60 {
			notes = notes[:57] + "..."
		}
		fmt.Fprintf(w, "%-24s %-13s %-10s %-12s %s\n", e.ID, e.Engine, orDash(e.Quant), ctx, notes)
	}
	return nil
}

// WriteModelShow prints one entry's full detail to w.
func WriteModelShow(w io.Writer, id string) error {
	e, err := ModelShow(id)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "id:               %s\n", e.ID)
	fmt.Fprintf(w, "engine:           %s\n", e.Engine)
	fmt.Fprintf(w, "arch:             %s\n", orDash(e.Arch))
	fmt.Fprintf(w, "quant:            %s\n", orDash(e.Quant))
	fmt.Fprintf(w, "context_window:   %s\n", intOrDash(e.ContextWindow))
	fmt.Fprintf(w, "max_output_tokens:%s\n", intOrDash(e.DefaultMaxOutputTokens))
	fmt.Fprintf(w, "gpu_mem_gb:       %s\n", floatOrDash(e.GPUMemGB))
	fmt.Fprintf(w, "ram_gb:           %s\n", floatOrDash(e.RAMGB))
	fmt.Fprintf(w, "model_path:       %s\n", orDash(e.ModelPath))
	if len(e.LaunchFlags) > 0 {
		fmt.Fprintf(w, "launch_flags:     %s\n", strings.Join(e.LaunchFlags, " "))
	}
	if e.Notes != "" {
		fmt.Fprintf(w, "notes:            %s\n", e.Notes)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func intOrDash(n int) string {
	if n <= 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

func floatOrDash(f float64) string {
	if f <= 0 {
		return "-"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
