package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const transcriptResponseIdentityQuery = `SELECT 1 FROM accounting_requests WHERE response_id = ? AND lifecycle_state = 'finalized' AND response_id <> '' LIMIT 1`

// TranscriptImportFileResult reports import accounting for one canonical file.
type TranscriptImportFileResult struct {
	Path            string
	Imported        int
	SkippedExisting int
	SkippedChanged  int
	UnreadableRows  int
}

// TranscriptImportResult reports the complete import run.
type TranscriptImportResult struct {
	Imported        int
	SkippedExisting int
	SkippedChanged  int
	UnreadableRows  int
	Files           []TranscriptImportFileResult
}

type transcriptAttempt struct {
	line        int
	identity    string
	startedAt   time.Time
	completedAt time.Time
	model       string
	provider    string
	usage       transcriptUsage
	stopReason  string
	shape       []string
	outcome     Outcome
}

// ImportTranscripts imports only canonical, unchanged files named by an
// inventory into the usage database.
func (store *Store) ImportTranscripts(
	ctx context.Context,
	inventoryPath string,
) (TranscriptImportResult, error) {
	if ctx == nil {
		return TranscriptImportResult{}, errors.New("import transcripts: context is nil")
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return TranscriptImportResult{}, fmt.Errorf("import transcripts context: %w", contextErr)
	}
	inventory, readErr := readTranscriptInventory(inventoryPath)
	if readErr != nil {
		return TranscriptImportResult{}, readErr
	}
	hostname, hostnameErr := os.Hostname()
	if hostnameErr != nil {
		return TranscriptImportResult{}, fmt.Errorf("import transcripts: hostname: %w", hostnameErr)
	}
	return importTranscriptFiles(ctx, store, inventory, hostname)
}

func importTranscriptFiles(
	ctx context.Context,
	store *Store,
	inventory TranscriptInventory,
	hostname string,
) (TranscriptImportResult, error) {
	duplicateOthers := transcriptDuplicateOthers(inventory.Duplicates)
	files := sortedTranscriptInventoryFiles(inventory.Files)
	result := TranscriptImportResult{Files: make([]TranscriptImportFileResult, 0, len(files))}
	for _, file := range files {
		if duplicateOthers[file.Path] {
			continue
		}
		fileResult, fileErr := importTranscriptFile(ctx, store, inventory, file, hostname)
		if fileErr != nil {
			return TranscriptImportResult{}, fileErr
		}
		result.Imported += fileResult.Imported
		result.SkippedExisting += fileResult.SkippedExisting
		result.SkippedChanged += fileResult.SkippedChanged
		result.UnreadableRows += fileResult.UnreadableRows
		result.Files = append(result.Files, fileResult)
	}
	return result, nil
}

func transcriptDuplicateOthers(duplicates []TranscriptInventoryDuplicate) map[string]bool {
	others := make(map[string]bool)
	for _, duplicate := range duplicates {
		for _, path := range duplicate.Others {
			others[path] = true
		}
	}
	return others
}

func sortedTranscriptInventoryFiles(files []TranscriptInventoryFile) []TranscriptInventoryFile {
	result := append([]TranscriptInventoryFile(nil), files...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].Path != result[j].Path {
			return result[i].Path < result[j].Path
		}
		return result[i].Kind < result[j].Kind
	})
	return result
}

func importTranscriptFile(
	ctx context.Context,
	store *Store,
	inventory TranscriptInventory,
	file TranscriptInventoryFile,
	hostname string,
) (TranscriptImportFileResult, error) {
	fileResult := TranscriptImportFileResult{Path: file.Path}
	filePath, pathErr := transcriptInventoryPath(inventory, file)
	if pathErr != nil {
		return TranscriptImportFileResult{}, pathErr
	}
	unchanged, verifyErr := verifyTranscriptInventoryFile(filePath, file)
	if verifyErr != nil {
		return TranscriptImportFileResult{}, verifyErr
	}
	if !unchanged {
		fileResult.SkippedChanged = 1
		return fileResult, nil
	}
	attempts, unreadableRows, attemptErr := readTranscriptAttempts(filePath, file)
	if attemptErr != nil {
		return TranscriptImportFileResult{}, attemptErr
	}
	fileResult.UnreadableRows = unreadableRows
	rows, skippedRows := deriveTranscriptRows(attempts, file, hostname)
	fileResult.UnreadableRows += skippedRows
	imported, skipped, importErr := store.importTranscriptChunks(ctx, filePath, rows)
	if importErr != nil {
		return TranscriptImportFileResult{}, importErr
	}
	fileResult.Imported = imported
	fileResult.SkippedExisting = skipped
	return fileResult, nil
}

