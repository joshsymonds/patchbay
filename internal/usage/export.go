package usage

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Export writes a complete, deterministic JSONL view of source to dest.
//
// The rows it writes are the current schema's columns, whatever schema the source
// records, so a source already at the current version is read directly and an
// older one is brought forward first. Bringing it forward in place is not an
// option: the source is a live accounting database and a read command must leave
// it byte for byte as it found it. It is copied instead, and the copy is what
// gets migrated and read.
func Export(ctx context.Context, source, dest string) (result error) {
	if contextErr := validateToolingContext(ctx); contextErr != nil {
		return contextErr
	}
	prepared, prepareErr := prepareToolingDestination(dest)
	if prepareErr != nil {
		return prepareErr
	}
	defer func() {
		temp := prepared.temp
		if removeErr := removeToolingStaging(prepared); removeErr != nil {
			result = errors.Join(result, fmt.Errorf("remove partial usage export %q: %w", temp, removeErr))
		}
	}()

	if writeErr := writeUsageExport(ctx, source, prepared.temp); writeErr != nil {
		return writeErr
	}
	if hook, ok := ctx.Value(toolingBeforePublishHookKey{}).(func(string)); ok {
		hook(prepared.temp)
	}
	return finalizeToolingDestination(prepared, dest)
}

// writeUsageExport reads source by whichever route its recorded schema calls for
// and leaves the finished export at temp.
func writeUsageExport(ctx context.Context, source, temp string) (result error) {
	db, version, openErr := openVersionedToolingDatabase(ctx, source)
	if openErr != nil {
		return openErr
	}
	closed := false
	defer func() {
		if closed {
			return
		}
		if closeErr := db.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close usage database %q: %w", source, closeErr))
		}
	}()

	if version == latestSchemaVersion() {
		if writeErr := writeUsageExportFile(ctx, db, temp); writeErr != nil {
			return writeErr
		}
		if closeErr := db.Close(); closeErr != nil {
			return fmt.Errorf("close usage database %q: %w", source, closeErr)
		}
		closed = true
		return nil
	}

	// The clone lives in the private staging directory the export is being built
	// in, so it is removed with everything else even if this returns early, and it
	// is removed explicitly here so it never sits beside the finished export.
	clone := temp + toolingMigrationCloneSuffix
	defer func() {
		result = errors.Join(result, removeToolingMigrationClone(clone))
	}()
	if cloneErr := cloneToolingSource(ctx, db, source, clone); cloneErr != nil {
		return cloneErr
	}
	if closeErr := db.Close(); closeErr != nil {
		return fmt.Errorf("close usage database %q: %w", source, closeErr)
	}
	closed = true
	return writeMigratedUsageExport(ctx, clone, temp)
}

// cloneToolingSource copies the open source through SQLite, so the copy is
// consistent with a live WAL writer rather than a torn read of the file, and
// secures it before anything opens it for writing.
func cloneToolingSource(ctx context.Context, db *sql.DB, source, clone string) error {
	if _, vacuumErr := db.ExecContext(ctx, `VACUUM INTO ?`, clone); vacuumErr != nil {
		return fmt.Errorf("copy usage database %q for migration: %w", source, vacuumErr)
	}
	return secureToolingMigrationClone(clone)
}

// secureToolingMigrationClone narrows the clone SQLite just created to the
// database mode through its own descriptor, opened without following a link, so
// the mode lands on that inode rather than on whatever the path resolves to by
// the time a second call gets there. The creation mode cannot be relied on: the
// process umask only ever narrows it, and SQLite chooses it.
func secureToolingMigrationClone(clone string) (result error) {
	file, openErr := openToolingFile(clone, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC)
	if openErr != nil {
		return fmt.Errorf("open usage migration clone %q to secure it: %w", clone, openErr)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			result = errors.Join(
				result,
				fmt.Errorf("close usage migration clone %q after securing it: %w", clone, closeErr),
			)
		}
	}()
	if chmodErr := file.Chmod(databaseMode); chmodErr != nil {
		return fmt.Errorf("secure usage migration clone %q: %w", clone, chmodErr)
	}
	return nil
}

