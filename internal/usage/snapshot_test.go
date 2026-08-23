package usage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// toolingTools enumerates the two commands that share the read-only source gate,
// so a source-side rejection is proven for both rather than for snapshot alone.
func toolingTools() []struct {
	name string
	run  func(context.Context, string, string) error
} {
	return []struct {
		name string
		run  func(context.Context, string, string) error
	}{
		{name: "snapshot", run: Snapshot},
		{name: "export", run: Export},
	}
}

// seedToolingSource creates a migrated usage database holding rows requests and
// closes it, so the source is quiescent and carries no WAL sidecar.
func seedToolingSource(t *testing.T, path string, requests int) {
	t.Helper()

	store, openErr := Open(path)
	if openErr != nil {
		t.Fatalf("create tooling source %q: %v", path, openErr)
	}
	for range requests {
		if _, insertErr := store.InsertPending(t.Context(), validPendingRequest()); insertErr != nil {
			t.Fatalf("seed tooling source %q: %v", path, insertErr)
		}
	}
	if closeErr := store.Close(); closeErr != nil {
		t.Fatalf("close tooling source %q: %v", path, closeErr)
	}
}

// toolingSourceBytes reads a source whole so a rejection can be proven
// non-mutating byte for byte, not merely by mode and sidecar absence.
func toolingSourceBytes(t *testing.T, path string) []byte {
	t.Helper()

	contents, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read tooling source %q: %v", path, readErr)
	}
	return contents
}

// assertRejectedToolingRunLeftNoOutput proves a rejection that could only happen
// after SQLite opened the source published nothing and left no staging residue,
// and that the source itself is untouched.
//
// Sidecar absence is not assertable at this point and would be a false
// requirement: a read-only connection to a WAL database creates `-wal` and `-shm`
// in order to read it at all, and cannot check them back in when it closes. What
// is assertable — and what the caller actually cares about — is that nothing was
// written through them: no rollback journal, and an empty write-ahead log.
func assertRejectedToolingRunLeftNoOutput(t *testing.T, source, dest string, before []byte) {
	t.Helper()

	assertMode(t, source, databaseMode)
	if !bytes.Equal(toolingSourceBytes(t, source), before) {
		t.Errorf("rejected source %q was modified", source)
	}
	if _, journalErr := os.Lstat(source + "-journal"); !errors.Is(journalErr, os.ErrNotExist) {
		t.Errorf("rollback journal %q appeared for a rejected source: %v", source+"-journal", journalErr)
	}
	if info, walErr := os.Lstat(source + "-wal"); walErr == nil && info.Size() != 0 {
		t.Errorf("write-ahead log %q holds %d bytes after a rejected source read", source+"-wal", info.Size())
	} else if walErr != nil && !errors.Is(walErr, os.ErrNotExist) {
		t.Errorf("inspect write-ahead log %q: %v", source+"-wal", walErr)
	}
	if _, destErr := os.Lstat(dest); !errors.Is(destErr, os.ErrNotExist) {
		t.Errorf("destination %q exists after rejection: %v", dest, destErr)
	}
	if partials := toolingStagingMatches(t, dest); len(partials) != 0 {
		t.Errorf("staging residue after rejection = %v; want none", partials)
	}
}

// damageIndexRootPage rewrites the b-tree page header of a named secondary index
// in place. The table b-trees are left alone, which is the whole point: the rows
// still read back and `VACUUM INTO` still rebuilds the index from them, so the
// damage is invisible to everything except a check of the source itself.
func damageIndexRootPage(t *testing.T, path, index string, damage func(header []byte)) {
	t.Helper()

	db, openErr := sql.Open("sqlite", path)
	if openErr != nil {
		t.Fatalf("open %q to locate index %q: %v", path, index, openErr)
	}
	var pageSize, rootPage int64
	if pageErr := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); pageErr != nil {
		t.Fatalf("read page size of %q: %v", path, pageErr)
	}
	const rootPageQuery = `SELECT rootpage FROM sqlite_master WHERE type = 'index' AND name = ?`
	if rootErr := db.QueryRow(rootPageQuery, index).Scan(&rootPage); rootErr != nil {
		t.Fatalf("locate root page of index %q in %q: %v", index, path, rootErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close %q after locating index %q: %v", path, index, closeErr)
	}

	file, fileErr := os.OpenFile(path, os.O_RDWR, 0)
	if fileErr != nil {
		t.Fatalf("open %q for index damage: %v", path, fileErr)
	}
	// A b-tree page header is eight bytes: type, first freeblock, cell count,
	// cell content start, and fragmented free bytes.
	header := make([]byte, 8)
	offset := (rootPage - 1) * pageSize
	if _, readErr := file.ReadAt(header, offset); readErr != nil {
		t.Fatalf("read index %q page header in %q: %v", index, path, readErr)
	}
	damage(header)
	if _, writeErr := file.WriteAt(header, offset); writeErr != nil {
		t.Fatalf("write damaged index %q page header in %q: %v", index, path, writeErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		t.Fatalf("close %q after index damage: %v", path, closeErr)
	}
}

// TestToolingRejectsCorruptSourceIndexBeforePublication covers the source whose
// tables are intact and whose secondary index is not. Schema validation reads
// only schema_migrations and sqlite_master, and `VACUUM INTO` rebuilds indexes
// from the table rows, so a snapshot of this source can be internally clean while
// the database it was taken from is already damaged — a corrupt original
// certified as a good backup. Only a check of the exact opened source catches it,
// and it must catch it before anything is staged or published.
func TestToolingRejectsCorruptSourceIndexBeforePublication(t *testing.T) {
	t.Parallel()

	for _, shape := range []struct {
		name   string
		damage func(header []byte)
	}{
		{
			// Survives VACUUM entirely: every row copies forward and the
			// rebuilt snapshot reports `ok`, so artifact validation cannot see it.
			name:   "index page reports impossible fragmentation",
			damage: func(header []byte) { header[7] = 9 },
		},
		{
			// Drops the last index entry, so the source silently reads back
			// one row short of what it stores.
			name: "index page undercounts its cells",
			damage: func(header []byte) {
				binary.BigEndian.PutUint16(header[3:5], binary.BigEndian.Uint16(header[3:5])-1)
			},
		},
	} {
		for _, tool := range toolingTools() {
			t.Run(shape.name+"/"+tool.name, func(t *testing.T) {
				t.Parallel()

				source := filepath.Join(t.TempDir(), "usage.sqlite")
				seedToolingSource(t, source, 3)
				damageIndexRootPage(t, source, summaryWindowIndexName, shape.damage)
				before := toolingSourceBytes(t, source)

				dest := filepath.Join(t.TempDir(), "usage.out")
				err := tool.run(t.Context(), source, dest)
				if err == nil || !strings.Contains(err.Error(), "failed integrity check") {
					t.Fatalf("%s(corrupt source) error = %v; want an integrity refusal", tool.name, err)
				}
				if !strings.Contains(err.Error(), source) {
					t.Errorf(
						"%s(corrupt source) error = %v; want it to name the source %q rather than a generated artifact",
						tool.name,
						err,
						source,
					)
				}
				assertRejectedToolingRunLeftNoOutput(t, source, dest, before)
			})
		}
	}
}

// forgetMigrationRecord deletes one applied migration's row while leaving every
// object that migration created in place. The result claims a supported older
// schema, passes schema validation as a valid prefix, and cannot migrate forward.
func forgetMigrationRecord(t *testing.T, path string, version int) {
	t.Helper()

	db, openErr := sql.Open("sqlite", path)
	if openErr != nil {
		t.Fatalf("open %q to forget migration %d: %v", path, version, openErr)
	}
	if _, execErr := db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, version); execErr != nil {
		t.Fatalf("forget migration %d in %q: %v", version, path, execErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close %q after forgetting migration %d: %v", path, version, closeErr)
	}
}

// TestSnapshotRejectsClaimedOlderSchemaThatCannotMigrateForward covers the
// snapshot that is readable, uncorrupted, and still worthless: it claims a
// schema version this build supports, but the objects on disk contradict that
// claim, so `patchbay serve` would fail to migrate it forward and refuse to
// start. Restoring it is the failure the restore gate exists to prevent, and
// nothing short of actually running the pending migrations can detect it.
//
// The contradiction here is a table a pending migration will try to create.
// Tables are where this gap still lives: an addition attached to a managed table
// is refused outright at the source gate, but a table is attached to nothing, so
// the recorded version's contract has nothing to say about it and the collision
// surfaces only when the migration that wanted the name actually runs.
func TestSnapshotRejectsClaimedOlderSchemaThatCannotMigrateForward(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	olderSchemaToolingSource(t, source, finalizedResponseAccountingMigrationVersion)
	spoilToolingSource(t, source, `CREATE TABLE `+rateCardsTable+` (id INTEGER PRIMARY KEY)`)
	before := toolingSourceBytes(t, source)

	// The read-only source gate accepts it: the recorded migrations are a valid
	// prefix, every object version 2 declares is exact, nothing extra is attached
	// to a table those migrations built, and every page reads back coherently.
	// The forward-migration proof is the only thing left that can refuse it.
	opened, openErr := openToolingDatabase(t.Context(), source)
	if openErr != nil {
		t.Fatalf("openToolingDatabase(claimed-old schema) error = %v; want the source gate to accept it", openErr)
	}
	if closeErr := opened.Close(); closeErr != nil {
		t.Fatalf("close claimed-old schema source: %v", closeErr)
	}

	dest := filepath.Join(t.TempDir(), "usage.snapshot.sqlite")
	err := Snapshot(t.Context(), source, dest)
	if err == nil || !strings.Contains(err.Error(), "migration clone") {
		t.Fatalf("Snapshot(claimed-old schema) error = %v; want a forward-migration refusal", err)
	}
	if !strings.Contains(err.Error(), rateCardsTable) {
		t.Errorf(
			"Snapshot(claimed-old schema) error = %v; want it to name the migration object %q that could not be created",
			err,
			rateCardsTable,
		)
	}
	assertRejectedToolingRunLeftNoOutput(t, source, dest, before)
}

// olderSchemaToolingSource creates a usage database carrying only the first
// `through` declared migrations, which is what a snapshot taken by an earlier
// build looks like to this one.
func olderSchemaToolingSource(t *testing.T, path string, through int) {
	t.Helper()

	db, openErr := sql.Open("sqlite", path)
	if openErr != nil {
		t.Fatalf("create older-schema source %q: %v", path, openErr)
	}
	if migrateErr := applyMigrations(t.Context(), db, declaredMigrations()[:through]); migrateErr != nil {
		t.Fatalf("apply the first %d migrations to %q: %v", through, path, migrateErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close older-schema source %q: %v", path, closeErr)
	}
	if chmodErr := os.Chmod(path, databaseMode); chmodErr != nil {
		t.Fatalf("secure older-schema source %q: %v", path, chmodErr)
	}
}

// TestSnapshotPublishesEverySupportedOlderSchemaUnchanged pins both halves of the
// older-schema contract. The forward-migration proof must not narrow which
// snapshots are restorable — every schema this build still declares has to
// publish — and it must not migrate what it publishes, because the artifact is
// the backup and `patchbay serve` is what migrates one forward on restore.
func TestSnapshotPublishesEverySupportedOlderSchemaUnchanged(t *testing.T) {
	t.Parallel()

	declared := declaredMigrations()
	for through := 1; through <= len(declared); through++ {
		t.Run(fmt.Sprintf("schema version %d", declared[through-1].version), func(t *testing.T) {
			t.Parallel()

			source := filepath.Join(t.TempDir(), "usage.sqlite")
			olderSchemaToolingSource(t, source, through)

			dest := filepath.Join(t.TempDir(), "usage.snapshot.sqlite")
			proofs := 0
			ctx := context.WithValue(t.Context(), toolingMigrationCloneHookKey{}, func(string) { proofs++ })
			if err := Snapshot(ctx, source, dest); err != nil {
				t.Fatalf("Snapshot(schema version %d) error = %v; want publication", declared[through-1].version, err)
			}
			if proofs != 1 {
				t.Errorf("forward-migration proof ran %d times; want exactly 1", proofs)
			}

			published := inspectDB(t, dest)
			const versionQuery = `SELECT MAX(version) FROM schema_migrations`
			var version int
			if versionErr := published.QueryRow(versionQuery).Scan(&version); versionErr != nil {
				t.Fatalf("read published schema version: %v", versionErr)
			}
			if want := declared[through-1].version; version != want {
				t.Errorf(
					"published schema version = %d; want the source's own %d, since only the throwaway clone is migrated",
					version,
					want,
				)
			}
			var integrity string
			if integrityErr := published.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); integrityErr != nil {
				t.Fatalf("integrity check the published snapshot: %v", integrityErr)
			}
			if integrity != "ok" {
				t.Errorf("published snapshot integrity = %q; want ok", integrity)
			}
			assertMode(t, dest, databaseMode)
			if partials := toolingStagingMatches(t, dest); len(partials) != 0 {
				t.Errorf("staging residue after publication = %v; want none", partials)
			}
		})
	}
}

// spoilToolingSource applies raw SQL to a database in place, so it can be made to
// hold objects that contradict the schema version it records while every
// schema_migrations row making that claim stays exactly where it is.
func spoilToolingSource(t *testing.T, path string, statements ...string) {
	t.Helper()

	db, openErr := sql.Open("sqlite", path)
	if openErr != nil {
		t.Fatalf("open %q to spoil its schema: %v", path, openErr)
	}
	for _, statement := range statements {
		if _, execErr := db.Exec(statement); execErr != nil {
			t.Fatalf("spoil %q with %q: %v", path, statement, execErr)
		}
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close %q after spoiling its schema: %v", path, closeErr)
	}
}

// TestToolingRejectsRecordedSchemaObjectsThatContradictTheirVersion covers the
// database whose schema_migrations rows are a valid prefix and whose schema no
// longer holds what those rows promised. Every check that runs before this one
// accepts it: the pages that remain read back coherently, so
// `PRAGMA integrity_check` reports `ok`, and the recorded migration names still
// parse as a supported version. Nothing downstream puts the missing object back
// either — a migration already recorded is never replayed — so `patchbay serve`
// is where a restored copy fails, which is far too late for a backup to be
// telling the truth about itself.
func TestToolingRejectsRecordedSchemaObjectsThatContradictTheirVersion(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		version int
		spoil   []string
		wantErr string
	}{
		{
			name:    "a version-5 database has lost its rate cards table",
			version: upstreamTargetDispatchEvidenceMigrationVersion,
			spoil:   []string{`DROP TABLE rate_cards`},
			wantErr: `schema version 5 requires table "rate_cards", which is missing`,
		},
		{
			name:    "the newest schema has lost its rate cards table",
			version: summaryQueryIndexesMigrationVersion,
			spoil:   []string{`DROP TABLE rate_cards`},
			wantErr: `schema version 6 requires table "rate_cards", which is missing`,
		},
		{
			name:    "the migration bookkeeping table has lost a column",
			version: 1,
			spoil:   []string{`ALTER TABLE schema_migrations DROP COLUMN applied_at`},
			wantErr: `schema version 1 requires column "applied_at" on table "schema_migrations", which is missing`,
		},
		{
			name:    "a version-1 database has lost a version-1 column",
			version: 1,
			spoil:   []string{`ALTER TABLE accounting_requests DROP COLUMN route_alias`},
			wantErr: `schema version 1 requires column "route_alias" on table "accounting_requests", which is missing`,
		},
		{
			name:    "a version-2 database has lost a finalized column",
			version: finalizedResponseAccountingMigrationVersion,
			spoil:   []string{`ALTER TABLE accounting_requests DROP COLUMN input_tokens`},
			wantErr: `schema version 2 requires column "input_tokens" on table "accounting_requests", which is missing`,
		},
		{
			name:    "a version-2 database has lost its finalized update trigger",
			version: finalizedResponseAccountingMigrationVersion,
			spoil:   []string{`DROP TRIGGER accounting_requests_validate_finalized_snapshot`},
			wantErr: `schema version 2 requires trigger "accounting_requests_validate_finalized_snapshot", which is missing`,
		},
		{
			name:    "a version-2 database has lost its response-id index",
			version: finalizedResponseAccountingMigrationVersion,
			spoil:   []string{`DROP INDEX accounting_requests_finalized_response_id`},
			wantErr: `schema version 2 requires index "accounting_requests_finalized_response_id", which is missing`,
		},
		{
			name:    "a version-3 database has lost a rate card column",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil:   []string{`ALTER TABLE rate_cards DROP COLUMN source`},
			wantErr: `schema version 3 requires column "source" on table "rate_cards", which is missing`,
		},
		{
			name:    "a version-3 database has lost a final cost column",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil:   []string{`ALTER TABLE accounting_requests DROP COLUMN cost_basis`},
			wantErr: `schema version 3 requires column "cost_basis" on table "accounting_requests", which is missing`,
		},
		{
			name:    "a version-3 database has lost a rate card immutability trigger",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil:   []string{`DROP TRIGGER rate_cards_forbid_update`},
			wantErr: `schema version 3 requires trigger "rate_cards_forbid_update", which is missing`,
		},
		{
			name:    "a version-4 database has lost its import provenance index",
			version: importedLedgerProvenanceMigrationVersion,
			spoil:   []string{`DROP INDEX accounting_requests_import_provenance`},
			wantErr: `schema version 4 requires index "accounting_requests_import_provenance", which is missing`,
		},
		{
			name:    "a version-4 database has lost its finalized insert trigger",
			version: importedLedgerProvenanceMigrationVersion,
			spoil:   []string{`DROP TRIGGER accounting_requests_validate_finalized_snapshot_insert`},
			wantErr: `schema version 4 requires trigger "accounting_requests_validate_finalized_snapshot_insert", which is missing`,
		},
		{
			name:    "a version-5 database has lost its upstream target column",
			version: upstreamTargetDispatchEvidenceMigrationVersion,
			spoil:   []string{`ALTER TABLE accounting_requests DROP COLUMN upstream_target`},
			wantErr: `schema version 5 requires column "upstream_target" on table "accounting_requests", which is missing`,
		},
		{
			name:    "the newest schema has lost a summary index",
			version: summaryQueryIndexesMigrationVersion,
			spoil:   []string{`DROP INDEX ` + summaryWindowIndexName},
			wantErr: `schema version 6 requires index "` + summaryWindowIndexName + `", which is missing`,
		},
		{
			name:    "the newest schema has lost its context summary index",
			version: summaryQueryIndexesMigrationVersion,
			spoil:   []string{`DROP INDEX ` + summaryContextWindowIndexName},
			wantErr: `schema version 6 requires index "` + summaryContextWindowIndexName + `", which is missing`,
		},
		{
			name:    "a required index name has been taken by a table",
			version: summaryQueryIndexesMigrationVersion,
			spoil: []string{
				`DROP INDEX ` + summaryWindowIndexName,
				`CREATE TABLE ` + summaryWindowIndexName + ` (id INTEGER PRIMARY KEY)`,
			},
			wantErr: `schema version 6 requires index "` + summaryWindowIndexName + `", but it is a table`,
		},
		{
			name:    "a required table name has been taken by a view",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil: []string{
				`DROP TABLE rate_cards`,
				`CREATE VIEW rate_cards AS SELECT 1 AS id`,
			},
			wantErr: `schema version 3 requires table "rate_cards", but it is a view`,
		},
		{
			name:    "a required index has been rebuilt against another table",
			version: summaryQueryIndexesMigrationVersion,
			spoil: []string{
				`DROP INDEX ` + summaryContextWindowIndexName,
				`CREATE INDEX ` + summaryContextWindowIndexName + ` ON rate_cards(model)`,
			},
			wantErr: `schema version 6 requires index "` + summaryContextWindowIndexName +
				`" on table "accounting_requests", but it is on "rate_cards"`,
		},
		{
			name:    "a required trigger has been rebuilt against another table",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil: []string{
				`DROP TRIGGER rate_cards_forbid_delete`,
				`CREATE TRIGGER rate_cards_forbid_delete BEFORE DELETE ON accounting_requests
				BEGIN
					SELECT RAISE(ABORT, 'wrong table');
				END`,
			},
			wantErr: `schema version 3 requires trigger "rate_cards_forbid_delete" on table "rate_cards", ` +
				`but it is on "accounting_requests"`,
		},
	} {
		for _, tool := range toolingTools() {
			t.Run(test.name+"/"+tool.name, func(t *testing.T) {
				t.Parallel()

				source := filepath.Join(t.TempDir(), "usage.sqlite")
				olderSchemaToolingSource(t, source, test.version)
				spoilToolingSource(t, source, test.spoil...)
				before := toolingSourceBytes(t, source)

				dest := filepath.Join(t.TempDir(), "usage.out")
				err := tool.run(t.Context(), source, dest)
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("%s(contradicted schema) error = %v; want %q", tool.name, err, test.wantErr)
				}
				if !strings.Contains(err.Error(), "not a patchbay usage database") {
					t.Errorf("%s(contradicted schema) error = %v; want the usage-database refusal", tool.name, err)
				}
				assertRejectedToolingRunLeftNoOutput(t, source, dest, before)
			})
		}
	}
}

