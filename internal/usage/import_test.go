package usage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
			Route:           validPendingRequest().Route,
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

func TestImportLedgerFileYieldsToConcurrentLiveWriters(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	ledger := writeLedger(
		t,
		t.TempDir(),
		"patchbay-2026-08-20.jsonl",
		strings.Repeat(
			`{"timestamp":"2026-08-20T12:00:00Z","host":"host","upstream":"upstream","model":"model","request_model":"model","auth_mode":"inject","status":200,"input_tokens":0,"output_tokens":1,"duration_ms":0,"upstream_ms":0,"block_shape":[]}`+"\n",
			importChunkLines*2+1,
		),
	)
	stop := make(chan struct{})
	errs := make(chan error, 1)
	var completed atomic.Int64
	var once sync.Once
	started := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for index := 0; ; index++ {
			select {
			case <-stop:
				return
			default:
			}
			request := validPendingRequest()
			request.StartedAt = request.StartedAt.Add(time.Duration(index))
			id, err := store.InsertPending(t.Context(), request)
			if err != nil {
				errs <- err
				return
			}
			finalized := validFinalizedRequest()
			finalized.CompletedAt = finalized.CompletedAt.Add(time.Duration(index))
			finalized.ResponseID = fmt.Sprintf("msg-concurrent-%d", index)
			if err := store.Finalize(t.Context(), id, finalized); err != nil {
				errs <- err
				return
			}
			completed.Add(1)
			once.Do(func() { close(started) })
		}
	}()
	select {
	case <-started:
	case err := <-errs:
		t.Fatalf("initial live writer failure: %v", err)
	case <-time.After(time.Second):
		t.Fatal("live writer did not start")
	}
	if _, err := store.ImportLedgerFile(t.Context(), ledger, filepath.Base(ledger), 0); err != nil {
		t.Fatalf("ImportLedgerFile() error: %v", err)
	}
	close(stop)
	writers.Wait()
	select {
	case err := <-errs:
		t.Fatalf("concurrent live writer failure: %v", err)
	default:
	}
	if completed.Load() == 0 {
		t.Fatal("concurrent live writer completed no requests")
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
