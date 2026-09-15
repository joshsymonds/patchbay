package usage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTranscriptResponseIdentityQueryUsesFinalizedIndex(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	rows, err := store.db.QueryContext(
		context.Background(),
		"EXPLAIN QUERY PLAN "+transcriptResponseIdentityQuery,
		"existing",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "SEARCH accounting_requests USING ") ||
		!strings.Contains(plan, "INDEX accounting_requests_finalized_response_id ") ||
		strings.Contains(plan, "SCAN") {
		t.Fatalf(
			"identity lookup must search accounting_requests_finalized_response_id without a scan; plan = %q",
			plan,
		)
	}
}

func TestTranscriptReadersSkipOversizedLines(t *testing.T) {
	const maxLineBytes = 2 * initialLedgerScanBuffer
	formats := []struct {
		name string
		row  string
		scan func(*os.File, string, int) ([]transcriptAttempt, int, error)
	}{
		{
			name: "claude",
			row:  `{"type":"assistant","timestamp":"2026-09-01T00:00:00Z","message":{"model":"test-model","content":"hello"}}`,
			scan: func(file *os.File, path string, limit int) ([]transcriptAttempt, int, error) {
				rows, order, builders, unreadable, err := scanClaudeTranscript(file, path, limit)
				return buildClaudeAttempts(rows, order, builders, "fixture"), unreadable, err
			},
		},
		{
			name: "pi",
			row:  `{"type":"message","timestamp":"2026-09-01T00:00:00Z","message":{"role":"assistant","model":"test-model","content":[{"type":"text"}]}}`,
			scan: func(file *os.File, path string, limit int) ([]transcriptAttempt, int, error) {
				return scanPiTranscript(file, path, "fixture", limit)
			},
		},
	}
	for _, format := range formats {
		t.Run(format.name, func(t *testing.T) {
			oversized := strings.Repeat(" ", 3*maxLineBytes) + format.row
			atLimit := strings.Repeat(" ", maxLineBytes-len(format.row)) + format.row
			fixtures := []struct {
				name       string
				contents   string
				lines      []int
				unreadable int
			}{
				{"middle", format.row + "\n" + oversized + "\n" + format.row, []int{1, 3}, 1},
				{"first", oversized + "\n" + format.row + "\n", []int{2}, 1},
				{"consecutive", oversized + "\n" + oversized + "\n" + format.row, []int{3}, 2},
				{"eof", format.row + "\n" + oversized, []int{1}, 1},
				{"at-limit", atLimit + "\n" + format.row, []int{1, 2}, 0},
				{"at-limit-eof", atLimit, []int{1}, 0},
			}
			for _, fixture := range fixtures {
				t.Run(fixture.name, func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "session.jsonl")
					if err := os.WriteFile(path, []byte(fixture.contents), 0o600); err != nil {
						t.Fatal(err)
					}
					file, err := openTranscriptFile(path)
					if err != nil {
						t.Fatal(err)
					}
					defer file.Close()
					attempts, unreadable, err := format.scan(file, path, maxLineBytes)
					if err != nil {
						t.Fatalf("read fixture: %v", err)
					}
					if unreadable != fixture.unreadable || len(attempts) != len(fixture.lines) {
						t.Fatalf("attempts = %d, unreadable = %d; want %d, %d",
							len(attempts), unreadable, len(fixture.lines), fixture.unreadable)
					}
					for i, line := range fixture.lines {
						wantIdentity := fmt.Sprintf("transcript:fixture:%d", line)
						if attempts[i].line != line || attempts[i].identity != wantIdentity {
							t.Fatalf("attempt %d = %#v; want line %d, identity %q", i, attempts[i], line, wantIdentity)
						}
					}
				})
			}
		})
	}
}

