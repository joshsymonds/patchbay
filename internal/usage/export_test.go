package usage

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/accounting"
)

func TestExportIsDeterministicAndComplete(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, source)
	if err := store.LoadRateCards(t.Context(), []RateCard{{
		Model:                            "test-model",
		EffectiveFrom:                    time.Unix(1_776_912_345, 0).UTC(),
		InputNanoUSDPerMillion:           accounting.NanoUSD(1),
		OutputNanoUSDPerMillion:          accounting.NanoUSD(2),
		CacheReadNanoUSDPerMillion:       accounting.NanoUSD(3),
		CacheCreationNanoUSDPerMillion:   accounting.NanoUSD(4),
		CacheCreation5mNanoUSDPerMillion: accounting.NanoUSD(5),
		CacheCreation1hNanoUSDPerMillion: accounting.NanoUSD(6),
		Source:                           "test",
		CreatedAt:                        time.Unix(1_776_912_346, 0).UTC(),
	}}); err != nil {
		t.Fatalf("seed rate card: %v", err)
	}
	id, err := store.InsertPending(t.Context(), validPendingRequest())
	if err != nil {
		t.Fatalf("seed pending request: %v", err)
	}
	const target = "arn:aws:bedrock:us-east-2:123:application-inference-profile/export"
	if err := store.MarkUpstreamDispatched(
		t.Context(),
		id,
		validPendingRequest().StartedAt.Add(time.Millisecond),
		target,
	); err != nil {
		t.Fatalf("mark dispatched request: %v", err)
	}
	undispatchedID, err := store.InsertPending(t.Context(), validPendingRequest())
	if err != nil {
		t.Fatalf("seed undispatched request: %v", err)
	}

	first := filepath.Join(t.TempDir(), "first.jsonl")
	second := filepath.Join(t.TempDir(), "second.jsonl")
	if err := Export(t.Context(), source, first); err != nil {
		t.Fatalf("first Export() error = %v", err)
	}
	if err := Export(t.Context(), source, second); err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	assertMode(t, first, 0o600)
	one, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != string(two) {
		t.Fatalf("exports differ:\nfirst: %s\nsecond: %s", one, two)
	}

	lines := exportLines(t, first)
	if len(lines) != 4 {
		t.Fatalf("export lines = %d; want header, rate card, and two accounting rows", len(lines))
	}
	var header struct {
		PatchbayUsageExport int `json:"patchbay_usage_export"`
		SchemaVersion       int `json:"schema_version"`
	}
	if err := json.Unmarshal(lines[0], &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.PatchbayUsageExport != 1 || header.SchemaVersion != summaryQueryIndexesMigrationVersion {
		t.Fatalf("header = %+v; want export 1 and schema %d", header, summaryQueryIndexesMigrationVersion)
	}

	var rateCardRow map[string]any
	var accountingRows []map[string]any
	for _, line := range lines[1:] {
		var record struct {
			Table string         `json:"table"`
			Row   map[string]any `json:"row"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode row %q: %v", line, err)
		}
		switch record.Table {
		case "rate_cards":
			rateCardRow = record.Row
		case "accounting_requests":
			accountingRows = append(accountingRows, record.Row)
		default:
			t.Fatalf("unexpected export table %q", record.Table)
		}
	}
	rateCardColumns := exportColumnNames(t, store.db, "rate_cards")
	gotRateCardColumns := mapKeys(rateCardRow)
	if !reflect.DeepEqual(rateCardColumns, gotRateCardColumns) {
		t.Fatalf("rate_cards export fields = %v; want every schema column %v", gotRateCardColumns, rateCardColumns)
	}
	if len(accountingRows) != 2 {
		t.Fatalf("accounting export rows = %d; want 2", len(accountingRows))
	}
	for _, row := range accountingRows {
		wantColumns := exportColumnNames(t, store.db, "accounting_requests")
		gotColumns := mapKeys(row)
		if !reflect.DeepEqual(wantColumns, gotColumns) {
			t.Fatalf("accounting_requests export fields = %v; want every schema column %v", gotColumns, wantColumns)
		}
	}
	dispatched := accountingRows[0]
	undispatched := accountingRows[1]
	if dispatched["id"] != float64(id) || undispatched["id"] != float64(undispatchedID) {
		t.Fatalf(
			"accounting export IDs = %#v, %#v; want %d, %d",
			dispatched["id"],
			undispatched["id"],
			id,
			undispatchedID,
		)
	}
	if got := dispatched["upstream_target"]; got != target {
		t.Fatalf("dispatched upstream_target = %#v; want %q", got, target)
	}
	if dispatched["upstream_dispatched_at"] == nil {
		t.Fatal("dispatched upstream_dispatched_at = JSON null; want timestamp")
	}
	// route_alias is set before contact on every row this build writes: it
	// carries the Seat the request rides, which is known at selection. The
	// column stays nullable for imported v1 history, where an empty alias meant
	// the built-in default route.
	if got := undispatched["route_alias"]; got != defaultSeatID {
		t.Fatalf("undispatched route_alias = %#v; want %q", got, defaultSeatID)
	}
	for _, column := range []string{
		"completed_at", "upstream_dispatched_at", "upstream_target", "outcome", "response_id", "http_status", "normalization_complete", "input_tokens", "output_tokens",
		"cache_creation_input_tokens", "cache_read_input_tokens", "cache_creation_5m_input_tokens",
		"cache_creation_1h_input_tokens", "provider_cost_present", "provider_cost_valid",
		"provider_cost_nano_usd", "stop_reason", "block_shape", "duration_ms", "upstream_ms",
		"cost_basis", "final_cost_nano_usd", "rate_card_id",
	} {
		if undispatched[column] != nil {
			t.Fatalf("undispatched %s = %#v; want JSON null", column, undispatched[column])
		}
	}
}

// exportSeedColumn is one accounting_requests column, the schema version that
// introduced it, and the value this suite writes into it. A source at version V
// is seeded with exactly the columns introduced at or before V, so the export of
// that source must carry those values and JSON null for every later column —
// which is what an older ledger genuinely looks like once the migrations have
// added the columns it never had.
type exportSeedColumn struct {
	name       string
	introduced int
	value      any
}

func exportSeedColumns() []exportSeedColumn {
	return []exportSeedColumn{
		{name: "started_at", introduced: 1, value: int64(1000)},
		{name: "completed_at", introduced: 1, value: int64(2000)},
		{name: "upstream_dispatched_at", introduced: 1, value: int64(1500)},
		{name: "request_kind", introduced: 1, value: "messages"},
		{name: "lifecycle_state", introduced: 1, value: "pending"},
		{name: "outcome", introduced: 1, value: "success"},
		{name: "upstream_contacted", introduced: 1, value: int64(1)},
		{name: "route_host", introduced: 1, value: "gnomon"},
		{name: "route_context", introduced: 1, value: "personal"},
		{name: "route_alias", introduced: 1, value: "seat-a"},
		{name: "route_upstream", introduced: 1, value: "https://api.anthropic.com"},
		{name: "request_model", introduced: 1, value: "claude-opus-5"},
		{name: "resolved_model", introduced: 1, value: "claude-opus-5-20260101"},
		{name: "auth_mode", introduced: 1, value: "forward"},
		{name: "credential_class", introduced: 1, value: "oauth"},
		{name: "billing_policy", introduced: 1, value: "credential_derived"},
		{name: "billing_class", introduced: 1, value: "subscription"},
		{name: "cost_status", introduced: 1, value: "known"},
		{name: "response_id", introduced: finalizedResponseAccountingMigrationVersion, value: "msg_01export"},
		{name: "http_status", introduced: finalizedResponseAccountingMigrationVersion, value: int64(200)},
		{name: "normalization_complete", introduced: finalizedResponseAccountingMigrationVersion, value: int64(1)},
		{name: "input_tokens", introduced: finalizedResponseAccountingMigrationVersion, value: int64(11)},
		{name: "output_tokens", introduced: finalizedResponseAccountingMigrationVersion, value: int64(22)},
		{
			name:       "cache_creation_input_tokens",
			introduced: finalizedResponseAccountingMigrationVersion,
			value:      int64(33),
		},
		{name: "cache_read_input_tokens", introduced: finalizedResponseAccountingMigrationVersion, value: int64(44)},
		{
			name:       "cache_creation_5m_input_tokens",
			introduced: finalizedResponseAccountingMigrationVersion,
			value:      int64(55),
		},
		{
			name:       "cache_creation_1h_input_tokens",
			introduced: finalizedResponseAccountingMigrationVersion,
			value:      int64(66),
		},
		{name: "provider_cost_present", introduced: finalizedResponseAccountingMigrationVersion, value: int64(1)},
		{name: "provider_cost_valid", introduced: finalizedResponseAccountingMigrationVersion, value: int64(1)},
		{name: "provider_cost_nano_usd", introduced: finalizedResponseAccountingMigrationVersion, value: int64(77)},
		{name: "stop_reason", introduced: finalizedResponseAccountingMigrationVersion, value: "end_turn"},
		{name: "block_shape", introduced: finalizedResponseAccountingMigrationVersion, value: `["text"]`},
		{name: "duration_ms", introduced: finalizedResponseAccountingMigrationVersion, value: int64(88)},
		{name: "upstream_ms", introduced: finalizedResponseAccountingMigrationVersion, value: int64(99)},
		{name: "cost_basis", introduced: rateCardsAndFinalCostMigrationVersion, value: "provider_reported"},
		{name: "final_cost_nano_usd", introduced: rateCardsAndFinalCostMigrationVersion, value: int64(77)},
		{name: "rate_card_id", introduced: rateCardsAndFinalCostMigrationVersion, value: int64(1)},
		{name: "import_source", introduced: importedLedgerProvenanceMigrationVersion, value: "ledger.jsonl"},
		{name: "import_line", introduced: importedLedgerProvenanceMigrationVersion, value: int64(7)},
		{
			name:       "upstream_target",
			introduced: upstreamTargetDispatchEvidenceMigrationVersion,
			value:      "arn:aws:bedrock:us-east-2:123:application-inference-profile/export",
		},
	}
}

// seedOlderSchemaExportSource writes one accounting row, and a rate card once the
// schema has the table, using only the columns the given version actually has.
func seedOlderSchemaExportSource(t *testing.T, path string, version int) {
	t.Helper()

	db, openErr := sql.Open("sqlite", path)
	if openErr != nil {
		t.Fatalf("open %q to seed it: %v", path, openErr)
	}
	if version >= rateCardsAndFinalCostMigrationVersion {
		if _, execErr := db.Exec(`INSERT INTO rate_cards (
			id, model, effective_from,
			input_nano_usd_per_million, output_nano_usd_per_million,
			cache_read_nano_usd_per_million, cache_creation_nano_usd_per_million,
			cache_creation_5m_nano_usd_per_million, cache_creation_1h_nano_usd_per_million,
			source, created_at
		) VALUES (1, 'claude-opus-5', 500, 1, 2, 3, 4, 5, 6, 'test', 600)`); execErr != nil {
			t.Fatalf("seed rate card in %q: %v", path, execErr)
		}
	}
	names := make([]string, 0, len(exportSeedColumns()))
	placeholders := make([]string, 0, len(exportSeedColumns()))
	values := make([]any, 0, len(exportSeedColumns()))
	for _, column := range exportSeedColumns() {
		if column.introduced > version {
			continue
		}
		names = append(names, column.name)
		placeholders = append(placeholders, "?")
		values = append(values, column.value)
	}
	insert := `INSERT INTO accounting_requests (` + strings.Join(names, ", ") +
		`) VALUES (` + strings.Join(placeholders, ", ") + `)`
	if _, execErr := db.Exec(insert, values...); execErr != nil {
		t.Fatalf("seed accounting row in %q at version %d: %v", path, version, execErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close %q after seeding: %v", path, closeErr)
	}
}

// exportValueEqual compares a decoded JSON value against a seeded Go value. JSON
// has one number type, so an integer arrives as a float64.
func exportValueEqual(got, want any) bool {
	switch typed := want.(type) {
	case nil:
		return got == nil
	case int64:
		number, ok := got.(float64)
		return ok && number == float64(typed)
	case string:
		text, ok := got.(string)
		return ok && text == typed
	default:
		return false
	}
}

func recordedSchemaVersion(t *testing.T, path string) int {
	t.Helper()

	db, openErr := sql.Open("sqlite", path)
	if openErr != nil {
		t.Fatalf("open %q to read its schema version: %v", path, openErr)
	}
	var version int
	if scanErr := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); scanErr != nil {
		t.Fatalf("read schema version of %q: %v", path, scanErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close %q after reading its schema version: %v", path, closeErr)
	}
	return version
}

// TestExportMigratesEverySupportedOlderSchemaWithoutTouchingTheSource pins the
// half of the older-schema contract export had never held up. A snapshot of an
// older schema is a byte copy, so it needs no schema knowledge; an export reads
// named columns, and those are the current schema's. Reading them from a source
// that predates them is what made `patchbay usage export` fail outright on every
// schema below the newest, and migrating the source in place to fix that would
// write to a live accounting database a read command must never touch.
//
// So the source is copied, the copy is migrated, and the export is read from the
// copy. Every supported version must export, the rows must carry what that
// version actually held with JSON null for every column it never had, and the
// source must come back byte for byte unchanged and still recording its own
// version.
func TestExportMigratesEverySupportedOlderSchemaWithoutTouchingTheSource(t *testing.T) {
	t.Parallel()

	// A column the migrations add without the seed list knowing about it would
	// silently go unasserted below.
	seeded := make(map[string]struct{}, len(exportSeedColumns()))
	for _, column := range exportSeedColumns() {
		seeded[column.name] = struct{}{}
	}
	canonical, canonicalErr := canonicalSchemaDefinitions(t.Context(), latestSchemaVersion())
	if canonicalErr != nil {
		t.Fatalf("read the canonical newest schema: %v", canonicalErr)
	}
	accounting, ok := canonical[schemaObjectIdentity{kind: schemaObjectTable, name: accountingRequestsTable}]
	if !ok {
		t.Fatalf("the canonical newest schema holds no %q table", accountingRequestsTable)
	}
	for _, column := range accounting.columns {
		if _, seen := seeded[column.name]; !seen && column.name != "id" {
			t.Fatalf("accounting_requests column %q is not covered by exportSeedColumns", column.name)
		}
	}

	declared := declaredMigrations()
	for through := 1; through <= len(declared); through++ {
		version := declared[through-1].version
		t.Run(fmt.Sprintf("schema version %d", version), func(t *testing.T) {
			t.Parallel()

			source := filepath.Join(t.TempDir(), "usage.sqlite")
			olderSchemaToolingSource(t, source, through)
			seedOlderSchemaExportSource(t, source, version)
			before := toolingSourceBytes(t, source)

			destDir := t.TempDir()
			dest := filepath.Join(destDir, "usage.jsonl")
			clones := 0
			ctx := context.WithValue(t.Context(), toolingMigrationCloneHookKey{}, func(string) { clones++ })
			if err := Export(ctx, source, dest); err != nil {
				t.Fatalf("Export(schema version %d) error = %v; want a complete export", version, err)
			}

			wantClones := 1
			if version == latestSchemaVersion() {
				wantClones = 0
			}
			if clones != wantClones {
				t.Errorf("migration clone ran %d times at version %d; want %d", clones, version, wantClones)
			}

			if !bytes.Equal(toolingSourceBytes(t, source), before) {
				t.Error("the export modified the source it was reading")
			}
			assertMode(t, source, databaseMode)
			if _, journalErr := os.Lstat(source + "-journal"); !errors.Is(journalErr, os.ErrNotExist) {
				t.Errorf("rollback journal appeared beside the exported source: %v", journalErr)
			}
			if info, walErr := os.Lstat(source + "-wal"); walErr == nil && info.Size() != 0 {
				t.Errorf("write-ahead log holds %d bytes after an export", info.Size())
			} else if walErr != nil && !errors.Is(walErr, os.ErrNotExist) {
				t.Errorf("inspect write-ahead log: %v", walErr)
			}
			if got := recordedSchemaVersion(t, source); got != version {
				t.Errorf("source schema version after export = %d; want its own %d", got, version)
			}
			assertMode(t, dest, databaseMode)
			assertDestinationDirectoryHoldsOnly(t, destDir, filepath.Base(dest))

			assertExportedOlderSchema(t, dest, version)
		})
	}
}

func assertExportedOlderSchema(t *testing.T, dest string, version int) {
	t.Helper()

	lines := exportLines(t, dest)
	wantLines := 2
	if version >= rateCardsAndFinalCostMigrationVersion {
		wantLines = 3
	}
	if len(lines) != wantLines {
		t.Fatalf("export lines = %d; want %d for schema version %d", len(lines), wantLines, version)
	}
	var header exportHeader
	if err := json.Unmarshal(lines[0], &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	// The rows below are the current schema's, whatever the source recorded, so
	// the header names the schema they are written in.
	if header.PatchbayUsageExport != 1 || header.SchemaVersion != latestSchemaVersion() {
		t.Fatalf("header = %+v; want export 1 and schema %d", header, latestSchemaVersion())
	}

	var rateCard map[string]any
	var accounting map[string]any
	for _, line := range lines[1:] {
		var record struct {
			Table string         `json:"table"`
			Row   map[string]any `json:"row"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode row %q: %v", line, err)
		}
		switch record.Table {
		case "rate_cards":
			rateCard = record.Row
		case "accounting_requests":
			accounting = record.Row
		default:
			t.Fatalf("unexpected export table %q", record.Table)
		}
	}
	if version >= rateCardsAndFinalCostMigrationVersion {
		if rateCard == nil {
			t.Fatal("export carries no rate card row")
		}
		if !exportValueEqual(rateCard["model"], "claude-opus-5") || !exportValueEqual(rateCard["source"], "test") {
			t.Errorf("rate card row = %v; want the seeded card", rateCard)
		}
	} else if rateCard != nil {
		t.Errorf("export carries a rate card row %v for a schema with no rate cards", rateCard)
	}
	if accounting == nil {
		t.Fatal("export carries no accounting row")
	}
	if !exportValueEqual(accounting["id"], int64(1)) {
		t.Errorf("accounting id = %#v; want 1", accounting["id"])
	}
	for _, column := range exportSeedColumns() {
		var want any
		if column.introduced <= version {
			want = column.value
		}
		if !exportValueEqual(accounting[column.name], want) {
			t.Errorf(
				"schema version %d export %s = %#v; want %#v",
				version, column.name, accounting[column.name], want,
			)
		}
	}
}