// spoilWith adapts a fixed statement list to the rewrite signature the
// definition table uses, so plain DDL and stored-text rewrites sit side by side.
func spoilWith(statements ...string) func(*testing.T, string) {
	return func(t *testing.T, path string) {
		t.Helper()

		spoilToolingSource(t, path, statements...)
	}
}

// rewriteStoredDefinition rewrites one clause of the CREATE text SQLite stores
// for an object. A table constraint cannot be dropped by ALTER, and rebuilding a
// forty-column table would prove nothing about the clause under test; SQLite
// parses this text when it next opens the file, so the weakened definition is
// what the database then means.
//
// The rewrite is proven to have landed. A clause that matched nothing would
// leave the database honest and the case that used it vacuous.
func rewriteStoredDefinition(object, clause, replacement string) func(*testing.T, string) {
	return func(t *testing.T, path string) {
		t.Helper()

		db, openErr := sql.Open("sqlite", path)
		if openErr != nil {
			t.Fatalf("open %q to rewrite %q: %v", path, object, openErr)
		}
		var stored string
		if scanErr := db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE name = ?`,
			object,
		).Scan(&stored); scanErr != nil {
			t.Fatalf("read the stored definition of %q: %v", object, scanErr)
		}
		if !strings.Contains(stored, clause) {
			t.Fatalf("%q is no longer defined with %q; the rewrite would change nothing", object, clause)
		}
		for _, statement := range []struct {
			text string
			args []any
		}{
			{text: `PRAGMA writable_schema = ON`},
			{
				text: `UPDATE sqlite_master SET sql = ? WHERE name = ?`,
				args: []any{strings.Replace(stored, clause, replacement, 1), object},
			},
			{text: `PRAGMA writable_schema = OFF`},
		} {
			if _, execErr := db.Exec(statement.text, statement.args...); execErr != nil {
				t.Fatalf("rewrite the stored definition of %q with %q: %v", object, statement.text, execErr)
			}
		}
		if closeErr := db.Close(); closeErr != nil {
			t.Fatalf("close %q after rewriting %q: %v", path, object, closeErr)
		}
	}
}

// rebuiltRateCards replaces rate_cards with a table built from the migration's
// own CREATE text, one clause rewritten. Deriving it from the migration keeps
// that clause the only difference, so a rejection is evidence about the clause
// rather than about incidental drift in a hand-copied definition.
//
// The table is rebuilt rather than rewritten in place because dropping a UNIQUE
// constraint from the stored text alone would strand the implicit index SQLite
// built for it. Both immutability triggers go with the table and are recreated:
// a missing trigger is refused before any table definition is compared.
func rebuiltRateCards(t *testing.T, clause, replacement string) func(*testing.T, string) {
	t.Helper()

	create := ""
	for _, statement := range rateCardsAndFinalCostMigration().statements {
		if strings.HasPrefix(statement, "CREATE TABLE "+rateCardsTable) {
			create = statement
		}
	}
	if create == "" {
		t.Fatalf("the rate cards migration no longer creates %q", rateCardsTable)
	}
	if !strings.Contains(create, clause) {
		t.Fatalf("%q is no longer declared with %q; the rebuild would change nothing", rateCardsTable, clause)
	}
	return spoilWith(
		`DROP TRIGGER rate_cards_forbid_update`,
		`DROP TRIGGER rate_cards_forbid_delete`,
		`DROP TABLE rate_cards`,
		strings.Replace(create, clause, replacement, 1),
		`CREATE TRIGGER rate_cards_forbid_update BEFORE UPDATE ON rate_cards `+
			`BEGIN SELECT RAISE(ABORT, 'rate cards are immutable'); END`,
		`CREATE TRIGGER rate_cards_forbid_delete BEFORE DELETE ON rate_cards `+
			`BEGIN SELECT RAISE(ABORT, 'rate cards are immutable'); END`,
	)
}

// TestToolingRejectsRecordedSchemaObjectDefinitionsThatDivergeFromTheirMigration
// covers the database that holds every object its recorded version promises
// under the promised name, on the promised table, with the promised columns —
// and means something else by them. A trigger keeping its name while its body
// becomes `SELECT 1` leaves immutable rate cards editable; a unique partial
// index rebuilt without UNIQUE lets one response id be billed twice.
//
// Nothing before this catches either. The pages read back coherently, so
// `PRAGMA integrity_check` reports `ok`; the migration rows are still a valid
// prefix; and a recorded migration is never replayed, so restoring the snapshot
// and serving from it never puts the real definition back. A backup that
// certifies such a database is a backup that lies about what it holds.
func TestToolingRejectsRecordedSchemaObjectDefinitionsThatDivergeFromTheirMigration(t *testing.T) {
	t.Parallel()

	const gutted = `CREATE TRIGGER %s
		BEFORE %s ON accounting_requests
		WHEN NEW.lifecycle_state = 'finalized' AND NEW.completed_at IS NULL
		BEGIN
			SELECT RAISE(ABORT, 'invalid finalized accounting snapshot');
		END`
	for _, test := range []struct {
		name    string
		version int
		spoil   func(*testing.T, string)
		wantErr string
	}{
		{
			name:    "the rate card update guard has been made inert",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil: spoilWith(
				`DROP TRIGGER rate_cards_forbid_update`,
				`CREATE TRIGGER rate_cards_forbid_update BEFORE UPDATE ON rate_cards BEGIN SELECT 1; END`,
			),
			wantErr: `schema version 3 requires trigger "rate_cards_forbid_update" to match its migration ` +
				`definition, but this database defines it differently`,
		},
		{
			name:    "the rate card delete guard has been made inert",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil: spoilWith(
				`DROP TRIGGER rate_cards_forbid_delete`,
				`CREATE TRIGGER rate_cards_forbid_delete BEFORE DELETE ON rate_cards BEGIN SELECT 1; END`,
			),
			wantErr: `schema version 3 requires trigger "rate_cards_forbid_delete" to match its migration ` +
				`definition, but this database defines it differently`,
		},
		{
			name:    "rate cards no longer refuse a duplicate effective date",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil:   rebuiltRateCards(t, `UNIQUE(model, effective_from)`, `CHECK (model IS NOT NULL)`),
			wantErr: `schema version 3 requires table "rate_cards" to match its migration definition, ` +
				`but this database defines it differently`,
		},
		{
			name:    "a rate card column has lost its NOT NULL",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil:   rewriteStoredDefinition(rateCardsTable, `source TEXT NOT NULL`, `source TEXT`),
			wantErr: `schema version 3 requires column "source" on table "rate_cards" to match its migration ` +
				`definition, but this database declares it differently`,
		},
		{
			name:    "a rate card column has changed type",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil:   rewriteStoredDefinition(rateCardsTable, `created_at INTEGER NOT NULL`, `created_at TEXT NOT NULL`),
			wantErr: `schema version 3 requires column "created_at" on table "rate_cards" to match its migration ` +
				`definition, but this database declares it differently`,
		},
		{
			name:    "the rate card primary key has been dropped",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil:   rewriteStoredDefinition(rateCardsTable, `id INTEGER PRIMARY KEY`, `id INTEGER`),
			wantErr: `schema version 3 requires column "id" on table "rate_cards" to match its migration ` +
				`definition, but this database declares it differently`,
		},
		{
			name:    "accounting rows no longer check their cost basis",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil: rewriteStoredDefinition(
				accountingRequestsTable,
				`cost_basis TEXT CHECK (cost_basis IS NULL OR `+
					`cost_basis IN ('provider_reported', 'rate_card', 'zero_marginal', 'unknown'))`,
				`cost_basis TEXT`,
			),
			wantErr: `schema version 3 requires table "accounting_requests" to match its migration definition, ` +
				`but this database defines it differently`,
		},
		{
			name:    "accounting rows no longer reference a rate card",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil: rewriteStoredDefinition(
				accountingRequestsTable,
				` REFERENCES rate_cards(id)`,
				``,
			),
			wantErr: `schema version 3 requires table "accounting_requests" to match its migration definition, ` +
				`but this database defines it differently`,
		},
		{
			name:    "the response id index no longer enforces uniqueness",
			version: finalizedResponseAccountingMigrationVersion,
			spoil: spoilWith(
				`DROP INDEX accounting_requests_finalized_response_id`,
				`CREATE INDEX accounting_requests_finalized_response_id ON accounting_requests(response_id)
				WHERE lifecycle_state = 'finalized' AND response_id IS NOT NULL AND response_id <> ''`,
			),
			wantErr: `schema version 2 requires index "accounting_requests_finalized_response_id" to match its ` +
				`migration definition, but this database builds it differently`,
		},
		{
			name:    "the response id index no longer restricts itself to finalized rows",
			version: finalizedResponseAccountingMigrationVersion,
			spoil: spoilWith(
				`DROP INDEX accounting_requests_finalized_response_id`,
				`CREATE UNIQUE INDEX accounting_requests_finalized_response_id ON accounting_requests(response_id)`,
			),
			wantErr: `schema version 2 requires index "accounting_requests_finalized_response_id" to match its ` +
				`migration definition, but this database builds it differently`,
		},
		{
			name:    "the response id index covers another column",
			version: finalizedResponseAccountingMigrationVersion,
			spoil: spoilWith(
				`DROP INDEX accounting_requests_finalized_response_id`,
				`CREATE UNIQUE INDEX accounting_requests_finalized_response_id ON accounting_requests(stop_reason)
				WHERE lifecycle_state = 'finalized' AND response_id IS NOT NULL AND response_id <> ''`,
			),
			wantErr: `schema version 2 requires index "accounting_requests_finalized_response_id" to match its ` +
				`migration definition, but this database builds it differently`,
		},
		{
			name:    "the finalized update guard has been gutted",
			version: finalizedResponseAccountingMigrationVersion,
			spoil: spoilWith(
				`DROP TRIGGER accounting_requests_validate_finalized_snapshot`,
				fmt.Sprintf(gutted, "accounting_requests_validate_finalized_snapshot", "UPDATE"),
			),
			wantErr: `schema version 2 requires trigger "accounting_requests_validate_finalized_snapshot" to ` +
				`match its migration definition, but this database defines it differently`,
		},
		{
			name:    "the finalized insert guard has been gutted",
			version: importedLedgerProvenanceMigrationVersion,
			spoil: spoilWith(
				`DROP TRIGGER accounting_requests_validate_finalized_snapshot_insert`,
				fmt.Sprintf(gutted, "accounting_requests_validate_finalized_snapshot_insert", "INSERT"),
			),
			wantErr: `schema version 4 requires trigger "accounting_requests_validate_finalized_snapshot_insert" ` +
				`to match its migration definition, but this database defines it differently`,
		},
		{
			name:    "a summary index orders its columns differently",
			version: summaryQueryIndexesMigrationVersion,
			spoil: spoilWith(
				`DROP INDEX `+summaryWindowIndexName,
				`CREATE INDEX `+summaryWindowIndexName+` ON accounting_requests(request_kind, started_at)`,
			),
			wantErr: `schema version 6 requires index "` + summaryWindowIndexName + `" to match its migration ` +
				`definition, but this database builds it differently`,
		},
		{
			name:    "a summary index has lost a column",
			version: summaryQueryIndexesMigrationVersion,
			spoil: spoilWith(
				`DROP INDEX `+summaryContextWindowIndexName,
				`CREATE INDEX `+summaryContextWindowIndexName+` ON accounting_requests(route_context, started_at)`,
			),
			wantErr: `schema version 6 requires index "` + summaryContextWindowIndexName + `" to match its ` +
				`migration definition, but this database builds it differently`,
		},
		{
			name:    "the migration bookkeeping stamps a different default",
			version: 1,
			spoil: rewriteStoredDefinition(
				schemaMigrationsTable,
				`DEFAULT (unixepoch('subsec') * 1000000000)`,
				`DEFAULT 0`,
			),
			wantErr: `schema version 1 requires column "applied_at" on table "schema_migrations" to match its ` +
				`migration definition, but this database declares it differently`,
		},
	} {
		for _, tool := range toolingTools() {
			t.Run(test.name+"/"+tool.name, func(t *testing.T) {
				t.Parallel()

				source := filepath.Join(t.TempDir(), "usage.sqlite")
				olderSchemaToolingSource(t, source, test.version)
				test.spoil(t, source)
				before := toolingSourceBytes(t, source)

				dest := filepath.Join(t.TempDir(), "usage.out")
				err := tool.run(t.Context(), source, dest)
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("%s(diverged definition) error = %v; want %q", tool.name, err, test.wantErr)
				}
				if !strings.Contains(err.Error(), "not a patchbay usage database") {
					t.Errorf("%s(diverged definition) error = %v; want the usage-database refusal", tool.name, err)
				}
				assertRejectedToolingRunLeftNoOutput(t, source, dest, before)
			})
		}
	}
}

// plantReservedSchemaObject writes the `sqlite_master` row for an object whose
// name SQLite reserves for itself. `CREATE` refuses such a name outright, but a
// principal who can write the file can insert the row directly, and SQLite
// honors what it finds there — the planted trigger fires on every write while
// `PRAGMA integrity_check` still reports `ok`. Skipping reserved names to spare
// SQLite's own implicit indexes would therefore hand an attacker the one
// namespace the check does not look at.
func plantReservedSchemaObject(kind, name, table, create string) func(*testing.T, string) {
	return func(t *testing.T, path string) {
		t.Helper()

		db, openErr := sql.Open("sqlite", path)
		if openErr != nil {
			t.Fatalf("open %q to plant %q: %v", path, name, openErr)
		}
		for _, statement := range []struct {
			text string
			args []any
		}{
			{text: `PRAGMA writable_schema = ON`},
			{
				text: `INSERT INTO sqlite_master(type, name, tbl_name, rootpage, sql) VALUES (?, ?, ?, 0, ?)`,
				args: []any{kind, name, table, create},
			},
			{text: `PRAGMA writable_schema = OFF`},
		} {
			if _, execErr := db.Exec(statement.text, statement.args...); execErr != nil {
				t.Fatalf("plant %q in %q with %q: %v", name, path, statement.text, execErr)
			}
		}
		if closeErr := db.Close(); closeErr != nil {
			t.Fatalf("close %q after planting %q: %v", path, name, closeErr)
		}
	}
}

// destructiveLedgerTrigger is the attack the extra-object sweep exists to stop:
// a trigger nobody declared, attached to the table that holds every accounting
// row, which empties it the moment anything writes.
func destructiveLedgerTrigger(name, event string) string {
	return `CREATE TRIGGER ` + name + ` AFTER ` + event + ` ON accounting_requests
		BEGIN
			DELETE FROM accounting_requests;
		END`
}

// TestUndeclaredManagedTriggersAreActiveNotInert establishes that an extra
// object on a managed table is a live capability rather than clutter, which is
// what makes accepting one a defect rather than a leniency. Both shapes leave
// every canonical definition exact, so nothing the recorded-schema check
// compared before this could see them, and both leave `PRAGMA integrity_check`
// reporting `ok`.
func TestUndeclaredManagedTriggersAreActiveNotInert(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		spoil func(*testing.T, string)
	}{
		{
			name:  "a trigger created under an ordinary name",
			spoil: spoilWith(destructiveLedgerTrigger("accounting_requests_purge", "INSERT")),
		},
		{
			name: "a trigger planted under a name SQLite reserves",
			spoil: plantReservedSchemaObject(
				"trigger",
				"sqlite_accounting_requests_purge",
				accountingRequestsTable,
				destructiveLedgerTrigger("sqlite_accounting_requests_purge", "INSERT"),
			),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			seedToolingSource(t, path, 3)
			test.spoil(t, path)

			db := inspectDB(t, path)
			var integrity string
			if integrityErr := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); integrityErr != nil {
				t.Fatalf("integrity check the tampered database: %v", integrityErr)
			}
			if integrity != "ok" {
				t.Fatalf("integrity check = %q; want ok, so the tampering is invisible to it", integrity)
			}
			if _, execErr := db.Exec(
				`INSERT INTO accounting_requests(
					started_at, request_kind, lifecycle_state, upstream_contacted, route_host, route_context,
					route_upstream, request_model, resolved_model, auth_mode, credential_class, billing_policy,
					cost_status
				) VALUES (1, 'messages', 'pending', 0, 'h', 'c', 'u', 'm', 'm', 'a', 'c', 'b', 'unknown')`,
			); execErr != nil {
				t.Fatalf("write to the tampered ledger: %v", execErr)
			}
			var remaining int
			if countErr := db.QueryRow(`SELECT COUNT(*) FROM accounting_requests`).Scan(&remaining); countErr != nil {
				t.Fatalf("count ledger rows: %v", countErr)
			}
			if remaining != 0 {
				t.Errorf(
					"ledger rows after one write = %d; want 0, since the undeclared trigger fires on every write",
					remaining,
				)
			}
			if closeErr := db.Close(); closeErr != nil {
				t.Fatalf("close the tampered database: %v", closeErr)
			}
		})
	}
}

// TestToolingRejectsUndeclaredActiveObjectsOnManagedTables covers the database
// that holds every object its recorded version promises, each one exactly as its
// migration declared it, and holds something extra beside them. Every check that
// runs before this accepts it: the recorded migrations are a valid prefix, the
// pages read back coherently, each canonical definition compares equal, pending
// migrations no-op or succeed, and `VACUUM INTO` copies the addition forward
// intact. Yet a trigger the migrations never created runs on every write to the
// table it is attached to, and an index they never created decides which writes
// that table accepts at all — so a snapshot of such a database certifies a ledger
// that empties itself, or one that silently refuses rows Patchbay must record.
func TestToolingRejectsUndeclaredActiveObjectsOnManagedTables(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		version int
		spoil   func(*testing.T, string)
		wantErr string
	}{
		{
			name:    "the ledger has gained a destructive after-update trigger",
			version: summaryQueryIndexesMigrationVersion,
			spoil:   spoilWith(destructiveLedgerTrigger("accounting_requests_purge", "UPDATE")),
			wantErr: `schema version 6 does not declare trigger "accounting_requests_purge" ` +
				`on table "accounting_requests"`,
		},
		{
			name:    "the ledger has gained a destructive after-insert trigger",
			version: summaryQueryIndexesMigrationVersion,
			spoil:   spoilWith(destructiveLedgerTrigger("accounting_requests_purge", "INSERT")),
			wantErr: `schema version 6 does not declare trigger "accounting_requests_purge" ` +
				`on table "accounting_requests"`,
		},
		{
			name:    "the ledger has gained a destructive after-delete trigger",
			version: summaryQueryIndexesMigrationVersion,
			spoil:   spoilWith(destructiveLedgerTrigger("accounting_requests_purge", "DELETE")),
			wantErr: `schema version 6 does not declare trigger "accounting_requests_purge" ` +
				`on table "accounting_requests"`,
		},
		{
			name:    "an older schema has gained a destructive ledger trigger",
			version: finalizedResponseAccountingMigrationVersion,
			spoil:   spoilWith(destructiveLedgerTrigger("accounting_requests_purge", "INSERT")),
			wantErr: `schema version 2 does not declare trigger "accounting_requests_purge" ` +
				`on table "accounting_requests"`,
		},
		{
			name:    "a trigger has been planted on the ledger under a reserved name",
			version: summaryQueryIndexesMigrationVersion,
			spoil: plantReservedSchemaObject(
				"trigger",
				"sqlite_accounting_requests_purge",
				accountingRequestsTable,
				destructiveLedgerTrigger("sqlite_accounting_requests_purge", "INSERT"),
			),
			wantErr: `schema version 6 does not declare trigger "sqlite_accounting_requests_purge" ` +
				`on table "accounting_requests"`,
		},
		{
			name:    "the rate cards table has gained a trigger",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil: spoilWith(
				`CREATE TRIGGER rate_cards_rewrite AFTER INSERT ON rate_cards
				BEGIN
					DELETE FROM accounting_requests;
				END`,
			),
			wantErr: `schema version 3 does not declare trigger "rate_cards_rewrite" on table "rate_cards"`,
		},
		{
			name:    "the migration bookkeeping table has gained a trigger",
			version: 1,
			spoil: spoilWith(
				`CREATE TRIGGER schema_migrations_rewrite AFTER INSERT ON schema_migrations
				BEGIN
					DELETE FROM accounting_requests;
				END`,
			),
			wantErr: `schema version 1 does not declare trigger "schema_migrations_rewrite" ` +
				`on table "schema_migrations"`,
		},
		{
			name:    "the ledger has gained a unique index",
			version: summaryQueryIndexesMigrationVersion,
			spoil: spoilWith(
				`CREATE UNIQUE INDEX accounting_requests_one_per_context ON accounting_requests(route_context)`,
			),
			wantErr: `schema version 6 does not declare index "accounting_requests_one_per_context" ` +
				`on table "accounting_requests"`,
		},
		{
			name:    "the ledger has gained a partial unique index",
			version: summaryQueryIndexesMigrationVersion,
			spoil: spoilWith(
				`CREATE UNIQUE INDEX accounting_requests_one_pending ON accounting_requests(route_context)
				WHERE lifecycle_state = 'pending'`,
			),
			wantErr: `schema version 6 does not declare index "accounting_requests_one_pending" ` +
				`on table "accounting_requests"`,
		},
		{
			name:    "the ledger has gained an expression unique index",
			version: summaryQueryIndexesMigrationVersion,
			spoil: spoilWith(
				`CREATE UNIQUE INDEX accounting_requests_one_model ON accounting_requests(lower(resolved_model))`,
			),
			wantErr: `schema version 6 does not declare index "accounting_requests_one_model" ` +
				`on table "accounting_requests"`,
		},
		{
			name:    "an older schema has gained a unique index",
			version: finalizedResponseAccountingMigrationVersion,
			spoil: spoilWith(
				`CREATE UNIQUE INDEX accounting_requests_one_per_context ON accounting_requests(route_context)`,
			),
			wantErr: `schema version 2 does not declare index "accounting_requests_one_per_context" ` +
				`on table "accounting_requests"`,
		},
		{
			name:    "the rate cards table has gained a unique index",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil:   spoilWith(`CREATE UNIQUE INDEX rate_cards_one_per_model ON rate_cards(model)`),
			wantErr: `schema version 3 does not declare index "rate_cards_one_per_model" on table "rate_cards"`,
		},
		{
			// Refused under the stricter contract: an index needs neither
			// UNIQUE nor a partial clause to decide writes. A key over an
			// expression or a collation the writing build cannot evaluate
			// fails every insert into the table it covers.
			name:    "the ledger has gained a nonunique index",
			version: summaryQueryIndexesMigrationVersion,
			spoil:   spoilWith(`CREATE INDEX accounting_requests_by_model ON accounting_requests(resolved_model)`),
			wantErr: `schema version 6 does not declare index "accounting_requests_by_model" ` +
				`on table "accounting_requests"`,
		},
		{
			// A name is not an identity: SQLite keeps triggers and indexes in
			// separate namespaces, so this trigger sits beside the canonical
			// index of the same name rather than replacing it. Every declared
			// object is present and exact, and the ledger empties on every
			// write.
			name:    "a destructive trigger has taken the name of a canonical index",
			version: summaryQueryIndexesMigrationVersion,
			spoil: spoilWith(
				`DROP INDEX `+summaryWindowIndexName,
				destructiveLedgerTrigger(summaryWindowIndexName, "INSERT"),
				canonicalSummaryWindowIndex,
			),
			wantErr: `schema version 6 does not declare trigger "` + summaryWindowIndexName + `" ` +
				`on table "accounting_requests"`,
		},
		{
			name:    "an index has taken the name of a canonical trigger",
			version: rateCardsAndFinalCostMigrationVersion,
			spoil: spoilWith(
				`DROP TRIGGER rate_cards_forbid_update`,
				`CREATE INDEX rate_cards_forbid_update ON rate_cards(source)`,
				canonicalRateCardsForbidUpdate,
			),
			wantErr: `schema version 3 does not declare index "rate_cards_forbid_update" on table "rate_cards"`,
		},
		{
			name:    "an index has taken the name of a canonical trigger on another managed table",
			version: summaryQueryIndexesMigrationVersion,
			spoil: spoilWith(
				`CREATE INDEX accounting_requests_validate_finalized_snapshot ON rate_cards(model)`,
			),
			wantErr: `schema version 6 does not declare index "accounting_requests_validate_finalized_snapshot" ` +
				`on table "rate_cards"`,
		},
	} {
		for _, tool := range toolingTools() {
			t.Run(test.name+"/"+tool.name, func(t *testing.T) {
				t.Parallel()

				source := filepath.Join(t.TempDir(), "usage.sqlite")
				olderSchemaToolingSource(t, source, test.version)
				test.spoil(t, source)
				before := toolingSourceBytes(t, source)

				dest := filepath.Join(t.TempDir(), "usage.out")
				err := tool.run(t.Context(), source, dest)
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("%s(undeclared object) error = %v; want %q", tool.name, err, test.wantErr)
				}
				if !strings.Contains(err.Error(), "not a patchbay usage database") {
					t.Errorf("%s(undeclared object) error = %v; want the usage-database refusal", tool.name, err)
				}
				assertRejectedToolingRunLeftNoOutput(t, source, dest, before)
			})
		}
	}
}

// TestToolingAcceptsExtraObjectsOffTheManagedTables pins the other half of the
// contract. The sweep refuses additions that can act on Patchbay's own tables;
// it must not turn into an inventory of the whole file, because a table or view
// somebody keeps alongside the ledger — and the triggers and indexes attached to
// that table — can only run when something writes to it, which Patchbay never
// does.
func TestToolingAcceptsExtraObjectsOffTheManagedTables(t *testing.T) {
	t.Parallel()

	for _, tool := range toolingTools() {
		t.Run(tool.name, func(t *testing.T) {
			t.Parallel()

			source := filepath.Join(t.TempDir(), "usage.sqlite")
			seedToolingSource(t, source, 2)
			spoilToolingSource(
				t,
				source,
				`CREATE TABLE operator_notes (id INTEGER PRIMARY KEY, note TEXT NOT NULL)`,
				`CREATE VIEW recent_requests AS SELECT id, started_at FROM accounting_requests`,
				`CREATE TRIGGER operator_notes_stamp AFTER INSERT ON operator_notes
				BEGIN
					UPDATE operator_notes SET note = note WHERE id = NEW.id;
				END`,
				`CREATE INDEX operator_notes_by_note ON operator_notes(note)`,
				`CREATE UNIQUE INDEX operator_notes_unique_note ON operator_notes(note)`,
			)

			dest := filepath.Join(t.TempDir(), "usage.out")
			if err := tool.run(t.Context(), source, dest); err != nil {
				t.Fatalf("%s(extra objects off the managed tables) error = %v; want acceptance", tool.name, err)
			}
			if _, statErr := os.Stat(dest); statErr != nil {
				t.Fatalf("%s produced no output: %v", tool.name, statErr)
			}
		})
	}
}

// TestMigratedCloneRejectsUndeclaredObjectsThatSurvivePendingMigrations pins the
// clone-side half of the sweep. Migrations create objects; they never sweep away
// ones they do not know about, so an addition made against an older schema is
// still attached to the table after the clone is brought forward — and is then
// undeclared at the newer version too. The source gate refuses such a database
// first, which is exactly why the clone-side check has to be proven directly:
// defense that only ever runs behind another check is defense nobody has tested.
func TestMigratedCloneRejectsUndeclaredObjectsThatSurvivePendingMigrations(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		through int
		spoil   []string
		want    string
	}{
		{
			name:    "a trigger the older schema never declared either",
			through: finalizedResponseAccountingMigrationVersion,
			spoil:   []string{destructiveLedgerTrigger("accounting_requests_purge", "INSERT")},
			want: `does not declare trigger "accounting_requests_purge" ` +
				`on table "accounting_requests"`,
		},
		{
			// The pending migration creates an index under the name this
			// trigger already holds, and SQLite keeps the two side by side. So
			// the collision does not exist at the version the source records —
			// only at the one the clone is brought to, which is precisely the
			// version the clone-side sweep judges it at.
			name:    "a trigger whose name only a pending migration turns canonical",
			through: upstreamTargetDispatchEvidenceMigrationVersion,
			spoil:   []string{destructiveLedgerTrigger(summaryWindowIndexName, "INSERT")},
			want: `does not declare trigger "` + summaryWindowIndexName + `" ` +
				`on table "accounting_requests"`,
		},
		{
			name:    "an index carrying the name of a trigger the older schema declares",
			through: importedLedgerProvenanceMigrationVersion,
			spoil: []string{
				`DROP TRIGGER rate_cards_forbid_delete`,
				`CREATE INDEX rate_cards_forbid_delete ON rate_cards(source)`,
				`CREATE TRIGGER rate_cards_forbid_delete
				BEFORE DELETE ON rate_cards
				BEGIN
					SELECT RAISE(ABORT, 'rate cards are immutable');
				END`,
			},
			want: `does not declare index "rate_cards_forbid_delete" on table "rate_cards"`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			clone := filepath.Join(t.TempDir(), "usage.snapshot.sqlite"+toolingMigrationCloneSuffix)
			olderSchemaToolingSource(t, clone, test.through)
			spoilToolingSource(t, clone, test.spoil...)

			err := migrateToolingMigrationClone(t.Context(), clone)
			want := `schema version ` + fmt.Sprint(latestSchemaVersion()) + ` ` + test.want
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("migrateToolingMigrationClone(clone carrying an undeclared object) = %v; want %q", err, want)
			}
			if !strings.Contains(err.Error(), clone) {
				t.Errorf("migrateToolingMigrationClone(spoiled clone) = %v; want it to name %q", err, clone)
			}
		})
	}
}

// TestSnapshotMigrationCloneIsPrivateDisposableAndLeavesNothing inspects the
// clone while it exists, then proves the destination directory holds nothing but
// the published snapshot. The clone is a full copy of the accounting database on
// the backup filesystem and it gets written to, so leaving one behind would both
// duplicate the data and strand a half-migrated database next to a good backup.
func TestSnapshotMigrationCloneIsPrivateDisposableAndLeavesNothing(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	seedToolingSource(t, source, 2)

	destDir := t.TempDir()
	dest := filepath.Join(destDir, "usage.snapshot.sqlite")
	var clonePath, stagingDir string
	ctx := context.WithValue(t.Context(), toolingMigrationCloneHookKey{}, func(clone string) {
		clonePath = clone
		artifact := strings.TrimSuffix(clone, toolingMigrationCloneSuffix)
		if artifact == clone {
			t.Errorf("migration clone %q does not sit beside its artifact", clone)
			return
		}
		stagingDir = filepath.Dir(clone)
		info, statErr := os.Lstat(clone)
		if statErr != nil {
			t.Errorf("stat migration clone %q: %v", clone, statErr)
			return
		}
		if !info.Mode().IsRegular() {
			t.Errorf("migration clone %q mode = %v; want a regular file", clone, info.Mode())
		}
		if got := info.Mode().Perm(); got != databaseMode {
			t.Errorf("migration clone %q mode = %#o; want %#o", clone, got, databaseMode)
		}
		if artifactDir := filepath.Dir(artifact); artifactDir != stagingDir {
			t.Errorf("migration clone %q is not inside the artifact's staging directory %q", clone, artifactDir)
		}
		if !bytes.Equal(toolingSourceBytes(t, clone), toolingSourceBytes(t, artifact)) {
			t.Errorf("migration clone %q is not a byte copy of the artifact %q", clone, artifact)
		}
	})

	if err := Snapshot(ctx, source, dest); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if clonePath == "" {
		t.Fatal("the forward-migration proof never ran; nothing about the clone was observed")
	}
	for _, residue := range append([]string{clonePath}, cloneSidecarPaths(clonePath)...) {
		if _, statErr := os.Lstat(residue); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("migration clone residue %q survived a successful snapshot: %v", residue, statErr)
		}
	}
	if _, statErr := os.Lstat(stagingDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("staging directory %q survived a successful snapshot: %v", stagingDir, statErr)
	}
	assertDestinationDirectoryHoldsOnly(t, destDir, filepath.Base(dest))
}

// cloneSidecarPaths names every SQLite companion file a migrating connection can
// derive from the clone's pathname.
func cloneSidecarPaths(clone string) []string {
	paths := make([]string, 0, len(databaseSidecarSuffixes()))
	for _, suffix := range databaseSidecarSuffixes() {
		paths = append(paths, clone+suffix)
	}
	return paths
}

func assertDestinationDirectoryHoldsOnly(t *testing.T, dir string, want ...string) {
	t.Helper()

	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read destination directory %q: %v", dir, readErr)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if len(got) != len(want) {
		t.Fatalf("destination directory %q holds %v; want exactly %v", dir, got, want)
	}
	for index, name := range want {
		if got[index] != name {
			t.Fatalf("destination directory %q holds %v; want exactly %v", dir, got, want)
		}
	}
}

// TestSnapshotRefusesAnOccupiedMigrationClonePath pins exclusive creation. A
// clone path that already holds a file is either a concurrent run or a plant, and
// opening it without O_EXCL would migrate — and then trust — whatever was already
// there, or truncate a live peer's clone out from under it.
func TestSnapshotRefusesAnOccupiedMigrationClonePath(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	seedToolingSource(t, source, 1)
	before := toolingSourceBytes(t, source)

	destDir := t.TempDir()
	dest := filepath.Join(destDir, "usage.snapshot.sqlite")
	occupied := ""
	ctx := context.WithValue(t.Context(), toolingBeforePublishHookKey{}, func(temp string) {
		occupied = temp + toolingMigrationCloneSuffix
		if writeErr := os.WriteFile(occupied, []byte("occupied"), databaseMode); writeErr != nil {
			t.Errorf("occupy the migration clone path %q: %v", occupied, writeErr)
		}
	})

	err := Snapshot(ctx, source, dest)
	if occupied == "" {
		t.Fatal("the before-publish hook never ran; the clone path was never occupied")
	}
	if err == nil || !strings.Contains(err.Error(), "create usage migration clone") {
		t.Fatalf("Snapshot(occupied clone path) error = %v; want an exclusive-creation refusal", err)
	}
	if !strings.Contains(err.Error(), "file exists") {
		t.Errorf("Snapshot(occupied clone path) error = %v; want it to report the existing file", err)
	}
	assertRejectedToolingRunLeftNoOutput(t, source, dest, before)
	assertDestinationDirectoryHoldsOnly(t, destDir)
}

// TestSnapshotMigrationCloneAcceptsADestinationTheSourcePolicyWouldRefuse extends
// the artifact's trust split to the clone. The clone is created and migrated on
// the destination filesystem, which in deployment is an all-squashed NAS export
// reporting one fixed remote owner Patchbay never runs as. Holding the clone to
// the strict source policy would make the forward-migration proof fail on exactly
// the destination it is meant to protect. A world-writable, non-sticky
// destination directory reproduces that refusal locally without root.
func TestSnapshotMigrationCloneAcceptsADestinationTheSourcePolicyWouldRefuse(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	seedToolingSource(t, source, 1)

	destDir := filepath.Join(t.TempDir(), "squashed")
	if err := os.Mkdir(destDir, 0o700); err != nil {
		t.Fatalf("create destination directory: %v", err)
	}
	if err := os.Chmod(destDir, 0o777); err != nil {
		t.Fatalf("expose destination directory: %v", err)
	}
	dest := filepath.Join(destDir, "usage.snapshot.sqlite")

	proved := false
	var destinationErr, strictErr error
	ctx := context.WithValue(t.Context(), toolingMigrationCloneHookKey{}, func(clone string) {
		proved = true
		_, destinationErr = inspectGeneratedToolingDatabase(clone)
		_, strictErr = openSecureToolingSource(clone)
	})
	if err := Snapshot(ctx, source, dest); err != nil {
		t.Fatalf("Snapshot(all-squash-shaped destination) error = %v; want publication", err)
	}
	if !proved {
		t.Fatal("the forward-migration proof never ran on the exposed destination")
	}
	if destinationErr != nil {
		t.Errorf(
			"inspectGeneratedToolingDatabase(clone) error = %v; want destination trust to accept it",
			destinationErr,
		)
	}
	if strictErr == nil || !strings.Contains(strictErr.Error(), "replaceable by another local user") {
		t.Errorf(
			"openSecureToolingSource(clone) error = %v; want the strict source policy to refuse the very path the clone uses",
			strictErr,
		)
	}
	assertMode(t, dest, databaseMode)
	assertDestinationDirectoryHoldsOnly(t, destDir, filepath.Base(dest))
}

// TestToolingIntegrityResultRejectsEverythingButOneCanonicalOK pins how the
// PRAGMA's answer is read. SQLite spells success exactly one way and reports
// faults as ordinary rows, so a check that stops at the first row, trims it, or
// compares case-insensitively passes a corrupt database while looking correct.
func TestToolingIntegrityResultRejectsEverythingButOneCanonicalOK(t *testing.T) {
	t.Parallel()

	const path = "/srv/usage.sqlite"
	if err := toolingIntegrityResultError(path, []string{"ok"}); err != nil {
		t.Fatalf("toolingIntegrityResultError(single ok) = %v; want acceptance", err)
	}
	for _, test := range []struct {
		name     string
		reported []string
	}{
		{name: "no result rows at all", reported: nil},
		{name: "an empty result set", reported: []string{}},
		{name: "ok repeated", reported: []string{"ok", "ok"}},
		{name: "ok followed by a fault", reported: []string{"ok", "row 3 missing from index summary"}},
		{name: "a fault followed by ok", reported: []string{"wrong # of entries in index summary", "ok"}},
		{name: "a single fault", reported: []string{"wrong # of entries in index summary"}},
		{name: "ok in the wrong case", reported: []string{"OK"}},
		{name: "ok with a trailing newline", reported: []string{"ok\n"}},
		{name: "ok with leading space", reported: []string{" ok"}},
		{name: "ok as a substring", reported: []string{"not ok"}},
		{name: "an empty result", reported: []string{""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := toolingIntegrityResultError(path, test.reported)
			if err == nil {
				t.Fatalf("toolingIntegrityResultError(%q) = nil; want a refusal", test.reported)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("toolingIntegrityResultError(%q) = %v; want it to name %q", test.reported, err, path)
			}
			for _, reported := range test.reported {
				if !strings.Contains(err.Error(), reported) {
					t.Errorf(
						"toolingIntegrityResultError(%q) = %v; want it to carry the reported %q",
						test.reported,
						err,
						reported,
					)
				}
			}
		})
	}
}

// TestVerifyToolingIntegrityReportsFailedQueriesAndIterations covers the two ways
// the check can fail without producing a verdict. Both must be refusals: a check
// that could not finish establishes nothing, and treating it as silence would
// accept the very database the check was run against.
func TestVerifyToolingIntegrityReportsFailedQueriesAndIterations(t *testing.T) {
	t.Parallel()

	t.Run("query cannot run", func(t *testing.T) {
		t.Parallel()

		source := filepath.Join(t.TempDir(), "usage.sqlite")
		seedToolingSource(t, source, 1)
		db, openErr := sql.Open("sqlite", source)
		if openErr != nil {
			t.Fatal(openErr)
		}
		if closeErr := db.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		err := verifyToolingIntegrity(t.Context(), db, source)
		if err == nil || !strings.Contains(err.Error(), "check usage database") {
			t.Fatalf("verifyToolingIntegrity(closed database) = %v; want a query refusal", err)
		}
		if !strings.Contains(err.Error(), source) {
			t.Errorf("verifyToolingIntegrity(closed database) = %v; want it to name %q", err, source)
		}
	})

	t.Run("iteration aborts partway", func(t *testing.T) {
		t.Parallel()

		source := filepath.Join(t.TempDir(), "usage.sqlite")
		seedToolingSource(t, source, 3)
		// An unusable page type stops the walk mid-result: SQLite emits the
		// faults it found, then fails the statement rather than finishing.
		damageIndexRootPage(t, source, summaryWindowIndexName, func(header []byte) { header[0] = 0 })
		db, openErr := sql.Open("sqlite", source)
		if openErr != nil {
			t.Fatal(openErr)
		}
		err := verifyToolingIntegrity(t.Context(), db, source)
		if closeErr := db.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err == nil {
			t.Fatal("verifyToolingIntegrity(unreadable index page) = nil; want a refusal")
		}
		if !strings.Contains(err.Error(), source) {
			t.Errorf("verifyToolingIntegrity(unreadable index page) = %v; want it to name %q", err, source)
		}
	})
}

// TestValidateSnapshotMigrationCloneDisposesOfItsCloneEitherWay proves the proof
// cleans up after itself rather than relying on the staging directory being
// removed later. The clone is a second full copy of the accounting database on
// the backup filesystem, and it is the copy that gets written to, so it must not
// still be there when the finished snapshot is published beside it — nor may the
// proof alter the artifact it is vouching for.
func TestValidateSnapshotMigrationCloneDisposesOfItsCloneEitherWay(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		build   func(t *testing.T, artifact string)
		wantErr string
	}{
		{
			name:  "the proof succeeds",
			build: func(t *testing.T, artifact string) { t.Helper(); seedToolingSource(t, artifact, 2) },
		},
		{
			name: "the proof fails",
			build: func(t *testing.T, artifact string) {
				t.Helper()
				seedToolingSource(t, artifact, 2)
				forgetMigrationRecord(t, artifact, summaryQueryIndexesMigrationVersion)
			},
			wantErr: "migration clone",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			artifact := filepath.Join(t.TempDir(), "usage.snapshot.sqlite")
			test.build(t, artifact)
			before := toolingSourceBytes(t, artifact)

			err := validateSnapshotMigrationClone(t.Context(), artifact)
			switch {
			case test.wantErr == "" && err != nil:
				t.Fatalf("validateSnapshotMigrationClone() = %v; want acceptance", err)
			case test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)):
				t.Fatalf("validateSnapshotMigrationClone() = %v; want %q", err, test.wantErr)
			}

			clone := artifact + toolingMigrationCloneSuffix
			for _, residue := range append([]string{clone}, cloneSidecarPaths(clone)...) {
				if _, statErr := os.Lstat(residue); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("migration clone residue %q survived: %v", residue, statErr)
				}
			}
			if !bytes.Equal(toolingSourceBytes(t, artifact), before) {
				t.Error("the forward-migration proof modified the artifact it was vouching for")
			}
		})
	}
}

// TestMigrateSnapshotMigrationCloneRevalidatesTheMigratedResult pins the checks
// that run after the migrations do. A migration that returns without an error has
// only established that its statements executed; whether the database it produced
// is coherent and still a Patchbay usage database is a separate question, and
// answering it by assumption is what publishing an unusable backup looks like.
func TestMigrateSnapshotMigrationCloneRevalidatesTheMigratedResult(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		spoil   func(t *testing.T, clone string)
		wantErr string
	}{
		{
			name:    "migrations run against a database that no longer reads back",
			spoil:   func(t *testing.T, clone string) { t.Helper(); damageSummaryIndex(t, clone) },
			wantErr: "failed integrity check",
		},
		{
			name:    "migrations no-op over a schema whose tables are gone",
			spoil:   func(t *testing.T, clone string) { t.Helper(); dropAccountingRequests(t, clone) },
			wantErr: "not a patchbay usage database",
		},
		{
			// The migrations that created rate_cards are recorded, so they are
			// skipped rather than replayed, and the migrations that do run
			// succeed without it. Only a check of the objects the recorded
			// versions promised sees that the table is gone.
			name:    "migrations no-op over a schema whose recorded objects are gone",
			spoil:   func(t *testing.T, clone string) { t.Helper(); spoilToolingSource(t, clone, `DROP TABLE rate_cards`) },
			wantErr: `requires table "rate_cards", which is missing`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			clone := filepath.Join(t.TempDir(), "usage.snapshot.sqlite"+toolingMigrationCloneSuffix)
			seedToolingSource(t, clone, 3)
			if err := migrateToolingMigrationClone(t.Context(), clone); err != nil {
				t.Fatalf("migrateToolingMigrationClone(healthy clone) = %v; want acceptance", err)
			}
			test.spoil(t, clone)

			err := migrateToolingMigrationClone(t.Context(), clone)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("migrateToolingMigrationClone(spoiled clone) = %v; want %q", err, test.wantErr)
			}
			if !strings.Contains(err.Error(), clone) {
				t.Errorf("migrateToolingMigrationClone(spoiled clone) = %v; want it to name %q", err, clone)
			}
		})
	}
}

// damageSummaryIndex leaves the tables readable and the summary index not, which
// no migration touches and no schema read notices.
func damageSummaryIndex(t *testing.T, path string) {
	t.Helper()

	damageIndexRootPage(t, path, summaryWindowIndexName, func(header []byte) { header[7] = 9 })
}

// dropAccountingRequests removes the table while leaving every schema_migrations
// row in place, so `migrate` finds nothing left to apply and returns success over
// a database that is no longer a usage database.
func dropAccountingRequests(t *testing.T, path string) {
	t.Helper()

	db, openErr := sql.Open("sqlite", path)
	if openErr != nil {
		t.Fatalf("open %q to drop accounting_requests: %v", path, openErr)
	}
	if _, execErr := db.Exec(`DROP TABLE accounting_requests`); execErr != nil {
		t.Fatalf("drop accounting_requests in %q: %v", path, execErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close %q after dropping accounting_requests: %v", path, closeErr)
	}
}

// TestRemoveSnapshotMigrationCloneClearsEveryDerivedPath pins the cleanup itself.
// A migrating connection writes, so the rollback journal exists for the duration
// of each migration; removing only the clone would leave the residue that made
// the cleanup necessary in the first place.
func TestRemoveSnapshotMigrationCloneClearsEveryDerivedPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	clone := filepath.Join(dir, "usage.snapshot.sqlite"+toolingMigrationCloneSuffix)
	paths := append([]string{clone}, cloneSidecarPaths(clone)...)
	for _, path := range paths {
		if writeErr := os.WriteFile(path, []byte("residue"), databaseMode); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := removeToolingMigrationClone(clone); err != nil {
		t.Fatalf("removeToolingMigrationClone() = %v; want a clean removal", err)
	}
	for _, path := range paths {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("clone residue %q survived cleanup: %v", path, statErr)
		}
	}
	if err := removeToolingMigrationClone(clone); err != nil {
		t.Errorf("removeToolingMigrationClone(already clean) = %v; want absent paths to be no-ops", err)
	}

	// Every removal is attempted and every failure is reported, so one
	// unremovable path cannot mask the state of the others.
	blocked := filepath.Join(dir, "blocked.sqlite"+toolingMigrationCloneSuffix)
	stubborn := blocked + databaseSidecarSuffixes()[0]
	if mkdirErr := os.Mkdir(stubborn, 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	if writeErr := os.WriteFile(filepath.Join(stubborn, "occupant"), []byte("x"), databaseMode); writeErr != nil {
		t.Fatal(writeErr)
	}
	if writeErr := os.WriteFile(blocked, []byte("residue"), databaseMode); writeErr != nil {
		t.Fatal(writeErr)
	}
	removeErr := removeToolingMigrationClone(blocked)
	if removeErr == nil || !strings.Contains(removeErr.Error(), stubborn) {
		t.Fatalf("removeToolingMigrationClone(unremovable sidecar) = %v; want it to name %q", removeErr, stubborn)
	}
	if _, statErr := os.Lstat(blocked); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("clone %q survived a cleanup that failed on a different path: %v", blocked, statErr)
	}
}

func toolingStagingMatches(t *testing.T, dest string) []string {
	t.Helper()
	matches, err := filepath.Glob(
		filepath.Join(filepath.Dir(dest), toolingStagingNamePrefix(dest)+"*"),
	)
	if err != nil {
		t.Fatalf("glob tooling staging directories: %v", err)
	}
	return matches
}

func TestSnapshotCopiesConsistentDatabase(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, source)
	if _, err := store.InsertPending(t.Context(), validPendingRequest()); err != nil {
		t.Fatalf("seed request: %v", err)
	}
	if err := os.Chmod(source, 0o644); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "usage.snapshot.sqlite")
	if err := Snapshot(t.Context(), source, dest); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	assertMode(t, dest, 0o600)
	assertMode(t, source, 0o644)

	db := inspectDB(t, dest)
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity check = %q; want ok", integrity)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM accounting_requests`).Scan(&rows); err != nil {
		t.Fatalf("count snapshot rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("snapshot rows = %d; want 1", rows)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close snapshot inspection: %v", err)
	}

	reopened, err := Open(dest)
	if err != nil {
		t.Fatalf("Open(snapshot) error = %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened snapshot: %v", err)
	}

	restored := filepath.Join(t.TempDir(), "restored.sqlite")
	contents, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read snapshot for restore: %v", err)
	}
	if err := os.WriteFile(restored, contents, 0o600); err != nil {
		t.Fatalf("copy snapshot for restore: %v", err)
	}
	restoredStore, err := Open(restored)
	if err != nil {
		t.Fatalf("Open(restored snapshot) error = %v", err)
	}
	if err := restoredStore.db.QueryRow(`SELECT COUNT(*) FROM accounting_requests`).Scan(&rows); err != nil {
		t.Fatalf("count restored rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("restored rows = %d; want 1", rows)
	}
	if err := restoredStore.Close(); err != nil {
		t.Fatalf("close restored snapshot: %v", err)
	}
}