// writeMigratedUsageExport brings the clone up to the current schema and exports
// from it. Migration proves the clone reads back coherently and is still a usage
// database afterwards; the reopen then reads it through the same validated
// read-only path any other database gets, with the migrating connection closed.
func writeMigratedUsageExport(ctx context.Context, clone, temp string) (result error) {
	if migrateErr := migrateToolingMigrationClone(ctx, clone); migrateErr != nil {
		return fmt.Errorf("migrate usage export source copy: %w", migrateErr)
	}
	db, openErr := openGeneratedToolingDatabase(ctx, clone)
	if openErr != nil {
		return fmt.Errorf("reopen migrated usage export source copy: %w", openErr)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close usage migration clone %q: %w", clone, closeErr))
		}
	}()
	return writeUsageExportFile(ctx, db, temp)
}

func writeUsageExportFile(ctx context.Context, db *sql.DB, temp string) error {
	file, createErr := openToolingFile(temp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL)
	if createErr != nil {
		return fmt.Errorf("create temporary usage export %q: %w", temp, createErr)
	}
	writer := bufio.NewWriter(file)
	if exportErr := writeExport(ctx, db, writer); exportErr != nil {
		return closeUsageExportFile(file, temp, exportErr)
	}
	if flushErr := writer.Flush(); flushErr != nil {
		return closeUsageExportFile(file, temp, fmt.Errorf("flush usage export: %w", flushErr))
	}
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("close temporary usage export %q: %w", temp, closeErr)
	}
	return nil
}

func closeUsageExportFile(file *os.File, temp string, cause error) error {
	if closeErr := file.Close(); closeErr != nil {
		return errors.Join(cause, fmt.Errorf("close temporary usage export %q: %w", temp, closeErr))
	}
	return cause
}

type exportHeader struct {
	PatchbayUsageExport int `json:"patchbay_usage_export"`
	SchemaVersion       int `json:"schema_version"`
}

type exportRecord[T any] struct {
	Table string `json:"table"`
	Row   T      `json:"row"`
}

type rateCardExportRow struct {
	ID                               int64  `json:"id"`
	Model                            string `json:"model"`
	EffectiveFrom                    int64  `json:"effective_from"`
	InputNanoUSDPerMillion           int64  `json:"input_nano_usd_per_million"`
	OutputNanoUSDPerMillion          int64  `json:"output_nano_usd_per_million"`
	CacheReadNanoUSDPerMillion       int64  `json:"cache_read_nano_usd_per_million"`
	CacheCreationNanoUSDPerMillion   int64  `json:"cache_creation_nano_usd_per_million"`
	CacheCreation5mNanoUSDPerMillion int64  `json:"cache_creation_5m_nano_usd_per_million"`
	CacheCreation1hNanoUSDPerMillion int64  `json:"cache_creation_1h_nano_usd_per_million"`
	Source                           string `json:"source"`
	CreatedAt                        int64  `json:"created_at"`
}

type accountingRequestExportRow struct {
	ID                         int64   `json:"id"`
	StartedAt                  int64   `json:"started_at"`
	CompletedAt                *int64  `json:"completed_at"`
	UpstreamDispatchedAt       *int64  `json:"upstream_dispatched_at"`
	UpstreamTarget             *string `json:"upstream_target"`
	RequestKind                string  `json:"request_kind"`
	LifecycleState             string  `json:"lifecycle_state"`
	Outcome                    *string `json:"outcome"`
	UpstreamContacted          int64   `json:"upstream_contacted"`
	RouteHost                  string  `json:"route_host"`
	RouteContext               string  `json:"route_context"`
	RouteAlias                 *string `json:"route_alias"`
	RouteUpstream              string  `json:"route_upstream"`
	RequestModel               string  `json:"request_model"`
	ResolvedModel              string  `json:"resolved_model"`
	AuthMode                   string  `json:"auth_mode"`
	CredentialClass            string  `json:"credential_class"`
	BillingPolicy              string  `json:"billing_policy"`
	BillingClass               *string `json:"billing_class"`
	CostStatus                 string  `json:"cost_status"`
	ResponseID                 *string `json:"response_id"`
	HTTPStatus                 *int64  `json:"http_status"`
	NormalizationComplete      *int64  `json:"normalization_complete"`
	InputTokens                *int64  `json:"input_tokens"`
	OutputTokens               *int64  `json:"output_tokens"`
	CacheCreationInputTokens   *int64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens       *int64  `json:"cache_read_input_tokens"`
	CacheCreation5mInputTokens *int64  `json:"cache_creation_5m_input_tokens"`
	CacheCreation1hInputTokens *int64  `json:"cache_creation_1h_input_tokens"`
	ProviderCostPresent        *int64  `json:"provider_cost_present"`
	ProviderCostValid          *int64  `json:"provider_cost_valid"`
	ProviderCostNanoUSD        *int64  `json:"provider_cost_nano_usd"`
	StopReason                 *string `json:"stop_reason"`
	BlockShape                 *string `json:"block_shape"`
	DurationMS                 *int64  `json:"duration_ms"`
	UpstreamMS                 *int64  `json:"upstream_ms"`
	CostBasis                  *string `json:"cost_basis"`
	FinalCostNanoUSD           *int64  `json:"final_cost_nano_usd"`
	RateCardID                 *int64  `json:"rate_card_id"`
	ImportSource               *string `json:"import_source"`
	ImportLine                 *int64  `json:"import_line"`
}