// collideWithPendingMigration adds an object a pending migration will try to
// create. It has to be a table: an index or trigger attached to a table the
// recorded version's migrations built is refused at the source gate, whereas a
// table is attached to nothing, so the contract for the version the source
// records has nothing to say about it and the collision surfaces where it has to
// — when the clone runs the migrations the source has not.
func collideWithPendingMigration(t *testing.T, source string) {
	t.Helper()

	spoilToolingSource(t, source, `CREATE TABLE `+rateCardsTable+` (id INTEGER PRIMARY KEY)`)
}

// TestExportMigratedSourceCopyRejectsUndeclaredManagedObjects proves the export's
// own migrated-clone path enforces the extra-object sweep rather than inheriting
// the source gate's verdict. Pending migrations add objects; they never remove
// one they do not know about, so a trigger attached to the ledger under an older
// schema is still attached after the clone reaches the current version — where it
// is undeclared too. Export reads its rows from that migrated copy, so a copy
// nobody re-checked is a copy that could hand back a ledger a trigger had already
// emptied.
func TestExportMigratedSourceCopyRejectsUndeclaredManagedObjects(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		through int
		spoil   []string
		object  string
		trigger bool
		table   string
	}{
		{
			name:    "a trigger the older schema never declared either",
			through: finalizedResponseAccountingMigrationVersion,
			spoil:   []string{destructiveLedgerTrigger("accounting_requests_purge", "INSERT")},
			object:  "accounting_requests_purge",
			trigger: true,
			table:   accountingRequestsTable,
		},
		{
			// The name is free at the version the source records and canonical
			// at the one the clone reaches, where SQLite holds the pending
			// migration's index beside this trigger rather than in place of it.
			name:    "a trigger whose name only a pending migration turns canonical",
			through: upstreamTargetDispatchEvidenceMigrationVersion,
			spoil:   []string{destructiveLedgerTrigger(summaryContextWindowIndexName, "INSERT")},
			object:  summaryContextWindowIndexName,
			trigger: true,
			table:   accountingRequestsTable,
		},
		{
			name:    "an index carrying the name of a trigger the older schema declares",
			through: importedLedgerProvenanceMigrationVersion,
			spoil: []string{
				`DROP TRIGGER rate_cards_forbid_update`,
				`CREATE INDEX rate_cards_forbid_update ON rate_cards(source)`,
				canonicalRateCardsForbidUpdate,
			},
			object: "rate_cards_forbid_update",
			table:  rateCardsTable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			clone := filepath.Join(dir, "usage.jsonl"+toolingMigrationCloneSuffix)
			olderSchemaToolingSource(t, clone, test.through)
			spoilToolingSource(t, clone, test.spoil...)

			temp := filepath.Join(dir, "usage.jsonl.partial")
			err := writeMigratedUsageExport(t.Context(), clone, temp)
			kind := "index"
			if test.trigger {
				kind = "trigger"
			}
			want := fmt.Sprintf(
				`schema version %d does not declare %s %q on table %q`,
				latestSchemaVersion(), kind, test.object, test.table,
			)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("writeMigratedUsageExport(clone carrying an undeclared object) = %v; want %q", err, want)
			}
			if _, statErr := os.Lstat(temp); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("export %q was written from a refused clone: %v", temp, statErr)
			}
		})
	}
}

