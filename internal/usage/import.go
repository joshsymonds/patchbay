package usage

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/ledger"
)

const (
	initialLedgerScanBuffer   = 64 * 1024
	maxLedgerLineBytes        = 16 * 1024 * 1024
	nanosecondsPerMillisecond = int64(time.Millisecond)
	importChunkLines          = 500
	importCleanupTimeout      = time.Second
)

// ImportResult reports one immutable ledger file's accounting outcome.
type ImportResult struct {
	Imported        int
	SkippedBoundary int
	SkippedExisting int
}

// ImportBoundary returns the first live accounting timestamp. Legacy records at
// or after it overlap the SQLite accounting window and are not imported.
func (store *Store) ImportBoundary(ctx context.Context) (int64, error) {
	if ctx == nil {
		return 0, errors.New("import boundary: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("import boundary context: %w", err)
	}
	var boundary sql.NullInt64
	if err := store.db.QueryRowContext(ctx, `SELECT MIN(started_at)
		FROM accounting_requests WHERE import_source IS NULL`).Scan(&boundary); err != nil {
		return 0, fmt.Errorf("import boundary: read live accounting timestamp: %w", err)
	}
	if !boundary.Valid {
		return 0, nil
	}
	return boundary.Int64, nil
}

// ImportLedgerFile imports one legacy ledger file in a single transaction. The
// caller owns file ordering and discovers the boundary once for the whole run.
func (store *Store) ImportLedgerFile(
	ctx context.Context,
	path string,
	source string,
	boundary int64,
) (result ImportResult, err error) {
	if ctx == nil {
		return ImportResult{}, errors.New("import ledger: context is nil")
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return ImportResult{}, fmt.Errorf("import ledger context: %w", contextErr)
	}
	if strings.TrimSpace(path) == "" {
		return ImportResult{}, errors.New("import ledger: path is blank")
	}
	if strings.TrimSpace(source) == "" {
		return ImportResult{}, errors.New("import ledger: source is blank")
	}

	file, openErr := os.Open(filepath.Clean(path))
	if openErr != nil {
		return ImportResult{}, fmt.Errorf("import ledger %q: open: %w", path, openErr)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("import ledger %q: close: %w", path, closeErr))
		}
	}()

	return store.importLedgerChunks(ctx, file, path, source, boundary)
}

func (store *Store) importLedgerChunks(
	ctx context.Context,
	file *os.File,
	path, source string,
	boundary int64,
) (ImportResult, error) {
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, initialLedgerScanBuffer), maxLedgerLineBytes)
	var total ImportResult
	line := 0
	for {
		chunk, finished, err := store.importLedgerChunk(ctx, scanner, path, source, boundary, &line)
		if err != nil {
			return ImportResult{}, err
		}
		total.Imported += chunk.Imported
		total.SkippedBoundary += chunk.SkippedBoundary
		total.SkippedExisting += chunk.SkippedExisting
		if finished {
			return total, nil
		}
	}
}

func (store *Store) importLedgerChunk(
	ctx context.Context,
	scanner *bufio.Scanner,
	path, source string,
	boundary int64,
	line *int,
) (ImportResult, bool, error) {
	var chunk ImportResult
	finished := false
	err := store.withImportTransaction(ctx, path, func(conn *sql.Conn) error {
		for range importChunkLines {
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return fmt.Errorf("scan line %d: %w", *line+1, err)
				}
				finished = true
				return nil
			}
			*line++
			if err := importLedgerLine(ctx, conn, scanner.Bytes(), path, source, *line, boundary, &chunk); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportResult{}, false, err
	}
	return chunk, finished, nil
}

func (store *Store) withImportTransaction(
	ctx context.Context,
	path string,
	action func(*sql.Conn) error,
) (result error) {
	if err := store.acquireWritePermit(ctx); err != nil {
		return fmt.Errorf("import ledger %q: %w", path, err)
	}
	defer store.releaseWritePermit()
	conn, err := store.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("import ledger %q: acquire connection: %w", path, err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("import ledger %q: close connection: %w", path, closeErr))
		}
	}()
	if _, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("import ledger %q: begin transaction: %w", path, err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), importCleanupTimeout)
		defer cancel()
		if rollbackErr := rollbackImport(cleanupCtx, conn); rollbackErr != nil {
			result = errors.Join(result, fmt.Errorf("import ledger %q: roll back transaction: %w", path, rollbackErr))
		}
	}()
	if actionErr := action(conn); actionErr != nil {
		return actionErr
	}
	if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("import ledger %q: commit transaction: %w", path, err)
	}
	committed = true
	return nil
}

