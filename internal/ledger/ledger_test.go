package ledger_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/joshsymonds/patchbay/internal/ledger"
)

const (
	testDay      = "2026-08-18"
	testDayFile  = "patchbay-2026-08-18.jsonl"
	nextDay      = "2026-08-19"
	nextDayFile  = "patchbay-2026-08-19.jsonl"
	testFileMode = 0o600
)

func testTimestamp(t *testing.T, day string) time.Time {
	t.Helper()

	when, err := time.Parse(time.RFC3339Nano, day+"T13:45:12.123456789Z")
	if err != nil {
		t.Fatalf("parse test timestamp for %q: %v", day, err)
	}
	return when
}

// validRecord returns a record that passes Validate, for tests that then mutate
// one field to exercise a specific rejection or behavior.
func validRecord(t *testing.T) ledger.Record {
	t.Helper()

	return ledger.Record{
		Timestamp:       testTimestamp(t, testDay),
		Host:            "gnomon",
		Context:         "personal",
		CredentialClass: ledger.CredentialOAuth,

		Alias:         "bedrock",
		Upstream:      "https://api.anthropic.com",
		Model:         "claude-fable-5",
		RequestModel:  "claude-fable-5",
		AuthMode:      "inject",
		Status:        200,
		InputTokens:   1234,
		OutputTokens:  567,
		DurationMS:    8910,
		UpstreamMS:    120,
		BlockShape:    []string{"thinking", "text"},
		DropSignature: false,
		StopReason:    "end_turn",
	}
}

func assertRecordEqual(t *testing.T, got, want ledger.Record) {
	t.Helper()

	if !got.Timestamp.Equal(want.Timestamp) {
		t.Fatalf(
			"timestamp = %s; want %s",
			got.Timestamp.Format(time.RFC3339Nano),
			want.Timestamp.Format(time.RFC3339Nano),
		)
	}
	got.Timestamp = time.Time{}
	want.Timestamp = time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func openLedger(t *testing.T, dir string) *ledger.Ledger {
	t.Helper()

	led, err := ledger.Open(dir)
	if err != nil {
		t.Fatalf("Open(%q) returned error: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := led.Close(); err != nil {
			t.Errorf("Close() returned error: %v", err)
		}
	})
	return led
}

func TestAppendScanRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	first := validRecord(t)
	second := validRecord(t)
	second.Alias = ""
	second.Status = 429
	second.BlockShape = []string{"thinking", "thinking", "text"}
	second.DropSignature = true
	second.ErrorBody = "rate limited"
	third := validRecord(t)
	third.AuthMode = "forward"
	third.RequestBody = `{"model":"claude-fable-5"}`
	third.ResponseBody = `{"content":[{"type":"thinking"}]}`

	want := []ledger.Record{first, second, third}
	for i, rec := range want {
		if err := led.Append(rec); err != nil {
			t.Fatalf("Append(record %d) returned error: %v", i, err)
		}
	}

	got, err := led.Scan(testDay)
	if err != nil {
		t.Fatalf("Scan(%q) returned error: %v", testDay, err)
	}
	if len(got) != len(want) {
		t.Fatalf("Scan returned %d records; want %d", len(got), len(want))
	}
	for i := range want {
		assertRecordEqual(t, got[i], want[i])
	}
}

func TestAppendWritesOneLinePerRecord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	for range 3 {
		if err := led.Append(validRecord(t)); err != nil {
			t.Fatalf("Append returned error: %v", err)
		}
	}

	raw, err := os.ReadFile(filepath.Join(dir, testDayFile))
	if err != nil {
		t.Fatalf("read ledger file: %v", err)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("ledger file does not end with a newline: %q", string(raw))
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("ledger file has %d lines; want 3", len(lines))
	}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			t.Fatalf("line %d is blank; want one JSON object per line", i)
		}
	}
}

