package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type migration struct {
	version    int
	name       string
	statements []string
}

func declaredMigrations() []migration {
	return []migration{
		{
			version: 1,
			name:    "accounting_requests",
			statements: []string{
				`CREATE TABLE accounting_requests (
					id INTEGER PRIMARY KEY,
					started_at INTEGER NOT NULL,
					completed_at INTEGER,
					upstream_dispatched_at INTEGER,
					request_kind TEXT NOT NULL,
					lifecycle_state TEXT NOT NULL,
					outcome TEXT,
					upstream_contacted INTEGER NOT NULL,
					route_host TEXT NOT NULL,
					route_context TEXT NOT NULL,
					route_alias TEXT,
					route_upstream TEXT NOT NULL,
					request_model TEXT NOT NULL,
					resolved_model TEXT NOT NULL,
					auth_mode TEXT NOT NULL,
					credential_class TEXT NOT NULL,
					billing_policy TEXT NOT NULL,
					billing_class TEXT,
					cost_status TEXT NOT NULL
				)`,
			},
		},
	}
}

func migrate(db *sql.DB) error {
	return applyMigrations(db, declaredMigrations())
}

func validateMigrations(declared []migration) error {
	previous := 0
	for _, candidate := range declared {
		if candidate.version <= previous {
			return fmt.Errorf(
				"migration version %d (%q) is duplicate or out of order after %d",
				candidate.version,
				candidate.name,
				previous,
			)
		}
		if strings.TrimSpace(candidate.name) == "" {
			return fmt.Errorf("migration version %d has a blank name", candidate.version)
		}
		previous = candidate.version
	}
	return nil
}

func applyMigrations(db *sql.DB, declared []migration) error {
	if err := validateMigrations(declared); err != nil {
		return err
	}

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at INTEGER NOT NULL DEFAULT (unixepoch('subsec') * 1000000000)
	)`); err != nil {
		return fmt.Errorf("bootstrap schema migrations: %w", err)
	}
	if err := rejectFutureVersion(ctx, db, declared); err != nil {
		return err
	}
	for _, candidate := range declared {
		if err := applyMigration(ctx, db, candidate); err != nil {
			return err
		}
	}
	return nil
}

func rejectFutureVersion(ctx context.Context, db *sql.DB, declared []migration) error {
	latest := 0
	if len(declared) > 0 {
		latest = declared[len(declared)-1].version
	}
	var future int
	err := db.QueryRowContext(
		ctx,
		`SELECT version FROM schema_migrations WHERE version > ? ORDER BY version DESC LIMIT 1`,
		latest,
	).Scan(&future)
	if err == nil {
		return fmt.Errorf(
			"unknown future schema version %d (latest supported is %d)",
			future,
			latest,
		)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect schema version: %w", err)
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, candidate migration) (result error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration %d: %w", candidate.version, err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close migration connection: %w", closeErr))
		}
	}()

	if _, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin migration %d (%s): %w", candidate.version, candidate.name, err)
	}
	if err = executeMigration(ctx, conn, candidate); err != nil {
		return rollbackMigration(ctx, conn, err)
	}
	if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
		return rollbackMigration(
			ctx,
			conn,
			fmt.Errorf("commit migration %d (%s): %w", candidate.version, candidate.name, err),
		)
	}
	return nil
}

func executeMigration(ctx context.Context, conn *sql.Conn, candidate migration) error {
	var recordedName string
	err := conn.QueryRowContext(
		ctx,
		`SELECT name FROM schema_migrations WHERE version = ?`,
		candidate.version,
	).Scan(&recordedName)
	if err == nil {
		if recordedName != candidate.name {
			return fmt.Errorf(
				"schema version %d is recorded as %q, want %q",
				candidate.version,
				recordedName,
				candidate.name,
			)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect migration %d: %w", candidate.version, err)
	}

	for _, statement := range candidate.statements {
		if _, err = conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", candidate.version, candidate.name, err)
		}
	}
	if _, err = conn.ExecContext(
		ctx,
		`INSERT INTO schema_migrations(version, name) VALUES (?, ?)`,
		candidate.version,
		candidate.name,
	); err != nil {
		return fmt.Errorf("record migration %d (%s): %w", candidate.version, candidate.name, err)
	}
	return nil
}

func rollbackMigration(ctx context.Context, conn *sql.Conn, cause error) error {
	if _, rollbackErr := conn.ExecContext(ctx, `ROLLBACK`); rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("roll back migration: %w", rollbackErr))
	}
	return cause
}
