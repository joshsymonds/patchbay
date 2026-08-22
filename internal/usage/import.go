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
	"github.com/joshsymonds/patchbay/internal/jsonstrict"
	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/registry"
)

const (
	initialLedgerScanBuffer   = 64 * 1024
	maxLedgerLineBytes        = 16 * 1024 * 1024
	nanosecondsPerMillisecond = int64(time.Millisecond)
	importChunkLines          = 500
	// importChunkBytes bounds pre-transaction preparation to two maximum-sized
	// ledger lines, preventing a small line count from becoming an unbounded batch.
	importChunkBytes     = 32 * 1024 * 1024
	importCleanupTimeout = time.Second
)

// ImportResult reports one immutable ledger file's accounting outcome.
type ImportResult struct {
	Imported        int
	SkippedBoundary int
	SkippedExisting int
}

// ImportBoundary returns the earliest live started_at, or the boundary-read time
// when no live rows exist. Legacy records at or after it overlap the SQLite
// accounting window and are not imported.
func (store *Store) ImportBoundary(ctx context.Context) (int64, error) {
	if ctx == nil {
		return 0, errors.New("import boundary: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("import boundary context: %w", err)
	}
	// An empty live table means the daemon has recorded nothing yet. The instant
	// this read begins is therefore the truthful start of live coverage: older
	// ledger rows are historical, while rows starting from here belong to SQLite.
	boundaryReadAt := time.Now().UnixNano()
	var boundary sql.NullInt64
	if err := store.db.QueryRowContext(ctx, `SELECT MIN(started_at)
		FROM accounting_requests WHERE import_source IS NULL`).Scan(&boundary); err != nil {
		return 0, fmt.Errorf("import boundary: read live accounting timestamp: %w", err)
	}
	if !boundary.Valid {
		return boundaryReadAt, nil
	}
	return boundary.Int64, nil
}

// ImportLedgerFile imports one v1 or v2 ledger file in atomic chunks. Each chunk
// is decoded, validated, and derived before its short write transaction begins.
// The caller owns file ordering and discovers the boundary once for the whole run.
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

	decode, err := ledgerDecoder(path)
	if err != nil {
		return ImportResult{}, err
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

	return store.importLedgerChunks(ctx, file, importPlan{
		path:     path,
		source:   source,
		boundary: boundary,
		decode:   decode,
	})
}

// importPlan is everything one file's import needs that does not change from
// line to line, including the reader its own name selected.
type importPlan struct {
	path     string
	source   string
	boundary int64
	decode   ledgerLineDecoder
}

// ledgerLineDecoder reads one JSONL line of a particular ledger version into
// the accounting projection every version reduces to.
type ledgerLineDecoder func([]byte) (importedRecord, error)

// Ledger filename components. Each schema has one exact dated writer shape.
const (
	ledgerV1FilePrefix = "patchbay-"
	ledgerV2FilePrefix = "patchbay-v2-"
	ledgerFileSuffix   = ".jsonl"
	ledgerDateLayout   = "2006-01-02"
)

// ledgerDecoder picks the reader for one file from the name the build that
// wrote it gave it.
//
// The name is the only evidence of schema a ledger file carries, and it is
// sound evidence: a build never writes a file under another build's prefix. So
// the version is decided once per file rather than sniffed per line — sniffing
// would mean a corrupt v2 line could fall back to being read as a valid v1 one
// and import wrong values under no error at all.
//
// The v2 shape is tested first because its prefix extends v1's. Prefix matches
// are insufficient: backups and arbitrary names must not select a decoder.
func ledgerDecoder(path string) (ledgerLineDecoder, error) {
	name := filepath.Base(path)
	switch {
	case isDatedLedgerName(name, ledgerV2FilePrefix):
		return decodeLedgerV2Line, nil
	case isDatedLedgerName(name, ledgerV1FilePrefix):
		return decodeLedgerV1Line, nil
	default:
		return nil, fmt.Errorf(
			"import ledger %q: %q names no known ledger schema",
			path,
			name,
		)
	}
}

func isDatedLedgerName(name, prefix string) bool {
	if len(name) != len(prefix)+len(ledgerDateLayout)+len(ledgerFileSuffix) ||
		!strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ledgerFileSuffix) {
		return false
	}
	date := name[len(prefix) : len(name)-len(ledgerFileSuffix)]
	_, err := time.Parse(ledgerDateLayout, date)
	return err == nil
}

type importAfterChunkHookKey struct{}

