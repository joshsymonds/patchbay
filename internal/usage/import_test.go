package usage

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/ledger"
)

func writeLedger(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	return path
}

func TestImportLedgerFileMapsRecordsAndBilling(t *testing.T) {
	t.Parallel()

	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	dir := t.TempDir()
	path := writeLedger(t, dir, "patchbay-2026-08-20.jsonl", strings.Join([]string{
		`{"timestamp":"2026-08-20T12:00:00.123456789Z","host":"gnomon","context":"personal","credential_class":"oauth","alias":"","upstream":"https://api.anthropic.com","model":"claude-opus-5","request_model":"claude-opus-5","auth_mode":"forward","status":200,"input_tokens":10,"output_tokens":2,"duration_ms":50,"upstream_ms":30,"block_shape":["text"],"stop_reason":"end_turn","request_body":"private","response_body":"private","error_body":"private"}`,
		`{"timestamp":"2026-08-20T12:01:00Z","host":"gnomon","upstream":"https://example.test","model":"model","request_model":"model","auth_mode":"inject","status":500,"input_tokens":0,"output_tokens":0,"duration_ms":7,"upstream_ms":0,"block_shape":[],"selection":"ignored","seat":"ignored","config_source":"ignored"}`,
		`{"timestamp":"2026-08-20T12:02:00Z","host":"gnomon","context":"work","credential_class":"api_key","alias":"a","upstream":"https://api.anthropic.com","model":"model","request_model":"request","auth_mode":"forward","status":400,"input_tokens":1,"output_tokens":0,"duration_ms":9,"upstream_ms":99,"block_shape":[]}`,
		`{"timestamp":"2026-08-20T12:03:00Z","host":"gnomon","context":"work","credential_class":"none","alias":"bedrock","upstream":"https://bedrock.test","model":"model","request_model":"request","auth_mode":"sigv4","status":204,"input_tokens":0,"output_tokens":0,"duration_ms":1,"upstream_ms":1,"block_shape":[]}`,
	}, "\n")+"\n")

	result, err := store.ImportLedgerFile(context.Background(), path, filepath.Base(path), 0)
	if err != nil {
		t.Fatalf("ImportLedgerFile() error: %v", err)
	}
	if want := (ImportResult{Imported: 4}); result != want {
		t.Fatalf("ImportLedgerFile() = %#v; want %#v", result, want)
	}

	rows, err := store.db.Query(`SELECT import_source, import_line, started_at, completed_at,
		upstream_dispatched_at, request_kind, outcome, upstream_contacted, route_context,
		route_alias, credential_class, billing_policy, billing_class, cost_status, cost_basis,
		final_cost_nano_usd, response_id, http_status, normalization_complete, input_tokens,
		output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
		cache_creation_5m_input_tokens, cache_creation_1h_input_tokens,
		provider_cost_present, provider_cost_valid, provider_cost_nano_usd, stop_reason,
		block_shape, duration_ms, upstream_ms FROM accounting_requests ORDER BY import_line`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		source, kind, outcome, context, credential, policy, costStatus                                    string
		line, started, completed, contacted, normalization, duration, upstream                            int64
		dispatched, finalCost, status                                                                     sql.NullInt64
		aliasText, classText, basisText, responseText, shape                                              sql.NullString
		input, output, creation, read, creation5, creation1, providerPresent, providerValid, providerCost sql.NullInt64
		stop                                                                                              sql.NullString
	}
	var got []row
	for rows.Next() {
		var value row
		if err := rows.Scan(
			&value.source,
			&value.line,
			&value.started,
			&value.completed,
			&value.dispatched,
			&value.kind,
			&value.outcome,
			&value.contacted,
			&value.context,
			&value.aliasText,
			&value.credential,
			&value.policy,
			&value.classText,
			&value.costStatus,
			&value.basisText,
			&value.finalCost,
			&value.responseText,
			&value.status,
			&value.normalization,
			&value.input,
			&value.output,
			&value.creation,
			&value.read,
			&value.creation5,
			&value.creation1,
			&value.providerPresent,
			&value.providerValid,
			&value.providerCost,
			&value.stop,
			&value.shape,
			&value.duration,
			&value.upstream,
		); err != nil {
			t.Fatal(err)
		}
		got = append(got, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("imported rows = %d; want 4", len(got))
	}
	for index, imported := range got {
		knownBasis := imported.basisText.Valid && imported.basisText.String != "unknown"
		if (imported.costStatus == CostStatusKnown.String()) != knownBasis {
			t.Errorf("imported row %d status/basis = %q/%#v; want known iff basis is non-unknown",
				index+1, imported.costStatus, imported.basisText)
		}
	}
	first := got[0]
	cacheTokensAbsent := !first.creation.Valid && !first.read.Valid &&
		!first.creation5.Valid && !first.creation1.Valid
	firstIdentityMatches := first.source == filepath.Base(path) && first.line == 1 &&
		first.started == time.Date(2026, 8, 20, 12, 0, 0, 123456789, time.UTC).UnixNano() &&
		first.completed == first.started+50_000_000 &&
		first.dispatched == (sql.NullInt64{Int64: first.completed - 30_000_000, Valid: true}) &&
		first.kind == "messages" && first.outcome == "success" && first.contacted == 1 &&
		first.context == "personal" && !first.aliasText.Valid && first.credential == "oauth"
	firstCostMatches := first.policy == "credential_derived" &&
		first.classText == (sql.NullString{String: "subscription", Valid: true}) &&
		first.costStatus == "known" &&
		first.basisText == (sql.NullString{String: "zero_marginal", Valid: true}) &&
		first.finalCost == (sql.NullInt64{Int64: 0, Valid: true})
	firstSnapshotMatches := !first.responseText.Valid &&
		first.status == (sql.NullInt64{Int64: 200, Valid: true}) && first.normalization == 0 &&
		first.input == (sql.NullInt64{Int64: 10, Valid: true}) &&
		first.output == (sql.NullInt64{Int64: 2, Valid: true}) && cacheTokensAbsent &&
		first.providerPresent == (sql.NullInt64{Int64: 0, Valid: true}) &&
		first.providerValid == (sql.NullInt64{Int64: 0, Valid: true}) &&
		first.providerCost == (sql.NullInt64{Int64: 0, Valid: true}) &&
		first.stop == (sql.NullString{String: "end_turn", Valid: true}) &&
		first.shape == (sql.NullString{String: `["text"]`, Valid: true}) &&
		first.duration == 50 && first.upstream == 30
	if !firstIdentityMatches || !firstCostMatches || !firstSnapshotMatches {
		t.Fatalf("first imported row = %#v", first)
	}
	second := got[1]
	if second.context != "" || second.credential != "" || second.kind != "other" ||
		second.outcome != "local_rejection" ||
		second.classText.Valid ||
		second.policy != "explicit" ||
		second.costStatus != "unknown" ||
		second.basisText != (sql.NullString{String: "unknown", Valid: true}) ||
		second.finalCost.Valid ||
		second.dispatched.Valid {
		t.Fatalf("legacy imported row = %#v", second)
	}
	third := got[2]
	if third.kind != "other" || third.outcome != "upstream_http_failure" || third.contacted != 1 ||
		third.dispatched != (sql.NullInt64{Int64: third.started, Valid: true}) ||
		third.classText != (sql.NullString{String: "metered", Valid: true}) ||
		third.policy != "credential_derived" {
		t.Fatalf("upstream failure row = %#v", third)
	}
	fourth := got[3]
	if fourth.kind != "other" || fourth.outcome != "success" || fourth.policy != "sigv4_metered" ||
		fourth.classText != (sql.NullString{String: "metered", Valid: true}) ||
		fourth.costStatus != "unknown" {
		t.Fatalf("sigv4 row = %#v", fourth)
	}
}

func TestImportLedgerFileBoundaryAndIdempotence(t *testing.T) {
	t.Parallel()

	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	liveAt := time.Date(2026, 8, 20, 12, 1, 0, 0, time.UTC)
	if _, err := store.InsertPending(
		context.Background(),
		PendingRequest{
			StartedAt:       liveAt,
			Kind:            RequestOther,
			Host:            "host",
			Context:         "personal",
			SeatID:          validPendingRequest().SeatID,
			Seat:            validPendingRequest().Seat,
			RequestModel:    "model",
			CredentialClass: "oauth",
		},
	); err != nil {
		t.Fatal(err)
	}
	boundary, err := store.ImportBoundary(context.Background())
	if err != nil || boundary != liveAt.UnixNano() {
		t.Fatalf("ImportBoundary() = %d, %v; want %d, nil", boundary, err, liveAt.UnixNano())
	}
	dir := t.TempDir()
	path := writeLedger(
		t,
		dir,
		"patchbay-2026-08-20.jsonl",
		`{"timestamp":"2026-08-20T12:00:00Z","host":"host","upstream":"upstream","model":"model","request_model":"model","auth_mode":"inject","status":200,"input_tokens":0,"output_tokens":1,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`+"\n"+`{"timestamp":"2026-08-20T12:01:00Z","host":"host","upstream":"upstream","model":"model","request_model":"model","auth_mode":"inject","status":200,"input_tokens":0,"output_tokens":1,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`+"\n",
	)
	first, err := store.ImportLedgerFile(context.Background(), path, filepath.Base(path), boundary)
	if err != nil || first != (ImportResult{Imported: 1, SkippedBoundary: 1}) {
		t.Fatalf("first import = %#v, %v", first, err)
	}
	before := importedRowSnapshot(t, store.db)
	second, err := store.ImportLedgerFile(context.Background(), path, filepath.Base(path), boundary)
	if err != nil || second != (ImportResult{SkippedBoundary: 1, SkippedExisting: 1}) {
		t.Fatalf("second import = %#v, %v", second, err)
	}
	after := importedRowSnapshot(t, store.db)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("full imported rows changed on idempotent import:\n before=%#v\n after=%#v", before, after)
	}
}

