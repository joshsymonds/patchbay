package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/joshsymonds/patchbay/internal/server"
)

func (c cli) bindings(ctx context.Context, args []string) int {
	flags := c.flagSet(cmdBindings)
	name := flags.String("ctx", "", "the Context whose public bindings to list")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() != 0 || *name == "" {
		c.usage(c.stderr)
		return exitUsage
	}
	doc, err := c.fetch(ctx)
	if err != nil {
		c.fail(err)
		return exitFailure
	}
	if writeErr := c.writeBindings(doc, *name); writeErr != nil {
		c.fail(writeErr)
		return exitFailure
	}
	return exitOK
}

func (c cli) writeBindings(doc server.Document, name string) error {
	summary, err := contextSummary(doc, name)
	if err != nil {
		return err
	}
	selectors := make([]string, 0, len(summary.Models))
	for selector := range summary.Models {
		selectors = append(selectors, selector)
	}
	slices.Sort(selectors)
	table := c.table()
	fmt.Fprintln(table, "MODEL\tSEAT")
	for _, selector := range selectors {
		fmt.Fprintf(table, "%s\t%s\n", quoted(selector), quoted(summary.Models[selector]))
	}
	c.flush(table)
	return nil
}