func TestImportTranscripts(t *testing.T) {
	claude := t.TempDir()
	pi := t.TempDir()
	if err := os.MkdirAll(filepath.Join(claude, "proj", "session", "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(claude, "proj", "session.jsonl")
	claudeLines := []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-09-01T00:00:00Z","sessionId":"s","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-09-01T00:00:01Z","sessionId":"s","requestId":"r1","message":{"id":"msg-1","model":"claude-test","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":2,"cache_creation_input_tokens":3,"output_tokens_details":{"thinking_tokens":4}},"content":[{"type":"thinking","thinking":"x"}],"stop_reason":null}}`,
		`{"type":"assistant","uuid":"bad","timestamp":"2026-09-01T00:00:02Z","requestId":"bad","message":{"model":"claude-test","content":{"type":"text"}}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"a1","timestamp":"2026-09-01T00:00:03Z","sessionId":"s","requestId":"r1","message":{"id":"msg-1","model":"claude-test","usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":2,"cache_creation_input_tokens":3,"output_tokens_details":{"thinking_tokens":5}},"content":[{"type":"text","text":"bye"}],"stop_reason":"end_turn"}}`,
		`{"type":"assistant","uuid":"a3","parentUuid":"u1","timestamp":"2026-09-01T00:00:04Z","sessionId":"s","requestId":"r2","isApiErrorMessage":true,"message":{"model":"claude-test","usage":{"input_tokens":1,"output_tokens":0},"content":[{"type":"text","text":"error"}],"stop_reason":null}}`,
		`{"type":"assistant","uuid":"nots","parentUuid":"u1","sessionId":"s","requestId":"r-nots","message":{"id":"msg-nots","model":"claude-test","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":"x"}],"stop_reason":"end_turn"}}`,
		`{"type":"assistant","uuid":"a4","parentUuid":"u1","timestamp":"2026-09-01T00:00:05Z","sessionId":"s","requestId":"r3","message":{"id":"msg-string","model":"claude-test","usage":{"input_tokens":2,"output_tokens":1},"content":"plain response","stop_reason":"end_turn"}}`,
	}
	if err := os.WriteFile(claudePath, []byte(joinLines(claudeLines)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(claude, "proj", "session", "subagents", "agent-x.jsonl"),
		[]byte(claudeLines[0]+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	piPath := filepath.Join(pi, "session.jsonl")
	piLines := []string{
		`{"type":"message","id":"pu","timestamp":"2026-09-01T00:01:00Z","message":{"role":"user","content":[{"type":"text","text":"go"}]}}`,
		`{"type":"message","id":"bad-pi","timestamp":"2026-09-01T00:01:01Z","message":{"role":"assistant","content":{"type":"text"}}}`,
		`{"type":"message","id":"pa","parentId":"pu","timestamp":"2026-09-01T00:01:02Z","message":{"role":"assistant","model":"pi-test","provider":"pi-host","usage":{"input":11,"output":3,"cacheRead":4,"cacheWrite":5,"reasoning":6},"stopReason":"error","content":[{"type":"thinking"},{"type":"text"}]}}`,
		`{"type":"message","id":"px","parentId":"pu","timestamp":"2026-09-01T00:02:02Z","message":{"role":"assistant","model":"pi-test","provider":"pi-host","usage":{"input":1,"output":1},"stopReason":"aborted","content":[{"type":"text"}]}}`,
		`{"type":"message","id":"nots-pi","parentId":"pu","message":{"role":"assistant","model":"pi-test","provider":"pi-host","usage":{"input":1,"output":1},"stopReason":"stop","content":[{"type":"text"}]}}`,
	}
	if err := os.WriteFile(piPath, []byte(joinLines(piLines)), 0o600); err != nil {
		t.Fatal(err)
	}
	inventoryPath := filepath.Join(t.TempDir(), "inventory.json")
	if err := InventoryTranscripts(context.Background(), claude, pi, inventoryPath); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, dbPath)
	result, err := store.ImportTranscripts(context.Background(), inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 5 || result.SkippedExisting != 0 || result.SkippedChanged != 0 ||
		result.UnreadableRows != 4 {
		t.Fatalf("result = %#v", result)
	}
	if result.Files[0].Path != "proj/session.jsonl" || result.Files[0].UnreadableRows != 2 {
		t.Fatalf("Claude file result = %#v", result.Files[0])
	}
	if result.Files[2].Path != "session.jsonl" || result.Files[2].UnreadableRows != 2 {
		t.Fatalf("pi file result = %#v", result.Files[2])
	}
	rows, err := store.db.Query(
		`SELECT response_id, import_source, import_line, request_model, resolved_model, request_kind, route_host, route_context, cost_status, http_status, input_tokens, output_tokens, reasoning_output_tokens, cache_creation_input_tokens, cache_read_input_tokens, stop_reason, block_shape, started_at, completed_at, outcome, first_content_at, upstream_dispatched_at, upstream_contacted FROM accounting_requests WHERE import_source IS NOT NULL ORDER BY id`,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type gotRow struct {
		responseID, source, model, resolved, kind, host, ctx, cost, stop string
		line                                                             int64
		input, output, reasoning, cacheWrite, cacheRead                  sql.NullInt64
		http, first, dispatched                                          sql.NullInt64
		shape                                                            string
		started, completed, contacted                                    int64
		outcome                                                          string
	}
	var got []gotRow
	for rows.Next() {
		var row gotRow
		if err := rows.Scan(
			&row.responseID,
			&row.source,
			&row.line,
			&row.model,
			&row.resolved,
			&row.kind,
			&row.host,
			&row.ctx,
			&row.cost,
			&row.http,
			&row.input,
			&row.output,
			&row.reasoning,
			&row.cacheWrite,
			&row.cacheRead,
			&row.stop,
			&row.shape,
			&row.started,
			&row.completed,
			&row.outcome,
			&row.first,
			&row.dispatched,
			&row.contacted,
		); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("rows = %#v", got)
	}
	for i, row := range got {
		if row.dispatched.Valid || row.contacted != 0 {
			t.Fatalf(
				"row %d dispatched = %#v contacted = %d; want NULL and 0, the gateway carried no imported request",
				i,
				row.dispatched,
				row.contacted,
			)
		}
	}
	if got[0].responseID != "msg-1" || got[0].source != "claude-transcript:proj/session.jsonl" || got[0].line != 2 ||
		got[0].input.Int64 != 10 ||
		got[0].output.Int64 != 2 ||
		got[0].reasoning.Int64 != 5 ||
		got[0].started != 1788220800000000000 ||
		got[0].completed != 1788220803000000000 ||
		got[0].stop != "end_turn" ||
		got[0].outcome != "success" {
		t.Fatalf("claude aggregate = %#v", got[0])
	}
	if got[0].http.Valid || got[0].first.Valid || got[0].kind != "messages" || got[0].ctx != "transcript" ||
		got[0].cost != "unknown" {
		t.Fatalf("claude fields = %#v", got[0])
	}
	if got[1].outcome != "upstream_http_failure" || !strings.HasPrefix(got[1].responseID, "transcript:") ||
		got[1].kind != "messages" {
		t.Fatalf("api error = %#v", got[1])
	}
	if got[2].responseID != "msg-string" || got[2].shape != `["text"]` {
		t.Fatalf("Claude string content = %#v", got[2])
	}
	if got[3].outcome != "upstream_http_failure" || got[3].stop != "error" {
		t.Fatalf("pi error = %#v", got[3])
	}
	if got[4].outcome != "response_interrupted" || got[4].stop != "aborted" {
		t.Fatalf("pi aborted = %#v", got[4])
	}
	second, err := store.ImportTranscripts(context.Background(), inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if second.Imported != 0 || second.SkippedExisting != 5 || second.UnreadableRows != 4 {
		t.Fatalf("second result = %#v", second)
	}
}

func joinLines(lines []string) string {
	out := ""
	for _, line := range lines {
		out += line + "\n"
	}
	return out
}

func TestImportTranscriptsCanonicalizesIdlessDuplicate(t *testing.T) {
	claude := t.TempDir()
	pi := t.TempDir()
	firstRoot := filepath.Join(claude, "a")
	secondRoot := filepath.Join(claude, "b")
	if err := os.MkdirAll(firstRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(secondRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte(
		`{"type":"assistant","timestamp":"2026-09-01T00:00:00Z","message":{"model":"claude-test","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text"}],"stop_reason":"end_turn"}}` + "\n",
	)
	if err := os.WriteFile(filepath.Join(firstRoot, "session.jsonl"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondRoot, "session.jsonl"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	inventoryPath := filepath.Join(t.TempDir(), "inventory.json")
	if err := InventoryTranscripts(context.Background(), claude, pi, inventoryPath); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, dbPath)
	result, err := store.ImportTranscripts(context.Background(), inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 1 || result.SkippedExisting != 0 || result.SkippedChanged != 0 {
		t.Fatalf("result = %#v", result)
	}
	sum := sha256.Sum256(contents)
	expectedID := "transcript:" + hex.EncodeToString(sum[:]) + ":1"
	var responseID, source string
	if err := store.db.QueryRow(`SELECT response_id, import_source FROM accounting_requests WHERE import_source IS NOT NULL`).
		Scan(&responseID, &source); err != nil {
		t.Fatal(err)
	}
	if responseID != expectedID || source != "claude-transcript:a/session.jsonl" {
		t.Fatalf("row identity/source = %q/%q", responseID, source)
	}
	second, err := store.ImportTranscripts(context.Background(), inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if second.Imported != 0 || second.SkippedExisting != 1 || second.SkippedChanged != 0 {
		t.Fatalf("second result = %#v", second)
	}
}

func TestImportTranscriptsSkipsExistingWithoutModification(t *testing.T) {
	claude := t.TempDir()
	pi := t.TempDir()
	claudePath := filepath.Join(claude, "session.jsonl")
	if err := os.WriteFile(
		claudePath,
		[]byte(
			`{"type":"assistant","uuid":"a","timestamp":"2026-09-01T00:00:00Z","requestId":"r","message":{"id":"existing","model":"claude-test","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text"}],"stop_reason":"end_turn"}}`+"\n",
		),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	inventoryPath := filepath.Join(t.TempDir(), "inventory.json")
	if err := InventoryTranscripts(context.Background(), claude, pi, inventoryPath); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, dbPath)
	_, err := store.db.Exec(
		`INSERT INTO accounting_requests (
		started_at, request_kind, lifecycle_state, upstream_contacted, route_host, route_context,
		route_upstream, request_model, resolved_model, auth_mode, credential_class, billing_policy,
		cost_status, response_id, completed_at, upstream_dispatched_at, outcome, normalization_complete,
		provider_cost_present, provider_cost_valid, provider_cost_nano_usd, stop_reason, block_shape,
		duration_ms, upstream_ms, cost_basis
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,2,1,'success',0,0,0,0,'end_turn','["thinking"]',0,0,'unknown')`,
		1,
		"messages",
		"finalized",
		1,
		"seed-host",
		"seed-context",
		"seed-upstream",
		"seed-model",
		"seed-model",
		"",
		"",
		"",
		"unknown",
		"existing",
	)
	if err != nil {
		t.Fatal(err)
	}
	before := allAccountingRows(t, store.db)
	result, err := store.ImportTranscripts(context.Background(), inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 0 || result.SkippedExisting != 1 {
		t.Fatalf("result = %#v", result)
	}
	afterDB := inspectDB(t, dbPath)
	after := allAccountingRows(t, afterDB)
	if len(before) != len(after) {
		t.Fatalf("row count changed: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if !reflect.DeepEqual(before[i], after[i]) {
			t.Fatalf("row %d changed: before=%v after=%v", i, before[i], after[i])
		}
	}
}

func TestImportTranscriptsSkipsChangedAndIgnoresDuplicateOthers(t *testing.T) {
	claude := t.TempDir()
	pi := t.TempDir()
	rootA := filepath.Join(claude, "a")
	rootB := filepath.Join(claude, "b")
	if err := os.MkdirAll(rootA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rootB, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte(
		`{"type":"assistant","uuid":"a","timestamp":"2026-09-01T00:00:00Z","message":{"model":"claude-test","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text"}],"stop_reason":"end_turn"}}` + "\n",
	)
	canonical := filepath.Join(rootA, "session.jsonl")
	duplicate := filepath.Join(rootB, "session.jsonl")
	if err := os.WriteFile(canonical, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(duplicate, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	inventoryPath := filepath.Join(t.TempDir(), "inventory.json")
	if err := InventoryTranscripts(context.Background(), claude, pi, inventoryPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(duplicate); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, append(contents, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	result, err := store.ImportTranscripts(context.Background(), inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 0 || result.SkippedChanged != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func allAccountingRows(t *testing.T, db *sql.DB) [][]string {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM accounting_requests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result [][]string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(values))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(values))
		for i, value := range values {
			row[i] = fmt.Sprintf("%T:%v", value, value)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDeriveTranscriptRowsSkipsUnderivableAttempts(t *testing.T) {
	file := TranscriptInventoryFile{Path: "session.jsonl", Kind: transcriptKindClaude}
	later := time.Date(2026, 9, 1, 0, 0, 5, 0, time.UTC)
	attempts := []transcriptAttempt{
		{
			line: 1, identity: "ok", model: "m",
			startedAt: later, completedAt: later.Add(time.Second), outcome: OutcomeSuccess,
		},
		{
			line: 2, identity: "backwards", model: "m",
			startedAt: later.Add(time.Minute), completedAt: later, outcome: OutcomeSuccess,
		},
		{line: 3, identity: "missing", model: "m", completedAt: later, outcome: OutcomeSuccess},
	}
	rows, skipped := deriveTranscriptRows(attempts, file, "host")
	if len(rows) != 1 || rows[0].identity != "ok" || skipped != 2 {
		t.Fatalf("rows = %#v, skipped = %d; want one derivable row and two skipped", rows, skipped)
	}
}