func TestSnapshotStagesPrivatelyBeforeAtomicPublish(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, source)
	if _, err := store.InsertPending(t.Context(), validPendingRequest()); err != nil {
		t.Fatalf("seed request: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "usage.snapshot.sqlite")
	staged := make(chan string, 1)
	release := make(chan struct{})
	ctx := context.WithValue(t.Context(), toolingBeforePublishHookKey{}, func(temp string) {
		staged <- temp
		<-release
	})
	result := make(chan error, 1)
	go func() { result <- Snapshot(ctx, source, dest) }()

	var temp string
	select {
	case temp = <-staged:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot did not reach its staged artifact")
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination exists before publish: %v", err)
	}
	stageDir := filepath.Dir(temp)
	assertMode(t, stageDir, 0o700)
	info, err := os.Stat(temp)
	if err != nil {
		t.Fatalf("stat staged snapshot: %v", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		t.Fatalf("staged artifact = mode %v size %d; want a non-empty regular file", info.Mode(), info.Size())
	}

	close(release)
	if err := <-result; err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	assertMode(t, dest, 0o600)
	if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory remains after publish: %v", err)
	}
}

func TestPrepareToolingDestinationScavengesOnlyOldUnlockedStaging(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "usage.snapshot.sqlite")
	prefix := toolingStagingNamePrefix(dest)
	oldUnlocked := filepath.Join(dir, prefix+"old-unlocked")
	oldLocked := filepath.Join(dir, prefix+"old-locked")
	fresh := filepath.Join(dir, prefix+"fresh")
	for _, path := range []string{oldUnlocked, oldLocked, fresh} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "artifact"), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, toolingStagingLockName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lockedFile, err := os.OpenFile(filepath.Join(oldLocked, toolingStagingLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lockedFile.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlockErr := unix.Flock(int(lockedFile.Fd()), unix.LOCK_UN); unlockErr != nil {
			t.Errorf("unlock live staging fixture: %v", unlockErr)
		}
		if closeErr := lockedFile.Close(); closeErr != nil {
			t.Errorf("close live staging fixture: %v", closeErr)
		}
	}()

	now := time.Now()
	oldTime := now.Add(-toolingStagingOrphanAge - time.Minute)
	for _, path := range []string{oldUnlocked, oldLocked} {
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(fresh, now, now); err != nil {
		t.Fatal(err)
	}

	prepared, err := prepareToolingDestination(dest)
	if err != nil {
		t.Fatalf("prepareToolingDestination() error: %v", err)
	}
	defer func() {
		if cleanupErr := removeToolingStaging(prepared); cleanupErr != nil {
			t.Errorf("clean prepared staging: %v", cleanupErr)
		}
	}()
	if _, err := os.Stat(oldUnlocked); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old unlocked staging directory remains: %v", err)
	}
	if _, err := os.Stat(oldLocked); err != nil {
		t.Fatalf("old locked staging directory was scavenged: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh staging directory was scavenged: %v", err)
	}
	if filepath.Dir(prepared.temp) == fresh {
		t.Fatal("prepareToolingDestination() reused the possible concurrent staging directory")
	}
}

