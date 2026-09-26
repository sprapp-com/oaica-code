package launch

// passthrough_managed_flags.go — the model flag a launch sets itself, refused
// when the user passes it through as well (2026-09-27 audit, round 22).
//
// oaica resolves the model, gates it, readies it and configures the provider
// for it, then hands the child its own -m/--model. Passing a second one through
// in the trailing arguments gives the child two contradictory values, and the
// child's parser usually takes the last — so the model that was checked is not
// the model that runs. kimi has refused these since round 20; copilot and
// poolside forwarded them.

import (
	"fmt"
	"strings"
)

// refuseManagedModelFlag returns an error naming the first argument in args
// that names the model flag oaica sets for this integration.
func refuseManagedModelFlag(integration string, args []string) error {
	for _, arg := range args {
		switch {
		case arg == "--model", strings.HasPrefix(arg, "--model="),
			arg == "-m", strings.HasPrefix(arg, "-m="):
			return fmt.Errorf("conflicting extra argument %q: oaica launch %s sets the model flag itself (-m/--model)", arg, integration)
		}
	}
	return nil
}