func TestImportBoundaryOnEmptyStoreUsesReadTimeAndSkipsOverlap(t *testing.T) {
	t.Parallel()

	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	before := time.Now().UnixNano()
	boundary, err := store.ImportBoundary(context.Background())
	after := time.Now().UnixNano()
	if err != nil {
		t.Fatalf("ImportBoundary() returned error: %v", err)
	}
	if boundary == 0 || boundary < before || boundary > after {
		t.Errorf("ImportBoundary() = %d; want non-zero read time in [%d, %d]", boundary, before, after)
	}

	pivot := boundary
	if pivot == 0 {
		pivot = before
	}
	older := time.Unix(0, pivot-1).UTC()
	newer := time.Unix(0, pivot+1).UTC()
	path := writeLedger(t, t.TempDir(), "patchbay-2026-08-20.jsonl", fmt.Sprintf(
		`{"timestamp":%q,"host":"host","upstream":"upstream","model":"model","request_model":"model","auth_mode":"inject","status":200,"input_tokens":0,"output_tokens":1,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`+"\n"+
			`{"timestamp":%q,"host":"host","upstream":"upstream","model":"model","request_model":"model","auth_mode":"inject","status":200,"input_tokens":0,"output_tokens":1,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`+"\n",
		older.Format(time.RFC3339Nano),
		newer.Format(time.RFC3339Nano),
	))
	result, err := store.ImportLedgerFile(context.Background(), path, filepath.Base(path), boundary)
	if err != nil {
		t.Fatalf("ImportLedgerFile() returned error: %v", err)
	}
	if result != (ImportResult{Imported: 1, SkippedBoundary: 1}) {
		t.Fatalf("ImportLedgerFile() = %#v; want one historical import and one boundary skip", result)
	}
	var storedStartedAt int64
	if err := store.db.QueryRow(`SELECT started_at FROM accounting_requests WHERE import_source IS NOT NULL`).
		Scan(&storedStartedAt); err != nil {
		t.Fatalf("read imported historical row: %v", err)
	}
	if storedStartedAt != older.UnixNano() {
		t.Fatalf("imported started_at = %d; want older line %d", storedStartedAt, older.UnixNano())
	}
}

