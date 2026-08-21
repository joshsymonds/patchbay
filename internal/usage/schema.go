package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const finalizedResponseAccountingMigrationVersion = 2

const rateCardsAndFinalCostMigrationVersion = 3

const importedLedgerProvenanceMigrationVersion = 4

const upstreamTargetDispatchEvidenceMigrationVersion = 5

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
		{
			version: finalizedResponseAccountingMigrationVersion,
			name:    "finalized_response_accounting",
			statements: []string{
				`ALTER TABLE accounting_requests ADD COLUMN response_id TEXT`,
				`ALTER TABLE accounting_requests ADD COLUMN http_status INTEGER CHECK (http_status IS NULL OR http_status BETWEEN 100 AND 599)`,
				`ALTER TABLE accounting_requests ADD COLUMN normalization_complete INTEGER CHECK (normalization_complete IS NULL OR normalization_complete IN (0, 1))`,
				`ALTER TABLE accounting_requests ADD COLUMN input_tokens INTEGER CHECK (input_tokens IS NULL OR input_tokens >= 0)`,
				`ALTER TABLE accounting_requests ADD COLUMN output_tokens INTEGER CHECK (output_tokens IS NULL OR output_tokens >= 0)`,
				`ALTER TABLE accounting_requests ADD COLUMN cache_creation_input_tokens INTEGER CHECK (cache_creation_input_tokens IS NULL OR cache_creation_input_tokens >= 0)`,
				`ALTER TABLE accounting_requests ADD COLUMN cache_read_input_tokens INTEGER CHECK (cache_read_input_tokens IS NULL OR cache_read_input_tokens >= 0)`,
				`ALTER TABLE accounting_requests ADD COLUMN cache_creation_5m_input_tokens INTEGER CHECK (cache_creation_5m_input_tokens IS NULL OR cache_creation_5m_input_tokens >= 0)`,
				`ALTER TABLE accounting_requests ADD COLUMN cache_creation_1h_input_tokens INTEGER CHECK (cache_creation_1h_input_tokens IS NULL OR cache_creation_1h_input_tokens >= 0)`,
				`ALTER TABLE accounting_requests ADD COLUMN provider_cost_present INTEGER CHECK (provider_cost_present IS NULL OR provider_cost_present IN (0, 1))`,
				`ALTER TABLE accounting_requests ADD COLUMN provider_cost_valid INTEGER CHECK (provider_cost_valid IS NULL OR provider_cost_valid IN (0, 1))`,
				`ALTER TABLE accounting_requests ADD COLUMN provider_cost_nano_usd INTEGER CHECK (provider_cost_nano_usd IS NULL OR provider_cost_nano_usd >= 0)`,
				`ALTER TABLE accounting_requests ADD COLUMN stop_reason TEXT`,
				`ALTER TABLE accounting_requests ADD COLUMN block_shape TEXT`,
				`ALTER TABLE accounting_requests ADD COLUMN duration_ms INTEGER CHECK (duration_ms IS NULL OR duration_ms >= 0)`,
				`ALTER TABLE accounting_requests ADD COLUMN upstream_ms INTEGER CHECK (upstream_ms IS NULL OR upstream_ms >= 0)`,
				`CREATE TRIGGER accounting_requests_validate_finalized_snapshot
				BEFORE UPDATE ON accounting_requests
				WHEN NEW.lifecycle_state = 'finalized' AND (
					NEW.completed_at IS NULL
					OR NEW.completed_at < NEW.started_at
					OR NEW.outcome IS NULL
					OR NEW.outcome NOT IN ('success', 'local_rejection', 'upstream_http_failure', 'transport_failure', 'response_interrupted', 'normalization_failure')
					OR NEW.cost_status NOT IN ('known', 'unknown')
					OR NEW.normalization_complete IS NULL
					OR NEW.provider_cost_present IS NULL
					OR NEW.provider_cost_valid IS NULL
					OR NEW.provider_cost_nano_usd IS NULL
					OR NOT (
						(NEW.provider_cost_present = 0 AND NEW.provider_cost_valid = 0 AND NEW.provider_cost_nano_usd = 0)
						OR (NEW.provider_cost_present = 1 AND NEW.provider_cost_valid = 0 AND NEW.provider_cost_nano_usd = 0)
						OR (NEW.provider_cost_present = 1 AND NEW.provider_cost_valid = 1 AND NEW.provider_cost_nano_usd >= 0)
					)
					OR NEW.response_id = ''
					OR NEW.stop_reason IS NULL
					OR NEW.block_shape IS NULL
					OR NEW.duration_ms IS NULL
					OR NEW.upstream_ms IS NULL
					OR NEW.upstream_contacted NOT IN (0, 1)
					OR (NEW.upstream_contacted = 0 AND NEW.upstream_dispatched_at IS NOT NULL)
					OR (NEW.upstream_contacted = 1 AND NEW.upstream_dispatched_at IS NULL)
					OR (NEW.upstream_dispatched_at IS NOT NULL AND NEW.completed_at < NEW.upstream_dispatched_at)
				)
				BEGIN
					SELECT RAISE(ABORT, 'invalid finalized accounting snapshot');
				END`,
				`CREATE UNIQUE INDEX accounting_requests_finalized_response_id ON accounting_requests(response_id) WHERE lifecycle_state = 'finalized' AND response_id IS NOT NULL AND response_id <> ''`,
			},
		},
		rateCardsAndFinalCostMigration(),
		importedLedgerProvenanceMigration(),
		upstreamTargetDispatchEvidenceMigration(),
	}
}