func readTranscriptAttempts(path string, file TranscriptInventoryFile) ([]transcriptAttempt, int, error) {
	switch file.Kind {
	case transcriptKindClaude:
		return readClaudeTranscript(path, file.SHA256)
	case transcriptKindPi:
		return readPiTranscript(path, file.SHA256)
	default:
		return nil, 0, fmt.Errorf("import transcripts: %q has unknown kind %q", file.Path, file.Kind)
	}
}

// deriveTranscriptRows derives one accounting row per attempt. An attempt whose
// timestamps cannot form a row is skipped and counted, never fatal: one odd
// record must not abort the file it sits in.
func deriveTranscriptRows(
	attempts []transcriptAttempt,
	file TranscriptInventoryFile,
	hostname string,
) ([]transcriptAccountingRow, int) {
	sourcePrefix := transcriptKindClaude + "-transcript:"
	if file.Kind == transcriptKindPi {
		sourcePrefix = transcriptKindPi + "-session:"
	}
	source := sourcePrefix + file.Path
	rows := make([]transcriptAccountingRow, 0, len(attempts))
	skipped := 0
	for _, attempt := range attempts {
		row, deriveErr := deriveTranscriptAccountingRow(attempt, source, hostname)
		if deriveErr != nil {
			skipped++
			continue
		}
		rows = append(rows, row)
	}
	return rows, skipped
}

func (store *Store) importTranscriptChunks(
	ctx context.Context,
	path string,
	rows []transcriptAccountingRow,
) (int, int, error) {
	imported, skipped := 0, 0
	for start := 0; start < len(rows); start += importChunkLines {
		end := start + importChunkLines
		if end > len(rows) {
			end = len(rows)
		}
		chunkImported, chunkSkipped, err := store.importTranscriptChunk(ctx, path, rows[start:end])
		if err != nil {
			return 0, 0, err
		}
		imported += chunkImported
		skipped += chunkSkipped
	}
	return imported, skipped, nil
}

func readTranscriptInventory(path string) (TranscriptInventory, error) {
	if strings.TrimSpace(path) == "" {
		return TranscriptInventory{}, errors.New("import transcripts: inventory path is blank")
	}
	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return TranscriptInventory{}, fmt.Errorf("import transcripts: read inventory %q: %w", path, err)
	}
	var inventory TranscriptInventory
	if parseErr := json.Unmarshal(contents, &inventory); parseErr != nil {
		return TranscriptInventory{}, fmt.Errorf("import transcripts: parse inventory %q: %w", path, parseErr)
	}
	if inventory.Roots.ClaudeProjects == "" || inventory.Roots.PISessions == "" {
		return TranscriptInventory{}, errors.New("import transcripts: inventory roots are incomplete")
	}
	return inventory, nil
}

func transcriptInventoryPath(inventory TranscriptInventory, file TranscriptInventoryFile) (string, error) {
	if file.Kind != transcriptKindClaude && file.Kind != transcriptKindPi {
		return "", fmt.Errorf("import transcripts: %q has unknown kind %q", file.Path, file.Kind)
	}
	if file.Path == "" || filepath.IsAbs(filepath.FromSlash(file.Path)) {
		return "", fmt.Errorf("import transcripts: invalid relative path %q", file.Path)
	}
	relative := filepath.Clean(filepath.FromSlash(file.Path))
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("import transcripts: invalid relative path %q", file.Path)
	}
	root := inventory.Roots.ClaudeProjects
	if file.Kind == transcriptKindPi {
		root = inventory.Roots.PISessions
	}
	return filepath.Join(root, relative), nil
}

func verifyTranscriptInventoryFile(path string, file TranscriptInventoryFile) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("import transcripts: stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() != file.Size {
		return false, nil
	}
	digest, err := transcriptFileSHA256(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("import transcripts: hash %q: %w", path, err)
	}
	return digest == file.SHA256, nil
}

type transcriptAccountingRow struct {
	source, host, upstream, model, identity, stopReason, shape           string
	line, startedAt, completedAt, durationMS                             int64
	inputTokens, outputTokens, reasoningTokens, cacheCreation, cacheRead any
	outcome                                                              Outcome
}

