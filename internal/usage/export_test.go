package usage

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/accounting"
)

func TestExportIsDeterministicAndComplete(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, source)
	if err := store.LoadRateCards(t.Context(), []RateCard{{
		Model:                            "test-model",
		EffectiveFrom:                    time.Unix(1_776_912_345, 0).UTC(),
		InputNanoUSDPerMillion:           accounting.NanoUSD(1),
		OutputNanoUSDPerMillion:          accounting.NanoUSD(2),
		CacheReadNanoUSDPerMillion:       accounting.NanoUSD(3),
		CacheCreationNanoUSDPerMillion:   accounting.NanoUSD(4),
		CacheCreation5mNanoUSDPerMillion: accounting.NanoUSD(5),
		CacheCreation1hNanoUSDPerMillion: accounting.NanoUSD(6),
		Source:                           "test",
		CreatedAt:                        time.Unix(1_776_912_346, 0).UTC(),
	}}); err != nil {
		t.Fatalf("seed rate card: %v", err)
	}
	id, err := store.InsertPending(t.Context(), validPendingRequest())
	if err != nil {
		t.Fatalf("seed pending request: %v", err)
	}
	const target = "arn:aws:bedrock:us-east-2:123:application-inference-profile/export"
	if err := store.MarkUpstreamDispatched(
		t.Context(),
		id,
		validPendingRequest().StartedAt.Add(time.Millisecond),
		target,
	); err != nil {
		t.Fatalf("mark dispatched request: %v", err)
	}
	undispatchedID, err := store.InsertPending(t.Context(), validPendingRequest())
	if err != nil {
		t.Fatalf("seed undispatched request: %v", err)
	}

	first := filepath.Join(t.TempDir(), "first.jsonl")
	second := filepath.Join(t.TempDir(), "second.jsonl")
	if err := Export(t.Context(), source, first); err != nil {
		t.Fatalf("first Export() error = %v", err)
	}
	if err := Export(t.Context(), source, second); err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	assertMode(t, first, 0o600)
	one, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != string(two) {
		t.Fatalf("exports differ:\nfirst: %s\nsecond: %s", one, two)
	}

	lines := exportLines(t, first)
	if len(lines) != 4 {
		t.Fatalf("export lines = %d; want header, rate card, and two accounting rows", len(lines))
	}
	var header struct {
		PatchbayUsageExport int `json:"patchbay_usage_export"`
		SchemaVersion       int `json:"schema_version"`
	}
	if err := json.Unmarshal(lines[0], &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.PatchbayUsageExport != 1 || header.SchemaVersion != upstreamTargetDispatchEvidenceMigrationVersion {
		t.Fatalf("header = %+v; want export 1 and schema %d", header, upstreamTargetDispatchEvidenceMigrationVersion)
	}

	var rateCardRow map[string]any
	var accountingRows []map[string]any
	for _, line := range lines[1:] {
		var record struct {
			Table string         `json:"table"`
			Row   map[string]any `json:"row"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode row %q: %v", line, err)
		}
		switch record.Table {
		case "rate_cards":
			rateCardRow = record.Row
		case "accounting_requests":
			accountingRows = append(accountingRows, record.Row)
		default:
			t.Fatalf("unexpected export table %q", record.Table)
		}
	}
	rateCardColumns := exportColumnNames(t, store.db, "rate_cards")
	gotRateCardColumns := mapKeys(rateCardRow)
	if !reflect.DeepEqual(rateCardColumns, gotRateCardColumns) {
		t.Fatalf("rate_cards export fields = %v; want every schema column %v", gotRateCardColumns, rateCardColumns)
	}
	if len(accountingRows) != 2 {
		t.Fatalf("accounting export rows = %d; want 2", len(accountingRows))
	}
	for _, row := range accountingRows {
		wantColumns := exportColumnNames(t, store.db, "accounting_requests")
		gotColumns := mapKeys(row)
		if !reflect.DeepEqual(wantColumns, gotColumns) {
			t.Fatalf("accounting_requests export fields = %v; want every schema column %v", gotColumns, wantColumns)
		}
	}
	dispatched := accountingRows[0]
	undispatched := accountingRows[1]
	if dispatched["id"] != float64(id) || undispatched["id"] != float64(undispatchedID) {
		t.Fatalf(
			"accounting export IDs = %#v, %#v; want %d, %d",
			dispatched["id"],
			undispatched["id"],
			id,
			undispatchedID,
		)
	}
	if got := dispatched["upstream_target"]; got != target {
		t.Fatalf("dispatched upstream_target = %#v; want %q", got, target)
	}
	if dispatched["upstream_dispatched_at"] == nil {
		t.Fatal("dispatched upstream_dispatched_at = JSON null; want timestamp")
	}
	// route_alias is set before contact on every row this build writes: it
	// carries the Seat the request rides, which is known at selection. The
	// column stays nullable for imported v1 history, where an empty alias meant
	// the built-in default route.
	if got := undispatched["route_alias"]; got != defaultSeatID {
		t.Fatalf("undispatched route_alias = %#v; want %q", got, defaultSeatID)
	}
	for _, column := range []string{
		"completed_at", "upstream_dispatched_at", "upstream_target", "outcome", "response_id", "http_status", "normalization_complete", "input_tokens", "output_tokens",
		"cache_creation_input_tokens", "cache_read_input_tokens", "cache_creation_5m_input_tokens",
		"cache_creation_1h_input_tokens", "provider_cost_present", "provider_cost_valid",
		"provider_cost_nano_usd", "stop_reason", "block_shape", "duration_ms", "upstream_ms",
		"cost_basis", "final_cost_nano_usd", "rate_card_id",
	} {
		if undispatched[column] != nil {
			t.Fatalf("undispatched %s = %#v; want JSON null", column, undispatched[column])
		}
	}
}

func TestExportRefusesExistingDestinationAndLeavesNoPartial(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	failedAfterTemp := filepath.Join(t.TempDir(), "failed.jsonl")
	exportErr := Export(t.Context(), missing, failedAfterTemp)
	if exportErr == nil || !strings.Contains(exportErr.Error(), "does not exist") {
		t.Fatalf("Export(missing source) error = %v; want source refusal", exportErr)
	}
	partials, err := filepath.Glob(failedAfterTemp + ".partial-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("partial exports = %v; want none", partials)
	}

	source := filepath.Join(t.TempDir(), "usage.sqlite")
	_ = openStore(t, source)
	if err := os.Chmod(source, 0o644); err != nil {
		t.Fatal(err)
	}

	existing := filepath.Join(t.TempDir(), "existing.jsonl")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Export(t.Context(), source, existing); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Export(existing destination) error = %v; want destination refusal", err)
	}
	contents, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "keep" {
		t.Fatalf("existing destination = %q; want untouched contents", contents)
	}
	assertMode(t, source, 0o644)

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	failed := filepath.Join(blocker, "export.jsonl")
	exportErr = Export(t.Context(), source, failed)
	if exportErr == nil || !strings.Contains(exportErr.Error(), "destination directory") {
		t.Fatalf("Export(blocked destination) error = %v; want destination failure", exportErr)
	}
	partials, err = filepath.Glob(filepath.Join(filepath.Dir(failed), filepath.Base(failed)+".partial-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("partial exports = %v; want none", partials)
	}
}

func TestExportFailureAfterWritingPartialLeavesNoOutput(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	store, openErr := Open(source)
	if openErr != nil {
		t.Fatalf("open source: %v", openErr)
	}
	if closeErr := store.Close(); closeErr != nil {
		t.Fatalf("close source: %v", closeErr)
	}
	db, openErr := sql.Open("sqlite", source)
	if openErr != nil {
		t.Fatal(openErr)
	}
	if _, execErr := db.Exec(`DROP TABLE rate_cards`); execErr != nil {
		t.Fatal(execErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	dest := filepath.Join(t.TempDir(), "failed.jsonl")
	exportErr := Export(t.Context(), source, dest)
	if exportErr == nil || !strings.Contains(exportErr.Error(), "rate cards") {
		t.Fatalf("Export() error = %v; want rate-card failure after opening the temporary output", exportErr)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("failed export destination stat error = %v; want absent destination", statErr)
	}
	partials, globErr := filepath.Glob(dest + ".partial-*")
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(partials) != 0 {
		t.Fatalf("partial exports = %v; want none", partials)
	}
}

func exportLines(t *testing.T, path string) [][]byte {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var lines [][]byte
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, append([]byte(nil), scanner.Bytes()...))
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func exportColumnNames(t *testing.T, db queryer, table string) []string {
	t.Helper()

	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var cid int
		var name, typeName string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typeName, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	return names
}

type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

func mapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
