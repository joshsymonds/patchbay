package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func openStore(t *testing.T, path string) *Store {
	t.Helper()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q) returned error: %v", path, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() returned error: %v", err)
		}
	})
	return store
}

func inspectDB(t *testing.T, path string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("inspection DB Close() returned error: %v", err)
		}
	})
	return db
}

func TestOpenCreatesSecureMigratedDatabase(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	parent := filepath.Join(root, "state", "patchbay")
	path := filepath.Join(parent, "usage.sqlite")
	store := openStore(t, path)

	assertMode(t, parent, 0o700)
	assertMode(t, path, 0o600)

	db := store.db
	assertPragma(t, db, "journal_mode", "wal")
	assertPragma(t, db, "foreign_keys", "1")
	assertPragma(t, db, "busy_timeout", "5000")
	assertConnectionPragmas(t, db)

	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read migration version: %v", err)
	}
	if version != 2 {
		t.Fatalf("migration version = %d; want 2", version)
	}

	var table string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'accounting_requests'`,
	).Scan(&table); err != nil {
		t.Fatalf("find accounting_requests table: %v", err)
	}
}

func TestPrepareDatabasePathPrecreatesSecureDatabase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "usage.sqlite")
	if err := prepareDatabasePath(path); err != nil {
		t.Fatalf("prepareDatabasePath(%q): %v", path, err)
	}
	assertMode(t, filepath.Dir(path), parentMode)
	assertMode(t, path, databaseMode)
}

func TestPrepareDatabasePathCorrectsRestrictiveUmask(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "usage.sqlite")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPrepareDatabasePathCorrectsRestrictiveUmaskHelper$")
	cmd.Env = append(os.Environ(), "PATCHBAY_USAGE_RESTRICTIVE_UMASK_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prepareDatabasePath under restrictive umask: %v\n%s", err, output)
	}

	assertMode(t, path, databaseMode)
}

func TestPrepareDatabasePathCorrectsRestrictiveUmaskHelper(t *testing.T) {
	path := os.Getenv("PATCHBAY_USAGE_RESTRICTIVE_UMASK_PATH")
	if path == "" {
		return
	}

	previousUmask := syscall.Umask(0o777)
	defer syscall.Umask(previousUmask)

	if err := prepareDatabasePath(path); err != nil {
		t.Fatalf("prepareDatabasePath(%q): %v", path, err)
	}
	assertMode(t, path, databaseMode)
}

func TestSecureCreatedDatabaseReportsChmodAndCloseFailures(t *testing.T) {
	t.Parallel()

	chmodFailure := errors.New("chmod failed")
	closeFailure := errors.New("close failed")
	for _, tc := range []struct {
		name     string
		chmodErr error
		closeErr error
	}{
		{name: "chmod", chmodErr: chmodFailure},
		{name: "close", closeErr: closeFailure},
		{name: "both", chmodErr: chmodFailure, closeErr: closeFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var calls []string
			err := secureCreatedDatabase(
				"usage.sqlite",
				123,
				func(int, uint32) error {
					calls = append(calls, "chmod")
					return tc.chmodErr
				},
				func(int) error {
					calls = append(calls, "close")
					return tc.closeErr
				},
			)
			if err == nil {
				t.Fatal("secureCreatedDatabase returned nil error")
			}
			if tc.chmodErr != nil && !errors.Is(err, tc.chmodErr) {
				t.Errorf("error = %v; want chmod error %v", err, tc.chmodErr)
			}
			if tc.closeErr != nil && !errors.Is(err, tc.closeErr) {
				t.Errorf("error = %v; want close error %v", err, tc.closeErr)
			}
			if got := strings.Join(calls, ","); got != "chmod,close" {
				t.Errorf("operations = %q; want chmod,close", got)
			}
		})
	}
}

func TestOpenRejectsDatabaseSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	targetParent := filepath.Join(root, "target")
	if err := os.Mkdir(targetParent, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetParent, "target.sqlite")
	contents := []byte("must remain unopened")
	if err := os.WriteFile(target, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "private", "usage.sqlite")
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Open(%q) error = %v; want symbolic-link rejection", path, err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(contents) {
		t.Fatalf("symlink target contents = %q; want %q", got, contents)
	}
	assertMode(t, targetParent, 0o755)
	assertMode(t, target, 0o644)
}

func TestOpenCorrectsPermissiveModes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	parent := filepath.Join(root, "state")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "usage.sqlite")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	_ = openStore(t, path)
	assertMode(t, parent, 0o700)
	assertMode(t, path, 0o600)
}

func TestOpenIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db := inspectDB(t, path)
	var count, version int
	if err := db.QueryRow(`SELECT COUNT(*), MAX(version) FROM schema_migrations`).
		Scan(&count, &version); err != nil {
		t.Fatal(err)
	}
	if count != 2 || version != 2 {
		t.Fatalf("migration rows = %d, max version = %d; want 2, 2", count, version)
	}
}

func TestAccountingRequestsSchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	_ = openStore(t, path)
	db := inspectDB(t, path)

	type column struct {
		name     string
		typeName string
		notNull  int
		pk       int
	}
	want := []column{
		{"id", "INTEGER", 0, 1},
		{"started_at", "INTEGER", 1, 0},
		{"completed_at", "INTEGER", 0, 0},
		{"upstream_dispatched_at", "INTEGER", 0, 0},
		{"request_kind", "TEXT", 1, 0},
		{"lifecycle_state", "TEXT", 1, 0},
		{"outcome", "TEXT", 0, 0},
		{"upstream_contacted", "INTEGER", 1, 0},
		{"route_host", "TEXT", 1, 0},
		{"route_context", "TEXT", 1, 0},
		{"route_alias", "TEXT", 0, 0},
		{"route_upstream", "TEXT", 1, 0},
		{"request_model", "TEXT", 1, 0},
		{"resolved_model", "TEXT", 1, 0},
		{"auth_mode", "TEXT", 1, 0},
		{"credential_class", "TEXT", 1, 0},
		{"billing_policy", "TEXT", 1, 0},
		{"billing_class", "TEXT", 0, 0},
		{"cost_status", "TEXT", 1, 0},
		{"response_id", "TEXT", 0, 0},
		{"http_status", "INTEGER", 0, 0},
		{"normalization_complete", "INTEGER", 0, 0},
		{"input_tokens", "INTEGER", 0, 0},
		{"output_tokens", "INTEGER", 0, 0},
		{"cache_creation_input_tokens", "INTEGER", 0, 0},
		{"cache_read_input_tokens", "INTEGER", 0, 0},
		{"cache_creation_5m_input_tokens", "INTEGER", 0, 0},
		{"cache_creation_1h_input_tokens", "INTEGER", 0, 0},
		{"provider_cost_present", "INTEGER", 0, 0},
		{"provider_cost_valid", "INTEGER", 0, 0},
		{"provider_cost_nano_usd", "INTEGER", 0, 0},
		{"stop_reason", "TEXT", 0, 0},
		{"block_shape", "TEXT", 0, 0},
		{"duration_ms", "INTEGER", 0, 0},
		{"upstream_ms", "INTEGER", 0, 0},
	}

	rows, err := db.Query(`PRAGMA table_info(accounting_requests)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []column
	for rows.Next() {
		var cid int
		var col column
		var defaultValue any
		if err := rows.Scan(
			&cid,
			&col.name,
			&col.typeName,
			&col.notNull,
			&defaultValue,
			&col.pk,
		); err != nil {
			t.Fatal(err)
		}
		got = append(got, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema mismatch:\n got %#v\nwant %#v", got, want)
	}
}