// TestExportMigrationCloneIsPrivateDisposableAndLeavesNothing pins the clone's
// lifetime. It is a full second copy of the accounting database, written to the
// backup filesystem and then written to again by the migrations, so leaving one
// behind would duplicate the ledger beside the export and strand a half-migrated
// database where the next run would find it.
func TestExportMigrationCloneIsPrivateDisposableAndLeavesNothing(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		collide bool
		cancel  bool
		wantErr string
	}{
		{name: "the export succeeds"},
		{
			name:    "the clone cannot migrate forward",
			collide: true,
			wantErr: "migrate usage export source copy",
		},
		{
			name:    "the caller cancels while the clone migrates",
			cancel:  true,
			wantErr: "migrate usage migration clone",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			source := filepath.Join(t.TempDir(), "usage.sqlite")
			olderSchemaToolingSource(t, source, 1)
			seedOlderSchemaExportSource(t, source, 1)
			if test.collide {
				collideWithPendingMigration(t, source)
			}
			before := toolingSourceBytes(t, source)

			destDir := t.TempDir()
			dest := filepath.Join(destDir, "usage.jsonl")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			clonePath := ""
			published := false
			// Publication fsyncs and links on the backup filesystem. The clone is a
			// second full copy of the ledger sitting there, so it has to be gone
			// before that window opens rather than merely gone by the time the
			// staging directory is torn down afterwards.
			ctx = context.WithValue(ctx, toolingBeforePublishHookKey{}, func(string) {
				published = true
				if clonePath == "" {
					return
				}
				for _, residue := range append([]string{clonePath}, cloneSidecarPaths(clonePath)...) {
					if _, statErr := os.Lstat(residue); !errors.Is(statErr, os.ErrNotExist) {
						t.Errorf("migration clone residue %q still existed when the export was published", residue)
					}
				}
			})
			ctx = context.WithValue(ctx, toolingMigrationCloneHookKey{}, func(clone string) {
				clonePath = clone
				assertMode(t, clone, databaseMode)
				if dir := filepath.Dir(clone); !strings.HasPrefix(filepath.Base(dir), toolingStagingNamePrefix(dest)) {
					t.Errorf("migration clone %q is not inside the export's private staging directory", clone)
				}
				if test.cancel {
					cancel()
				}
			})

			err := Export(ctx, source, dest)
			switch {
			case test.wantErr == "" && err != nil:
				t.Fatalf("Export() error = %v; want a complete export", err)
			case test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)):
				t.Fatalf("Export() error = %v; want %q", err, test.wantErr)
			}
			if test.cancel && !errors.Is(err, context.Canceled) {
				t.Errorf("Export() error = %v; want it to wrap %v", err, context.Canceled)
			}
			if clonePath == "" {
				t.Fatal("the migration clone never ran; nothing about it was observed")
			}
			if published != (test.wantErr == "") {
				t.Errorf("export reached publication = %t; want %t", published, test.wantErr == "")
			}
			for _, residue := range append([]string{clonePath}, cloneSidecarPaths(clonePath)...) {
				if _, statErr := os.Lstat(residue); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("migration clone residue %q survived: %v", residue, statErr)
				}
			}
			if partials := toolingStagingMatches(t, dest); len(partials) != 0 {
				t.Errorf("staging residue = %v; want none", partials)
			}
			if !bytes.Equal(toolingSourceBytes(t, source), before) {
				t.Error("the export modified the source it was reading")
			}
			assertMode(t, source, databaseMode)
			if test.wantErr == "" {
				assertDestinationDirectoryHoldsOnly(t, destDir, filepath.Base(dest))
			} else {
				assertDestinationDirectoryHoldsOnly(t, destDir)
			}
		})
	}
}

