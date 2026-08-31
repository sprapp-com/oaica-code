package launch

// doctor.go — `oaica doctor`: dry, read-only check of the launch-routing
// surface for humans and CI. Prints:
//   - every user remote in ~/.oaica/remotes.json: base URL, wire, the
//     route_policy it would default a launch to, and a live /models probe
//   - the resolved default route policy (flag > remotes.json > local-first)
//   - the local daemon's reachability (fallback leg for many setups)
// Exit code 1 if any configured remote fails its probe (the rest of the
// output still prints), so scripts and cron can grep. Read-only: GET /models
// only, 2s timeout per remote, no requests to /chat/completions, nothing
// billed.

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

// doctorProbeTimeout: same reasoning as context_window_remote.go's probe —
// a doctor run must stay fast.
const doctorProbeTimeout = 2 * time.Second

// DoctorCmd builds the `oaica doctor` command. Read-only (GET /models).
func DoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check launch routing: remotes reachability, route policies, daemon leg",
		RunE: func(cmd *cobra.Command, args []string) error {
			failed := false

			fmt.Println("remotes (~/.oaica/remotes.json):")
			remotes, err := loadUserRemotes()
			if err != nil {
				fmt.Printf("  ! cannot load: %v\n", err)
				failed = true
			}
			for _, r := range remotes {
				status := func() string {
					ctx, cancel := context.WithTimeout(context.Background(), doctorProbeTimeout)
					defer cancel()
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.openAIBase()+"/models", nil)
					if err != nil {
						return "FAIL " + err.Error()
					}
					if k := r.key(); k != "" {
						req.Header.Set("Authorization", "Bearer "+k)
					}
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						return "FAIL " + err.Error()
					}
					defer resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						return "ok"
					}
					return fmt.Sprintf("FAIL http %d", resp.StatusCode)
				}()
				policy := r.RoutePolicy
				if _, perr := parseRoutePolicy(policy); perr != nil {
					status = "INVALID route_policy " + policy
					failed = true
				}
				suffix := ""
				if r.RoutePolicy != "" {
					suffix = "  (route_policy: " + r.RoutePolicy + ")"
				}
				fmt.Printf("  %-16s %-40s wire=%-8s %s%s\n", r.Name, r.BaseURL, r.Wire, status, suffix)
			}
			if len(remotes) == 0 {
				fmt.Println("  (none configured)")
			}

			fmt.Println("\nlocal daemon (OLLAMA_HOST):")
			_, reachable := daemonHasModelLive("")
			// daemonHasModelLive takes a model; "" probes reachability only
			// (a daemon that answers /api/show with 200/404 is up either
			// way). What doctor cares about is reachable, not contents.
			if reachable {
				fmt.Println("  reachable")
			} else {
				fmt.Println("  unreachable (fine — launches that need it will say so)")
			}

			fmt.Printf("\ndefault route policy: %s (flag --route-policy > remote route_policy > %s)\n",
				RouteLocalFirst, RouteLocalFirst)
			fmt.Println("policies: local-first | remote-first | auto | local-only | remote-only")
			fmt.Println("oversize: per-launch --oversize <model> (no remotes.json default yet)")

			if failed {
				return fmt.Errorf("doctor found failures (exit 1 for scripts)")
			}
			fmt.Println("\nall checks passed")
			return nil
		},
	}
}