func TestSnapshotCoexistsWithLiveWriter(t *testing.T) {
	const attempts = 5
	for attempt := range attempts {
		source := filepath.Join(t.TempDir(), "usage.sqlite")
		store := openStore(t, source)
		if _, err := store.InsertPending(t.Context(), validPendingRequest()); err != nil {
			t.Fatalf("seed request: %v", err)
		}

		started := make(chan struct{})
		committed := make(chan struct{}, 1)
		stop := make(chan struct{})
		writerErrors := make(chan error, 1)
		var finalized atomic.Int64
		var writer sync.WaitGroup
		writer.Add(1)
		go func() {
			defer writer.Done()
			close(started)
			for {
				select {
				case <-stop:
					return
				default:
				}
				id, insertErr := store.InsertPending(context.Background(), validPendingRequest())
				if insertErr != nil {
					writerErrors <- insertErr
					return
				}
				request := validFinalizedRequest()
				request.ResponseID = fmt.Sprintf("msg_01snapshot%d", id)
				if finalizeErr := store.Finalize(context.Background(), id, request); finalizeErr != nil {
					writerErrors <- finalizeErr
					return
				}
				finalized.Add(1)
				select {
				case committed <- struct{}{}:
				default:
				}
			}
		}()
		<-started
		<-committed

		dest := filepath.Join(t.TempDir(), "usage.snapshot.sqlite")
		startCount := finalized.Load()
		snapshotErr := Snapshot(t.Context(), source, dest)
		endCount := finalized.Load()
		close(stop)
		writer.Wait()
		select {
		case writerErr := <-writerErrors:
			t.Fatalf("live writer: %v", writerErr)
		default:
		}
		if snapshotErr != nil {
			t.Fatalf("Snapshot() with live writer error = %v", snapshotErr)
		}
		if endCount == startCount {
			if attempt+1 == attempts {
				t.Fatalf("no writer commits overlapped any snapshot after %d attempts", attempts)
			}
			continue
		}

		db := inspectDB(t, dest)
		var integrity string
		if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
			t.Fatalf("integrity check: %v", err)
		}
		if integrity != "ok" {
			t.Fatalf("integrity check = %q; want ok", integrity)
		}
		var rows int
		if err := db.QueryRow(`SELECT COUNT(*) FROM accounting_requests`).Scan(&rows); err != nil {
			t.Fatalf("count snapshot rows: %v", err)
		}
		if rows < 1 {
			t.Fatalf("snapshot rows = %d; want at least seed row", rows)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close snapshot inspection: %v", err)
		}

		reopened, reopenErr := Open(dest)
		if reopenErr != nil {
			t.Fatalf("Open(snapshot) error = %v", reopenErr)
		}
		if closeErr := reopened.Close(); closeErr != nil {
			t.Fatalf("close reopened snapshot: %v", closeErr)
		}
		return
	}
}