func TestFinalizedResponseAccountingMigrationDeclarationAndIndex(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)

	var name string
	if err := store.db.QueryRow(
		`SELECT name FROM schema_migrations WHERE version = 2`,
	).Scan(&name); err != nil {
		t.Fatalf("read migration 2 name: %v", err)
	}
	if name != "finalized_response_accounting" {
		t.Fatalf("migration 2 name = %q; want %q", name, "finalized_response_accounting")
	}

	var indexSQL string
	if err := store.db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type = 'index' AND name = 'accounting_requests_finalized_response_id'`).Scan(&indexSQL); err != nil {
		t.Fatalf("read finalized response ID index: %v", err)
	}
	want := "CREATE UNIQUE INDEX accounting_requests_finalized_response_id ON accounting_requests(response_id) WHERE lifecycle_state = 'finalized' AND response_id IS NOT NULL AND response_id <> ''"
	if indexSQL != want {
		t.Fatalf("finalized response ID index SQL = %q; want %q", indexSQL, want)
	}
}

func TestFinalizedAccountingColumnConstraints(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		column string
		value  int64
	}{
		{name: "normalization boolean below range", column: "normalization_complete", value: -1},
		{name: "normalization boolean above range", column: "normalization_complete", value: 2},
		{name: "provider present boolean", column: "provider_cost_present", value: 2},
		{name: "provider valid boolean", column: "provider_cost_valid", value: -1},
		{name: "input tokens", column: "input_tokens", value: -1},
		{name: "output tokens", column: "output_tokens", value: -1},
		{name: "cache creation tokens", column: "cache_creation_input_tokens", value: -1},
		{name: "cache read tokens", column: "cache_read_input_tokens", value: -1},
		{name: "cache 5m tokens", column: "cache_creation_5m_input_tokens", value: -1},
		{name: "cache 1h tokens", column: "cache_creation_1h_input_tokens", value: -1},
		{name: "provider cost", column: "provider_cost_nano_usd", value: -1},
		{name: "duration", column: "duration_ms", value: -1},
		{name: "upstream duration", column: "upstream_ms", value: -1},
		{name: "HTTP below range", column: "http_status", value: 99},
		{name: "HTTP above range", column: "http_status", value: 600},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			store := openStore(t, path)
			id, err := store.InsertPending(context.Background(), validPendingRequest())
			if err != nil {
				t.Fatalf("InsertPending() returned error: %v", err)
			}

			query := fmt.Sprintf(`UPDATE accounting_requests SET %s = ? WHERE id = ?`, test.column)
			if _, err := store.db.Exec(query, test.value, id); err == nil {
				t.Fatalf("setting %s to %d returned nil error", test.column, test.value)
			}
			var stored sql.NullInt64
			query = fmt.Sprintf(`SELECT %s FROM accounting_requests WHERE id = ?`, test.column)
			if err := store.db.QueryRow(query, id).Scan(&stored); err != nil {
				t.Fatalf("read %s: %v", test.column, err)
			}
			if stored.Valid {
				t.Fatalf("%s = %#v after rejected update; want NULL", test.column, stored)
			}
		})
	}
}

func TestFinalizedSnapshotTriggerPreservesPendingRows(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id := insertPendingRequest(t, store)

	if _, err := store.db.Exec(`UPDATE accounting_requests
		SET lifecycle_state = 'finalized' WHERE id = ?`, id); err == nil {
		t.Fatal("incomplete finalized snapshot update returned nil error")
	}
	var lifecycle string
	if err := store.db.QueryRow(`SELECT lifecycle_state FROM accounting_requests WHERE id = ?`, id).
		Scan(&lifecycle); err != nil {
		t.Fatalf("read lifecycle after trigger failure: %v", err)
	}
	if lifecycle != "pending" {
		t.Fatalf("lifecycle after trigger failure = %q; want pending", lifecycle)
	}

	if _, err := store.db.Exec(`UPDATE accounting_requests SET
		completed_at = started_at,
		lifecycle_state = 'finalized',
		outcome = 'success',
		cost_status = 'known',
		normalization_complete = 1,
		provider_cost_present = 0,
		provider_cost_valid = 1,
		provider_cost_nano_usd = 0,
		stop_reason = '',
		block_shape = '[]',
		duration_ms = 0,
		upstream_ms = 0
		WHERE id = ?`, id); err == nil {
		t.Fatal("invalid provider-cost tuple update returned nil error")
	}
	if err := store.db.QueryRow(`SELECT lifecycle_state FROM accounting_requests WHERE id = ?`, id).
		Scan(&lifecycle); err != nil {
		t.Fatalf("read lifecycle after tuple trigger failure: %v", err)
	}
	if lifecycle != "pending" {
		t.Fatalf("lifecycle after tuple trigger failure = %q; want pending", lifecycle)
	}
}

func TestMigrationTwoPreservesExistingPendingData(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	db := inspectDB(t, path)
	if err := applyMigrations(db, declaredMigrations()[:1]); err != nil {
		t.Fatalf("apply migration 1: %v", err)
	}
	const startedAt int64 = 1_776_912_345_678_901_234
	if _, err := db.Exec(`INSERT INTO accounting_requests (
		started_at, completed_at, upstream_dispatched_at, request_kind, lifecycle_state,
		outcome, upstream_contacted, route_host, route_context, route_alias, route_upstream,
		request_model, resolved_model, auth_mode, credential_class, billing_policy,
		billing_class, cost_status
	) VALUES (?, NULL, NULL, 'messages', 'pending', NULL, 0, 'gnomon', 'personal', NULL,
		'https://api.anthropic.com', 'claude-opus-5', 'claude-opus-5', 'forward', 'oauth',
		'credential_derived', 'subscription', 'pending')`, startedAt); err != nil {
		t.Fatalf("insert migration-1 pending row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close migration-1 database: %v", err)
	}

	store := openStore(t, path)
	var gotStartedAt int64
	var lifecycle, costStatus string
	var responseID sql.NullString
	if err := store.db.QueryRow(`SELECT started_at, lifecycle_state, cost_status, response_id
		FROM accounting_requests WHERE id = 1`).Scan(
		&gotStartedAt, &lifecycle, &costStatus, &responseID,
	); err != nil {
		t.Fatalf("read migrated pending row: %v", err)
	}
	if gotStartedAt != startedAt || lifecycle != "pending" || costStatus != "pending" || responseID.Valid {
		t.Fatalf("migrated row = start %d lifecycle %q cost %q response %#v; want preserved pending row",
			gotStartedAt, lifecycle, costStatus, responseID)
	}
}

func TestOpenRejectsInvalidPathsAndDatabases(t *testing.T) {
	t.Parallel()

	if _, err := Open("  \t"); err == nil || err.Error() != "usage database path is blank" {
		t.Fatalf("blank path error = %v", err)
	}

	dir := t.TempDir()
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("directory path error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "corrupt.sqlite")
	if err := os.WriteFile(path, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil ||
		!strings.Contains(err.Error(), "open usage database") ||
		!strings.Contains(err.Error(), "ping:") {
		t.Fatalf("corrupt database error = %v; want ping stage", err)
	}
}

func TestOpenRejectsUnknownFutureMigration(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db := inspectDB(t, path)
	if _, err := db.Exec(
		`INSERT INTO schema_migrations(version, name) VALUES (99, 'future')`,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(
		path,
	); err == nil ||
		!strings.Contains(err.Error(), "migrate:") ||
		!strings.Contains(err.Error(), "unknown future schema version 99") {
		t.Fatalf("future migration error = %v; want migration stage", err)
	}
}

func TestOpenRejectsRecordedMigrationNameMismatch(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	if _, err := store.db.Exec(
		`UPDATE schema_migrations SET name = 'wrong_name' WHERE version = 1`,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(
		path,
	); err == nil ||
		!strings.Contains(err.Error(), `recorded as "wrong_name"`) {
		t.Fatalf("Open(%q) error = %v; want recorded-name mismatch", path, err)
	}
}

func TestMigrationDeclarationsMustBeOrderedAndUnique(t *testing.T) {
	for _, tc := range []struct {
		name       string
		migrations []migration
	}{
		{"duplicate", []migration{{version: 1, name: "one"}, {version: 1, name: "again"}}},
		{"out of order", []migration{{version: 2, name: "two"}, {version: 1, name: "one"}}},
		{"blank", []migration{{version: 1, name: ""}}},
		{"whitespace", []migration{{version: 1, name: " \t\n"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateMigrations(tc.migrations); err == nil {
				t.Fatal("validateMigrations returned nil error")
			}
		})
	}
}

func TestFailingMigrationIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	failing := migration{version: 3, name: "fail", statements: []string{
		`CREATE TABLE must_not_survive (id INTEGER)`,
		`THIS IS NOT SQL`,
	}}
	if err := applyMigrations(db, append(declaredMigrations(), failing)); err == nil {
		t.Fatal("applyMigrations returned nil error")
	}

	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'must_not_survive'`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration published partial schema")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 3`).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration published its version")
	}
}

func TestCommitFailureRollsBackMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	readerDB := inspectDB(t, path)
	writerDB := inspectDB(t, path)
	for _, db := range []*sql.DB{readerDB, writerDB} {
		if _, err := db.Exec(`PRAGMA journal_mode = DELETE`); err != nil {
			t.Fatal(err)
		}
	}
	const migrationTable = `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at INTEGER NOT NULL DEFAULT (unixepoch('subsec') * 1000000000)
	)`
	if _, err := writerDB.Exec(migrationTable); err != nil {
		t.Fatal(err)
	}
	if _, err := writerDB.Exec(`PRAGMA busy_timeout = 1`); err != nil {
		t.Fatal(err)
	}

	reader, err := readerDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.ExecContext(context.Background(), `BEGIN`); err != nil {
		t.Fatal(err)
	}
	defer reader.ExecContext(context.Background(), `ROLLBACK`)
	var version int
	row := reader.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM schema_migrations`,
	)
	if err := row.Scan(&version); err != nil {
		t.Fatal(err)
	}

	writerDB.SetMaxOpenConns(1)
	candidate := migration{
		version:    1,
		name:       "blocked_commit",
		statements: []string{`CREATE TABLE must_not_commit (id INTEGER)`},
	}
	migrationErr := applyMigration(context.Background(), writerDB, candidate)
	if migrationErr == nil || !strings.Contains(migrationErr.Error(), "commit migration") {
		t.Fatalf("applyMigration commit error = %v; want busy commit failure", migrationErr)
	}
	if _, err := reader.ExecContext(context.Background(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if err := writerDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'must_not_commit'`).
		Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("failed commit published partial schema: %d tables", version)
	}
	if err := writerDB.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).
		Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("failed commit recorded migration version: %d rows", version)
	}
	if _, err := writerDB.Exec(`CREATE TABLE connection_is_usable (id INTEGER)`); err != nil {
		t.Fatalf("connection unusable after commit failure: %v", err)
	}
}