func TestAppendRotatesByUTCDay(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	today := validRecord(t)
	tomorrow := validRecord(t)
	tomorrow.Timestamp = testTimestamp(t, nextDay)

	if err := led.Append(today); err != nil {
		t.Fatalf("Append(today) returned error: %v", err)
	}
	if err := led.Append(tomorrow); err != nil {
		t.Fatalf("Append(tomorrow) returned error: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read ledger dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	want := []string{testDayFile, nextDayFile}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("ledger dir contains %v; want %v", names, want)
	}

	for day, wantRec := range map[string]ledger.Record{testDay: today, nextDay: tomorrow} {
		got, scanErr := led.Scan(day)
		if scanErr != nil {
			t.Fatalf("Scan(%q) returned error: %v", day, scanErr)
		}
		if len(got) != 1 {
			t.Fatalf("Scan(%q) returned %d records; want 1", day, len(got))
		}
		assertRecordEqual(t, got[0], wantRec)
	}
}

func TestAppendUsesUTCDayForLocalTimestamps(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	// 2026-08-19T02:30 in a +14 zone is 2026-08-18T12:30Z: the timestamp's own
	// calendar day and its UTC day differ, and the file must follow the UTC one.
	zone := time.FixedZone("plus14", 14*60*60)
	rec := validRecord(t)
	rec.Timestamp = time.Date(2026, time.August, 19, 2, 30, 0, 0, zone)

	if err := led.Append(rec); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, testDayFile)); err != nil {
		t.Fatalf("expected record in %s (the UTC day): %v", testDayFile, err)
	}
	if _, err := os.Stat(filepath.Join(dir, nextDayFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record landed in %s (the local day); stat err = %v", nextDayFile, err)
	}

	got, err := led.Scan(testDay)
	if err != nil {
		t.Fatalf("Scan(%q) returned error: %v", testDay, err)
	}
	if len(got) != 1 {
		t.Fatalf("Scan(%q) returned %d records; want 1", testDay, len(got))
	}
	assertRecordEqual(t, got[0], rec)
}

func TestScanMissingFileReturnsEmpty(t *testing.T) {
	t.Parallel()

	led := openLedger(t, t.TempDir())

	got, err := led.Scan(testDay)
	if err != nil {
		t.Fatalf("Scan of missing day returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Scan of missing day returned %d records; want 0", len(got))
	}
}

func TestOpenCreatesMissingDirectory(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "nested", "ledger")
	led := openLedger(t, dir)

	if err := led.Append(validRecord(t)); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat ledger dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("ledger dir mode = %o; want 700", perm)
	}
}

func TestAppendCreatesFileWithOwnerOnlyMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	if err := led.Append(validRecord(t)); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, testDayFile))
	if err != nil {
		t.Fatalf("stat ledger file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != testFileMode {
		t.Fatalf("ledger file mode = %o; want 600", perm)
	}
}

func TestOpenRejectsNonDirectory(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), testFileMode); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	if _, err := ledger.Open(path); err == nil {
		t.Fatal("Open on a regular file returned nil error; want failure")
	}
}

