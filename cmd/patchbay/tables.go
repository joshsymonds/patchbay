package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"text/tabwriter"

	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/server"
)

const (
	callerCredential = "caller"

	// nullValue is how every absent string field prints. It is deliberately not a
	// string the CLI could ever print as a value: present strings are quoted, so
	// a Seat rewriting to a model an operator named "null" shows as `"null"` and
	// only a genuinely absent one shows as null.
	nullValue = "null"

	tabMinWidth = 0
	tabStop     = 8
	tabPadding  = 2
)

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
	fmt.Fprintln(table, "CONTEXT\tDEFAULT_SEAT\tBINDINGS\tSUBAGENT_BINDINGS")
	for _, name := range contextNames(doc) {
		summary := doc.Contexts[name]
		sub := 0
		if summary.Subagents != nil {
			sub = len(summary.Subagents.Models) + 1
		}
		fmt.Fprintf(table, "%s\t%s\t%d\t%d\n", name, summary.DefaultSeat, len(summary.Models), sub)
	}
	c.flush(table)
	return exitOK
}

func (c cli) fetch(ctx context.Context) (server.Document, error) {
	reached, err := c.dial()
	if err != nil {
		return server.Document{}, err
	}
	doc, err := reached.document(ctx)
	if err != nil {
		return server.Document{}, err
	}
	if !doc.RegistryValid {
		return server.Document{}, fmt.Errorf("registry invalid: %s", doc.RegistryError)
	}
	return doc, nil
}
func (c cli) flagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(c.stderr)
	return flags
}
func (c cli) table() *tabwriter.Writer {
	return tabwriter.NewWriter(c.stdout, tabMinWidth, tabStop, tabPadding, ' ', 0)
}
func (c cli) flush(table *tabwriter.Writer) {
	if err := table.Flush(); err != nil {
		c.noticef("the table could not be written: %v", err)
	}
}
func contextNames(doc server.Document) []string {
	names := make([]string, 0, len(doc.Contexts))
	for name := range doc.Contexts {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
func seatNames(doc server.Document) []string {
	names := make([]string, 0, len(doc.Seats))
	for name := range doc.Seats {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
func tokenCeiling(tokens int) string {
	if tokens == 0 {
		return nullValue
	}
	return strconv.Itoa(tokens)
}

// quoted renders a present string as a Go literal.
//
// Model selectors and Seat rewrite targets are opaque exact strings: whatever
// bytes an operator wrote, tabs and newlines included. Written raw into a
// tab-separated table or a "key: value" line, those bytes stop being data and
// become structure — a newline splits one row in two, a tab invents a column.
// A quoted literal has no spelling for either, so the value stays one cell and
// every byte of it is still visible.
func quoted(value string) string { return strconv.Quote(value) }

// nullable renders a string field that may be absent: unquoted null when it is,
// a quoted literal when it is not. Absence is the one rendering no present
// string can produce, which is what makes the two readable apart.
func nullable(value string) string {
	if value == "" {
		return nullValue
	}
	return quoted(value)
}

// nullableString is nullable for a field whose absence is a nil pointer rather
// than an empty string — a selection that named no model at all.
func nullableString(value *string) string {
	if value == nil {
		return nullValue
	}
	return quoted(*value)
}
func writeLine(w io.Writer, key, value string) { fmt.Fprintf(w, "%s: %s\n", key, value) }

func registryState(doc server.Document) registry.State {
	state := registry.State{
		Valid:    doc.RegistryValid,
		Error:    doc.RegistryError,
		Seats:    map[string]registry.Seat{},
		Contexts: map[string]registry.Context{},
	}
	if doc.DefaultContext != nil {
		state.DefaultContext = *doc.DefaultContext
	}
	for id, summary := range doc.Seats {
		state.Seats[id] = registry.Seat{
			Upstream:          summary.Upstream,
			Model:             summary.Model,
			AuthMode:          registry.AuthMode(summary.AuthMode),
			BillingClass:      billing.Class(summary.BillingClass),
			BillingClassSet:   summary.BillingClass != "",
			APIKeyEnvFile:     summary.APIKeyEnvFile,
			AWSProfile:        summary.AWSProfile,
			AWSRegion:         summary.AWSRegion,
			ModelMapEnvFile:   summary.ModelMapEnvFile,
			MaxInputTokens:    summary.MaxInputTokens,
			MaxOutputTokens:   summary.MaxOutputTokens,
			Insecure:          summary.Insecure,
			StripCacheControl: summary.StripCacheControl,
			CaptureBodies:     summary.CaptureBodies,
		}
	}
	for id, summary := range doc.Contexts {
		ctx := registry.Context{DefaultSeat: summary.DefaultSeat, Models: summary.Models}
		if summary.Subagents != nil {
			ctx.Subagents = &registry.Subagents{
				DefaultSeat: summary.Subagents.DefaultSeat,
				Models:      summary.Subagents.Models,
			}
		}
		state.Contexts[id] = ctx
	}
	return state
}