func TestSnapshotCanceledAfterPartialLeavesNoOutput(t *testing.T) {
	const (
		attempts    = 3
		payloadSize = 32 << 20
	)
	for attempt := range attempts {
		source := filepath.Join(t.TempDir(), "usage.sqlite")
		store, openErr := Open(source)
		if openErr != nil {
			t.Fatalf("open source: %v", openErr)
		}
		if closeErr := store.Close(); closeErr != nil {
			t.Fatalf("close source: %v", closeErr)
		}
		db, openErr := sql.Open("sqlite", source)
		if openErr != nil {
			t.Fatal(openErr)
		}
		if _, execErr := db.Exec(`CREATE TABLE snapshot_payload (payload BLOB NOT NULL)`); execErr != nil {
			t.Fatal(execErr)
		}
		const insertPayload = `INSERT INTO snapshot_payload(payload) VALUES (zeroblob(?))`
		if _, execErr := db.Exec(insertPayload, payloadSize); execErr != nil {
			t.Fatal(execErr)
		}
		if closeErr := db.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}

		dest := filepath.Join(t.TempDir(), "canceled.sqlite")
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() { result <- Snapshot(ctx, source, dest) }()

		ticker := time.NewTicker(time.Millisecond)
		deadline := time.NewTimer(5 * time.Second)
		partialObserved := false
		finished := false
		var snapshotErr error
		for !partialObserved && !finished {
			select {
			case snapshotErr = <-result:
				finished = true
			case <-ticker.C:
				partials := toolingStagingMatches(t, dest)
				if len(partials) == 1 {
					artifact := filepath.Join(partials[0], filepath.Base(dest))
					info, statErr := os.Stat(artifact)
					if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
						t.Fatal(statErr)
					}
					if statErr == nil && info.Size() > 0 {
						cancel()
						partialObserved = true
						snapshotErr = <-result
					}
				}
			case <-deadline.C:
				cancel()
				snapshotErr = <-result
				finished = true
			}
		}
		ticker.Stop()
		deadline.Stop()
		cancel()
		if !partialObserved {
			if attempt+1 == attempts {
				t.Fatalf("could not observe a partial snapshot before cancellation after %d attempts", attempts)
			}
			continue
		}
		if snapshotErr == nil {
			t.Fatal("snapshot succeeded after cancellation of a partial output")
		}
		if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
			t.Fatalf("canceled snapshot destination stat error = %v; want absent destination", statErr)
		}
		partials := toolingStagingMatches(t, dest)
		if len(partials) != 0 {
			t.Fatalf("partial snapshots = %v; want none", partials)
		}
		return
	}
	t.Fatal("could not observe a partial snapshot before cancellation")
}