func writeExport(ctx context.Context, db *sql.DB, writer *bufio.Writer) (result error) {
	tx, beginErr := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if beginErr != nil {
		return fmt.Errorf("begin usage export transaction: %w", beginErr)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			result = errors.Join(result, fmt.Errorf("roll back usage export transaction: %w", rollbackErr))
		}
	}()
	version, versionErr := validateToolingSchema(ctx, tx)
	if versionErr != nil {
		return versionErr
	}
	encoder := json.NewEncoder(writer)
	if headerErr := encoder.Encode(exportHeader{PatchbayUsageExport: 1, SchemaVersion: version}); headerErr != nil {
		return fmt.Errorf("write usage export header: %w", headerErr)
	}
	if rateCardErr := writeRateCardExport(ctx, tx, encoder); rateCardErr != nil {
		return rateCardErr
	}
	if requestErr := writeAccountingRequestExport(ctx, tx, encoder); requestErr != nil {
		return requestErr
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("commit usage export transaction: %w", commitErr)
	}
	return nil
}

func writeRateCardExport(ctx context.Context, tx *sql.Tx, encoder *json.Encoder) (result error) {
	rows, queryErr := tx.QueryContext(ctx, `SELECT
		id, model, effective_from,
		input_nano_usd_per_million, output_nano_usd_per_million,
		cache_read_nano_usd_per_million, cache_creation_nano_usd_per_million,
		cache_creation_5m_nano_usd_per_million, cache_creation_1h_nano_usd_per_million,
		source, created_at
	FROM rate_cards ORDER BY id`)
	if queryErr != nil {
		return fmt.Errorf("read rate cards for usage export: %w", queryErr)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close rate cards export rows: %w", closeErr))
		}
	}()
	for rows.Next() {
		var row rateCardExportRow
		if scanErr := rows.Scan(
			&row.ID, &row.Model, &row.EffectiveFrom,
			&row.InputNanoUSDPerMillion, &row.OutputNanoUSDPerMillion,
			&row.CacheReadNanoUSDPerMillion, &row.CacheCreationNanoUSDPerMillion,
			&row.CacheCreation5mNanoUSDPerMillion, &row.CacheCreation1hNanoUSDPerMillion,
			&row.Source, &row.CreatedAt,
		); scanErr != nil {
			return fmt.Errorf("scan rate card for usage export: %w", scanErr)
		}
		record := exportRecord[rateCardExportRow]{Table: "rate_cards", Row: row}
		if encodeErr := encoder.Encode(record); encodeErr != nil {
			return fmt.Errorf("write rate card usage export: %w", encodeErr)
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("iterate rate cards for usage export: %w", rowsErr)
	}
	return nil
}

func writeAccountingRequestExport(ctx context.Context, tx *sql.Tx, encoder *json.Encoder) (result error) {
	rows, queryErr := tx.QueryContext(ctx, `SELECT
		id, started_at, completed_at, upstream_dispatched_at, upstream_target, request_kind, lifecycle_state,
		outcome, upstream_contacted, route_host, route_context, route_alias, route_upstream,
		request_model, resolved_model, auth_mode, credential_class, billing_policy, billing_class,
		cost_status, response_id, http_status, normalization_complete, input_tokens, output_tokens,
		cache_creation_input_tokens, cache_read_input_tokens, cache_creation_5m_input_tokens,
		cache_creation_1h_input_tokens, provider_cost_present, provider_cost_valid,
		provider_cost_nano_usd, stop_reason, block_shape, duration_ms, upstream_ms, cost_basis,
		final_cost_nano_usd, rate_card_id, import_source, import_line
	FROM accounting_requests ORDER BY id`)
	if queryErr != nil {
		return fmt.Errorf("read accounting requests for usage export: %w", queryErr)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close accounting request export rows: %w", closeErr))
		}
	}()
	for rows.Next() {
		row, scanErr := scanAccountingRequestExport(rows)
		if scanErr != nil {
			return scanErr
		}
		record := exportRecord[accountingRequestExportRow]{
			Table: "accounting_requests",
			Row:   row,
		}
		if encodeErr := encoder.Encode(record); encodeErr != nil {
			return fmt.Errorf("write accounting request usage export: %w", encodeErr)
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("iterate accounting requests for usage export: %w", rowsErr)
	}
	return nil
}

