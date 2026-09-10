package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/joshsymonds/patchbay/internal/usage"
)

func (c cli) usageInventoryTranscripts(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("inventory-transcripts", flag.ContinueOnError)
	flags.SetOutput(c.stderr)
	claudeProjects := flags.String("claude-projects", "", "Claude Code projects root")
	piSessions := flags.String("pi-sessions", "", "pi sessions root")
	out := flags.String("out", "", "inventory output path")
	if err := flags.Parse(
		args,
	); err != nil || flags.NArg() != 0 || strings.TrimSpace(*claudeProjects) == "" || strings.TrimSpace(*piSessions) == "" ||
		strings.TrimSpace(*out) == "" {
		if err == nil {
			fmt.Fprintln(
				c.stderr,
				"patchbay: usage inventory-transcripts requires --claude-projects, --pi-sessions and --out",
			)
		}
		return exitUsage
	}
	err := usage.InventoryTranscripts(ctx, *claudeProjects, *piSessions, *out)
	if err != nil {
		c.fail(err)
		return exitFailure
	}
	fmt.Fprintf(c.stdout, "OK usage inventory-transcripts: %s\n", *out)
	return exitOK
}

func (c cli) usageImportTranscripts(ctx context.Context, args []string) (exitCode int) {
	flags := flag.NewFlagSet("import-transcripts", flag.ContinueOnError)
	flags.SetOutput(c.stderr)
	inventory := flags.String("inventory", "", "transcript inventory path")
	dbPath := flags.String("db", "", "usage database path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(*inventory) == "" {
		if err == nil {
			fmt.Fprintln(c.stderr, "patchbay: usage import-transcripts requires --inventory")
		}
		return exitUsage
	}
	resolvedDB := strings.TrimSpace(*dbPath)
	if resolvedDB == "" {
		cfg, err := resolveConfig(c.lookupEnv)
		if err != nil {
			c.fail(err)
			return exitFailure
		}
		resolvedDB = cfg.usageDB
	}
	store, err := usage.Open(resolvedDB)
	if err != nil {
		c.fail(fmt.Errorf("open usage database %s: %w", resolvedDB, err))
		return exitFailure
	}
	var operationErr error
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			operationErr = errors.Join(
				operationErr,
				fmt.Errorf("close usage database %s: %w", resolvedDB, closeErr),
			)
		}
		if operationErr != nil {
			c.fail(operationErr)
			exitCode = exitFailure
		}
	}()
	result, operationErr := store.ImportTranscripts(ctx, *inventory)
	if operationErr != nil {
		return exitFailure
	}
	for _, file := range result.Files {
		if file.UnreadableRows > 0 {
			fmt.Fprintf(c.stderr, "patchbay: %s: %d unreadable rows skipped\n", file.Path, file.UnreadableRows)
		}
		fmt.Fprintf(
			c.stdout,
			"OK usage import-transcripts %s: imported=%d skipped-existing=%d skipped-changed=%d\n",
			file.Path,
			file.Imported,
			file.SkippedExisting,
			file.SkippedChanged,
		)
	}
	return exitOK
}