func TestSnapshotRefusesUnsafeSourcesAndDestinations(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	dest := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := Snapshot(t.Context(), missing, dest); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Snapshot(missing) error = %v; want missing-source refusal", err)
	}
	partials := toolingStagingMatches(t, dest)
	if len(partials) != 0 {
		t.Fatalf("partial snapshots = %v; want none", partials)
	}

	source := filepath.Join(t.TempDir(), "usage.sqlite")
	_ = openStore(t, source)
	existing := filepath.Join(t.TempDir(), "existing.sqlite")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Snapshot(t.Context(), source, existing); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Snapshot(existing destination) error = %v; want destination refusal", err)
	}
	contents, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "keep" {
		t.Fatalf("existing destination = %q; want untouched contents", contents)
	}

	missingDest := filepath.Join(t.TempDir(), "missing", "snapshot.sqlite")
	snapshotErr := Snapshot(t.Context(), source, missingDest)
	if snapshotErr == nil || !strings.Contains(snapshotErr.Error(), "destination directory") {
		t.Fatalf("Snapshot(missing destination directory) error = %v; want refusal", snapshotErr)
	}
}

// closedToolingSource creates a migrated usage database at path and closes it so
// no WAL sidecars remain, leaving a quiescent source for tooling path tests.
func closedToolingSource(t *testing.T, path string) {
	t.Helper()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("create tooling source %q: %v", path, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close tooling source %q: %v", path, err)
	}
}