type preparedImportLine struct {
	line            int
	skippedBoundary bool
	row             importedLedgerRow
}

type importBatch struct {
	lines       []preparedImportLine
	sourceBytes int
	finished    bool
}

type rawImportLine struct {
	line        int
	contents    []byte
	sourceBytes int
}

type importBatchReader struct {
	scanner   *bufio.Scanner
	line      int
	pending   *rawImportLine
	exhausted bool
}

func (store *Store) importLedgerChunks(
	ctx context.Context,
	file *os.File,
	plan importPlan,
) (ImportResult, error) {
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, initialLedgerScanBuffer), maxLedgerLineBytes)
	reader := importBatchReader{scanner: scanner}
	var total ImportResult
	committed := 0
	for {
		batch, prepareErr := reader.prepareBatch(plan, importChunkLines, importChunkBytes)
		if prepareErr != nil {
			return ImportResult{}, prepareErr
		}
		if len(batch.lines) == 0 && batch.finished {
			return total, nil
		}
		chunk, writeErr := store.importLedgerBatch(ctx, plan, batch)
		if writeErr != nil {
			return ImportResult{}, writeErr
		}
		total.Imported += chunk.Imported
		total.SkippedBoundary += chunk.SkippedBoundary
		total.SkippedExisting += chunk.SkippedExisting
		committed++
		if hook, ok := ctx.Value(importAfterChunkHookKey{}).(func(int)); ok {
			hook(committed)
		}
		if batch.finished {
			return total, nil
		}
	}
}

func (reader *importBatchReader) prepareBatch(
	plan importPlan,
	maxLines int,
	maxBytes int,
) (importBatch, error) {
	batch := importBatch{lines: make([]preparedImportLine, 0, maxLines)}
	for len(batch.lines) < maxLines {
		raw, finished, err := reader.nextRawLine()
		if err != nil {
			return importBatch{}, err
		}
		if finished {
			batch.finished = true
			return batch, nil
		}
		if len(batch.lines) > 0 && batch.sourceBytes+raw.sourceBytes > maxBytes {
			reader.pending = &raw
			return batch, nil
		}
		prepared, prepareErr := prepareImportLedgerLine(raw.contents, plan, raw.line)
		if prepareErr != nil {
			return importBatch{}, prepareErr
		}
		batch.lines = append(batch.lines, prepared)
		batch.sourceBytes += raw.sourceBytes
		if raw.sourceBytes > maxBytes {
			return batch, nil
		}
	}
	return batch, nil
}

func (reader *importBatchReader) nextRawLine() (rawImportLine, bool, error) {
	if reader.pending != nil {
		raw := *reader.pending
		reader.pending = nil
		return raw, false, nil
	}
	if reader.exhausted {
		return rawImportLine{}, true, nil
	}
	if !reader.scanner.Scan() {
		reader.exhausted = true
		if err := reader.scanner.Err(); err != nil {
			return rawImportLine{}, false, fmt.Errorf("scan line %d: %w", reader.line+1, err)
		}
		return rawImportLine{}, true, nil
	}
	reader.line++
	contents := reader.scanner.Bytes()
	return rawImportLine{
		line:        reader.line,
		contents:    contents,
		sourceBytes: len(contents) + 1,
	}, false, nil
}

func prepareImportLedgerLine(
	contents []byte,
	plan importPlan,
	line int,
) (preparedImportLine, error) {
	record, err := plan.decode(contents)
	if err != nil {
		return preparedImportLine{}, importLineError(plan.path, line, err)
	}
	if plan.boundary > 0 && record.timestamp.UnixNano() >= plan.boundary {
		return preparedImportLine{line: line, skippedBoundary: true}, nil
	}
	row, err := deriveImportedLedgerRow(record, plan.source, line)
	if err != nil {
		return preparedImportLine{}, importLineError(plan.path, line, err)
	}
	return preparedImportLine{line: line, row: row}, nil
}