func TestAppendRejectsInvalidRecordBeforeIO(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	invalid := validRecord(t)
	invalid.Host = ""

	if err := led.Append(invalid); err == nil {
		t.Fatal("Append of an invalid record returned nil error; want failure")
	}
	if _, err := os.Stat(filepath.Join(dir, testDayFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid Append created a ledger file (stat err = %v); want none", err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*ledger.Record)
		wantErr bool
	}{
		{name: "valid", mutate: func(*ledger.Record) {}, wantErr: false},
		{name: "valid with empty alias", mutate: func(r *ledger.Record) { r.Alias = "" }, wantErr: false},
		{name: "valid auth mode forward", mutate: func(r *ledger.Record) { r.AuthMode = "forward" }, wantErr: false},
		{name: "zero timestamp", mutate: func(r *ledger.Record) { r.Timestamp = time.Time{} }, wantErr: true},
		{name: "empty host", mutate: func(r *ledger.Record) { r.Host = "" }, wantErr: true},
		{name: "blank host", mutate: func(r *ledger.Record) { r.Host = "   " }, wantErr: true},
		{name: "empty upstream", mutate: func(r *ledger.Record) { r.Upstream = "" }, wantErr: true},
		{name: "empty model", mutate: func(r *ledger.Record) { r.Model = "" }, wantErr: true},
		{name: "empty request model", mutate: func(r *ledger.Record) { r.RequestModel = "" }, wantErr: true},
		{name: "empty auth mode", mutate: func(r *ledger.Record) { r.AuthMode = "" }, wantErr: true},
		{name: "unknown auth mode", mutate: func(r *ledger.Record) { r.AuthMode = "passthrough" }, wantErr: true},
		{
			name:    "credential class api_key",
			mutate:  func(r *ledger.Record) { r.CredentialClass = ledger.CredentialAPIKey },
			wantErr: false,
		},
		{
			name:    "credential class none",
			mutate:  func(r *ledger.Record) { r.CredentialClass = ledger.CredentialNone },
			wantErr: false,
		},
		{
			name:    "empty credential class, as a record written before contexts existed",
			mutate:  func(r *ledger.Record) { r.CredentialClass = "" },
			wantErr: false,
		},
		{
			name:    "unknown credential class",
			mutate:  func(r *ledger.Record) { r.CredentialClass = "sigv4" },
			wantErr: true,
		},
		{
			name:    "a credential class that is a header value",
			mutate:  func(r *ledger.Record) { r.CredentialClass = "Bearer sk-ant-secret" },
			wantErr: true,
		},
		{
			name:    "empty context, as a record written before contexts existed",
			mutate:  func(r *ledger.Record) { r.Context = "" },
			wantErr: false,
		},
		{name: "blank context", mutate: func(r *ledger.Record) { r.Context = "   " }, wantErr: true},
		{name: "named context", mutate: func(r *ledger.Record) { r.Context = "work" }, wantErr: false},
		{name: "context with a slash", mutate: func(r *ledger.Record) { r.Context = "work/eu" }, wantErr: true},
		{name: "uppercase context", mutate: func(r *ledger.Record) { r.Context = "Work" }, wantErr: true},
		{name: "negative status", mutate: func(r *ledger.Record) { r.Status = -1 }, wantErr: true},
		{name: "negative input tokens", mutate: func(r *ledger.Record) { r.InputTokens = -1 }, wantErr: true},
		{name: "negative output tokens", mutate: func(r *ledger.Record) { r.OutputTokens = -1 }, wantErr: true},
		{name: "negative duration", mutate: func(r *ledger.Record) { r.DurationMS = -1 }, wantErr: true},
		{name: "negative upstream duration", mutate: func(r *ledger.Record) { r.UpstreamMS = -1 }, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := validRecord(t)
			tc.mutate(&rec)

			err := rec.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil; want an error for %s", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v; want nil for %s", err, tc.name)
			}
		})
	}
}

func TestScanSurfacesCorruptLine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	if err := led.Append(validRecord(t)); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	path := filepath.Join(dir, testDayFile)
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger file: %v", err)
	}
	appendRaw(t, path, "this is not json\n")

	got, scanErr := led.Scan(testDay)
	if len(got) != 1 {
		t.Fatalf("Scan returned %d records; want the 1 valid record before the corruption", len(got))
	}
	var corrupt *ledger.CorruptionError
	if !errors.As(scanErr, &corrupt) {
		t.Fatalf("Scan error = %v; want a *ledger.CorruptionError", scanErr)
	}
	if corrupt.Offset != int64(len(valid)) {
		t.Fatalf("CorruptionError.Offset = %d; want %d", corrupt.Offset, len(valid))
	}
	if corrupt.Err == nil {
		t.Fatal("CorruptionError.Err is nil; want the underlying parse failure")
	}
	if corrupt.Error() == "" {
		t.Fatal("CorruptionError.Error() is empty")
	}
}

func TestScanSurfacesInvalidRecordLine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	if err := led.Append(validRecord(t)); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	path := filepath.Join(dir, testDayFile)
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger file: %v", err)
	}
	// Well-formed JSON that is not a well-formed record: host is empty.
	const emptyHostLine = `{"timestamp":"2026-08-18T13:45:12Z","host":"",` +
		`"upstream":"u","model":"m","request_model":"m","auth_mode":"inject"}`
	appendRaw(t, path, emptyHostLine+"\n")

	got, scanErr := led.Scan(testDay)
	if len(got) != 1 {
		t.Fatalf("Scan returned %d records; want 1", len(got))
	}
	var corrupt *ledger.CorruptionError
	if !errors.As(scanErr, &corrupt) {
		t.Fatalf("Scan error = %v; want a *ledger.CorruptionError", scanErr)
	}
	if corrupt.Offset != int64(len(valid)) {
		t.Fatalf("CorruptionError.Offset = %d; want %d", corrupt.Offset, len(valid))
	}
}