// assertToolingSourceUntouched proves a rejected tooling run neither published a
// destination, nor left staging residue, nor let SQLite create source sidecars.
func assertToolingSourceUntouched(t *testing.T, source, dest string, mode os.FileMode) {
	t.Helper()

	assertMode(t, source, mode)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(source + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("SQLite sidecar %q appeared for a rejected source: %v", source+suffix, err)
		}
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("destination %q exists after rejection: %v", dest, err)
	}
	if partials := toolingStagingMatches(t, dest); len(partials) != 0 {
		t.Errorf("staging residue after rejection = %v; want none", partials)
	}
}

func TestToolingRejectsUnsafeSourcePathBeforeSQLiteContact(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		build   func(*testing.T) (source string, mode os.FileMode)
		wantErr string
	}{
		{
			name: "replaceable immediate parent",
			build: func(t *testing.T) (string, os.FileMode) {
				t.Helper()

				parent := t.TempDir()
				source := filepath.Join(parent, "usage.sqlite")
				closedToolingSource(t, source)
				if err := os.Chmod(parent, 0o777); err != nil {
					t.Fatal(err)
				}
				return source, databaseMode
			},
			wantErr: "replaceable by another local user",
		},
		{
			name: "unsafe earlier ancestor",
			build: func(t *testing.T) (string, os.FileMode) {
				t.Helper()

				ancestor := t.TempDir()
				parent := filepath.Join(ancestor, "private")
				if err := os.Mkdir(parent, 0o700); err != nil {
					t.Fatal(err)
				}
				source := filepath.Join(parent, "usage.sqlite")
				closedToolingSource(t, source)
				if err := os.Chmod(ancestor, 0o777); err != nil {
					t.Fatal(err)
				}
				return source, databaseMode
			},
			wantErr: "replaceable by another local user",
		},
		{
			name: "source parent symlink",
			build: func(t *testing.T) (string, os.FileMode) {
				t.Helper()

				root := t.TempDir()
				target := filepath.Join(root, "target")
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				closedToolingSource(t, filepath.Join(target, "usage.sqlite"))
				if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(root, "link", "usage.sqlite"), databaseMode
			},
			wantErr: "symbolic link",
		},
		{
			name: "world writable source leaf",
			build: func(t *testing.T) (string, os.FileMode) {
				t.Helper()

				source := filepath.Join(t.TempDir(), "usage.sqlite")
				closedToolingSource(t, source)
				if err := os.Chmod(source, 0o666); err != nil {
					t.Fatal(err)
				}
				return source, 0o666
			},
			wantErr: "writable by another local user",
		},
	} {
		for _, tool := range []struct {
			name string
			run  func(context.Context, string, string) error
		}{
			{name: "snapshot", run: Snapshot},
			{name: "export", run: Export},
		} {
			t.Run(test.name+"/"+tool.name, func(t *testing.T) {
				t.Parallel()

				source, mode := test.build(t)
				dest := filepath.Join(t.TempDir(), "artifact.out")
				err := tool.run(t.Context(), source, dest)
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("%s(%q) error = %v; want %q", tool.name, source, err, test.wantErr)
				}
				assertToolingSourceUntouched(t, source, dest, mode)
			})
		}
	}
}

func TestToolingRejectsSharedWritableImmediateSourceParent(t *testing.T) {
	tempRoot := assertSharedStickyTemporaryRoot(t)
	staged := filepath.Join(t.TempDir(), "usage.sqlite")
	closedToolingSource(t, staged)
	source := filepath.Join(tempRoot, uniqueTemporaryName(t)+".sqlite")
	if err := os.Rename(staged, source); err != nil {
		t.Fatalf("stage source in %q: %v", tempRoot, err)
	}
	t.Cleanup(func() {
		if err := os.Remove(source); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove shared-directory source %q: %v", source, err)
		}
	})

	dest := filepath.Join(t.TempDir(), "snapshot.sqlite")
	err := Snapshot(t.Context(), source, dest)
	if err == nil || !strings.Contains(err.Error(), "shared writable immediate parent") {
		t.Fatalf("Snapshot(%q) error = %v; want shared-writable-immediate-parent rejection", source, err)
	}
	assertToolingSourceUntouched(t, source, dest, databaseMode)
}

func TestToolingAcceptsPrivateSubdirectoryOfSharedTemporaryRoot(t *testing.T) {
	tempRoot := assertSharedStickyTemporaryRoot(t)
	t.Setenv("TMPDIR", tempRoot)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatalf("secure temporary tooling parent: %v", err)
	}
	source := filepath.Join(parent, "usage.sqlite")
	closedToolingSource(t, source)

	dest := filepath.Join(parent, "snapshot.sqlite")
	if err := Snapshot(t.Context(), source, dest); err != nil {
		t.Fatalf("Snapshot(%q) error = %v; want acceptance beneath a private subdirectory", source, err)
	}
	assertMode(t, dest, databaseMode)
}

// TestToolingRejectsUnsafeSourceSidecarBeforePublication applies the live path's
// sidecar rule to the read-only tooling source. Snapshot and Export hand the source
// pathname to SQLite, which opens its sidecars by path during Ping, so a preplanted
// hostile `-wal`, `-shm`, or `-journal` in an otherwise private parent must abort
// the run before any destination artifact is staged or published.
func TestToolingRejectsUnsafeSourceSidecarBeforePublication(t *testing.T) {
	t.Parallel()

	for _, tool := range []struct {
		name string
		run  func(context.Context, string, string) error
	}{
		{name: "snapshot", run: Snapshot},
		{name: "export", run: Export},
	} {
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			t.Run(tool.name+" "+suffix, func(t *testing.T) {
				t.Parallel()

				source := filepath.Join(t.TempDir(), "usage.sqlite")
				closedToolingSource(t, source)
				sidecar := source + suffix
				plant := []byte("hostile preplanted sidecar")
				if err := os.WriteFile(sidecar, plant, 0o600); err != nil {
					t.Fatalf("preplant sidecar %q: %v", sidecar, err)
				}
				if err := os.Chmod(sidecar, 0o666); err != nil {
					t.Fatalf("expose preplanted sidecar %q: %v", sidecar, err)
				}

				dest := filepath.Join(t.TempDir(), "usage.out")
				err := tool.run(t.Context(), source, dest)
				if err == nil || !strings.Contains(err.Error(), "writable by another local user") {
					t.Fatalf("%s(%q) error = %v; want preplanted-sidecar rejection", tool.name, source, err)
				}
				if !strings.Contains(err.Error(), sidecar) {
					t.Errorf(
						"%s(%q) error = %v; want it to name the rejected sidecar %q",
						tool.name,
						source,
						err,
						sidecar,
					)
				}

				got, readErr := os.ReadFile(sidecar)
				if readErr != nil {
					t.Fatalf("read preplanted sidecar after rejection: %v", readErr)
				}
				if string(got) != string(plant) {
					t.Errorf("preplanted sidecar contents = %q; want %q", got, plant)
				}
				assertMode(t, sidecar, 0o666)
				assertMode(t, source, databaseMode)
				if _, statErr := os.Lstat(dest); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("destination %q exists after rejection: %v", dest, statErr)
				}
				if partials := toolingStagingMatches(t, dest); len(partials) != 0 {
					t.Errorf("staging residue after rejection = %v; want none", partials)
				}
			})
		}
	}
}

// TestToolingRejectsMissingSourceParentComponent pins rejectMissingComponents.
// The live serve path creates an absent ancestor; read-only tooling must refuse
// one instead, because creating a component along an unvalidated path is the
// substitution the traversal exists to prevent — and because a snapshot of a
// database that does not exist would otherwise materialize the directory that
// makes a later plant possible. A missing intermediate component must be refused
// as firmly as a missing immediate parent, and nothing may be left on disk.
func TestToolingRejectsMissingSourceParentComponent(t *testing.T) {
	t.Parallel()

	for _, tool := range []struct {
		name string
		run  func(context.Context, string, string) error
	}{
		{name: "snapshot", run: Snapshot},
		{name: "export", run: Export},
	} {
		for _, shape := range []struct {
			name      string
			relative  []string
			wantNamed string
		}{
			{name: "immediate parent", relative: []string{"absent"}, wantNamed: "absent"},
			{name: "intermediate ancestor", relative: []string{"absent", "deeper"}, wantNamed: "absent"},
		} {
			t.Run(tool.name+" "+shape.name, func(t *testing.T) {
				t.Parallel()

				root := t.TempDir()
				missing := make([]string, 0, len(shape.relative))
				current := root
				for _, component := range shape.relative {
					current = filepath.Join(current, component)
					missing = append(missing, current)
				}
				source := filepath.Join(current, "usage.sqlite")
				dest := filepath.Join(t.TempDir(), "usage.out")

				err := tool.run(t.Context(), source, dest)
				wantErr := fmt.Sprintf("component %q does not exist", shape.wantNamed)
				if err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Fatalf("%s(%q) error = %v; want %s", tool.name, source, err, wantErr)
				}

				for _, absent := range missing {
					if _, statErr := os.Lstat(absent); !errors.Is(statErr, os.ErrNotExist) {
						t.Errorf("tooling created missing source ancestor %q: %v", absent, statErr)
					}
				}
				if _, statErr := os.Lstat(source); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("tooling created missing source %q: %v", source, statErr)
				}
				if _, statErr := os.Lstat(dest); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("destination %q exists after rejection: %v", dest, statErr)
				}
				if partials := toolingStagingMatches(t, dest); len(partials) != 0 {
					t.Errorf("staging residue after rejection = %v; want none", partials)
				}
			})
		}
	}
}

// TestToolingRejectsBlockingSpecialFileSource is the read-only half of
// TestOpenRejectsBlockingSpecialFileDatabase, and the sharper of the two: the
// tooling source is opened read-only, and a plain O_RDONLY open of a FIFO with no
// writer never returns. A snapshot or export aimed at a planted pipe would hang
// forever instead of refusing, so the open must not block before fstat can judge
// the shape.
func TestToolingRejectsBlockingSpecialFileSource(t *testing.T) {
	t.Parallel()

	for _, tool := range []struct {
		name string
		run  func(context.Context, string, string) error
	}{
		{name: "snapshot", run: Snapshot},
		{name: "export", run: Export},
	} {
		for _, shape := range []struct {
			name    string
			build   func(t *testing.T, dir string) (source, blocking string)
			wantErr string
		}{
			{
				name: "source is a named pipe",
				build: func(t *testing.T, dir string) (string, string) {
					t.Helper()
					source := filepath.Join(dir, "usage.sqlite")
					if err := unix.Mkfifo(source, 0o600); err != nil {
						t.Fatalf("create FIFO source %q: %v", source, err)
					}
					return source, source
				},
				wantErr: "not a regular file",
			},
			{
				name: "source is a socket",
				build: func(t *testing.T, dir string) (string, string) {
					t.Helper()
					source := filepath.Join(dir, "usage.sqlite")
					plantUnixSocket(t, source)
					return source, source
				},
				wantErr: "open usage database",
			},
			{
				name: "sidecar is a named pipe",
				build: func(t *testing.T, dir string) (string, string) {
					t.Helper()
					source := filepath.Join(dir, "usage.sqlite")
					closedToolingSource(t, source)
					sidecar := source + "-journal"
					if err := unix.Mkfifo(sidecar, 0o600); err != nil {
						t.Fatalf("create FIFO sidecar %q: %v", sidecar, err)
					}
					return source, sidecar
				},
				wantErr: "sidecar",
			},
		} {
			t.Run(tool.name+" "+shape.name, func(t *testing.T) {
				t.Parallel()

				source, blocking := shape.build(t, t.TempDir())
				dest := filepath.Join(t.TempDir(), "usage.out")
				before := assertSpecialFileMode(t, blocking)

				err := assertReturnsPromptly(t, 10*time.Second, func() error {
					return tool.run(t.Context(), source, dest)
				})
				if err == nil || !strings.Contains(err.Error(), shape.wantErr) {
					t.Fatalf("%s(%q) error = %v; want %q refusal", tool.name, source, err, shape.wantErr)
				}

				assertSpecialFileUnchanged(t, blocking, before)
				if _, destErr := os.Lstat(dest); !errors.Is(destErr, os.ErrNotExist) {
					t.Errorf("destination %q exists after rejection: %v", dest, destErr)
				}
				if partials := toolingStagingMatches(t, dest); len(partials) != 0 {
					t.Errorf("staging residue after rejection = %v; want none", partials)
				}
			})
		}
	}
}