// TestExportMigrationCloneAcceptsADestinationTheSourcePolicyWouldRefuse pins the
// trust split for the export's own clone. The clone is written to the destination
// filesystem, which in deployment is an all-squashed NAS export where every file
// reports a mapped owner Patchbay never runs as; holding it to the strict source
// policy would refuse a file Patchbay had just created itself. A world-writable,
// non-sticky destination directory reproduces that refusal locally without root.
func TestExportMigrationCloneAcceptsADestinationTheSourcePolicyWouldRefuse(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	olderSchemaToolingSource(t, source, 1)
	seedOlderSchemaExportSource(t, source, 1)

	destDir := filepath.Join(t.TempDir(), "squashed")
	if err := os.Mkdir(destDir, 0o700); err != nil {
		t.Fatalf("create destination directory: %v", err)
	}
	if err := os.Chmod(destDir, 0o777); err != nil {
		t.Fatalf("expose destination directory: %v", err)
	}
	dest := filepath.Join(destDir, "usage.jsonl")

	cloned := false
	var destinationErr, strictErr error
	ctx := context.WithValue(t.Context(), toolingMigrationCloneHookKey{}, func(clone string) {
		cloned = true
		_, destinationErr = inspectGeneratedToolingDatabase(clone)
		_, strictErr = openSecureToolingSource(clone)
	})
	if err := Export(ctx, source, dest); err != nil {
		t.Fatalf("Export(all-squash-shaped destination) error = %v; want a complete export", err)
	}
	if !cloned {
		t.Fatal("the migration clone never ran on the exposed destination")
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

func TestExportRefusesExistingDestinationAndLeavesNoPartial(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	failedAfterTemp := filepath.Join(t.TempDir(), "failed.jsonl")
	exportErr := Export(t.Context(), missing, failedAfterTemp)
	if exportErr == nil || !strings.Contains(exportErr.Error(), "does not exist") {
		t.Fatalf("Export(missing source) error = %v; want source refusal", exportErr)
	}
	partials := toolingStagingMatches(t, failedAfterTemp)
	if len(partials) != 0 {
		t.Fatalf("partial exports = %v; want none", partials)
	}

	source := filepath.Join(t.TempDir(), "usage.sqlite")
	_ = openStore(t, source)
	if err := os.Chmod(source, 0o644); err != nil {
		t.Fatal(err)
	}

	existing := filepath.Join(t.TempDir(), "existing.jsonl")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Export(t.Context(), source, existing); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Export(existing destination) error = %v; want destination refusal", err)
	}
	contents, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "keep" {
		t.Fatalf("existing destination = %q; want untouched contents", contents)
	}
	assertMode(t, source, 0o644)

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	failed := filepath.Join(blocker, "export.jsonl")
	exportErr = Export(t.Context(), source, failed)
	if exportErr == nil || !strings.Contains(exportErr.Error(), "destination directory") {
		t.Fatalf("Export(blocked destination) error = %v; want destination failure", exportErr)
	}
	partials = toolingStagingMatches(t, failed)
	if len(partials) != 0 {
		t.Fatalf("partial exports = %v; want none", partials)
	}
}

// TestExportFailureAfterWritingPartialLeavesNoOutput covers a failure that can
// only surface once the temporary output is open and the export is under way. The
// schema here is whole — a source contradicting its recorded schema is refused
// before any output exists at all, which
// TestToolingRejectsRecordedSchemaObjectsThatContradictTheirVersion covers for
// both tools — so the export gets as far as streaming rows and then meets one it
// cannot scan. Nothing written by then may reach the destination or survive in
// staging.
//
// The unscannable value is planted directly: SQLite keeps a string an INTEGER
// column cannot convert losslessly, which is what a hand-edited or partially
// restored ledger looks like to the exporter.
func TestExportFailureAfterWritingPartialLeavesNoOutput(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, source)
	if err := store.LoadRateCards(t.Context(), []RateCard{{
		Model:                            "test-model",
		EffectiveFrom:                    time.Unix(1_776_912_345, 0).UTC(),
		InputNanoUSDPerMillion:           accounting.NanoUSD(1),
		OutputNanoUSDPerMillion:          accounting.NanoUSD(2),
		CacheReadNanoUSDPerMillion:       accounting.NanoUSD(3),
		CacheCreationNanoUSDPerMillion:   accounting.NanoUSD(4),
		CacheCreation5mNanoUSDPerMillion: accounting.NanoUSD(5),
		CacheCreation1hNanoUSDPerMillion: accounting.NanoUSD(6),
		Source:                           "test",
		CreatedAt:                        time.Unix(1_776_912_346, 0).UTC(),
	}}); err != nil {
		t.Fatalf("seed rate card: %v", err)
	}
	id, err := store.InsertPending(t.Context(), validPendingRequest())
	if err != nil {
		t.Fatalf("seed pending request: %v", err)
	}
	if _, execErr := store.db.Exec(
		`UPDATE accounting_requests SET started_at = 'not-a-timestamp' WHERE id = ?`,
		id,
	); execErr != nil {
		t.Fatalf("plant an unscannable started_at: %v", execErr)
	}

	dest := filepath.Join(t.TempDir(), "failed.jsonl")
	exportErr := Export(t.Context(), source, dest)
	if exportErr == nil || !strings.Contains(exportErr.Error(), "scan accounting request for usage export") {
		t.Fatalf("Export() error = %v; want a row failure after opening the temporary output", exportErr)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("failed export destination stat error = %v; want absent destination", statErr)
	}
	partials := toolingStagingMatches(t, dest)
	if len(partials) != 0 {
		t.Fatalf("partial exports = %v; want none", partials)
	}
}

func exportLines(t *testing.T, path string) [][]byte {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var lines [][]byte
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, append([]byte(nil), scanner.Bytes()...))
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func exportColumnNames(t *testing.T, db queryer, table string) []string {
	t.Helper()

	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var cid int
		var name, typeName string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typeName, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	return names
}

type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

func mapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