func scanAccountingRequestExport(rows *sql.Rows) (accountingRequestExportRow, error) {
	var row accountingRequestExportRow
	var completedAt, upstreamDispatchedAt, httpStatus, normalizationComplete sql.NullInt64
	var upstreamTarget sql.NullString
	var inputTokens, outputTokens, cacheCreationInputTokens, cacheReadInputTokens sql.NullInt64
	var cacheCreation5mInputTokens, cacheCreation1hInputTokens sql.NullInt64
	var providerCostPresent, providerCostValid, providerCostNanoUSD sql.NullInt64
	var durationMS, upstreamMS, finalCostNanoUSD, rateCardID, importLine sql.NullInt64
	var outcome, routeAlias, billingClass, responseID, stopReason, blockShape, costBasis, importSource sql.NullString
	if scanErr := rows.Scan(
		&row.ID,
		&row.StartedAt,
		&completedAt,
		&upstreamDispatchedAt,
		&upstreamTarget,
		&row.RequestKind,
		&row.LifecycleState,
		&outcome,
		&row.UpstreamContacted,
		&row.RouteHost,
		&row.RouteContext,
		&routeAlias,
		&row.RouteUpstream,
		&row.RequestModel,
		&row.ResolvedModel,
		&row.AuthMode,
		&row.CredentialClass,
		&row.BillingPolicy,
		&billingClass,
		&row.CostStatus,
		&responseID,
		&httpStatus,
		&normalizationComplete,
		&inputTokens,
		&outputTokens,
		&cacheCreationInputTokens,
		&cacheReadInputTokens,
		&cacheCreation5mInputTokens,
		&cacheCreation1hInputTokens,
		&providerCostPresent,
		&providerCostValid,
		&providerCostNanoUSD,
		&stopReason,
		&blockShape,
		&durationMS,
		&upstreamMS,
		&costBasis,
		&finalCostNanoUSD,
		&rateCardID,
		&importSource,
		&importLine,
	); scanErr != nil {
		return accountingRequestExportRow{}, fmt.Errorf("scan accounting request for usage export: %w", scanErr)
	}
	row.CompletedAt = nullableExportInt(completedAt)
	row.UpstreamDispatchedAt = nullableExportInt(upstreamDispatchedAt)
	row.UpstreamTarget = nullableExportString(upstreamTarget)
	row.Outcome = nullableExportString(outcome)
	row.RouteAlias = nullableExportString(routeAlias)
	row.BillingClass = nullableExportString(billingClass)
	row.ResponseID = nullableExportString(responseID)
	row.HTTPStatus = nullableExportInt(httpStatus)
	row.NormalizationComplete = nullableExportInt(normalizationComplete)
	row.InputTokens = nullableExportInt(inputTokens)
	row.OutputTokens = nullableExportInt(outputTokens)
	row.CacheCreationInputTokens = nullableExportInt(cacheCreationInputTokens)
	row.CacheReadInputTokens = nullableExportInt(cacheReadInputTokens)
	row.CacheCreation5mInputTokens = nullableExportInt(cacheCreation5mInputTokens)
	row.CacheCreation1hInputTokens = nullableExportInt(cacheCreation1hInputTokens)
	row.ProviderCostPresent = nullableExportInt(providerCostPresent)
	row.ProviderCostValid = nullableExportInt(providerCostValid)
	row.ProviderCostNanoUSD = nullableExportInt(providerCostNanoUSD)
	row.StopReason = nullableExportString(stopReason)
	row.BlockShape = nullableExportString(blockShape)
	row.DurationMS = nullableExportInt(durationMS)
	row.UpstreamMS = nullableExportInt(upstreamMS)
	row.CostBasis = nullableExportString(costBasis)
	row.FinalCostNanoUSD = nullableExportInt(finalCostNanoUSD)
	row.RateCardID = nullableExportInt(rateCardID)
	row.ImportSource = nullableExportString(importSource)
	row.ImportLine = nullableExportInt(importLine)
	return row, nil
}

func nullableExportInt(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func nullableExportString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