func TestConcurrentOpenConverges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "usage.sqlite")
	const workers = 12
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			errs <- store.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Open: %v", err)
		}
	}

	db := inspectDB(t, path)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("migration 1 rows = %d; want 1", count)
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).
		Scan(&integrity); err != nil ||
		integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v; want ok", integrity, err)
	}
}

func TestInitializeWALRetriesTypedBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	blocker := inspectDB(t, path)
	contender := inspectDB(t, path)
	blockerConn, err := blocker.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blockerConn.Close()
	if _, err := blockerConn.ExecContext(t.Context(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if !locked {
			return
		}
		if _, err := blockerConn.ExecContext(t.Context(), `ROLLBACK`); err != nil {
			t.Errorf("release SQLite lock: %v", err)
		}
	}()

	attempts := 0
	var firstErr error
	err = initializeWAL(t.Context(), func(ctx context.Context) (string, error) {
		attempts++
		var mode string
		setErr := contender.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode)
		if attempts == 1 {
			firstErr = setErr
			if _, rollbackErr := blockerConn.ExecContext(ctx, `ROLLBACK`); rollbackErr != nil {
				return "", rollbackErr
			}
			locked = false
		}
		return mode, setErr
	})
	if err != nil {
		t.Fatalf("initializeWAL returned error: %v", err)
	}
	var sqliteErr *sqlite.Error
	if !errors.As(firstErr, &sqliteErr) {
		t.Fatalf("first journal_mode error = %T %v; want *sqlite.Error", firstErr, firstErr)
	}
	if sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Fatalf("first journal_mode code = %d; want primary SQLITE_BUSY", sqliteErr.Code())
	}
	if attempts < 2 {
		t.Fatalf("journal_mode attempts = %d; want at least 2 after typed SQLITE_BUSY", attempts)
	}
	assertPragma(t, contender, "journal_mode", "wal")
}