// TestSnapshotPublishesWhereStrictSourcePolicyWouldRefuse pins the trust split the
// generated artifact depends on. Patchbay's own snapshot output is not an external
// source: it lands on the destination filesystem, which in deployment is an
// all-squashed NAS export where every file reports a mapped owner Patchbay never
// runs as. Validating it with the strict source traversal would reject Patchbay's
// own artifact. A world-writable, non-sticky destination directory reproduces that
// refusal locally without root, so this proves the artifact takes the distinct
// destination-trust path rather than the source policy.
func TestSnapshotPublishesWhereStrictSourcePolicyWouldRefuse(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, source)
	if _, err := store.InsertPending(t.Context(), validPendingRequest()); err != nil {
		t.Fatalf("seed request: %v", err)
	}

	destDir := filepath.Join(t.TempDir(), "squashed")
	if err := os.Mkdir(destDir, 0o700); err != nil {
		t.Fatalf("create destination directory: %v", err)
	}
	if err := os.Chmod(destDir, 0o777); err != nil {
		t.Fatalf("expose destination directory: %v", err)
	}
	dest := filepath.Join(destDir, "usage.snapshot.sqlite")

	var artifactErr, strictErr error
	inspected := false
	ctx := context.WithValue(t.Context(), toolingBeforePublishHookKey{}, func(temp string) {
		inspected = true
		_, artifactErr = inspectGeneratedToolingDatabase(temp)
		_, strictErr = openSecureToolingSource(temp)
	})
	if err := Snapshot(ctx, source, dest); err != nil {
		t.Fatalf(
			"Snapshot(%q) error = %v; want publication onto a destination the source policy would refuse",
			dest,
			err,
		)
	}
	if !inspected {
		t.Fatal("before-publish hook did not run; the artifact validators were never compared")
	}
	if artifactErr != nil {
		t.Errorf("inspectGeneratedToolingDatabase(staged) error = %v; want acceptance", artifactErr)
	}
	if strictErr == nil || !strings.Contains(strictErr.Error(), "replaceable by another local user") {
		t.Errorf(
			"openSecureToolingSource(staged) error = %v; want the strict source policy to refuse the same path",
			strictErr,
		)
	}
	assertMode(t, dest, databaseMode)
}

// TestGeneratedSnapshotArtifactValidatorRejectsUnsafeShapes proves dropping the
// ancestor and ownership policy did not drop the checks the artifact still needs.
func TestGeneratedSnapshotArtifactValidatorRejectsUnsafeShapes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.sqlite")
	if _, err := inspectGeneratedToolingDatabase(missing); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("inspectGeneratedToolingDatabase(missing) error = %v; want missing-artifact refusal", err)
	}

	target := filepath.Join(dir, "target.sqlite")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.sqlite")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectGeneratedToolingDatabase(link); err == nil ||
		!strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("inspectGeneratedToolingDatabase(symlink) error = %v; want symlink refusal", err)
	}
	if _, err := inspectGeneratedToolingDatabase(dir); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("inspectGeneratedToolingDatabase(directory) error = %v; want regular-file refusal", err)
	}
}

// TestGeneratedSnapshotArtifactIdentityDetectsReplacement proves the artifact
// validator records a real device and inode, so the post-open recheck around
// SQLite still detects a swap rather than comparing an empty identity.
func TestGeneratedSnapshotArtifactIdentityDetectsReplacement(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	artifact := filepath.Join(dir, "snapshot.sqlite")
	if err := os.WriteFile(artifact, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, inspectErr := inspectGeneratedToolingDatabase(artifact)
	if inspectErr != nil {
		t.Fatalf("inspectGeneratedToolingDatabase(%q): %v", artifact, inspectErr)
	}
	if recheckErr := recheckToolingSource(artifact, identity); recheckErr != nil {
		t.Fatalf("recheckToolingSource(unchanged artifact) = %v; want the recorded identity to match", recheckErr)
	}

	replacement := filepath.Join(dir, "replacement.sqlite")
	if err := os.WriteFile(replacement, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, artifact); err != nil {
		t.Fatal(err)
	}
	recheckErr := recheckToolingSource(artifact, identity)
	if recheckErr == nil || !strings.Contains(recheckErr.Error(), "changed while opening") {
		t.Fatalf("recheckToolingSource(replaced artifact) = %v; want replacement refusal", recheckErr)
	}
}

// TestReadmeMatchesToolingTrustBoundary keeps the documented guarantee tied to the
// code. The strict traversal covers tooling sources and their sidecars; the
// generated artifact and its destination ancestors are trusted through private
// staging and no-replace publication instead, because an all-squashed destination
// cannot supply local-UID ownership evidence.
func TestReadmeMatchesToolingTrustBoundary(t *testing.T) {
	t.Parallel()

	readme, readErr := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if readErr != nil {
		t.Fatalf("read README.md: %v", readErr)
	}
	// README prose wraps at a fixed column, so match against a single-spaced
	// rendering; a phrase split across two lines is still the same claim.
	text := strings.Join(strings.Fields(string(readme)), " ")

	for _, stale := range []string{
		"and to the staged artifact they hand back to SQLite",
		"A destination directory that another local UID could replace is therefore refused too",
		// Schema validation no longer stops at the recorded migration names.
		"Schema validation reads only `schema_migrations` and `sqlite_master`",
		// The floor is now scoped: on the tables the migrations create, an
		// object they did not create is refused rather than tolerated.
		"objects it does not name — SQLite's own implicit indexes among them — are neither required nor refused",
	} {
		if strings.Contains(text, stale) {
			t.Errorf("README.md still claims %q; destination ancestors are neither traversed nor refused", stale)
		}
	}
	for _, required := range []string{
		"`-wal`, `-shm`, and `-journal`",
		"all-squashed",
		"its identity mapping are trusted",
		// The source gate is a check of the source, not an inference from the
		// snapshot taken of it.
		"run a read-only `PRAGMA integrity_check` against the exact database that connection opened",
		"refuse unless it reports the single result `ok`",
		"`VACUUM INTO` rebuilds secondary indexes from the table rows rather than copying them",
		// A recorded version is a claim about which migrations ran; the objects
		// those migrations built are checked rather than assumed.
		"Schema validation is a check of objects, not of names",
		"a database is held to everything the migrations it records declared",
		"a migration already recorded is skipped rather than replayed",
		"not a database missing a table, column, index, or trigger the version it records promised",
		// The definitions themselves are compared, against a schema rebuilt from
		// the migrations rather than transcribed from them.
		"rebuilt from this build's own migrations, in a private in-memory database that touches nothing on disk",
		"a table's constraints and a trigger's body are compared, not merely its name",
		"an object dropped or redefined afterwards is never rebuilt",
		"Definitions are compared for meaning rather than for text",
		"comments, whitespace and keyword case are normalized away, while quoted strings and quoted identifiers " +
			"are compared byte for byte",
		"Migrations are immutable, so a database written by an earlier build stored the text of the same " +
			"statements this build declares",
		"holding one that keeps its name over a weakened definition",
		// An object the migrations never created is refused on the tables they
		// did create, because an addition is a live capability there.
		"Additions are refused as well as omissions",
		"Every trigger and index attached to a table those migrations create",
		"empties the ledger the first time Patchbay records a request",
		"Indexes on those tables are refused whether or not they are `UNIQUE`",
		"those carry no stored `CREATE` text, and that absence is what identifies them",
		"exempting that prefix would leave one namespace unexamined",
		// Away from those tables the floor stands, so an unrelated table or view
		// and everything hanging off it still publishes.
		"an unrelated table or view, and the triggers and indexes attached to it, are neither required nor refused",
		// The generated artifact is validated in its own right and is never the
		// thing that gets migrated.
		"must pass its own `PRAGMA integrity_check`",
		"disposable migration clone, created exclusively at mode 0600",
		"The clone is opened under the same destination trust the artifact gets",
		"removed whether the proof succeeds or fails",
		"The artifact itself is never migrated",
		// Export reads the current schema's columns, so an older source is
		// migrated as a copy rather than in place.
		"It is never brought forward in place",
		"into a disposable migration clone inside the same private staging directory",
		"removed whether the export succeeds, fails, or is canceled",
		"A source already at the current schema is read directly, with no copy and no migration",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("README.md is missing %q; the documented boundary no longer matches the code", required)
		}
	}
}

func TestRecheckToolingSourceDetectsReplacement(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "usage.sqlite")
	if err := os.WriteFile(source, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, probeErr := openSecureToolingSource(source)
	if probeErr != nil {
		t.Fatalf("probe source: %v", probeErr)
	}
	replacement := filepath.Join(dir, "replacement.sqlite")
	if err := os.WriteFile(replacement, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, source); err != nil {
		t.Fatal(err)
	}
	recheckErr := recheckToolingSource(source, identity)
	if recheckErr == nil || !strings.Contains(recheckErr.Error(), "changed while opening") {
		t.Fatalf("recheckToolingSource() error = %v; want replacement refusal", recheckErr)
	}
}

func TestToolingOpenRejectsInvalidSources(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.sqlite")
	_, missingErr := openToolingDatabase(t.Context(), missing)
	if missingErr == nil || !strings.Contains(missingErr.Error(), "does not exist") {
		t.Fatalf("openToolingDatabase(missing) error = %v; want missing-source refusal", missingErr)
	}

	target := filepath.Join(dir, "target.sqlite")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "source.sqlite")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	_, symlinkErr := openToolingDatabase(t.Context(), symlink)
	if symlinkErr == nil || !strings.Contains(symlinkErr.Error(), "symbolic link") {
		t.Fatalf("openToolingDatabase(symlink) error = %v; want symlink refusal", symlinkErr)
	}
	_, directoryErr := openToolingDatabase(t.Context(), dir)
	if directoryErr == nil || !strings.Contains(directoryErr.Error(), "not a regular file") {
		t.Fatalf("openToolingDatabase(directory) error = %v; want regular-file refusal", directoryErr)
	}

	notDB := filepath.Join(dir, "not-db.sqlite")
	if err := os.WriteFile(notDB, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, notDBErr := openToolingDatabase(t.Context(), notDB)
	if notDBErr == nil {
		t.Fatal("openToolingDatabase(non-database) error = nil; want refusal")
	}

	notPatchbay := filepath.Join(dir, "not-patchbay.sqlite")
	db, err := sql.Open("sqlite", notPatchbay)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, foreignDatabaseErr := openToolingDatabase(t.Context(), notPatchbay)
	if foreignDatabaseErr == nil || !strings.Contains(foreignDatabaseErr.Error(), "not a patchbay usage database") {
		t.Fatalf("foreign database error = %v; want patchbay-database refusal", foreignDatabaseErr)
	}

	future := filepath.Join(dir, "future.sqlite")
	store, err := Open(future)
	if err != nil {
		t.Fatalf("open future source: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close future source: %v", err)
	}
	db, err = sql.Open("sqlite", future)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, name) VALUES (99, 'future')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, futureErr := openToolingDatabase(t.Context(), future)
	if futureErr == nil || !strings.Contains(futureErr.Error(), "unknown future schema version 99") {
		t.Fatalf("openToolingDatabase(future) error = %v; want future-schema refusal", futureErr)
	}

	spoofed := filepath.Join(dir, "spoofed.sqlite")
	spoofedStore, openErr := Open(spoofed)
	if openErr != nil {
		t.Fatalf("open spoofed source: %v", openErr)
	}
	if closeErr := spoofedStore.Close(); closeErr != nil {
		t.Fatalf("close spoofed source: %v", closeErr)
	}
	db, err = sql.Open("sqlite", spoofed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_migrations SET name = 'spoofed' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, spoofedErr := openToolingDatabase(t.Context(), spoofed)
	if spoofedErr == nil || !strings.Contains(spoofedErr.Error(), "not a patchbay usage database") {
		t.Fatalf("spoofed database error = %v; want patchbay-database refusal", spoofedErr)
	}

	missingRequests := filepath.Join(dir, "missing-requests.sqlite")
	requestsStore, openErr := Open(missingRequests)
	if openErr != nil {
		t.Fatalf("open missing-requests source: %v", openErr)
	}
	if closeErr := requestsStore.Close(); closeErr != nil {
		t.Fatalf("close missing-requests source: %v", closeErr)
	}
	db, err = sql.Open("sqlite", missingRequests)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE accounting_requests`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, missingRequestsErr := openToolingDatabase(t.Context(), missingRequests)
	if missingRequestsErr == nil || !strings.Contains(missingRequestsErr.Error(), "not a patchbay usage database") {
		t.Fatalf("missing accounting_requests error = %v; want patchbay-database refusal", missingRequestsErr)
	}
}

// TestSnapshotMigrationCloneHonoursCallerCancellation proves the forward-migration
// proof runs under the caller's context rather than a detached background one. The
// proof replays every pending migration against a full copy of the accounting
// database, so on a large ledger over a slow NAS mount it is the longest-running
// step in the command; a Ctrl-C or a systemd timer's timeout has to reach it.
func TestSnapshotMigrationCloneHonoursCallerCancellation(t *testing.T) {
	t.Parallel()

	source := filepath.Join(t.TempDir(), "usage.sqlite")
	olderSchemaToolingSource(t, source, 1)
	before := toolingSourceBytes(t, source)

	dest := filepath.Join(t.TempDir(), "usage.snapshot.sqlite")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	proofs := 0
	ctx = context.WithValue(ctx, toolingMigrationCloneHookKey{}, func(string) {
		proofs++
		cancel()
	})

	err := Snapshot(ctx, source, dest)
	if err == nil {
		t.Fatal("Snapshot() published a snapshot whose migration proof was canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Snapshot() error = %v; want it to wrap %v", err, context.Canceled)
	}
	// The step matters, not just the eventual refusal. A detached background
	// context inside the migration would still be caught by the integrity check
	// that follows it, which would report cancellation from the wrong step and
	// only after the whole schema had been replayed.
	if want := "forward migration: migrate usage migration clone"; !strings.Contains(err.Error(), want) {
		t.Errorf("Snapshot() error = %v; want cancellation reported by the migration itself (%q)", err, want)
	}
	if proofs != 1 {
		t.Errorf("forward-migration proof ran %d times; want exactly 1", proofs)
	}
	assertRejectedToolingRunLeftNoOutput(t, source, dest, before)
}
