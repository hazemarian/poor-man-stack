package cli

import "fmt"

// errNotImplemented is the placeholder error returned by skeleton commands.
// Each command names the phase that owns its real implementation so the
// hand-off path is obvious from a single shell invocation.
func errNotImplemented(cmdName, phase string) error {
	return fmt.Errorf("%s is not implemented yet (lands in %s — see docs/refactor-plan.md)", cmdName, phase)
}