func deriveTranscriptAccountingRow(
	attempt transcriptAttempt,
	source, hostname string,
) (transcriptAccountingRow, error) {
	if !attempt.startedAt.IsZero() && !attempt.completedAt.IsZero() && attempt.completedAt.Before(attempt.startedAt) {
		return transcriptAccountingRow{}, errors.New("completed timestamp precedes started timestamp")
	}
	if attempt.startedAt.IsZero() || attempt.completedAt.IsZero() {
		return transcriptAccountingRow{}, errors.New("attempt timestamps are incomplete")
	}
	duration := millisecondsBetween(attempt.startedAt, attempt.completedAt)
	shape, err := canonicalImportedBlockShape(attempt.shape)
	if err != nil {
		return transcriptAccountingRow{}, err
	}
	upstream := attempt.provider
	if upstream == "" {
		upstream = hostname
	}
	return transcriptAccountingRow{
		source:          source,
		line:            int64(attempt.line),
		startedAt:       attempt.startedAt.UnixNano(),
		completedAt:     attempt.completedAt.UnixNano(),
		durationMS:      duration,
		host:            hostname,
		upstream:        upstream,
		model:           attempt.model,
		identity:        attempt.identity,
		stopReason:      attempt.stopReason,
		shape:           shape,
		inputTokens:     optionalInt64Any(attempt.usage.input),
		outputTokens:    optionalInt64Any(attempt.usage.output),
		reasoningTokens: optionalInt64Any(attempt.usage.reasoning),
		cacheCreation:   optionalInt64Any(attempt.usage.cacheCreation),
		cacheRead:       optionalInt64Any(attempt.usage.cacheRead),
		outcome:         attempt.outcome,
	}, nil
}

func optionalInt64Any(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func (store *Store) importTranscriptChunk(
	ctx context.Context,
	path string,
	rows []transcriptAccountingRow,
) (int, int, error) {
	imported, skipped := 0, 0
	err := store.withImportTransaction(ctx, path, func(conn *sql.Conn) error {
		var rowErr error
		imported, skipped, rowErr = importTranscriptRows(ctx, conn, rows)
		return rowErr
	})
	if err != nil {
		return 0, 0, err
	}
	return imported, skipped, nil
}

func importTranscriptRows(ctx context.Context, conn *sql.Conn, rows []transcriptAccountingRow) (int, int, error) {
	imported, skipped := 0, 0
	for _, row := range rows {
		inserted, err := importTranscriptRow(ctx, conn, row)
		if err != nil {
			return 0, 0, err
		}
		if inserted {
			imported++
		} else {
			skipped++
		}
	}
	return imported, skipped, nil
}

func importTranscriptRow(ctx context.Context, conn *sql.Conn, row transcriptAccountingRow) (bool, error) {
	exists, err := transcriptResponseIdentityExists(ctx, conn, row.identity)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	return insertTranscriptAccountingRow(ctx, conn, row)
}

func transcriptResponseIdentityExists(ctx context.Context, conn *sql.Conn, identity string) (bool, error) {
	if identity == "" {
		return false, nil
	}
	var exists int
	err := conn.QueryRowContext(ctx, transcriptResponseIdentityQuery, identity).
		Scan(&exists)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("check transcript response identity %q: %w", identity, err)
}

// insertTranscriptAccountingRow writes one imported attempt. The gateway never
// carried this request, so it records no upstream contact and no dispatch
// instant: the transcript timestamps are the harness's own, and binding them to
// upstream_dispatched_at would hand a reader gateway-observed timing that was
// never measured.
func insertTranscriptAccountingRow(ctx context.Context, conn *sql.Conn, row transcriptAccountingRow) (bool, error) {
	result, err := conn.ExecContext(ctx, `INSERT INTO accounting_requests (
		started_at, completed_at, upstream_dispatched_at, request_kind, lifecycle_state, outcome,
		upstream_contacted, route_host, route_context, route_alias, route_upstream, request_model,
		resolved_model, auth_mode, credential_class, billing_policy, billing_class, cost_status,
		response_id, http_status, normalization_complete, input_tokens, output_tokens,
		reasoning_output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
		cache_creation_5m_input_tokens, cache_creation_1h_input_tokens, provider_cost_present,
		provider_cost_valid, provider_cost_nano_usd, stop_reason, block_shape, duration_ms, upstream_ms,
		cost_basis, final_cost_nano_usd, rate_card_id, first_content_at, import_source, import_line
	) VALUES (?, ?, NULL, 'messages', 'finalized', ?, 0, ?, 'transcript', NULL, ?, ?, ?, '', '', '', NULL, 'unknown',
		?, NULL, 0, ?, ?, ?, ?, ?, NULL, NULL, 0, 0, 0, ?, ?, ?, ?, 'unknown', NULL, NULL, NULL, ?, ?)
	ON CONFLICT(import_source, import_line) WHERE import_source IS NOT NULL DO NOTHING`,
		row.startedAt, row.completedAt, row.outcome.String(), row.host, row.upstream,
		row.model, row.model, row.identity, row.inputTokens, row.outputTokens, row.reasoningTokens,
		row.cacheCreation, row.cacheRead, row.stopReason, row.shape, row.durationMS, row.durationMS,
		row.source, row.line,
	)
	if err != nil {
		return false, fmt.Errorf("insert transcript accounting row: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read inserted transcript accounting row count: %w", err)
	}
	if affected == 1 {
		return true, nil
	}
	if affected == 0 {
		return false, nil
	}
	return false, fmt.Errorf("insert transcript accounting row affected %d rows", affected)
}