func importedRowSnapshot(t *testing.T, db *sql.DB) [][]string {
	t.Helper()

	rows, err := db.Query(`SELECT * FROM accounting_requests WHERE import_source IS NOT NULL ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var snapshot [][]string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(values))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(values))
		for index, value := range values {
			row[index] = fmt.Sprintf("%T:%v", value, value)
		}
		snapshot = append(snapshot, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestImportLedgerFileRejectsMalformedLineWithFileAndLine(t *testing.T) {
	t.Parallel()

	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	dir := t.TempDir()
	good := writeLedger(
		t,
		dir,
		"patchbay-2026-08-19.jsonl",
		`{"timestamp":"2026-08-19T12:00:00Z","host":"host","upstream":"upstream","model":"model","request_model":"model","auth_mode":"inject","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`+"\n",
	)
	if _, err := store.ImportLedgerFile(
		context.Background(),
		good,
		filepath.Base(good),
		0,
	); err != nil {
		t.Fatal(err)
	}
	badContents := strings.Repeat(
		`{"timestamp":"2026-08-20T12:00:00Z","host":"host","upstream":"upstream","model":"model","request_model":"model","auth_mode":"inject","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`+"\n",
		importChunkLines+1,
	) + "not json\n"
	bad := writeLedger(t, dir, "patchbay-2026-08-20.jsonl", badContents)
	_, err := store.ImportLedgerFile(context.Background(), bad, filepath.Base(bad), 0)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%s:%d", bad, importChunkLines+2)) {
		t.Fatalf("malformed import error = %v; want current chunk line", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM accounting_requests`).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != importChunkLines+1 {
		t.Fatalf("rows after failed file = %d; want prior file plus committed chunk", count)
	}
	invalid := writeLedger(
		t,
		dir,
		"patchbay-2026-08-21.jsonl",
		`{"timestamp":"2026-08-21T12:00:00Z","host":"host","upstream":"upstream","model":"model","request_model":"model","auth_mode":"invalid","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`+"\n",
	)
	if _, err := store.ImportLedgerFile(context.Background(), invalid, filepath.Base(invalid), 0); err == nil ||
		!strings.Contains(err.Error(), invalid+":1") {
		t.Fatalf("invalid record import error = %v; want %s:1", err, invalid)
	}
}

// TestImportLedgerFileKeepsV1ReadLeniency pins the four ways a v1 line differs
// from anything this build writes, and that each still imports.
//
// The live ledger is strict on purpose: it refuses an unknown member, requires
// the context it stores to match today's ID grammar, and never omits a
// credential class. Every one of those rules would reject real history. A v1
// record predating attribution carries no credential class; one predating
// contexts carries no context; v1's context grammar admitted a leading dash that
// registry.ValidID now refuses; and a later writer may add Seat metadata to a
// v1-named file. Import keeps both historical alias and current Seat fields,
// preferring the Seat when present.
func TestImportLedgerFileKeepsV1ReadLeniency(t *testing.T) {
	t.Parallel()

	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	dir := t.TempDir()
	path := writeLedger(t, dir, "patchbay-2026-08-18.jsonl", strings.Join([]string{
		`{"timestamp":"2026-08-18T12:00:00Z","host":"gnomon","context":"","credential_class":"","alias":"","upstream":"https://api.anthropic.com","model":"m","request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
		`{"timestamp":"2026-08-18T12:01:00Z","host":"gnomon","upstream":"https://api.anthropic.com","model":"m","request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
		`{"timestamp":"2026-08-18T12:02:00Z","host":"gnomon","context":"-legacy","credential_class":"oauth","alias":"opus","upstream":"https://api.anthropic.com","model":"m","request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
		`{"timestamp":"2026-08-18T12:03:00Z","host":"gnomon","context":"work","credential_class":"api_key","upstream":"https://api.anthropic.com","model":"m","request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[],"seat":"opus-seat","selection":{"kind":"model","model":"m"},"subagent":{"agent_id":"agent-1"},"config_source":"caller"}`,
	}, "\n")+"\n")

	result, err := store.ImportLedgerFile(context.Background(), path, filepath.Base(path), 0)
	if err != nil {
		t.Fatalf("ImportLedgerFile() error: %v", err)
	}
	if want := (ImportResult{Imported: 4}); result != want {
		t.Fatalf("ImportLedgerFile() = %#v; want %#v", result, want)
	}

	rows, err := store.db.Query(
		`SELECT route_context, credential_class, route_alias FROM accounting_requests ORDER BY import_line`,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type imported struct {
		context, credential string
		alias               sql.NullString
	}
	var got []imported
	for rows.Next() {
		var value imported
		if err := rows.Scan(&value.context, &value.credential, &value.alias); err != nil {
			t.Fatal(err)
		}
		got = append(got, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []imported{
		{context: "", credential: ""},
		{context: "", credential: ""},
		{context: "-legacy", credential: "oauth", alias: sql.NullString{String: "opus", Valid: true}},
		{context: "work", credential: "api_key", alias: sql.NullString{String: "opus-seat", Valid: true}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("imported v1 identities:\n got %#v\nwant %#v", got, want)
	}
}

// TestImportLedgerFileRejectsInvalidV1Records pins that leniency is not
// permissiveness. Every rule v1 enforced is still enforced, and a line that
// breaks one costs the whole file rather than being skipped: a ledger that
// cannot be read whole is corruption, and importing only its readable part
// would silently understate what was spent.
func TestImportLedgerFileRejectsInvalidV1Records(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		line         string
		wantContains string
	}{
		{
			name:         "missing host",
			line:         `{"timestamp":"2026-08-18T12:00:00Z","upstream":"u","model":"m","request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
			wantContains: "host must not be empty",
		},
		{
			name:         "whitespace upstream",
			line:         `{"timestamp":"2026-08-18T12:00:00Z","host":"h","upstream":" \t","model":"m","request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
			wantContains: "upstream must not be empty",
		},
		{
			name:         "missing model",
			line:         `{"timestamp":"2026-08-18T12:00:00Z","host":"h","upstream":"u","request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
			wantContains: "model must not be empty",
		},
		{
			name:         "null model",
			line:         `{"timestamp":"2026-08-18T12:00:00Z","host":"h","upstream":"u","model":null,"request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
			wantContains: "model must not be empty",
		},
		{
			name:         "missing request model",
			line:         `{"timestamp":"2026-08-18T12:00:00Z","host":"h","upstream":"u","model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
			wantContains: "request_model must not be empty",
		},
		{
			name:         "missing auth mode",
			line:         `{"timestamp":"2026-08-18T12:00:00Z","host":"h","upstream":"u","model":"m","request_model":"m","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
			wantContains: "invalid auth_mode",
		},
		{
			name:         "credential class outside the vocabulary",
			line:         `{"timestamp":"2026-08-18T12:00:00Z","host":"h","credential_class":"bearer","upstream":"u","model":"m","request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
			wantContains: "invalid credential_class",
		},
		{
			name:         "context outside the v1 grammar",
			line:         `{"timestamp":"2026-08-18T12:00:00Z","host":"h","context":"Work/Dev","upstream":"u","model":"m","request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
			wantContains: "invalid context",
		},
		{
			name:         "missing timestamp",
			line:         `{"host":"h","upstream":"u","model":"m","request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
			wantContains: "timestamp must not be zero",
		},
		{
			name:         "negative status",
			line:         `{"timestamp":"2026-08-18T12:00:00Z","host":"h","upstream":"u","model":"m","request_model":"m","auth_mode":"forward","status":-1,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
			wantContains: "status must not be negative",
		},
		{
			name:         "negative output tokens",
			line:         `{"timestamp":"2026-08-18T12:00:00Z","host":"h","upstream":"u","model":"m","request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":-2,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
			wantContains: "token counts must not be negative",
		},
		{
			name:         "negative upstream duration",
			line:         `{"timestamp":"2026-08-18T12:00:00Z","host":"h","upstream":"u","model":"m","request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":-3,"block_shape":[]}`,
			wantContains: "durations must not be negative",
		},
		{
			name:         "token count is not a number",
			line:         `{"timestamp":"2026-08-18T12:00:00Z","host":"h","upstream":"u","model":"m","request_model":"m","auth_mode":"forward","status":200,"input_tokens":"ten","output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
			wantContains: "parse JSON",
		},
		{
			name:         "trailing garbage after the record",
			line:         `{"timestamp":"2026-08-18T12:00:00Z","host":"h","upstream":"u","model":"m","request_model":"m","auth_mode":"forward","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]} oops`,
			wantContains: "parse JSON",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
			path := writeLedger(t, t.TempDir(), "patchbay-2026-08-18.jsonl", test.line+"\n")
			result, err := store.ImportLedgerFile(
				context.Background(),
				path,
				filepath.Base(path),
				0,
			)
			if err == nil {
				t.Fatalf("ImportLedgerFile() = %#v, nil; want refusal", result)
			}
			if !strings.Contains(err.Error(), path+":1") {
				t.Fatalf("error = %v; want it to name %s:1", err, path)
			}
			if !strings.Contains(err.Error(), test.wantContains) {
				t.Fatalf("error = %v; want it to contain %q", err, test.wantContains)
			}
			if count := countStoredRequests(t, store.db); count != 0 {
				t.Fatalf("rows after refused file = %d; want none", count)
			}
		})
	}
}

// v2Encoded renders a record exactly as the live ledger would write it, so a
// fixture can never drift from the shape the writer actually produces.
func v2Encoded(t *testing.T, record ledger.Record) string {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("encode v2 ledger record: %v", err)
	}
	return string(encoded)
}

// v2Line is the same, for a record that must also be one the ledger would have
// accepted. A fixture that fails its own validation is testing nothing.
func v2Line(t *testing.T, record ledger.Record) string {
	t.Helper()
	if err := record.Validate(); err != nil {
		t.Fatalf("v2 fixture is not a valid ledger record: %v", err)
	}
	return v2Encoded(t, record)
}

type importedIdentity struct {
	contextName, requestModel, resolvedModel, authMode, credential string
	policy, kind, outcome, costStatus                              string
	alias, class, basis                                            sql.NullString
	upstream                                                       string
	contacted                                                      int64
	dispatched, finalCost                                          sql.NullInt64
}

func readImportedIdentities(t *testing.T, db *sql.DB) []importedIdentity {
	t.Helper()

	rows, err := db.Query(`SELECT route_context, route_alias, route_upstream, request_model,
		resolved_model, auth_mode, credential_class, billing_policy, billing_class, request_kind,
		outcome, upstream_contacted, upstream_dispatched_at, cost_status, cost_basis,
		final_cost_nano_usd FROM accounting_requests ORDER BY import_line`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []importedIdentity
	for rows.Next() {
		var value importedIdentity
		if err := rows.Scan(
			&value.contextName,
			&value.alias,
			&value.upstream,
			&value.requestModel,
			&value.resolvedModel,
			&value.authMode,
			&value.credential,
			&value.policy,
			&value.class,
			&value.kind,
			&value.outcome,
			&value.contacted,
			&value.dispatched,
			&value.costStatus,
			&value.basis,
			&value.finalCost,
		); err != nil {
			t.Fatal(err)
		}
		got = append(got, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestImportLedgerFileImportsV2Shapes pins the mapping from the live ledger's
// three record shapes onto the accounting row.
//
// The Seat is the thing that must survive: it is the name an operator reads a
// cost back by, and it lands in the same column v1 aliases occupy. A
// pre-selection record has no Seat to carry, and the row says so with a NULL
// rather than with an invented name — and says nothing about who paid, because
// a request that reached no Seat spent nobody's money.
func TestImportLedgerFileImportsV2Shapes(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	requested := "claude-opus-5"
	rewritten := "claude-opus-5-20260101"

	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	path := writeLedger(t, t.TempDir(), "patchbay-v2-2026-08-20.jsonl", strings.Join([]string{
		v2Line(t, ledger.Record{
			Timestamp:       base,
			Host:            "gnomon",
			Context:         "personal",
			Selection:       &ledger.Selection{Kind: ledger.SelectionModel, Model: &requested},
			Seat:            "opus",
			Upstream:        "https://api.anthropic.com",
			Model:           &rewritten,
			RequestModel:    &requested,
			AuthMode:        "forward",
			CredentialClass: billing.CredentialOAuth,
			ConfigSource:    "caller",
			Status:          200,
			InputTokens:     10,
			OutputTokens:    2,
			DurationMS:      50,
			UpstreamMS:      30,
			BlockShape:      []string{"text"},
			StopReason:      "end_turn",
		}),
		// A default selection with neither model present: the Context bound no
		// rewrite and the caller named nothing.
		v2Line(t, ledger.Record{
			Timestamp:       base.Add(time.Minute),
			Host:            "gnomon",
			Context:         "work",
			Selection:       &ledger.Selection{Kind: ledger.SelectionDefault},
			Seat:            "bedrock",
			Upstream:        "https://bedrock-runtime.us-west-2.amazonaws.com",
			AuthMode:        "sigv4",
			CredentialClass: billing.CredentialNone,
			ConfigSource:    "aws_profile:default",
			Status:          200,
			DurationMS:      20,
			UpstreamMS:      10,
			BlockShape:      []string{},
		}),
		// A pre-selection record: the caller named a Context no registry could
		// produce, so nothing was selected and nothing was contacted.
		v2Line(t, ledger.Record{
			Timestamp:       base.Add(2 * time.Minute),
			Host:            "gnomon",
			Context:         "Nope",
			RequestModel:    &requested,
			CredentialClass: billing.CredentialOAuth,
			Status:          404,
			DurationMS:      5,
			BlockShape:      []string{},
		}),
	}, "\n")+"\n")

	result, err := store.ImportLedgerFile(context.Background(), path, filepath.Base(path), 0)
	if err != nil {
		t.Fatalf("ImportLedgerFile() error: %v", err)
	}
	if want := (ImportResult{Imported: 3}); result != want {
		t.Fatalf("ImportLedgerFile() = %#v; want %#v", result, want)
	}

	started := base.UnixNano()
	defaultStarted := base.Add(time.Minute).UnixNano()
	want := []importedIdentity{
		{
			contextName:   "personal",
			alias:         sql.NullString{String: "opus", Valid: true},
			upstream:      "https://api.anthropic.com",
			requestModel:  "claude-opus-5",
			resolvedModel: "claude-opus-5-20260101",
			authMode:      "forward",
			credential:    "oauth",
			policy:        "credential_derived",
			class:         sql.NullString{String: "subscription", Valid: true},
			kind:          "messages",
			outcome:       "success",
			contacted:     1,
			dispatched:    sql.NullInt64{Int64: started + 20_000_000, Valid: true},
			costStatus:    "known",
			basis:         sql.NullString{String: "zero_marginal", Valid: true},
			finalCost:     sql.NullInt64{Int64: 0, Valid: true},
		},
		{
			contextName:   "work",
			alias:         sql.NullString{String: "bedrock", Valid: true},
			upstream:      "https://bedrock-runtime.us-west-2.amazonaws.com",
			requestModel:  "",
			resolvedModel: "",
			authMode:      "sigv4",
			credential:    "none",
			policy:        "sigv4_metered",
			class:         sql.NullString{String: "metered", Valid: true},
			kind:          "other",
			outcome:       "success",
			contacted:     1,
			dispatched:    sql.NullInt64{Int64: defaultStarted + 10_000_000, Valid: true},
			costStatus:    "unknown",
			basis:         sql.NullString{String: "unknown", Valid: true},
		},
		{
			contextName:   "Nope",
			requestModel:  "claude-opus-5",
			resolvedModel: "",
			authMode:      "",
			credential:    "oauth",
			policy:        "",
			kind:          "other",
			outcome:       "local_rejection",
			contacted:     0,
			costStatus:    "unknown",
			basis:         sql.NullString{String: "unknown", Valid: true},
		},
	}
	if got := readImportedIdentities(t, store.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("imported v2 rows:\n got %#v\nwant %#v", got, want)
	}
}

func TestImportLedgerFileV2UsesStrictUnknownFieldRule(t *testing.T) {
	t.Parallel()

	model := "model"
	record := ledger.Record{
		Timestamp:       time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		Host:            "h",
		Context:         "personal",
		Selection:       &ledger.Selection{Kind: ledger.SelectionDefault},
		Seat:            "current-seat",
		Upstream:        "https://api.anthropic.com",
		Model:           &model,
		RequestModel:    &model,
		AuthMode:        "forward",
		CredentialClass: billing.CredentialOAuth,
		ConfigSource:    "caller",
		Status:          200,
		BlockShape:      []string{},
	}
	line := strings.Replace(v2Line(t, record), `"host":"h"`, `"host":"h","alias":"legacy"`, 1)
	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	path := writeLedger(t, t.TempDir(), "patchbay-v2-2026-08-20.jsonl", line+"\n")
	_, err := store.ImportLedgerFile(context.Background(), path, filepath.Base(path), 0)
	if err == nil || !strings.Contains(err.Error(), `unknown field "alias"`) {
		t.Fatalf("ImportLedgerFile() error = %v; want strict unknown-field refusal", err)
	}
	if count := countStoredRequests(t, store.db); count != 0 {
		t.Fatalf("rows after refused v2 file = %d; want none", count)
	}
}

// TestImportLedgerFileChoosesDecoderByFileName pins that the file's own name
// decides whether v1's lenient historical decoder or v2's strict decoder applies.
func TestImportLedgerFileChoosesDecoderByFileName(t *testing.T) {
	t.Parallel()

	v1Line := `{"timestamp":"2026-08-20T12:00:00Z","host":"h","alias":"opus","upstream":"u","model":"m","request_model":"m","auth_mode":"inject","status":200,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`

	tests := []struct {
		name         string
		fileName     string
		line         func(*testing.T) string
		wantContains string
	}{
		{
			name:     "v1 contents under a v2 name",
			fileName: "patchbay-v2-2026-08-20.jsonl",
			line: func(t *testing.T) string {
				t.Helper()

				return v1Line
			},
			// The strict v2 decoder rejects the v1-only alias before any values are
			// projected into accounting.
			wantContains: `unknown field "alias"`,
		},
		{
			name:     "v2 contents under a v1 name",
			fileName: "patchbay-2026-08-20.jsonl",
			line: func(t *testing.T) string {
				t.Helper()

				return v2Line(t, ledger.Record{
					Timestamp:       time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
					Host:            "h",
					Context:         "personal",
					Selection:       &ledger.Selection{Kind: ledger.SelectionDefault},
					Seat:            "anthropic",
					Upstream:        "https://api.anthropic.com",
					AuthMode:        "forward",
					CredentialClass: billing.CredentialOAuth,
					ConfigSource:    "caller",
					Status:          200,
					BlockShape:      []string{},
				})
			},
			wantContains: "model must not be empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
			path := writeLedger(t, t.TempDir(), test.fileName, test.line(t)+"\n")
			_, err := store.ImportLedgerFile(context.Background(), path, filepath.Base(path), 0)
			if err == nil {
				t.Fatal("ImportLedgerFile() = nil; want refusal")
			}
			if !strings.Contains(err.Error(), path+":1") ||
				!strings.Contains(err.Error(), test.wantContains) {
				t.Fatalf("error = %v; want %s:1 and %q", err, path, test.wantContains)
			}
			if count := countStoredRequests(t, store.db); count != 0 {
				t.Fatalf("rows after refused file = %d; want none", count)
			}
		})
	}

	t.Run("name claims no schema", func(t *testing.T) {
		t.Parallel()

		store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
		path := writeLedger(t, t.TempDir(), "ledger.jsonl", v1Line+"\n")
		_, err := store.ImportLedgerFile(context.Background(), path, filepath.Base(path), 0)
		if err == nil || !strings.Contains(err.Error(), "names no known ledger schema") {
			t.Fatalf("error = %v; want a refusal naming the unknown schema", err)
		}
		if count := countStoredRequests(t, store.db); count != 0 {
			t.Fatalf("rows after refused file = %d; want none", count)
		}
	})
}

func TestImportLedgerFileRequiresExactDatedName(t *testing.T) {
	t.Parallel()

	const v1 = `{"timestamp":"2026-08-20T12:00:00Z","host":"h","upstream":"u",` +
		`"model":"m","request_model":"m","auth_mode":"inject","status":200,` +
		`"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`
	model := "m"
	v2 := v2Line(t, ledger.Record{
		Timestamp:       time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		Host:            "h",
		Context:         "personal",
		Selection:       &ledger.Selection{Kind: ledger.SelectionDefault},
		Seat:            "anthropic",
		Upstream:        "https://api.anthropic.com",
		Model:           &model,
		RequestModel:    &model,
		AuthMode:        "forward",
		CredentialClass: billing.CredentialOAuth,
		ConfigSource:    "caller",
		Status:          200,
		BlockShape:      []string{},
	})

	for _, test := range []struct {
		name, fileName, line string
		wantError            bool
	}{
		{name: "v2 backup suffix", fileName: "patchbay-v2-backup.txt", line: v2, wantError: true},
		{name: "garbage after v1 prefix", fileName: "patchbay-garbage", line: v1, wantError: true},
		{name: "valid v1 date", fileName: "patchbay-2026-08-20.jsonl", line: v1},
		{name: "valid v2 date", fileName: "patchbay-v2-2026-08-20.jsonl", line: v2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
			path := writeLedger(t, t.TempDir(), test.fileName, test.line+"\n")
			result, err := store.ImportLedgerFile(context.Background(), path, filepath.Base(path), 0)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "names no known ledger schema") {
					t.Fatalf("ImportLedgerFile() = %#v, %v; want unknown-schema refusal", result, err)
				}
				return
			}
			if err != nil || result != (ImportResult{Imported: 1}) {
				t.Fatalf("ImportLedgerFile() = %#v, %v; want one imported row", result, err)
			}
		})
	}
}

// TestImportLedgerFileRejectsInvalidV2Records pins that a v2 file is read under
// the ledger package's exact strict decoder, including wire-key shape rules.
func TestImportLedgerFileRejectsInvalidV2Records(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	seated := func() ledger.Record {
		return ledger.Record{
			Timestamp:       base,
			Host:            "h",
			Context:         "personal",
			Selection:       &ledger.Selection{Kind: ledger.SelectionDefault},
			Seat:            "anthropic",
			Upstream:        "https://api.anthropic.com",
			AuthMode:        "forward",
			CredentialClass: billing.CredentialOAuth,
			ConfigSource:    "caller",
			Status:          200,
			BlockShape:      []string{},
		}
	}

	tests := []struct {
		name         string
		line         func(*testing.T) string
		wantContains string
	}{
		{
			name: "duplicate member name",
			line: func(t *testing.T) string {
				t.Helper()

				encoded := v2Encoded(t, seated())
				return strings.Replace(encoded, `"host":"h"`, `"host":"h","host":"other"`, 1)
			},
			wantContains: "duplicate member",
		},
		{
			name: "selected record missing model keys",
			line: func(t *testing.T) string {
				t.Helper()

				var fields map[string]json.RawMessage
				if err := json.Unmarshal([]byte(v2Encoded(t, seated())), &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields, "model")
				delete(fields, "request_model")
				encoded, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				return string(encoded)
			},
			wantContains: "must carry both model and request_model",
		},
		{
			name: "pre-selection record with null request model",
			line: func(t *testing.T) string {
				t.Helper()

				record := ledger.Record{
					Timestamp:       base,
					Host:            "h",
					Context:         "missing",
					CredentialClass: billing.CredentialOAuth,
					Status:          404,
					BlockShape:      []string{},
				}
				encoded := v2Encoded(t, record)
				return strings.Replace(encoded, `"host":"h"`, `"host":"h","request_model":null`, 1)
			},
			wantContains: "must omit an absent request_model rather than null it",
		},
		{
			name: "seat no registry could have admitted",
			line: func(t *testing.T) string {
				t.Helper()

				record := seated()
				record.Seat = "Anthropic"
				return v2Encoded(t, record)
			},
			wantContains: "invalid seat",
		},
		{
			name: "selection kind outside the vocabulary",
			line: func(t *testing.T) string {
				t.Helper()

				record := seated()
				record.Selection = &ledger.Selection{Kind: "guessed"}
				return v2Encoded(t, record)
			},
			wantContains: "invalid selection kind",
		},
		{
			name: "pre-selection record carrying a seat",
			line: func(t *testing.T) string {
				t.Helper()

				record := seated()
				record.Selection = nil
				return v2Encoded(t, record)
			},
			wantContains: "must omit selection-dependent fields",
		},
		{
			name: "drop signature disagreeing with block shape",
			line: func(t *testing.T) string {
				t.Helper()

				record := seated()
				record.DropSignature = true
				return v2Encoded(t, record)
			},
			wantContains: "drop_signature",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
			path := writeLedger(
				t,
				t.TempDir(),
				"patchbay-v2-2026-08-20.jsonl",
				test.line(t)+"\n",
			)
			_, err := store.ImportLedgerFile(context.Background(), path, filepath.Base(path), 0)
			if err == nil {
				t.Fatal("ImportLedgerFile() = nil; want refusal")
			}
			if !strings.Contains(err.Error(), path+":1") ||
				!strings.Contains(err.Error(), test.wantContains) {
				t.Fatalf("error = %v; want %s:1 and %q", err, path, test.wantContains)
			}
			if count := countStoredRequests(t, store.db); count != 0 {
				t.Fatalf("rows after refused file = %d; want none", count)
			}
		})
	}
}

func TestImportLedgerFileYieldsToConcurrentLiveWriters(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	ledgerPath := writeLedger(
		t,
		t.TempDir(),
		"patchbay-2026-08-20.jsonl",
		strings.Repeat(
			`{"timestamp":"2026-08-20T12:00:00Z","host":"host","upstream":"upstream","model":"model","request_model":"model","auth_mode":"inject","status":200,"input_tokens":0,"output_tokens":1,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`+"\n",
			importChunkLines*2+1,
		),
	)
	betweenChunks := make(chan struct{})
	continueImport := make(chan struct{})
	ctx := context.WithValue(t.Context(), importAfterChunkHookKey{}, func(committed int) {
		if committed == 1 {
			close(betweenChunks)
			<-continueImport
		}
	})
	type importOutcome struct {
		result ImportResult
		err    error
	}
	importDone := make(chan importOutcome, 1)
	go func() {
		result, err := store.ImportLedgerFile(ctx, ledgerPath, filepath.Base(ledgerPath), 0)
		importDone <- importOutcome{result: result, err: err}
	}()

	select {
	case <-betweenChunks:
	case <-time.After(5 * time.Second):
		t.Fatal("import did not commit its first chunk")
	}
	writerDone := make(chan error, 1)
	go func() {
		_, err := store.InsertPending(t.Context(), validPendingRequest())
		writerDone <- err
	}()
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("live writer between import chunks: %v", err)
		}
	case <-time.After(time.Second):
		close(continueImport)
		<-importDone
		t.Fatal("live writer did not complete between import chunk transactions")
	}
	close(continueImport)

	outcome := <-importDone
	if outcome.err != nil {
		t.Fatalf("ImportLedgerFile() error: %v", outcome.err)
	}
	if outcome.result.Imported != importChunkLines*2+1 {
		t.Fatalf("imported rows = %d; want %d", outcome.result.Imported, importChunkLines*2+1)
	}
}

func TestPrepareImportBatchBoundsCountAndBytesBeforeDecode(t *testing.T) {
	line := []byte(
		`{"timestamp":"2026-08-20T12:00:00Z","host":"host","upstream":"upstream",` +
			`"model":"model","request_model":"model","auth_mode":"inject","status":200,` +
			`"input_tokens":0,"output_tokens":1,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`,
	)
	lineBytes := len(line) + 1
	decodes := 0
	plan := importPlan{
		path:   "ledger.jsonl",
		source: "ledger.jsonl",
		decode: func(contents []byte) (importedRecord, error) {
			decodes++
			return decodeLedgerV1Line(contents)
		},
	}

	countScanner := bufio.NewScanner(strings.NewReader(strings.Repeat(string(line)+"\n", 3)))
	countScanner.Buffer(make([]byte, initialLedgerScanBuffer), maxLedgerLineBytes)
	countReader := importBatchReader{scanner: countScanner}
	countBatch, err := countReader.prepareBatch(plan, 2, lineBytes*3)
	if err != nil {
		t.Fatalf("prepare count-bounded batch: %v", err)
	}
	if len(countBatch.lines) != 2 || countBatch.sourceBytes != lineBytes*2 || countBatch.finished {
		t.Fatalf("count-bounded batch = %d lines, %d bytes, finished=%t; want 2, %d, false",
			len(countBatch.lines), countBatch.sourceBytes, countBatch.finished, lineBytes*2)
	}
	if decodes != 2 {
		t.Fatalf("count-bounded decoder calls = %d; want 2 admitted lines", decodes)
	}

	decodes = 0
	byteScanner := bufio.NewScanner(strings.NewReader(strings.Repeat(string(line)+"\n", 3)))
	byteScanner.Buffer(make([]byte, initialLedgerScanBuffer), maxLedgerLineBytes)
	byteReader := importBatchReader{scanner: byteScanner}
	byteBatch, err := byteReader.prepareBatch(plan, 3, lineBytes*2-1)
	if err != nil {
		t.Fatalf("prepare byte-bounded batch: %v", err)
	}
	if len(byteBatch.lines) != 1 || byteBatch.sourceBytes != lineBytes || byteBatch.finished {
		t.Fatalf("byte-bounded batch = %d lines, %d bytes, finished=%t; want 1, %d, false",
			len(byteBatch.lines), byteBatch.sourceBytes, byteBatch.finished, lineBytes)
	}
	if decodes != 1 {
		t.Fatalf("byte-bounded decoder calls = %d; want overflow line carried raw", decodes)
	}
	secondBatch, err := byteReader.prepareBatch(plan, 3, lineBytes*2-1)
	if err != nil {
		t.Fatalf("prepare byte-bounded carry batch: %v", err)
	}
	if len(secondBatch.lines) != 1 || secondBatch.lines[0].line != 2 || decodes != 2 {
		t.Fatalf("carry batch = %#v with %d decoder calls; want only source line 2 decoded next", secondBatch, decodes)
	}

	decodes = 0
	oversizedScanner := bufio.NewScanner(strings.NewReader(string(line) + "\n"))
	oversizedScanner.Buffer(make([]byte, initialLedgerScanBuffer), maxLedgerLineBytes)
	oversizedReader := importBatchReader{scanner: oversizedScanner}
	oversizedBatch, err := oversizedReader.prepareBatch(plan, 3, lineBytes-1)
	if err != nil {
		t.Fatalf("prepare oversized single-line batch: %v", err)
	}
	if len(oversizedBatch.lines) != 1 || oversizedBatch.sourceBytes != lineBytes || decodes != 1 {
		t.Fatalf("oversized batch = %#v with %d decoder calls; want one admitted line of %d bytes",
			oversizedBatch, decodes, lineBytes)
	}
}

func TestImportLedgerFileFeedsSummary(t *testing.T) {
	t.Parallel()

	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	dir := t.TempDir()
	path := writeLedger(
		t,
		dir,
		"patchbay-2026-08-20.jsonl",
		`{"timestamp":"2026-08-20T12:00:00Z","host":"host","context":"personal","credential_class":"oauth","upstream":"upstream","model":"model","request_model":"model","auth_mode":"forward","status":200,"input_tokens":3,"output_tokens":2,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`+"\n",
	)
	if _, err := store.ImportLedgerFile(
		context.Background(),
		path,
		filepath.Base(path),
		0,
	); err != nil {
		t.Fatal(err)
	}
	summary, err := store.SummarizeUsage(
		context.Background(),
		SummaryFilter{
			Since: time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC),
			Until: time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests.Messages != 1 || summary.Messages.Finalized != 1 ||
		summary.Messages.KnownCostNanoUSD != 0 ||
		summary.Messages.UnknownCostRows != 0 ||
		summary.Messages.InputTokens != 3 ||
		summary.Messages.OutputTokens != 2 ||
		summary.Messages.ByBillingClass["subscription"].Requests != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}