func TestInitializeWALRejectsUnexpectedResultOrError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		err     error
		wantErr string
	}{
		{name: "wrong mode", mode: "delete", wantErr: `returned "delete"; want "wal"`},
		{name: "non-busy error", err: errors.New("disk failure"), wantErr: "set journal mode to WAL: disk failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			err := initializeWAL(t.Context(), func(context.Context) (string, error) {
				attempts++
				return tc.mode, tc.err
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("initializeWAL error = %v; want %q", err, tc.wantErr)
			}
			if attempts != 1 {
				t.Fatalf("journal_mode attempts = %d; want 1", attempts)
			}
		})
	}
}

func TestInitializeWALStopsAfterContextCancellation(t *testing.T) {
	busyErr := sqliteBusyError(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	attempts := 0
	err := initializeWAL(ctx, func(context.Context) (string, error) {
		attempts++
		cancel()
		return "", busyErr
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("initializeWAL error = %v; want context cancellation", err)
	}
	if attempts != 1 {
		t.Fatalf("journal_mode attempts = %d; want 1", attempts)
	}
}

func sqliteBusyError(t *testing.T) error {
	t.Helper()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	blocker := inspectDB(t, path)
	contender := inspectDB(t, path)
	blockerConn, err := blocker.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blockerConn.Close()
	if _, err := blockerConn.ExecContext(t.Context(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := blockerConn.ExecContext(t.Context(), `ROLLBACK`); err != nil {
			t.Errorf("release SQLite lock: %v", err)
		}
	}()

	var mode string
	err = contender.QueryRowContext(t.Context(), `PRAGMA journal_mode = WAL`).Scan(&mode)
	if err == nil {
		t.Fatal("PRAGMA journal_mode = WAL returned nil error while EXCLUSIVE lock was held")
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Fatalf("journal_mode error = %T %v; want typed SQLITE_BUSY", err, err)
	}
	return err
}

func assertConnectionPragmas(t *testing.T, db *sql.DB) {
	t.Helper()

	db.SetMaxOpenConns(2)
	first, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, conn := range []*sql.Conn{first, second} {
		for name, want := range map[string]string{
			"busy_timeout": "5000",
			"foreign_keys": "1",
			"journal_mode": "wal",
		} {
			var got string
			if err := conn.QueryRowContext(context.Background(), fmt.Sprintf("PRAGMA %s", name)).
				Scan(&got); err != nil {
				t.Fatalf("read PRAGMA %s: %v", name, err)
			}
			if got != want {
				t.Fatalf("PRAGMA %s = %q; want %q", name, got, want)
			}
		}
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %#o; want %#o", path, got, want)
	}
}

func assertPragma(t *testing.T, db *sql.DB, name, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(fmt.Sprintf("PRAGMA %s", name)).Scan(&got); err != nil {
		t.Fatalf("read PRAGMA %s: %v", name, err)
	}
	if got != want {
		t.Fatalf("PRAGMA %s = %q; want %q", name, got, want)
	}
}