func TestScanSurfacesTruncatedFinalLine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	if err := led.Append(validRecord(t)); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if err := led.Append(validRecord(t)); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	path := filepath.Join(dir, testDayFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger file: %v", err)
	}
	// Chop the final newline and part of the last line: a torn write.
	firstLineLen := strings.Index(string(raw), "\n") + 1
	torn := raw[:len(raw)-8]
	if err := os.WriteFile(path, torn, testFileMode); err != nil {
		t.Fatalf("rewrite torn ledger file: %v", err)
	}

	got, scanErr := led.Scan(testDay)
	if len(got) != 1 {
		t.Fatalf("Scan returned %d records; want the 1 complete record", len(got))
	}
	var corrupt *ledger.CorruptionError
	if !errors.As(scanErr, &corrupt) {
		t.Fatalf("Scan error = %v; want a *ledger.CorruptionError", scanErr)
	}
	if corrupt.Offset != int64(firstLineLen) {
		t.Fatalf("CorruptionError.Offset = %d; want %d", corrupt.Offset, firstLineLen)
	}
	if !errors.Is(corrupt, io.ErrUnexpectedEOF) {
		t.Fatalf("CorruptionError does not unwrap to io.ErrUnexpectedEOF: %v", corrupt)
	}
}

func TestScanSkipsNothingSilently(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	if err := led.Append(validRecord(t)); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	path := filepath.Join(dir, testDayFile)
	appendRaw(t, path, "garbage\n")
	after := validRecord(t)
	after.Alias = "after-corruption"
	if err := led.Append(after); err != nil {
		t.Fatalf("Append after corruption returned error: %v", err)
	}

	got, scanErr := led.Scan(testDay)
	if scanErr == nil {
		t.Fatal("Scan returned nil error over a corrupt line; corruption must never be silent")
	}
	if len(got) != 1 {
		t.Fatalf("Scan returned %d records; want 1 (scan stops at the corruption)", len(got))
	}
}

func TestErrorBodyTruncatedToLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	rec := validRecord(t)
	rec.Status = 500
	rec.ErrorBody = strings.Repeat("e", 5000)

	if err := led.Append(rec); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	got, err := led.Scan(testDay)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Scan returned %d records; want 1", len(got))
	}
	if len(got[0].ErrorBody) != 4096 {
		t.Fatalf("stored error_body is %d bytes; want it truncated to 4096", len(got[0].ErrorBody))
	}
}

func TestErrorBodyTruncationKeepsValidUTF8(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	rec := validRecord(t)
	rec.Status = 502
	// Three-byte runes do not divide 4096 evenly, so a naive byte cut would
	// split the rune straddling the limit.
	rec.ErrorBody = strings.Repeat("は", 2000)

	if err := led.Append(rec); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	got, err := led.Scan(testDay)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Scan returned %d records; want 1", len(got))
	}
	body := got[0].ErrorBody
	if len(body) > 4096 {
		t.Fatalf("stored error_body is %d bytes; want at most 4096", len(body))
	}
	if !utf8.ValidString(body) {
		t.Fatal("truncated error_body is not valid UTF-8")
	}
	if strings.ContainsRune(body, utf8.RuneError) {
		t.Fatal("truncated error_body contains U+FFFD; the cut split a rune")
	}
}

func TestErrorBodyUnderLimitIsUntouched(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	rec := validRecord(t)
	rec.Status = 400
	rec.ErrorBody = `{"type":"error","error":{"type":"invalid_request_error"}}`

	if err := led.Append(rec); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	got, err := led.Scan(testDay)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if got[0].ErrorBody != rec.ErrorBody {
		t.Fatalf("error_body = %q; want it stored verbatim", got[0].ErrorBody)
	}
}

func TestConcurrentAppendLosesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	const (
		writers        = 8
		perWriter      = 25
		wantTotalCount = writers * perWriter
	)

	var wg sync.WaitGroup
	errs := make(chan error, wantTotalCount)
	for writer := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seq := range perWriter {
				rec := ledger.Record{
					Timestamp:       testTimestamp(t, testDay),
					Host:            "gnomon",
					Context:         "personal",
					CredentialClass: ledger.CredentialOAuth,
					Alias:           fmt.Sprintf("w%d-r%d", writer, seq),
					Upstream:        "https://api.anthropic.com",
					Model:           "claude-fable-5",
					RequestModel:    "claude-fable-5",
					AuthMode:        "inject",
					Status:          200,
				}
				if err := led.Append(rec); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Append returned error: %v", err)
	}

	got, err := led.Scan(testDay)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(got) != wantTotalCount {
		t.Fatalf("Scan returned %d records; want %d", len(got), wantTotalCount)
	}
	seen := make(map[string]bool, wantTotalCount)
	for _, rec := range got {
		if seen[rec.Alias] {
			t.Fatalf("alias %q appears twice; records interleaved", rec.Alias)
		}
		seen[rec.Alias] = true
	}
	if len(seen) != wantTotalCount {
		t.Fatalf("found %d distinct records; want %d", len(seen), wantTotalCount)
	}
}

// TestRecordCarriesContextAndCredentialClass pins the two attribution fields
// on the wire. credential_class is a classification of the INBOUND credential
// and never a value read off a header, which is why the vocabulary is closed:
// anything outside it is refused rather than written.
func TestRecordCarriesContextAndCredentialClass(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	rec := validRecord(t)
	rec.Context = "work"
	rec.CredentialClass = ledger.CredentialAPIKey

	if err := led.Append(rec); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	line, err := os.ReadFile(filepath.Join(dir, testDayFile))
	if err != nil {
		t.Fatalf("read ledger file: %v", err)
	}

	var fields map[string]any
	if unmarshalErr := json.Unmarshal(line, &fields); unmarshalErr != nil {
		t.Fatalf("decode ledger line %q: %v", line, unmarshalErr)
	}

	if fields["context"] != "work" {
		t.Fatalf("context field = %v; want %q", fields["context"], "work")
	}

	if fields["credential_class"] != "api_key" {
		t.Fatalf("credential_class field = %v; want %q", fields["credential_class"], "api_key")
	}

	records, scanErr := led.Scan(testDay)
	if scanErr != nil {
		t.Fatalf("Scan returned error: %v", scanErr)
	}

	if len(records) != 1 || records[0].Context != "work" || records[0].CredentialClass != ledger.CredentialAPIKey {
		t.Fatalf("Scan returned %+v; want the record's own context and credential class", records)
	}
}

// TestCredentialClassVocabularyIsClosed names the only three classifications a
// record may carry, so no future caller can smuggle a credential in as one.
func TestCredentialClassVocabularyIsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "oauth", got: ledger.CredentialOAuth, want: "oauth"},
		{name: "api key", got: ledger.CredentialAPIKey, want: "api_key"},
		{name: "none", got: ledger.CredentialNone, want: "none"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if tc.got != tc.want {
				t.Fatalf("credential class = %q; want %q", tc.got, tc.want)
			}
		})
	}
}

// TestRecordSchemaCarriesNoAuthHeaders is the type-level guarantee that no
// credential-bearing header can reach the ledger: the schema has no header
// field at all, so a fully-populated record cannot serialize one.
func TestRecordSchemaCarriesNoAuthHeaders(t *testing.T) {
	t.Parallel()

	rec := ledger.Record{
		Timestamp:       testTimestamp(t, testDay),
		Host:            "gnomon",
		Context:         "work",
		CredentialClass: ledger.CredentialOAuth,

		Alias:         "bedrock",
		Upstream:      "https://api.anthropic.com",
		Model:         "claude-fable-5",
		RequestModel:  "claude-opus-5",
		AuthMode:      "inject",
		Status:        500,
		InputTokens:   11,
		OutputTokens:  22,
		DurationMS:    33,
		UpstreamMS:    44,
		BlockShape:    []string{"thinking", "thinking"},
		DropSignature: true,
		StopReason:    "end_turn",
		ErrorBody:     "upstream exploded",
		RequestBody:   `{"model":"claude-opus-5"}`,
		ResponseBody:  `{"content":[]}`,
	}

	// Every field must be populated, so this test fails loudly if someone adds
	// a field (a header map above all) without extending the fixture.
	value := reflect.ValueOf(rec)
	for i := range value.NumField() {
		if value.Field(i).IsZero() {
			t.Fatalf("Record field %q is unset; populate it so this test covers it", value.Type().Field(i).Name)
		}
	}

	encoded, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	lower := strings.ToLower(string(encoded))
	for _, banned := range []string{"authorization", "x-api-key"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("marshaled record contains %q: %s", banned, encoded)
		}
	}
}

