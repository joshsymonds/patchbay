package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"text/tabwriter"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/server"
)

// builtinDefault is how the built-in default route is described in a table: a
// plain forward to Anthropic on the caller's own credentials, which is what
// every context that declares nothing of its own rides.
const builtinDefault = "built-in anthropic forward"

// callerCredential is the credential source of a forward route: whatever the
// caller itself presented. There is no file and no environment variable.
const callerCredential = "caller"

// awsProfileCredential prefixes the credential source of a signing route: the
// AWS profile the gateway exports credentials for. It is a name in the
// operator's own AWS config, never a credential.
const awsProfileCredential = "aws profile "

// absent is what a table prints for a field a route does not set. An empty cell
// would be ambiguous with a field the gateway failed to report.
const absent = "-"

// tabwriter geometry: no minimum cell width, no padding character games, two
// spaces between the widest cell and the next column.
const (
	tabMinWidth = 0
	tabStop     = 8
	tabPadding  = 2
)

// contexts lists every context the gateway serves.
func (c cli) contexts(ctx context.Context, args []string) int {
	flags := c.flagSet(cmdContexts)
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}

	if flags.NArg() != 0 {
		c.usage(c.stderr)

		return exitUsage
	}

	doc, err := c.fetch(ctx)
	if err != nil {
		c.fail(err)

		return exitFailure
	}

	table := c.table()
	fmt.Fprintln(table, "CONTEXT\tDEFAULT_ROUTE\tROUTES")

	for _, name := range contextNames(doc) {
		summary := doc.Contexts[name]
		fmt.Fprintf(table, "%s\t%s\t%d\n", name, defaultRouteLabel(summary.DefaultRoute), len(summary.Routes))
	}

	c.flush(table)

	return exitOK
}

// routes lists one context's aliases.
func (c cli) routes(ctx context.Context, args []string) int {
	flags := c.flagSet(cmdRoutes)
	name := flags.String("ctx", registry.PersonalContext, "the context whose aliases to list")

	if err := flags.Parse(args); err != nil {
		return exitUsage
	}

	if flags.NArg() != 0 {
		c.usage(c.stderr)

		return exitUsage
	}

	doc, err := c.fetch(ctx)
	if err != nil {
		c.fail(err)

		return exitFailure
	}

	if routesErr := c.writeRoutes(doc, *name); routesErr != nil {
		c.fail(routesErr)

		return exitFailure
	}

	return exitOK
}

// writeRoutes prints one context's aliases as a table.
func (c cli) writeRoutes(doc server.Document, name string) error {
	summary, err := contextSummary(doc, name)
	if err != nil {
		return err
	}

	table := c.table()
	fmt.Fprintln(table, "ALIAS\tAUTH\tUPSTREAM\tMODEL\tMAX_INPUT_TOKENS\tCREDENTIAL_SOURCE")

	aliases := make([]string, 0, len(summary.Routes))
	for alias := range summary.Routes {
		aliases = append(aliases, alias)
	}

	slices.Sort(aliases)

	for _, alias := range aliases {
		route := summary.Routes[alias]
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n",
			alias, route.Auth, route.Upstream, orAbsent(route.Model),
			tokenCeiling(route.MaxInputTokens), credentialSource(route))
	}

	c.flush(table)

	return nil
}

// fetch asks the running gateway for its whole routing document.
func (c cli) fetch(ctx context.Context) (server.Document, error) {
	reached, err := c.dial()
	if err != nil {
		return server.Document{}, err
	}

	return reached.document(ctx)
}

// flagSet returns a flag set for one subcommand: it reports its own errors on
// stderr, and never exits the process out from under a caller.
func (c cli) flagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(c.stderr)

	return flags
}

// table returns a writer that aligns columns on stdout. Every row is one
// record, tab separated before alignment, so the output stays greppable.
func (c cli) table() *tabwriter.Writer {
	return tabwriter.NewWriter(c.stdout, tabMinWidth, tabStop, tabPadding, ' ', 0)
}

// flush writes an aligned table out, reporting a stdout that went away rather
// than leaving the caller with a silently half-written table.
func (c cli) flush(table *tabwriter.Writer) {
	if err := table.Flush(); err != nil {
		c.noticef("the table could not be written: %v", err)
	}
}

// contextNames returns a document's context names in order.
func contextNames(doc server.Document) []string {
	names := make([]string, 0, len(doc.Contexts))
	for name := range doc.Contexts {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

// defaultRouteLabel describes a context's default route in one cell. The
// built-in one is named as such rather than spelled out, because "forward to
// api.anthropic.com with no rewrite" is exactly what having no configuration
// means.
func defaultRouteLabel(route server.RouteSummary) string {
	if route.Auth == string(registry.AuthForward) &&
		route.Upstream == anthropic.BaseURL &&
		route.Model == "" {
		return builtinDefault
	}

	label := route.Auth + " " + route.Upstream
	if route.Model != "" {
		label += " model=" + route.Model
	}

	return label
}

// credentialSource names what a route pays with: the environment variable that
// names its key file, the AWS profile the gateway exports credentials from, or
// the caller's own credentials. It is a variable name or a profile name, never a
// path and never a secret.
//
// A signing route's model map is deliberately not the answer here. The map
// translates the model; what pays is the profile.
func credentialSource(route server.RouteSummary) string {
	switch {
	case route.KeyEnvFile != "":
		return route.KeyEnvFile
	case route.AWSProfile != "":
		return awsProfileCredential + route.AWSProfile
	default:
		return callerCredential
	}
}

// tokenCeiling renders an advertised input ceiling, which a route need not set.
func tokenCeiling(tokens int) string {
	if tokens == 0 {
		return absent
	}

	return strconv.Itoa(tokens)
}

// orAbsent renders a field a route need not set.
func orAbsent(value string) string {
	if value == "" {
		return absent
	}

	return value
}

// writeLine prints one "key: value" record.
func writeLine(w io.Writer, key, value string) {
	fmt.Fprintf(w, "%s: %s\n", key, value)
}
