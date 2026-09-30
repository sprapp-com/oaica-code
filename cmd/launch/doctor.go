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
//
// --report adds the environment section (version, platform, config paths and
// their permissions, which credentials are set) and refuses to print if any
// secret value would appear in the output. See doctor_report.go.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// doctorProbeTimeout: same reasoning as context_window_remote.go's probe —
// a doctor run must stay fast.
const doctorProbeTimeout = 2 * time.Second

// redactBaseURL lives in redact.go, with the rest of the credential handling.

// probeRemote asks a remote's /models. Read-only, no completions.
func probeRemote(r userRemote) string {
	ctx, cancel := context.WithTimeout(context.Background(), doctorProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.modelsURL(), nil)
	if err != nil {
		// redactErr: net/http echoes the request URL — which can carry the
		// key as userinfo — in these messages.
		return "FAIL " + redactErr(err).Error()
	}
	if k := r.key(); k != "" {
		if r.Descriptor().Wire == "anthropic" {
			// Anthropic-wire remotes (zai-coding-plan, minimax-coding-plan,
			// a raw api.anthropic.com entry) authenticate with x-api-key +
			// anthropic-version, not a Bearer header — the same branch
			// fetchRemoteModels takes. Sending a Bearer to them gets a 401, so
			// doctor reported a healthy remote as FAIL and (since 2026-09-26)
			// exited 1 on it.
			req.Header.Set("x-api-key", k)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("Authorization", "Bearer "+k)
		}
	}
	resp, err := credentialSafeDefaultClient().Do(req)
	if err != nil {
		return "FAIL " + redactErr(err).Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		// Some Anthropic-wire vendors answer a DEAD key with 200 and the error in the body (z.ai: {"code":401,
		// "success":false}) — "ok" on that sent a user into launch 401s with a doctor's blessing (2026-09-30
		// audit, round 141, F141-B). Bounded sniff, only on the anthropic wire.
		if r.Descriptor().Wire == "anthropic" {
			b := make([]byte, 4096)
			n, _ := io.ReadFull(resp.Body, b)
			s := strings.ToLower(string(b[:n]))
			if strings.Contains(s, `"success":false`) || strings.Contains(s, `"code":401`) || strings.Contains(s, "token expired") || strings.Contains(s, "invalid api key") {
				return "FAIL the remote answered 200 with an authentication error in the body (check the key)"
			}
		}
		return "ok"
	}
	return fmt.Sprintf("FAIL http %d", resp.StatusCode)
}

// doctorChecks prints the routing checks and reports whether any failed.
// It writes to w so --report can render the same checks into a buffer and
// scan that buffer before it is printed.
func doctorChecks(w io.Writer) bool {
	failed := false

	fmt.Fprintln(w, "remotes (~/.oaica/remotes.json):")
	remotes, err := loadUserRemotes()
	if err != nil {
		fmt.Fprintf(w, "  ! cannot load: %v\n", err)
		failed = true
	}
	for _, r := range remotes {
		status := probeRemote(r)
		// A probe that failed is a failure of the CHECK, not just a
		// line of output: doctor promised exit 1 for scripts (header
		// above, docs/CLAUDE_TIERS.md, site/index.html) but only a
		// malformed remotes.json ever set this, so an unreachable
		// remote or a 401 printed FAIL, then "all checks passed",
		// exit 0 — a cron job that greps the exit code saw green
		// (2026-09-26 audit).
		if strings.HasPrefix(status, "FAIL") {
			failed = true
		}
		policy := r.RoutePolicy
		if _, perr := parseRoutePolicy(policy); perr != nil {
			status = "INVALID route_policy " + printableName(policy)
			failed = true
		}
		suffix := ""
		if policy != "" {
			// Quoted like a name: this value comes from the same hand-editable
			// file and is printed on the same line (2026-09-26 audit, tenth
			// round).
			suffix = "  (route_policy: " + printableName(policy) + ")"
		}
		// The RESOLVED endpoint, not the configured prefix: the probe above
		// fetched openAIBase()+"/models", so printing BaseURL showed a URL one
		// version segment short of the one that was actually tried (a v4 row
		// printed ".../api/paas" beside an "ok" earned at ".../api/paas/v4"),
		// and the printed link 404s when followed by hand — which is the one
		// thing this line is for.
		// Credential-embedded URLs (https://key@host/...) print
		// redacted — doctor output lands in terminals and CI logs.
		fmt.Fprintf(w, "  %-16s %-40s wire=%-8s %s%s\n", printableName(r.Name), printableName(redactBaseURL(r.openAIBase())), printableName(r.Wire), status, suffix)
	}
	if len(remotes) == 0 {
		fmt.Fprintln(w, "  (none configured)")
	}

	fmt.Fprintln(w, "\nlocal daemon (OLLAMA_HOST):")
	_, reachable := daemonHasModelLive("")
	// daemonHasModelLive takes a model; "" probes reachability only
	// (a daemon that answers /api/show with 200/404 is up either
	// way). What doctor cares about is reachable, not contents.
	if reachable {
		fmt.Fprintln(w, "  reachable")
	} else {
		fmt.Fprintln(w, "  unreachable (fine — launches that need it will say so)")
	}

	fmt.Fprintf(w, "\ndefault route policy (per launch: --route-policy flag > the primary remote's route_policy > %s)\n",
		RouteLocalFirst)
	fmt.Fprintln(w, "policies: local-first | remote-first | auto | local-only | remote-only | weighted")
	fmt.Fprintln(w, "oversize: per-launch --oversize <model> (no remotes.json default yet)")

	return failed
}

// DoctorCmd builds the `oaica doctor` command. Read-only (GET /models).
func DoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check launch routing: remotes reachability, route policies, daemon leg",
		RunE: func(cmd *cobra.Command, args []string) error {
			if report, _ := cmd.Flags().GetBool("report"); report {
				return runDoctorReport(os.Stdout)
			}
			failed := doctorChecks(os.Stdout)
			if failed {
				return fmt.Errorf("doctor found failures (exit 1 for scripts)")
			}
			fmt.Println("\nall checks passed")
			return nil
		},
	}
	cmd.Flags().Bool("report", false,
		"Print a redacted support bundle (version, platform, config paths and permissions, credential presence) instead of the plain checks")
	return cmd
}