func (store *Store) importLedgerBatch(
	ctx context.Context,
	plan importPlan,
	batch importBatch,
) (ImportResult, error) {
	var chunk ImportResult
	err := store.withImportTransaction(ctx, plan.path, func(conn *sql.Conn) error {
		for _, prepared := range batch.lines {
			if prepared.skippedBoundary {
				chunk.SkippedBoundary++
				continue
			}
			inserted, insertErr := insertImportedLedgerRow(ctx, conn, prepared.row)
			if insertErr != nil {
				return importLineError(plan.path, prepared.line, insertErr)
			}
			if inserted {
				chunk.Imported++
			} else {
				chunk.SkippedExisting++
			}
		}
		return nil
	})
	if err != nil {
		return ImportResult{}, err
	}
	return chunk, nil
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

// importedRecord is the accounting projection every ledger version reduces to.
//
// The accounting schema is one shape, so two ledger schemas have to arrive at
// it somehow, and this is where each one stops being different. Everything
// downstream — the timeline, the request kind, the outcome, the billing — reads
// this and has exactly one implementation, so a rule can never be applied to v1
// history and quietly skipped for v2 traffic.
//
// selected says whether the request reached a Seat at all. v1 had no way to say
// otherwise: it only ever wrote a record for a request that rode a route, so a
// v1 record is selected by construction. v2 records both outcomes, and the
// difference decides what may be claimed about who paid.
type importedRecord struct {
	timestamp       time.Time
	host            string
	contextName     string
	seat            string
	upstream        string
	requestModel    string
	resolvedModel   string
	authMode        string
	credentialClass string
	selected        bool
	status          int
	inputTokens     int64
	outputTokens    int64
	durationMS      int64
	upstreamMS      int64
	blockShape      []string
	stopReason      string
}

// decodeLedgerV1Line reads one patchbay-v1 line under v1's own rules. The
// union decode also captures a Seat when a later writer added one; Seat wins
// over alias in the accounting projection, exactly as the files on disk say.
func decodeLedgerV1Line(contents []byte) (importedRecord, error) {
	record, err := decodeImportRecord(contents)
	if err != nil {
		return importedRecord{}, err
	}
	if validateErr := record.validateV1(); validateErr != nil {
		return importedRecord{}, fmt.Errorf("validate ledger record: %w", validateErr)
	}
	return record.imported(true), nil
}

// decodeLedgerV2Line delegates the entire live wire contract to the ledger
// package, then projects the accepted Record onto accounting. There is no
// import-side restatement of model-key or validation rules.
func decodeLedgerV2Line(contents []byte) (importedRecord, error) {
	record, err := ledger.DecodeStrictLine(contents)
	if err != nil {
		return importedRecord{}, fmt.Errorf("decode strict v2 ledger record: %w", err)
	}
	return importedV2Record(record), nil
}

func decodeImportRecord(contents []byte) (importRecord, error) {
	if err := jsonstrict.RejectDuplicateNames(contents); err != nil {
		return importRecord{}, fmt.Errorf("parse JSON: %w", err)
	}
	var record importRecord
	if err := json.Unmarshal(contents, &record); err != nil {
		return importRecord{}, fmt.Errorf("parse JSON: %w", err)
	}
	return record, nil
}

// imported projects the lenient v1 union shape onto accounting. A Seat added by
// a later writer wins when present; alias preserves earlier history otherwise.
func (record importRecord) imported(selected bool) importedRecord {
	seat := record.Seat
	if seat == "" {
		seat = record.Alias
	}
	return importedRecord{
		timestamp:       record.Timestamp,
		host:            record.Host,
		contextName:     record.Context,
		seat:            seat,
		upstream:        record.Upstream,
		requestModel:    importedModel(record.RequestModel),
		resolvedModel:   importedModel(record.Model),
		authMode:        record.AuthMode,
		credentialClass: string(record.CredentialClass),
		selected:        selected,
		status:          record.Status,
		inputTokens:     record.InputTokens,
		outputTokens:    record.OutputTokens,
		durationMS:      record.DurationMS,
		upstreamMS:      record.UpstreamMS,
		blockShape:      record.BlockShape,
		stopReason:      record.StopReason,
	}
}

func importedV2Record(record ledger.Record) importedRecord {
	return importedRecord{
		timestamp:       record.Timestamp,
		host:            record.Host,
		contextName:     record.Context,
		seat:            record.Seat,
		upstream:        record.Upstream,
		requestModel:    importedModel(record.RequestModel),
		resolvedModel:   importedModel(record.Model),
		authMode:        record.AuthMode,
		credentialClass: string(record.CredentialClass),
		selected:        record.Selection != nil,
		status:          record.Status,
		inputTokens:     record.InputTokens,
		outputTokens:    record.OutputTokens,
		durationMS:      record.DurationMS,
		upstreamMS:      record.UpstreamMS,
		blockShape:      record.BlockShape,
		stopReason:      record.StopReason,
	}
}

func importedModel(value *string) string {
	if value == nil {
		return ""
	}
	return *value
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
	record importedRecord,
	source string,
	line int,
) (importedLedgerRow, error) {
	timeline, err := deriveImportedTimeline(record)
	if err != nil {
		return importedLedgerRow{}, err
	}
	shape, err := canonicalImportedBlockShape(record.blockShape)
	if err != nil {
		return importedLedgerRow{}, err
	}
	policy, class := importedBilling(record)
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
		outcome:         importedOutcome(record.status, timeline.contacted),
		routeHost:       record.host,
		routeContext:    record.contextName,
		routeAlias:      nullableImportedAlias(record.seat),
		routeUpstream:   record.upstream,
		requestModel:    record.requestModel,
		resolvedModel:   record.resolvedModel,
		authMode:        record.authMode,
		credentialClass: record.credentialClass,
		billingPolicy:   policy,
		billingClass:    class,
		costStatus:      costStatus,
		costBasis:       costBasis,
		finalCost:       finalCost,
		httpStatus: nullableImportedHTTPStatus(
			record.status,
		),
		stopReason:   record.stopReason,
		blockShape:   shape,
		inputTokens:  record.inputTokens,
		outputTokens: record.outputTokens,
		durationMS:   record.durationMS,
		upstreamMS:   record.upstreamMS,
	}, nil
}