func importedLedgerProvenanceMigration() migration {
	return migration{
		version: importedLedgerProvenanceMigrationVersion,
		name:    "imported_ledger_provenance",
		statements: []string{
			`ALTER TABLE accounting_requests ADD COLUMN import_source TEXT`,
			`ALTER TABLE accounting_requests ADD COLUMN import_line INTEGER CHECK (
				(import_line IS NULL OR import_line > 0)
				AND (import_source IS NULL) = (import_line IS NULL)
			)`,
			`CREATE UNIQUE INDEX accounting_requests_import_provenance
			ON accounting_requests(import_source, import_line) WHERE import_source IS NOT NULL`,
			`CREATE TRIGGER accounting_requests_validate_finalized_snapshot_insert
			BEFORE INSERT ON accounting_requests
			WHEN NEW.lifecycle_state = 'finalized' AND (
				NEW.completed_at IS NULL
				OR NEW.completed_at < NEW.started_at
				OR NEW.outcome IS NULL
				OR NEW.outcome NOT IN ('success', 'local_rejection', 'upstream_http_failure', 'transport_failure', 'response_interrupted', 'normalization_failure')
				OR NEW.cost_status NOT IN ('known', 'unknown')
				OR NEW.normalization_complete IS NULL
				OR NEW.provider_cost_present IS NULL
				OR NEW.provider_cost_valid IS NULL
				OR NEW.provider_cost_nano_usd IS NULL
				OR NOT (
					(NEW.provider_cost_present = 0 AND NEW.provider_cost_valid = 0 AND NEW.provider_cost_nano_usd = 0)
					OR (NEW.provider_cost_present = 1 AND NEW.provider_cost_valid = 0 AND NEW.provider_cost_nano_usd = 0)
					OR (NEW.provider_cost_present = 1 AND NEW.provider_cost_valid = 1 AND NEW.provider_cost_nano_usd >= 0)
				)
				OR NEW.response_id = ''
				OR NEW.stop_reason IS NULL
				OR NEW.block_shape IS NULL
				OR NEW.duration_ms IS NULL
				OR NEW.upstream_ms IS NULL
				OR NEW.upstream_contacted NOT IN (0, 1)
				OR (NEW.upstream_contacted = 0 AND NEW.upstream_dispatched_at IS NOT NULL)
				OR (NEW.upstream_contacted = 1 AND NEW.upstream_dispatched_at IS NULL)
				OR (NEW.upstream_dispatched_at IS NOT NULL AND NEW.completed_at < NEW.upstream_dispatched_at)
			)
			BEGIN
				SELECT RAISE(ABORT, 'invalid finalized accounting snapshot');
			END`,
		},
	}
}

func upstreamTargetDispatchEvidenceMigration() migration {
	return migration{
		version: upstreamTargetDispatchEvidenceMigrationVersion,
		name:    "upstream_target_dispatch_evidence",
		statements: []string{
			`ALTER TABLE accounting_requests ADD COLUMN upstream_target TEXT CHECK (
				upstream_target IS NULL OR (length(upstream_target) > 0 AND upstream_contacted = 1)
			)`,
		},
	}
}

func rateCardsAndFinalCostMigration() migration {
	return migration{
		version: rateCardsAndFinalCostMigrationVersion,
		name:    "rate_cards_and_final_cost",
		statements: []string{
			`CREATE TABLE rate_cards (
				id INTEGER PRIMARY KEY,
				model TEXT NOT NULL,
				effective_from INTEGER NOT NULL,
				input_nano_usd_per_million INTEGER NOT NULL CHECK (input_nano_usd_per_million >= 0),
				output_nano_usd_per_million INTEGER NOT NULL CHECK (output_nano_usd_per_million >= 0),
				cache_read_nano_usd_per_million INTEGER NOT NULL CHECK (cache_read_nano_usd_per_million >= 0),
				cache_creation_nano_usd_per_million INTEGER NOT NULL CHECK (cache_creation_nano_usd_per_million >= 0),
				cache_creation_5m_nano_usd_per_million INTEGER NOT NULL CHECK (cache_creation_5m_nano_usd_per_million >= 0),
				cache_creation_1h_nano_usd_per_million INTEGER NOT NULL CHECK (cache_creation_1h_nano_usd_per_million >= 0),
				source TEXT NOT NULL,
				created_at INTEGER NOT NULL,
				UNIQUE(model, effective_from)
			)`,
			`CREATE TRIGGER rate_cards_forbid_update
			BEFORE UPDATE ON rate_cards
			BEGIN
				SELECT RAISE(ABORT, 'rate cards are immutable');
			END`,
			`CREATE TRIGGER rate_cards_forbid_delete
			BEFORE DELETE ON rate_cards
			BEGIN
				SELECT RAISE(ABORT, 'rate cards are immutable');
			END`,
			`ALTER TABLE accounting_requests ADD COLUMN cost_basis TEXT CHECK (cost_basis IS NULL OR cost_basis IN ('provider_reported', 'rate_card', 'zero_marginal', 'unknown'))`,
			`ALTER TABLE accounting_requests ADD COLUMN final_cost_nano_usd INTEGER CHECK (final_cost_nano_usd IS NULL OR final_cost_nano_usd >= 0)`,
			`ALTER TABLE accounting_requests ADD COLUMN rate_card_id INTEGER CHECK (rate_card_id IS NULL OR rate_card_id > 0) REFERENCES rate_cards(id)`,
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
