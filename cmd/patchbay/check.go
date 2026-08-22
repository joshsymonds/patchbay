package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/joshsymonds/patchbay/internal/registry"
)

// check validates a registry file the way the gateway does, without asking a
// gateway. It is the one subcommand that needs no running patchbay: an operator
// editing the registry wants to know the file is good before anything reloads
// it, and after a bad edit there may be nothing running to ask.
func (c cli) check(args []string) int {
	flags := c.flagSet(cmdCheck)
	path := flags.String(
		"registry",
		"",
		"the registry file to validate (default: $"+envRegistry+", or the path the gateway would use)",
	)

	if err := flags.Parse(args); err != nil {
		return exitUsage
	}

	if flags.NArg() != 0 {
		c.usage(c.stderr)

		return exitUsage
	}

	target, err := c.registryPath(*path)
	if err != nil {
		c.fail(err)

		return exitFailure
	}

	if _, statErr := os.Stat(filepath.Clean(target)); statErr != nil {
		c.fail(fmt.Errorf("registry file: %w", statErr))

		return exitFailure
	}

	reg, regErr := registry.New(target)
	if regErr != nil {
		c.fail(regErr)
		return exitFailure
	}

	return c.reportCheck(target, reg)
}

// reportCheck renders the verdict for a registry that has already been opened.
//
// Opening the file and summarizing it are two reads of the same path, and a
// home-manager generation can swap it between them. Summarized through a second
// read of its own, a replacement the gateway will refuse is reported as the
// empty document it is not — "0 seats, 0 contexts", exit 0 — because a
// fail-closed snapshot answers with no summary at all and nothing here asked
// whether it was valid. So the summary comes from one atomic inspection: a
// registry that no longer validates is refused with the field and the value
// behind it, and only an inspection that came back valid renders counts.
func (c cli) reportCheck(target string, reg *registry.Registry) int {
	state, inspectErr := reg.Inspect()
	if inspectErr != nil {
		c.fail(inspectErr)

		return exitFailure
	}
	if !state.Valid {
		c.fail(fmt.Errorf("registry %s: %s", quoted(target), state.Error))

		return exitFailure
	}

	names := make([]string, 0, len(state.Contexts))
	bindings := 0
	for name, ctx := range state.Contexts {
		names = append(names, name)
		bindings += len(ctx.Models)
	}
	slices.Sort(names)
	fmt.Fprintf(
		c.stdout,
		"OK %s: %s, %s (%s), %s\n",
		quoted(target),
		count(
			len(state.Seats),
			"seat",
			"seats",
		),
		count(len(names), "context", "contexts"),
		strings.Join(names, ", "),
		count(bindings, "binding", "bindings"),
	)

	return exitOK
}

// count renders a quantity with the right singular or plural noun.
func count(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}

	return fmt.Sprintf("%d %s", n, plural)
}

// registryPath resolves which file to validate: the one named on the command
// line, or the one the gateway itself would read — resolved by exactly the code
// the gateway resolves it with, so check can never validate a different file
// from the one that will be served.
func (c cli) registryPath(named string) (string, error) {
	if named != "" {
		return named, nil
	}

	return resolveRegistryPath(c.lookupEnv)
}