func deriveImportedTimeline(record importedRecord) (importedTimeline, error) {
	startedAt := record.timestamp.UnixNano()
	if record.durationMS > math.MaxInt64/nanosecondsPerMillisecond ||
		startedAt > math.MaxInt64-record.durationMS*nanosecondsPerMillisecond {
		return importedTimeline{}, errors.New("completion timestamp overflows UnixNano")
	}
	completedAt := startedAt + record.durationMS*nanosecondsPerMillisecond
	if record.upstreamMS == 0 {
		return importedTimeline{startedAt: startedAt, completedAt: completedAt}, nil
	}
	if record.upstreamMS >= record.durationMS {
		return importedTimeline{
			startedAt:   startedAt,
			completedAt: completedAt,
			contacted:   1,
			dispatched:  startedAt,
		}, nil
	}
	return importedTimeline{
		startedAt: startedAt, completedAt: completedAt, contacted: 1,
		dispatched: completedAt - record.upstreamMS*nanosecondsPerMillisecond,
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

func importedRequestKind(record importedRecord) string {
	// Imported records lack request-path evidence, so only response evidence can
	// classify a Messages request; this intentionally makes no billing inference.
	if record.status >= 200 && record.status <= 299 &&
		(record.outputTokens > 0 || len(record.blockShape) > 0 || strings.TrimSpace(record.stopReason) != "") {
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

// importedCost prices an imported row, which it can only do when the class it
// realized carries no marginal cost. The class is compared against billing's own
// vocabulary rather than a literal, so a renamed class can never quietly stop
// matching here and start pricing subscription traffic as unknown.
func importedCost(class any) (string, string, any) {
	basis := CostStatusUnknown.String()
	var amount any
	if class == billing.ClassSubscription.String() {
		basis = "zero_marginal"
		amount = int64(0)
	}
	// Imports intentionally never rate-card price incomplete normalization data.
	return costStatusForBasis(basis), basis, amount
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

// importedBilling reads what an imported record can honestly say about who paid
// for it, which is less than a live row knows.
//
// A record that reached no Seat answers nothing. No credential was presented on
// anyone's behalf and no policy decided anything, so the policy is left empty
// rather than defaulted: naming one would claim a payer for a request that
// never left the machine. Only a pre-selection v2 record can be in that state —
// v1 wrote records for selected requests alone.
//
// A selected record answers as far as its auth mode goes. Forward and sigv4
// derive their class from the credential, which the record carries. An inject
// seat declares its class in the registry and no ledger schema ever wrote that
// declaration down, so the policy is known and the class is not: it stays NULL
// and the row prices as unknown rather than guessing which side of the
// subscription line an operator key fell on.
func importedBilling(record importedRecord) (string, any) {
	if !record.selected {
		return "", nil
	}
	switch record.authMode {
	case string(registry.AuthForward):
		switch record.credentialClass {
		case string(billing.CredentialOAuth):
			return billing.PolicyCallerDerived.String(), billing.ClassSubscription.String()
		case string(billing.CredentialAPIKey):
			return billing.PolicyCallerDerived.String(), billing.ClassMetered.String()
		default:
			return billing.PolicyCallerDerived.String(), nil
		}
	case string(registry.AuthSigV4):
		return billing.PolicySigV4Metered.String(), billing.ClassMetered.String()
	default:
		return billing.PolicyExplicit.String(), nil
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
