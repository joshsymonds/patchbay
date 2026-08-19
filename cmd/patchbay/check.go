package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joshsymonds/patchbay/internal/registry"
)

// check validates a registry file the way the gateway does, without asking a
// gateway. It is the one subcommand that needs no running patchbay: an operator
// editing the registry wants to know the file is good before anything reloads
// it, and after a bad edit there may be nothing running to ask.
func (c cli) check(args []string) int {
	flags := c.flagSet(cmdCheck)
	path := flags.String("registry", "",
		"the registry file to validate (default: $"+envRegistry+", or the path the gateway would use)")

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

	reg := registry.New(target)
	if regErr := reg.Err(); regErr != nil {
		c.fail(regErr)

		return exitFailure
	}

	contexts, _ := reg.Contexts()

	names := make([]string, 0, len(contexts))
	routes := 0

	for _, res := range contexts {
		names = append(names, res.Context)
		routes += len(res.Aliases())
	}

	fmt.Fprintf(c.stdout, "OK %s: %s (%s), %s\n",
		target, count(len(names), "context", "contexts"), strings.Join(names, ", "),
		count(routes, "route", "routes"))

	return exitOK
}

// count renders a quantity with the right noun, so a valid registry with one
// route does not report "1 routes".
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
