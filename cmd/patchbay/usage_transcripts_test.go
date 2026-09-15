package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// noAccountingTable is what accountingRowsIn reports for a database no
// migration has ever run against, which has no accounting table to count.
const noAccountingTable = -1

// TestImportTranscriptsNeverMigratesTheDaemonDatabase pins the difference
// between the two databases the subcommand can be pointed at. The daemon's own
// database is opened as it stands, because a daemon serving an older schema
// could not reopen a database this build migrated under it; a copy the operator
// names with --db is theirs, and is migrated.
func TestImportTranscriptsNeverMigratesTheDaemonDatabase(t *testing.T) {
	inventory := writeTranscriptInventory(t)
	usageDB := filepath.Join(t.TempDir(), "usage.sqlite")
	env := map[string]string{envUsageDB: usageDB, envHome: t.TempDir()}

	got := invoke(t, env, "usage", "import-transcripts", "--inventory", inventory)
	if got.code != exitFailure {
		t.Fatalf("default-database import code = %d; want %d (stderr %q)", got.code, exitFailure, got.stderr)
	}
	got.contains(t, "stderr", "schema version 0 is behind the version")
	got.contains(t, "stderr", "upgrade the running patchbay daemon")
	if rows := accountingRowsIn(t, usageDB); rows != noAccountingTable {
		t.Fatalf("refused import left %d accounting rows in %s; want no accounting table at all", rows, usageDB)
	}

	got = invoke(t, env, "usage", "import-transcripts", "--inventory", inventory, "--db", usageDB)
	if got.code != exitOK {
		t.Fatalf("named-database import code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}
	got.contains(t, "stdout", "imported=1 skipped-existing=0 skipped-changed=0")
	if rows := accountingRowsIn(t, usageDB); rows != 1 {
		t.Fatalf("accounting rows after the named-database import = %d; want 1", rows)
	}
}

// writeTranscriptInventory writes one assistant record under a Claude projects
// root and inventories it, returning the inventory path.
func writeTranscriptInventory(t *testing.T) string {
	t.Helper()

	claude := t.TempDir()
	project := filepath.Join(claude, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(project, "session.jsonl"),
		[]byte(
			`{"type":"assistant","uuid":"a","timestamp":"2026-09-01T00:00:00Z","message":{"id":"msg-1",`+
				`"model":"claude-test","usage":{"input_tokens":1,"output_tokens":1},`+
				`"content":[{"type":"text"}],"stop_reason":"end_turn"}}`+"\n",
		),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	inventory := filepath.Join(t.TempDir(), "inventory.json")
	got := invoke(t, nil, "usage", "inventory-transcripts",
		"--claude-projects", claude,
		"--pi-sessions", t.TempDir(),
		"--out", inventory,
	)
	if got.code != exitOK {
		t.Fatalf("inventory command code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}
	return inventory
}

// accountingRowsIn counts the accounting rows in path, reporting
// noAccountingTable when nothing ever migrated it.
func accountingRowsIn(t *testing.T, path string) int {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s for inspection: %v", path, err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close %s: %v", path, err)
		}
	})
	var present int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'accounting_requests'`,
	).Scan(&present); err != nil {
		t.Fatalf("inspect %s: %v", path, err)
	}
	if present == 0 {
		return noAccountingTable
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM accounting_requests`).Scan(&rows); err != nil {
		t.Fatalf("count accounting rows in %s: %v", path, err)
	}
	return rows
}