func rollbackImport(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		return fmt.Errorf("execute rollback: %w", err)
	}
	return nil
}

func importLedgerLine(
	ctx context.Context,
	conn *sql.Conn,
	contents []byte,
	path, source string,
	line int,
	boundary int64,
	result *ImportResult,
) error {
	var record ledger.Record
	if err := json.Unmarshal(contents, &record); err != nil {
		return importLineError(path, line, fmt.Errorf("parse JSON: %w", err))
	}
	if err := record.Validate(); err != nil {
		return importLineError(path, line, fmt.Errorf("validate ledger record: %w", err))
	}
	if boundary > 0 && record.Timestamp.UnixNano() >= boundary {
		result.SkippedBoundary++
		return nil
	}
	row, err := deriveImportedLedgerRow(record, source, line)
	if err != nil {
		return importLineError(path, line, err)
	}
	inserted, err := insertImportedLedgerRow(ctx, conn, row)
	if err != nil {
		return importLineError(path, line, err)
	}
	if inserted {
		result.Imported++
	} else {
		result.SkippedExisting++
	}
	return nil
}

func importLineError(path string, line int, cause error) error {
	return fmt.Errorf("import ledger %s:%d: %w", path, line, cause)
}

type importedLedgerRow struct {
	source, routeHost, routeContext, routeUpstream, requestModel, resolvedModel                        string
	line, startedAt, completedAt, upstreamContacted, inputTokens, outputTokens, durationMS, upstreamMS int64
	upstreamDispatchedAt                                                                               any
	requestKind, outcome, authMode, credentialClass, billingPolicy, costStatus                         string
	routeAlias, billingClass, httpStatus, finalCost                                                    any
	stopReason, blockShape, costBasis                                                                  string
}

type importedTimeline struct {
	startedAt, completedAt, contacted int64
	dispatched                        any
}

func deriveImportedLedgerRow(
	record ledger.Record,
	source string,
	line int,
) (importedLedgerRow, error) {
	timeline, err := deriveImportedTimeline(record)
	if err != nil {
		return importedLedgerRow{}, err
	}
	shape, err := canonicalImportedBlockShape(record.BlockShape)
	if err != nil {
		return importedLedgerRow{}, err
	}
	policy, class := importedBilling(record.AuthMode, string(record.CredentialClass))
	costStatus, costBasis, finalCost := importedCost(class)
	return importedLedgerRow{
		source:               source,
		line:                 int64(line),
		startedAt:            timeline.startedAt,
		completedAt:          timeline.completedAt,
		upstreamContacted:    timeline.contacted,
		upstreamDispatchedAt: timeline.dispatched,
		requestKind: importedRequestKind(
			record,
		),
		outcome:         importedOutcome(record.Status, timeline.contacted),
		routeHost:       record.Host,
		routeContext:    record.Context,
		routeAlias:      nullableImportedAlias(record.Alias),
		routeUpstream:   record.Upstream,
		requestModel:    record.RequestModel,
		resolvedModel:   record.Model,
		authMode:        record.AuthMode,
		credentialClass: string(record.CredentialClass),
		billingPolicy:   policy,
		billingClass:    class,
		costStatus:      costStatus,
		costBasis:       costBasis,
		finalCost:       finalCost,
		httpStatus: nullableImportedHTTPStatus(
			record.Status,
		),
		stopReason:   record.StopReason,
		blockShape:   shape,
		inputTokens:  record.InputTokens,
		outputTokens: record.OutputTokens,
		durationMS:   record.DurationMS,
		upstreamMS:   record.UpstreamMS,
	}, nil
}

func deriveImportedTimeline(record ledger.Record) (importedTimeline, error) {
	startedAt := record.Timestamp.UnixNano()
	if record.DurationMS > math.MaxInt64/nanosecondsPerMillisecond ||
		startedAt > math.MaxInt64-record.DurationMS*nanosecondsPerMillisecond {
		return importedTimeline{}, errors.New("completion timestamp overflows UnixNano")
	}
	completedAt := startedAt + record.DurationMS*nanosecondsPerMillisecond
	if record.UpstreamMS == 0 {
		return importedTimeline{startedAt: startedAt, completedAt: completedAt}, nil
	}
	if record.UpstreamMS >= record.DurationMS {
		return importedTimeline{
			startedAt:   startedAt,
			completedAt: completedAt,
			contacted:   1,
			dispatched:  startedAt,
		}, nil
	}
	return importedTimeline{
		startedAt: startedAt, completedAt: completedAt, contacted: 1,
		dispatched: completedAt - record.UpstreamMS*nanosecondsPerMillisecond,
	}, nil
}