func TestRecordJSONFieldNames(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validRecord(t))
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal record into a field map: %v", err)
	}

	for _, name := range []string{
		"timestamp", "host", "alias", "upstream", "model", "request_model",
		"auth_mode", "status", "input_tokens", "output_tokens", "duration_ms",
		"upstream_ms", "block_shape", "drop_signature", "stop_reason",
	} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("marshaled record is missing field %q: %s", name, encoded)
		}
	}
	// The capture and error fields carry bodies; they must vanish entirely when
	// unset rather than writing empty strings into every line.
	for _, name := range []string{"error_body", "request_body", "response_body"} {
		if _, ok := fields[name]; ok {
			t.Fatalf("optional field %q must be omitted when empty: %s", name, encoded)
		}
	}
}

func TestTimestampSerializesAsRFC3339Nano(t *testing.T) {
	t.Parallel()

	rec := validRecord(t)
	encoded, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	want := `"timestamp":"2026-08-18T13:45:12.123456789Z"`
	if !strings.Contains(string(encoded), want) {
		t.Fatalf("marshaled record does not contain %s: %s", want, encoded)
	}
}

func TestAppendAfterCloseFails(t *testing.T) {
	t.Parallel()

	led, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := led.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if err := led.Append(validRecord(t)); err == nil {
		t.Fatal("Append on a closed ledger returned nil error; want failure")
	}
}

func TestScanAfterCloseFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led, err := ledger.Open(dir)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := led.Append(validRecord(t)); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if err := led.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if _, err := led.Scan(testDay); err == nil {
		t.Fatal("Scan on a closed ledger returned nil error; want failure")
	}
}

func TestScanReportsUnreadableDayFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)
	// A directory where the day file belongs: opening succeeds, reading does
	// not. The read failure must surface rather than pass as an empty day.
	if err := os.Mkdir(filepath.Join(dir, testDayFile), 0o700); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}

	if _, err := led.Scan(testDay); err == nil {
		t.Fatal("Scan of an unreadable day file returned nil error; want failure")
	}
}

func appendRaw(t *testing.T, path, text string) {
	t.Helper()

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, testFileMode)
	if err != nil {
		t.Fatalf("open %s for raw append: %v", path, err)
	}
	if _, err := file.WriteString(text); err != nil {
		t.Fatalf("raw append to %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s after raw append: %v", path, err)
	}
}

// TestScanReadsRecordsWrittenBeforeContexts is the compatibility guarantee for
// the ledger files already on disk across the fleet: they carry neither
// attribution field, and reading our own history must not report them as
// corruption.
func TestScanReadsRecordsWrittenBeforeContexts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	const legacy = `{"timestamp":"2026-02-03T13:45:12.123456789Z","host":"gnomon","alias":"bedrock",` +
		`"upstream":"https://api.anthropic.com","model":"claude-fable-5","request_model":"claude-fable-5",` +
		`"auth_mode":"inject","status":200,"input_tokens":1234,"output_tokens":567,"duration_ms":8910,` +
		`"upstream_ms":120,"block_shape":["thinking","text"],"drop_signature":false,"stop_reason":"end_turn"}` +
		"\n"

	if err := os.WriteFile(filepath.Join(dir, testDayFile), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy ledger file: %v", err)
	}

	led := openLedger(t, dir)

	records, err := led.Scan(testDay)
	if err != nil {
		t.Fatalf("Scan of a pre-contexts ledger file returned error: %v", err)
	}

	if len(records) != 1 {
		t.Fatalf("Scan returned %d records; want the one legacy record", len(records))
	}

	if records[0].Context != "" || records[0].CredentialClass != "" {
		t.Fatalf("legacy record = %+v; want both attribution fields empty", records[0])
	}

	if records[0].Model != "claude-fable-5" {
		t.Fatalf("legacy record model = %q; want the value it was written with", records[0].Model)
	}
}
