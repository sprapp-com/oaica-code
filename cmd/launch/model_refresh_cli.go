package launch

// model_refresh_cli.go — `oaica model refresh`: forces a fresh, live probe
// of every model source (local daemon, ~/.oaica/remotes.json, OAICA
// router) and prints what's currently discoverable, right now, without
// waiting for anyone else to fix or update anything.
//
// Every `oaica launch`/`oaica model` invocation already re-probes live —
// modelInventory has no cross-process cache, so a plain `ollama pull` or a
// remotes.json edit is visible on the very next launch with zero action
// needed (see docs/MODELS_AND_PLANS.md's "Discovery, drift-safety, and
// manual refresh" section). This command exists for the narrower case:
// confirming RIGHT NOW that a model you just pulled/added is actually
// discoverable, without launching Claude Code just to see the picker list.

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/ollama/ollama/api"
)

// RefreshedModelSources is the result of one live discovery pass, grouped
// the same way the picker groups the Local/Remote/Recommended sections
// (see cmd/tui/selector.go) so "what would the picker show" and "what did
// refresh find" always describe the same thing.
type RefreshedModelSources struct {
	Local  []string // this box's own daemon (ollama list / pulled + :cloud)
	Remote []string // ~/.oaica/remotes.json entries, "<remote>/<id>"
	Router []string // OAICA router catalog (api.oaica.com)
	// Errs collects non-fatal source failures (e.g. router unreachable) —
	// refresh degrades per-source, same as the picker: one dead source
	// never empties the others.
	Errs []error
}

// RefreshModelSources does one live discovery pass across every source.
// Bounded by timeout so a hung box can't hang the CLI forever.
func RefreshModelSources(timeout time.Duration) (RefreshedModelSources, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	apiClient, err := api.ClientFromEnvironment()
	if err != nil {
		return RefreshedModelSources{}, fmt.Errorf("local daemon client: %w", err)
	}
	inv := newModelInventory(apiClient)
	models, loadErr := inv.Refresh(ctx)

	var out RefreshedModelSources
	if loadErr != nil {
		out.Errs = append(out.Errs, loadErr)
	}
	for _, m := range models {
		if m.Name == "" {
			continue
		}
		if m.Remote {
			out.Remote = append(out.Remote, m.Name)
		} else {
			out.Local = append(out.Local, m.Name)
		}
	}

	routerEntries, routerErr := oaicaFetchCloudModelEntries()
	if routerErr != nil {
		out.Errs = append(out.Errs, fmt.Errorf("router: %w", routerErr))
	}
	for _, e := range routerEntries {
		out.Router = append(out.Router, e.ID)
	}

	sort.Strings(out.Local)
	sort.Strings(out.Remote)
	sort.Strings(out.Router)
	return out, nil
}

// WriteRefreshedModelSources prints RefreshModelSources' result in the same
// section order the picker renders: Local, Remote, Router (Recommended/
// More in the picker are both sourced from Router — see
// requestRecommendations).
func WriteRefreshedModelSources(w io.Writer, r RefreshedModelSources) {
	printSection := func(title string, names []string) {
		if len(names) == 0 {
			return
		}
		fmt.Fprintf(w, "%s (%d):\n", title, len(names))
		for _, n := range names {
			fmt.Fprintf(w, "  %s\n", n)
		}
	}
	printSection("Local", r.Local)
	printSection("Remote", r.Remote)
	printSection("Router", r.Router)
	if len(r.Local) == 0 && len(r.Remote) == 0 && len(r.Router) == 0 {
		fmt.Fprintln(w, "No models discovered from any source.")
	}
	for _, e := range r.Errs {
		fmt.Fprintf(w, "warning: %v\n", e)
	}
}