func canonicalImportedBlockShape(shape []string) (string, error) {
	if len(shape) == 0 {
		shape = []string{}
	}
	encoded, err := json.Marshal(shape)
	if err != nil {
		return "", fmt.Errorf("encode block shape: %w", err)
	}
	return string(encoded), nil
}

func importedRequestKind(record ledger.Record) string {
	// Legacy records lack request-path evidence, so only response evidence can
	// classify a Messages request; this intentionally makes no billing inference.
	if record.Status >= 200 && record.Status <= 299 &&
		(record.OutputTokens > 0 || len(record.BlockShape) > 0 || strings.TrimSpace(record.StopReason) != "") {
		return RequestMessages.String()
	}
	return RequestOther.String()
}

func importedOutcome(status int, contacted int64) string {
	if status >= 200 && status <= 299 {
		return OutcomeSuccess.String()
	}
	if contacted == 1 {
		return OutcomeUpstreamHTTPFailure.String()
	}
	return OutcomeLocalRejection.String()
}

func importedCost(class any) (string, string, any) {
	if class == "subscription" {
		return CostStatusKnown.String(), "zero_marginal", int64(0)
	}
	// Imports intentionally never rate-card price incomplete normalization data.
	return CostStatusUnknown.String(), "unknown", nil
}

func nullableImportedAlias(alias string) any {
	if alias == "" {
		return nil
	}
	return alias
}

func nullableImportedHTTPStatus(status int) any {
	if status < 100 || status > 599 {
		return nil
	}
	return int64(status)
}

func importedBilling(authMode, credential string) (string, any) {
	switch authMode {
	case "forward":
		switch credential {
		case "oauth":
			return billing.PolicyCallerDerived.String(), "subscription"
		case "api_key":
			return billing.PolicyCallerDerived.String(), "metered"
		default:
			return billing.PolicyCallerDerived.String(), nil
		}
	case "sigv4":
		return "sigv4_metered", "metered"
	default:
		return "explicit", nil
	}
}

func insertImportedLedgerRow(
	ctx context.Context,
	conn *sql.Conn,
	row importedLedgerRow,
) (bool, error) {
	result, err := conn.ExecContext(
		ctx,
		`INSERT INTO accounting_requests (
		started_at, completed_at, upstream_dispatched_at, request_kind, lifecycle_state, outcome,
		upstream_contacted, route_host, route_context, route_alias, route_upstream, request_model,
		resolved_model, auth_mode, credential_class, billing_policy, billing_class, cost_status,
		response_id, http_status, normalization_complete, input_tokens, output_tokens,
		cache_creation_input_tokens, cache_read_input_tokens, cache_creation_5m_input_tokens,
		cache_creation_1h_input_tokens, provider_cost_present, provider_cost_valid,
		provider_cost_nano_usd, stop_reason, block_shape, duration_ms, upstream_ms, cost_basis,
		final_cost_nano_usd, rate_card_id, import_source, import_line
	) VALUES (?, ?, ?, ?, 'finalized', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		NULL, ?, 0, ?, ?, NULL, NULL, NULL, NULL, 0, 0, 0, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
	ON CONFLICT(import_source, import_line) WHERE import_source IS NOT NULL DO NOTHING`,
		row.startedAt,
		row.completedAt,
		row.upstreamDispatchedAt,
		row.requestKind,
		row.outcome,
		row.upstreamContacted,
		row.routeHost,
		row.routeContext,
		row.routeAlias,
		row.routeUpstream,
		row.requestModel,
		row.resolvedModel,
		row.authMode,
		row.credentialClass,
		row.billingPolicy,
		row.billingClass,
		row.costStatus,
		row.httpStatus,
		row.inputTokens,
		row.outputTokens,
		row.stopReason,
		row.blockShape,
		row.durationMS,
		row.upstreamMS,
		row.costBasis,
		row.finalCost,
		row.source,
		row.line,
	)
	if err != nil {
		return false, fmt.Errorf("insert accounting row: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read inserted accounting row count: %w", err)
	}
	if affected == 1 {
		return true, nil
	}
	if affected == 0 {
		return false, nil
	}
	return false, fmt.Errorf("insert accounting row affected %d rows", affected)
}
