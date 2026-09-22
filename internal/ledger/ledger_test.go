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

	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/registry"
)

const (
	testDay      = "2026-08-18"
	testDayFile  = "patchbay-v2-2026-08-18.jsonl"
	nextDay      = "2026-08-19"
	nextDayFile  = "patchbay-v2-2026-08-19.jsonl"
	testFileMode = 0o600
	errorLimit   = 4096
)

func testTimestamp(t *testing.T) time.Time {
	t.Helper()
	return dayTimestamp(t, testDay)
}

func dayTimestamp(t *testing.T, day string) time.Time {
	t.Helper()
	when, err := time.Parse(time.RFC3339Nano, day+"T13:45:12.123456789Z")
	if err != nil {
		t.Fatal(err)
	}
	return when
}

// appendRaw writes text to an existing ledger file behind the ledger's back,
// which is how a corrupt, torn or hand-edited line gets into a scan.
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

func model(value string) *string { return &value }

func validRecord(t *testing.T) ledger.Record {
	t.Helper()
	return ledger.Record{
		Timestamp:       testTimestamp(t),
		Host:            "gnomon",
		Context:         "personal",
		CredentialClass: billing.CredentialOAuth,
		Selection:       &ledger.Selection{Kind: ledger.SelectionModel, Model: model("luna-fast")},
		Seat:            "luna",
		Upstream:        "https://openrouter.ai/api",
		Model:           model("openai/gpt-6-luna"),
		RequestModel:    model("luna-fast"),
		AuthMode:        "inject",
		ConfigSource:    "OPENROUTER_API_KEY_FILE",
		Status:          200,
		InputTokens:     11,
		OutputTokens:    22,
		DurationMS:      33,
		UpstreamMS:      4,
		BlockShape:      []string{"thinking", "text"},
		StopReason:      "end_turn",
	}
}

// preSelectionRecord is the reduced shape a request refused before Seat
// selection leaves behind.
func preSelectionRecord(t *testing.T) ledger.Record {
	t.Helper()
	return ledger.Record{
		Timestamp:       testTimestamp(t),
		Host:            "gnomon",
		Context:         "personal",
		RequestModel:    model("claude-opus-5"),
		CredentialClass: billing.CredentialOAuth,
		Status:          404,
	}
}

// wireObject renders rec as the JSON object a ledger line carries, so a test can
// take one apart key by key.
func wireObject(t *testing.T, rec ledger.Record) map[string]json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	object := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("decode marshaled record %s: %v", encoded, err)
	}
	return object
}

// writeDayFile writes lines as the whole of the test day's file, which is how a
// hand-built wire record no producer would ever emit gets into a scan.
func writeDayFile(t *testing.T, dir string, object map[string]json.RawMessage) {
	t.Helper()
	line, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("encode wire object: %v", err)
	}
	writeRawDayFile(t, dir, string(line))
}

// writeRawDayFile writes line as the whole of the test day's file. A map cannot
// hold a repeated member, so a line that names one has to be written as text.
func writeRawDayFile(t *testing.T, dir, line string) {
	t.Helper()
	path := filepath.Join(dir, testDayFile)
	if err := os.WriteFile(path, []byte(line+"\n"), testFileMode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// scanOne scans the test day of a fresh ledger over dir and returns the single
// record it holds.
func scanOne(t *testing.T, dir string) ledger.Record {
	t.Helper()
	got, err := openLedger(t, dir).Scan(testDay)
	if err != nil {
		t.Fatalf("Scan = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Scan returned %d records; want 1", len(got))
	}
	return got[0]
}

// scanRejects asserts the hand-built wire object is refused as corruption
// rather than read back as a record.
func scanRejects(t *testing.T, object map[string]json.RawMessage, why string) {
	t.Helper()
	dir := t.TempDir()
	writeDayFile(t, dir, object)
	got, err := openLedger(t, dir).Scan(testDay)
	var corrupt *ledger.CorruptionError
	if !errors.As(err, &corrupt) {
		t.Fatalf("Scan of %s = %+v, %v; want a *ledger.CorruptionError because %s", object, got, err, why)
	}
	if len(got) != 0 {
		t.Fatalf("Scan returned %d records past the damage; want none", len(got))
	}
}

// TestNullableModelsSeparateAbsenceFromEveryLiteral is the whole point of the
// nullable schema: a model is an opaque exact string, so no literal can stand in
// for a model that was not there.
//
// "(none)" used to be that literal, which made a Seat rewriting to nothing and a
// Seat rewriting to a model an operator genuinely named "(none)" persist as the
// same bytes — one record, two irreconcilable readings. Absence is JSON null
// now, which is not a string at all and so collides with no string.
func TestNullableModelsSeparateAbsenceFromEveryLiteral(t *testing.T) {
	t.Parallel()

	absent := validRecord(t)
	absent.Model = nil
	absent.RequestModel = nil
	absent.Selection = &ledger.Selection{Kind: ledger.SelectionDefault}

	named := validRecord(t)
	named.Model = model("(none)")
	named.RequestModel = model("(none)")
	named.Selection = &ledger.Selection{Kind: ledger.SelectionModel, Model: model("(none)")}

	for _, rec := range []ledger.Record{absent, named} {
		if err := rec.Validate(); err != nil {
			t.Fatalf("selected record %+v rejected: %v", rec, err)
		}
	}

	absentWire := wireObject(t, absent)
	namedWire := wireObject(t, named)

	for key, want := range map[string]string{"model": "null", "request_model": "null"} {
		if got, present := absentWire[key]; !present || string(got) != want {
			t.Errorf("exact absence encoded %s as %s (present=%v); want %s", key, got, present, want)
		}
	}
	for key, want := range map[string]string{"model": `"(none)"`, "request_model": `"(none)"`} {
		if got, present := namedWire[key]; !present || string(got) != want {
			t.Errorf("the literal selector encoded %s as %s (present=%v); want %s", key, got, present, want)
		}
	}

	dir := t.TempDir()
	led := openLedger(t, dir)
	for _, rec := range []ledger.Record{absent, named} {
		if err := led.Append(rec); err != nil {
			t.Fatalf("Append %+v = %v", rec, err)
		}
	}

	raw, err := os.ReadFile(filepath.Join(dir, testDayFile))
	if err != nil {
		t.Fatalf("read ledger file: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("ledger file has %d lines; want 2", len(lines))
	}
	if lines[0] == lines[1] {
		t.Fatalf("exact absence and the literal selector persisted identically: %s", lines[0])
	}

	got, err := led.Scan(testDay)
	if err != nil || len(got) != 2 {
		t.Fatalf("Scan = %+v, %v", got, err)
	}
	if !reflect.DeepEqual(got[0], absent) {
		t.Errorf("absence round trip\n got %+v\nwant %+v", got[0], absent)
	}
	if !reflect.DeepEqual(got[1], named) {
		t.Errorf("literal round trip\n got %+v\nwant %+v", got[1], named)
	}
	if got[0].Model != nil {
		t.Errorf("scanned absence model = %q; want nil", *got[0].Model)
	}
	if got[1].Model == nil || *got[1].Model != "(none)" {
		t.Errorf("scanned literal model = %v; want the exact string %q", got[1].Model, "(none)")
	}
}

// TestSelectedWireRequiresBothModelKeys pins the selected shape's requiredness
// to key presence rather than to a value. A reader that finds no model key
// cannot tell a record written before the field existed from one whose model was
// absent, so a line missing either key is damage, and explicit null — the one
// spelling of absence — is not.
func TestSelectedWireRequiresBothModelKeys(t *testing.T) {
	t.Parallel()

	rec := validRecord(t)
	rec.Model = nil
	rec.RequestModel = nil
	rec.Selection = &ledger.Selection{Kind: ledger.SelectionDefault}

	for _, missing := range []string{"model", "request_model"} {
		object := wireObject(t, rec)
		delete(object, missing)
		scanRejects(t, object, "a selected record omitted "+missing)
	}

	dir := t.TempDir()
	writeDayFile(t, dir, wireObject(t, rec))
	got := scanOne(t, dir)
	if got.Model != nil || got.RequestModel != nil {
		t.Fatalf("explicit nulls scanned as %v/%v; want absence for both", got.Model, got.RequestModel)
	}
	if !reflect.DeepEqual(got, rec) {
		t.Fatalf("explicit-null round trip\n got %+v\nwant %+v", got, rec)
	}
}

// TestPreSelectionWireOmitsModelAndSpellsAbsenceByOmission pins the reduced
// shape's own rules. A pre-selection record named no Seat, so it has no model to
// report at all — a model key on one, even a null one, describes a selection
// that did not happen. Its request model is spelled absent by omission, so an
// explicit null is a second spelling of the same thing and would not survive a
// round trip as itself.
func TestPreSelectionWireOmitsModelAndSpellsAbsenceByOmission(t *testing.T) {
	t.Parallel()

	rec := preSelectionRecord(t)
	object := wireObject(t, rec)
	if _, present := object["model"]; present {
		t.Fatalf("a pre-selection record encoded a model key: %s", object["model"])
	}
	if got := string(object["request_model"]); got != `"claude-opus-5"` {
		t.Fatalf("request_model encoded as %s; want the exact requested model", got)
	}

	for _, value := range []string{"null", `"openai/gpt-6-luna"`} {
		carrying := wireObject(t, rec)
		carrying["model"] = json.RawMessage(value)
		scanRejects(t, carrying, "a pre-selection record carried model "+value)
	}

	nulled := wireObject(t, rec)
	nulled["request_model"] = json.RawMessage("null")
	scanRejects(t, nulled, "a pre-selection record spelled an absent request_model as null")

	dir := t.TempDir()
	writeDayFile(t, dir, wireObject(t, rec))
	if got := scanOne(t, dir); !reflect.DeepEqual(got, rec) {
		t.Fatalf("present request_model round trip\n got %+v\nwant %+v", got, rec)
	}

	absent := preSelectionRecord(t)
	absent.RequestModel = nil
	omitted := wireObject(t, absent)
	if _, present := omitted["request_model"]; present {
		t.Fatalf("an absent request_model encoded a key: %s", omitted["request_model"])
	}

	dir = t.TempDir()
	writeDayFile(t, dir, omitted)
	if got := scanOne(t, dir); !reflect.DeepEqual(got, absent) {
		t.Fatalf("omitted request_model round trip\n got %+v\nwant %+v", got, absent)
	}
}

// TestNoncanonicalModelPointersAreRefused holds the in-memory shape to one
// spelling of absence too. A pointer to the empty string is a producer claiming
// a model that is not a model, and it would encode as `""` — a wire value no
// reader could tell from a Seat bound to the empty selector, which the registry
// does not admit.
func TestNoncanonicalModelPointersAreRefused(t *testing.T) {
	t.Parallel()

	for _, field := range []struct {
		name   string
		mutate func(*ledger.Record)
	}{
		{name: "model", mutate: func(r *ledger.Record) { r.Model = model("") }},
		{name: "request_model", mutate: func(r *ledger.Record) { r.RequestModel = model("") }},
	} {
		rec := validRecord(t)
		field.mutate(&rec)
		if err := rec.Validate(); err == nil {
			t.Errorf("selected record pointing %s at the empty string validated", field.name)
		}
	}

	pre := preSelectionRecord(t)
	pre.RequestModel = model("")
	if err := pre.Validate(); err == nil {
		t.Error("pre-selection record pointing request_model at the empty string validated")
	}

	pre = preSelectionRecord(t)
	pre.Model = model("openai/gpt-6-luna")
	if err := pre.Validate(); err == nil {
		t.Error("pre-selection record naming a model validated")
	}
}

// TestSanitizeRequestedContextBoundsWhatAPreSelectionRecordKeeps pins the one
// policy the proxy is allowed to borrow rather than reimplement.
//
// A requested Context is a path segment: caller-controlled text of any length
// and any bytes. The pre-selection record keeps it because it is the whole point
// of the record, but only while it stays a bounded printable name — and a
// producer that hands Append an unrepresentable one loses the entire record over
// a field that was never essential. Sanitizing first keeps the request recorded
// and drops only the name.
func TestSanitizeRequestedContextBoundsWhatAPreSelectionRecordKeeps(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "an ordinary requested name", value: "not-a-real-context", want: "not-a-real-context"},
		{name: "a name no registry admits", value: "Bad_Name", want: "Bad_Name"},
		{name: "printable punctuation", value: "not/a/context", want: "not/a/context"},
		{name: "at the bound", value: strings.Repeat("a", 256), want: strings.Repeat("a", 256)},
		{name: "one byte past the bound", value: strings.Repeat("a", 257), want: ""},
		{name: "already absent", value: "", want: ""},
		{name: "an embedded space", value: "spaced name", want: ""},
		{name: "an embedded tab", value: "tab\tname", want: ""},
		{name: "an embedded newline", value: "line\nname", want: ""},
		{name: "a delete byte", value: "del\x7fname", want: ""},
		{name: "a non-ascii byte", value: "café", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ledger.SanitizeRequestedContext(tc.value)
			if got != tc.want {
				t.Fatalf("SanitizeRequestedContext(%q) = %q; want %q", tc.value, got, tc.want)
			}

			// Whatever comes back is a Context a pre-selection record can hold, which
			// is the only promise a producer relies on.
			rec := preSelectionRecord(t)
			rec.Context = got
			if err := rec.Validate(); err != nil {
				t.Fatalf("sanitized context %q was still refused: %v", got, err)
			}
		})
	}
}

func openLedger(t *testing.T, dir string) *ledger.Ledger {
	t.Helper()
	led, err := ledger.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := led.Close(); err != nil {
			t.Error(err)
		}
	})
	return led
}

func TestAppendScanV2RoundTripAndFilename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	led := openLedger(t, dir)
	want := validRecord(t)
	want.Subagent = &ledger.Subagent{AgentID: "agent", ParentAgentID: "parent"}
	if err := led.Append(want); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, testDayFile)); err != nil {
		t.Fatalf("v2 file: %v", err)
	}
	if _, err := os.Stat(
		filepath.Join(dir, "patchbay-"+testDay+".jsonl"),
	); !errors.Is(
		err,
		os.ErrNotExist,
	) {
		t.Fatalf("v1 file exists: %v", err)
	}
	got, err := led.Scan(testDay)
	if err != nil || len(got) != 1 {
		t.Fatalf("Scan = %+v, %v", got, err)
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("round trip\n got %+v\nwant %+v", got[0], want)
	}
}

func TestScanReadsOnlyV2AndHasNoV1Decoder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacy := `{"timestamp":"2026-08-18T00:00:00Z","host":"gnomon","alias":"old","upstream":"u","model":"m","request_model":"m","auth_mode":"forward"}` + "\n"
	if err := os.WriteFile(
		filepath.Join(dir, "patchbay-"+testDay+".jsonl"),
		[]byte(legacy),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	led := openLedger(t, dir)
	got, err := led.Scan(testDay)
	if err != nil || len(got) != 0 {
		t.Fatalf("Scan legacy-only day = %+v, %v", got, err)
	}

	if err := os.WriteFile(filepath.Join(dir, testDayFile), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := led.Scan(testDay); err == nil {
		t.Fatal("v1-shaped line in v2 file was decoded")
	}
}

func TestSelectedAndPreselectionValidation(t *testing.T) {
	t.Parallel()
	selected := validRecord(t)
	if err := selected.Validate(); err != nil {
		t.Fatalf("selected Validate = %v", err)
	}

	pre := ledger.Record{
		Timestamp:       testTimestamp(t),
		Host:            "gnomon",
		Context:         "personal",
		RequestModel:    model("luna-fast"),
		CredentialClass: billing.CredentialOAuth,
		Status:          503,
	}
	if err := pre.Validate(); err != nil {
		t.Fatalf("preselection Validate = %v", err)
	}
	encoded, err := json.Marshal(pre)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"selection", "seat", "upstream", "model", "auth_mode", "config_source"} {
		if strings.Contains(string(encoded), `"`+absent+`"`) {
			t.Errorf("preselection record contains %s: %s", absent, encoded)
		}
	}

	for _, mutate := range []func(*ledger.Record){
		func(r *ledger.Record) { r.Context = "" }, func(r *ledger.Record) { r.Selection = nil },
		func(r *ledger.Record) { r.Seat = "" },
		func(r *ledger.Record) { r.Upstream = "" }, func(r *ledger.Record) { r.Model = model("") },
		func(r *ledger.Record) { r.RequestModel = model("") },
		func(r *ledger.Record) { r.AuthMode = "" }, func(r *ledger.Record) { r.ConfigSource = "" },
	} {
		rec := validRecord(t)
		mutate(&rec)
		if err := rec.Validate(); err == nil {
			t.Errorf("selected mutation validated: %+v", rec)
		}
	}
}

func TestSelectionVocabularyAndModelNullability(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind  string
		model *string
		valid bool
	}{
		{ledger.SelectionModel, model("public"), true},
		{ledger.SelectionSubagent, model("requested"), true},
		// An unnamed marked-subagent fallback is a subagent selection naming no
		// model: nil is the distinction between it and an exact marked hit.
		{ledger.SelectionSubagent, nil, true},
		{ledger.SelectionDefault, nil, true},
		{ledger.SelectionModel, nil, false},
		{ledger.SelectionModel, model(""), false},
		{ledger.SelectionSubagent, model(""), false},
		{"route", model("x"), false}, {ledger.SelectionDefault, model("x"), false},
	} {
		rec := validRecord(t)
		rec.Selection = &ledger.Selection{Kind: tc.kind, Model: tc.model}
		if got := rec.Validate() == nil; got != tc.valid {
			t.Errorf("selection %+v valid=%v", rec.Selection, got)
		}
	}
}

// TestSelectedModelsAreOpaqueExactStrings splits the two nonempty rules a
// selected record carries, which are not the same rule.
//
// A model selector is opaque: the registry binds the exact bytes an operator
// wrote and a request names the exact bytes back, with no trimming, folding or
// normalization anywhere between. A selector that is nothing but spaces is a
// strange thing to configure and a perfectly valid one, so the record that
// stores it has to keep it — and it has to survive the round trip through the
// file as the same bytes, since no literal stands in for absence any more.
//
// Everything else a selected record names is patchbay's own vocabulary, held to
// its own grammar and still refused when it is blank.
func TestSelectedModelsAreOpaqueExactStrings(t *testing.T) {
	t.Parallel()

	for _, opaque := range []string{" ", "   ", "\t", "\n", " luna-fast ", "(none)", "-", "null"} {
		rec := validRecord(t)
		rec.Model = model(opaque)
		rec.RequestModel = model(opaque)
		rec.Selection = &ledger.Selection{Kind: ledger.SelectionModel, Model: model(opaque)}
		if err := rec.Validate(); err != nil {
			t.Errorf("selected record naming the opaque model %q rejected: %v", opaque, err)
			continue
		}

		dir := t.TempDir()
		led := openLedger(t, dir)
		if err := led.Append(rec); err != nil {
			t.Errorf("Append opaque model %q = %v", opaque, err)
			continue
		}
		got, err := led.Scan(testDay)
		if err != nil || len(got) != 1 {
			t.Errorf("Scan opaque model %q = %+v, %v", opaque, got, err)
			continue
		}
		if got[0].Model == nil || *got[0].Model != opaque ||
			got[0].RequestModel == nil || *got[0].RequestModel != opaque {
			t.Errorf("opaque model %q round tripped as %v/%v", opaque, got[0].Model, got[0].RequestModel)
		}
	}

	for _, field := range []struct {
		name   string
		mutate func(*ledger.Record, string)
	}{
		{name: "seat", mutate: func(r *ledger.Record, v string) { r.Seat = v }},
		{name: "upstream", mutate: func(r *ledger.Record, v string) { r.Upstream = v }},
		{name: "auth_mode", mutate: func(r *ledger.Record, v string) { r.AuthMode = v }},
		{name: "config_source", mutate: func(r *ledger.Record, v string) { r.ConfigSource = v }},
	} {
		for _, blank := range []string{"", " ", "\t"} {
			rec := validRecord(t)
			field.mutate(&rec, blank)
			if err := rec.Validate(); err == nil {
				t.Errorf("selected record with blank %s %q validated", field.name, blank)
			}
		}
	}
}

func TestCredentialSourcesAreNonSecretDescriptors(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"caller", "OPENROUTER_API_KEY_FILE", "_key9", "aws_profile:work"} {
		rec := validRecord(t)
		rec.ConfigSource = source
		if err := rec.Validate(); err != nil {
			t.Errorf("source %q: %v", source, err)
		}
	}
	for _, source := range []string{"", "Bearer secret", "9KEY", "KEY-FILE", "aws_profile:", "aws_profile:work/evil"} {
		rec := validRecord(t)
		rec.ConfigSource = source
		if err := rec.Validate(); err == nil {
			t.Errorf("source %q accepted", source)
		}
	}
}

// TestConfigSourceEnvNamesFollowTheRegistryGrammar pins the ledger to the same
// exported environment-variable grammar registry admission uses. An inject Seat
// the registry loads must produce a config_source the ledger stores, and one
// copy of the rule is the only way that stays true.
func TestConfigSourceEnvNamesFollowTheRegistryGrammar(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"KEY", "_key", "PATCHBAY_OPENROUTER_KEY_FILE9", "9KEY", "KEY-FILE", "KEY FILE", "Bearer secret"} {
		rec := validRecord(t)
		rec.ConfigSource = name
		accepted := rec.Validate() == nil
		if admitted := registry.ValidEnvName(name) == nil; accepted != admitted {
			t.Errorf("config_source %q accepted=%v; registry admits=%v", name, accepted, admitted)
		}
	}
}

// TestPreSelectionRecordsKeepAnInvalidRequestedContext pins the pre-selection
// half of the schema: a request naming a Context no registry can produce is
// exactly the failure worth recording, so the requested name is retained rather
// than judged against the configured-ID grammar. Selected records stay strict.
func TestPreSelectionRecordsKeepAnInvalidRequestedContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	led := openLedger(t, dir)

	for _, name := range []string{"Bad_Name", "not/a/context", "UPPER", strings.Repeat("a", 256)} {
		rec := ledger.Record{
			Timestamp:       testTimestamp(t),
			Host:            "gnomon",
			Context:         name,
			RequestModel:    model("claude-opus-5"),
			CredentialClass: billing.CredentialNone,
			Status:          404,
		}
		if err := rec.Validate(); err != nil {
			t.Errorf("pre-selection context %q rejected: %v", name, err)
			continue
		}
		if err := led.Append(rec); err != nil {
			t.Errorf("Append pre-selection context %q = %v", name, err)
		}
	}

	got, err := led.Scan(testDay)
	if err != nil {
		t.Fatalf("Scan = %v", err)
	}
	if len(got) != 4 || got[0].Context != "Bad_Name" {
		t.Fatalf("scanned %d records: %+v", len(got), got)
	}

	// The bound is real: an unbounded or non-printable requested name is still
	// refused rather than written to a file that gets shipped off the machine.
	for _, name := range []string{strings.Repeat("a", 257), "spaced name", "tab\tname"} {
		rec := ledger.Record{
			Timestamp:       testTimestamp(t),
			Host:            "gnomon",
			CredentialClass: billing.CredentialNone,
			Context:         name,
			Status:          404,
		}
		if err := rec.Validate(); err == nil {
			t.Errorf("unbounded pre-selection context %q accepted", name)
		}
	}

	// A selected record still has to name a Context the registry could produce.
	selected := validRecord(t)
	selected.Context = "Bad_Name"
	if err := selected.Validate(); err == nil {
		t.Error("selected record accepted an invalid context ID")
	}
}

// TestSelectedSeatIDsFollowTheRegistryGrammar pins the Seat half of a selected
// record to the same exported ID grammar its Context is held to. Both names are
// registry IDs — the record is the pair the registry itself produced — so a
// stored Seat that could never have been loaded is a record naming something
// that does not exist.
func TestSelectedSeatIDsFollowTheRegistryGrammar(t *testing.T) {
	t.Parallel()
	for _, seat := range []string{
		"luna", "bedrock", "seat-1", "s9",
		"BAD/ID", "UPPER", "bad_seat", "-leading-dash", "not a seat", "seat.1", "",
	} {
		rec := validRecord(t)
		rec.Seat = seat
		accepted := rec.Validate() == nil
		if admitted := registry.ValidID(seat) == nil; accepted != admitted {
			t.Errorf("selected seat %q accepted=%v; registry admits=%v", seat, accepted, admitted)
		}
	}

	// The pre-selection shape is untouched: it names no Seat at all, and the
	// requested Context it does keep stays bounded opaque input.
	pre := ledger.Record{
		Timestamp:       testTimestamp(t),
		Host:            "gnomon",
		Context:         "not/a/context",
		RequestModel:    model("claude-opus-5"),
		CredentialClass: billing.CredentialNone,
		Status:          404,
	}
	if err := pre.Validate(); err != nil {
		t.Fatalf("pre-selection Validate = %v; want it accepted", err)
	}
}

func TestRecordJSONNamesV2RoutingFields(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(validRecord(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"context", "selection", "seat", "upstream", "model", "request_model", "auth_mode", "credential_class", "config_source"} {
		if !strings.Contains(string(encoded), `"`+name+`"`) {
			t.Errorf("missing %s: %s", name, encoded)
		}
	}
	for _, legacy := range []string{"alias", "route", "default_route"} {
		if strings.Contains(string(encoded), `"`+legacy+`"`) {
			t.Errorf("legacy %s present: %s", legacy, encoded)
		}
	}
}

// TestAppendWritesOneLinePerRecord pins the JSONL shape: one record, one line,
// newline-terminated. A shipped ledger is read line by line by tools that are
// not this one.
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
	for index, line := range lines {
		if strings.TrimSpace(line) == "" {
			t.Fatalf("line %d is blank; want one JSON object per line", index)
		}
	}
}

// TestTimestampSerializesAsRFC3339Nano pins the timestamp encoding. Records are
// shipped off the machine and correlated with other logs; the nanosecond form is
// the only one that survives that intact.
func TestTimestampSerializesAsRFC3339Nano(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(validRecord(t))
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	const want = `"timestamp":"2026-08-18T13:45:12.123456789Z"`
	if !strings.Contains(string(encoded), want) {
		t.Fatalf("marshaled record does not contain %s: %s", want, encoded)
	}
}

// TestAppendRotatesByUTCDay pins rotation: the day file is chosen from the
// record's own timestamp at append time, with nothing running in the background.
func TestAppendRotatesByUTCDay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	led := openLedger(t, dir)

	today := validRecord(t)
	tomorrow := validRecord(t)
	tomorrow.Timestamp = dayTimestamp(t, nextDay)

	for _, rec := range []ledger.Record{today, tomorrow} {
		if err := led.Append(rec); err != nil {
			t.Fatalf("Append returned error: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read ledger dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want := []string{testDayFile, nextDayFile}; !reflect.DeepEqual(names, want) {
		t.Fatalf("ledger dir contains %v; want %v", names, want)
	}

	for day, want := range map[string]ledger.Record{testDay: today, nextDay: tomorrow} {
		got, scanErr := led.Scan(day)
		if scanErr != nil {
			t.Fatalf("Scan(%q) returned error: %v", day, scanErr)
		}
		if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("Scan(%q) = %+v; want %+v", day, got, want)
		}
	}
}

// TestAppendUsesUTCDayForLocalTimestamps pins the day to UTC rather than the
// timestamp's own zone, so a fleet in several zones produces one comparable set
// of files.
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
	if len(got) != 1 || !got[0].Timestamp.Equal(rec.Timestamp) {
		t.Fatalf("Scan(%q) = %+v; want the record as written", testDay, got)
	}
}

// TestLedgerPermissionsAreOwnerOnly pins both modes. The records name every model
// a machine used and when, which is nobody else's business on a shared host.
func TestLedgerPermissionsAreOwnerOnly(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "nested", "ledger")
	led := openLedger(t, dir)

	if err := led.Append(validRecord(t)); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat ledger dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("ledger dir mode = %o; want 700", perm)
	}

	fileInfo, err := os.Stat(filepath.Join(dir, testDayFile))
	if err != nil {
		t.Fatalf("stat ledger file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != testFileMode {
		t.Errorf("ledger file mode = %o; want 600", perm)
	}
}

// TestOpenRejectsNonDirectory keeps a misconfigured ledger path from being
// mistaken for a working one.
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

// TestAppendRejectsInvalidRecordBeforeIO pins validation ahead of the write, so
// a rejected record leaves the ledger untouched rather than creating an empty
// day file nobody asked for.
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

// TestAppendAndScanAfterCloseFail pin the closed ledger. A host process that
// closed the directory handle must be told, not silently written past.
func TestAppendAndScanAfterCloseFail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	led, err := ledger.Open(dir)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if appendErr := led.Append(validRecord(t)); appendErr != nil {
		t.Fatalf("Append returned error: %v", appendErr)
	}
	if closeErr := led.Close(); closeErr != nil {
		t.Fatalf("Close returned error: %v", closeErr)
	}

	if appendErr := led.Append(validRecord(t)); appendErr == nil {
		t.Error("Append on a closed ledger returned nil error; want failure")
	}
	if _, scanErr := led.Scan(testDay); scanErr == nil {
		t.Error("Scan on a closed ledger returned nil error; want failure")
	}
}

// TestScanMissingDayReturnsEmpty pins the one absence that is not an error: a
// day nothing was written on.
func TestScanMissingDayReturnsEmpty(t *testing.T) {
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

// TestScanReportsUnreadableDayFile pins the absence that IS an error: a day file
// that exists and cannot be read must never pass as an empty day.
func TestScanReportsUnreadableDayFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	led := openLedger(t, dir)
	// A directory where the day file belongs: opening succeeds, reading does not.
	if err := os.Mkdir(filepath.Join(dir, testDayFile), 0o700); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}

	if _, err := led.Scan(testDay); err == nil {
		t.Fatal("Scan of an unreadable day file returned nil error; want failure")
	}
}

// TestScanSurfacesDamageWithItsOffset covers the three ways a line can be
// unreadable. Each one stops the scan, returns the records read before it, and
// names the byte offset — damage is visible rather than silently skipped.
func TestScanSurfacesDamageWithItsOffset(t *testing.T) {
	t.Parallel()
	// Well-formed JSON in the v2 schema that is not a well-formed record: the
	// host is empty, which Validate refuses.
	const invalidRecordLine = `{"timestamp":"2026-08-18T13:45:12Z","host":"",` +
		`"credential_class":"none","status":0,"input_tokens":0,"output_tokens":0,` +
		`"duration_ms":0,"upstream_ms":0,"block_shape":[],"drop_signature":false}`

	for _, tc := range []struct {
		name   string
		damage string
	}{
		{name: "not json", damage: "this is not json\n"},
		{name: "invalid record", damage: invalidRecordLine + "\n"},
		{name: "torn final line", damage: `{"timestamp":"2026-08-18T13:4`},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			appendRaw(t, path, tc.damage)

			got, scanErr := led.Scan(testDay)
			if len(got) != 1 {
				t.Fatalf("Scan returned %d records; want the 1 valid record before the damage", len(got))
			}
			var corrupt *ledger.CorruptionError
			if !errors.As(scanErr, &corrupt) {
				t.Fatalf("Scan error = %v; want a *ledger.CorruptionError", scanErr)
			}
			if corrupt.Offset != int64(len(valid)) {
				t.Fatalf("CorruptionError.Offset = %d; want %d", corrupt.Offset, len(valid))
			}
			if corrupt.Err == nil || corrupt.Error() == "" {
				t.Fatalf("CorruptionError does not describe the failure: %+v", corrupt)
			}
		})
	}
}

// TestScanTornFinalLineUnwrapsUnexpectedEOF separates a torn write from other
// damage: the last line has no newline, which is what a process killed mid-write
// leaves behind.
func TestScanTornFinalLineUnwrapsUnexpectedEOF(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	led := openLedger(t, dir)
	for range 2 {
		if err := led.Append(validRecord(t)); err != nil {
			t.Fatalf("Append returned error: %v", err)
		}
	}
	path := filepath.Join(dir, testDayFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger file: %v", err)
	}
	firstLineLen := strings.Index(string(raw), "\n") + 1
	if err := os.WriteFile(path, raw[:len(raw)-8], testFileMode); err != nil {
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

// TestScanSkipsNothingSilently pins the scan stopping AT the damage rather than
// stepping over it: a reader must never be handed a partial day that looks whole.
func TestScanSkipsNothingSilently(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	led := openLedger(t, dir)

	if err := led.Append(validRecord(t)); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	appendRaw(t, filepath.Join(dir, testDayFile), "garbage\n")
	after := validRecord(t)
	after.RequestModel = model("after-corruption")
	after.Selection = &ledger.Selection{Kind: ledger.SelectionModel, Model: model("after-corruption")}
	if err := led.Append(after); err != nil {
		t.Fatalf("Append after corruption returned error: %v", err)
	}

	got, scanErr := led.Scan(testDay)
	if scanErr == nil {
		t.Fatal("Scan returned nil error over a corrupt line; corruption must never be silent")
	}
	if len(got) != 1 {
		t.Fatalf("Scan returned %d records; want 1 (the scan stops at the damage)", len(got))
	}
}

// insertMember returns rec's wire line with member spliced in immediately
// before anchor, which is how a repeated member gets into a line no encoder and
// no map could produce.
func insertMember(t *testing.T, rec ledger.Record, anchor, member string) string {
	t.Helper()
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	text := string(line)
	if count := strings.Count(text, anchor); count != 1 {
		t.Fatalf("anchor %q appears %d times in %s; want exactly once", anchor, count, text)
	}
	return strings.Replace(text, anchor, member+","+anchor, 1)
}

// appendMember returns rec's wire line with member added as its last one, so
// the repeat is the value ordinary decoding would keep.
func appendMember(t *testing.T, rec ledger.Record, member string) string {
	t.Helper()
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	text := string(line)
	if !strings.HasSuffix(text, "}") {
		t.Fatalf("marshaled record %s does not end an object", text)
	}
	return text[:len(text)-1] + "," + member + "}"
}

// scanRejectsLine asserts the hand-written line is refused as corruption by an
// error naming each of wants.
func scanRejectsLine(t *testing.T, line string, wants ...string) {
	t.Helper()
	dir := t.TempDir()
	writeRawDayFile(t, dir, line)
	got, err := openLedger(t, dir).Scan(testDay)
	var corrupt *ledger.CorruptionError
	if !errors.As(err, &corrupt) {
		t.Fatalf("Scan of %s = %+v, %v; want a *ledger.CorruptionError", line, got, err)
	}
	if len(got) != 0 {
		t.Fatalf("Scan returned %d records past the damage; want none", len(got))
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Scan error %q does not name %q", err, want)
		}
	}
}

// TestScanRefusesRepeatedMembers covers the damage a decoder is happy to read.
// JSON member names are case-sensitive and a repeated one is ambiguous, but
// encoding/json keeps whichever came last: the line says two things and the
// record carries one, with the evidence gone by the time a struct exists. A
// repeat is refused wherever it sits — at the root, or inside the selection and
// subagent objects a record nests.
func TestScanRefusesRepeatedMembers(t *testing.T) {
	t.Parallel()

	marked := validRecord(t)
	marked.Subagent = &ledger.Subagent{AgentID: "agent-01"}

	for _, tc := range []struct {
		name, line, member string
	}{
		{
			name:   "a repeated model, the repeat last",
			line:   appendMember(t, validRecord(t), `"model":"decoy-last"`),
			member: "model",
		},
		{
			name:   "a repeated model, the repeat first",
			line:   insertMember(t, validRecord(t), `"timestamp":`, `"model":"decoy-first"`),
			member: "model",
		},
		{
			name:   "a repeated ordinary key",
			line:   appendMember(t, validRecord(t), `"host":"decoy-host"`),
			member: "host",
		},
		{
			name:   "a repeated nested selection kind",
			line:   insertMember(t, validRecord(t), `"kind":`, `"kind":"default"`),
			member: "kind",
		},
		{
			name:   "a repeated nested subagent agent_id",
			line:   insertMember(t, marked, `"agent_id":`, `"agent_id":"decoy-agent"`),
			member: "agent_id",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scanRejectsLine(t, tc.line, "duplicate", tc.member)
		})
	}
}

// TestScanRefusesALineCarryingMoreThanOneValue pins the other half of reading
// raw bytes: a line is one record, so anything after the record it holds is
// damage. A second object, a stray scalar and bytes that are not JSON at all
// are each refused rather than read as the record they follow.
func TestScanRefusesALineCarryingMoreThanOneValue(t *testing.T) {
	t.Parallel()

	line, err := json.Marshal(validRecord(t))
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}

	for _, tc := range []struct{ name, trailing string }{
		{name: "a second object", trailing: ` {"host":"decoy"}`},
		{name: "a stray scalar", trailing: ` 7`},
		{name: "bytes that are not JSON", trailing: ` garbage`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scanRejectsLine(t, string(line)+tc.trailing)
		})
	}
}

// TestScanKeepsNoValueFromARepeatedMember pins what the refusal is for: the
// later value must not reach a reader as the record's own. Scanning the same
// line with the repeat removed is what proves the record was otherwise fine —
// the repeat is the whole of the damage.
func TestScanKeepsNoValueFromARepeatedMember(t *testing.T) {
	t.Parallel()

	rec := validRecord(t)
	rec.Model = model("openai/gpt-6-luna")

	dir := t.TempDir()
	writeRawDayFile(t, dir, appendMember(t, rec, `"model":"decoy-last"`))
	got, err := openLedger(t, dir).Scan(testDay)
	if err == nil {
		t.Fatalf("Scan of a repeated model returned %+v and no error", got)
	}
	for _, rec := range got {
		if rec.Model != nil && *rec.Model == "decoy-last" {
			t.Fatal("Scan handed back the later value of a repeated member")
		}
	}

	clean := t.TempDir()
	writeDayFile(t, clean, wireObject(t, rec))
	if scanned := scanOne(t, clean); !reflect.DeepEqual(scanned, rec) {
		t.Fatalf("the same record without the repeat\n got %+v\nwant %+v", scanned, rec)
	}
}

// TestErrorBodyTruncation pins the one field that carries an upstream's own
// bytes: it is capped, the cut never splits a rune, and a body under the limit
// is stored exactly as it arrived.
func TestErrorBodyTruncation(t *testing.T) {
	t.Parallel()
	const shortBody = `{"type":"error","error":{"type":"invalid_request_error"}}`
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "over the limit", body: strings.Repeat("e", 5000)},
		// Three-byte runes do not divide the limit evenly, so a naive byte cut
		// would split the rune straddling it.
		{name: "multibyte over the limit", body: strings.Repeat("は", 2000)},
		{name: "under the limit", body: shortBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			led := openLedger(t, dir)
			rec := validRecord(t)
			rec.Status = 500
			rec.ErrorBody = tc.body
			if err := led.Append(rec); err != nil {
				t.Fatalf("Append returned error: %v", err)
			}

			got, err := led.Scan(testDay)
			if err != nil || len(got) != 1 {
				t.Fatalf("Scan = %+v, %v", got, err)
			}
			stored := got[0].ErrorBody
			if len(tc.body) <= errorLimit {
				if stored != tc.body {
					t.Fatalf("error_body = %q; want it stored verbatim", stored)
				}
				return
			}
			if len(stored) > errorLimit {
				t.Fatalf("stored error_body is %d bytes; want at most %d", len(stored), errorLimit)
			}
			if !utf8.ValidString(stored) {
				t.Fatal("truncated error_body is not valid UTF-8")
			}
			if strings.ContainsRune(stored, utf8.RuneError) {
				t.Fatal("truncated error_body contains U+FFFD; the cut split a rune")
			}
		})
	}
}

// TestConcurrentAppendLosesNothing pins the append lock. Every request records
// off its own goroutine, so interleaved writes must produce whole lines and lose
// none of them.
func TestConcurrentAppendLosesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	led := openLedger(t, dir)

	const (
		writers   = 8
		perWriter = 25
		wantTotal = writers * perWriter
	)

	var group sync.WaitGroup
	errs := make(chan error, wantTotal)
	for writer := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			for seq := range perWriter {
				rec := validRecord(t)
				name := fmt.Sprintf("w%d-r%d", writer, seq)
				rec.RequestModel = model(name)
				rec.Selection = &ledger.Selection{Kind: ledger.SelectionModel, Model: model(name)}
				if err := led.Append(rec); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Append returned error: %v", err)
	}

	got, err := led.Scan(testDay)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(got) != wantTotal {
		t.Fatalf("Scan returned %d records; want %d", len(got), wantTotal)
	}
	seen := make(map[string]bool, wantTotal)
	for _, rec := range got {
		if rec.RequestModel == nil {
			t.Fatalf("record %+v lost its request_model", rec)
		}
		if seen[*rec.RequestModel] {
			t.Fatalf("request_model %q appears twice; records interleaved", *rec.RequestModel)
		}
		seen[*rec.RequestModel] = true
	}
	if len(seen) != wantTotal {
		t.Fatalf("found %d distinct records; want %d", len(seen), wantTotal)
	}
}

// TestRecordCarriesContextAndCredentialClass pins the two attribution fields on
// the wire. credential_class is a classification of the INBOUND credential and
// never a value read off a header, which is why the vocabulary is closed:
// anything outside it is refused rather than written. The class is billing's
// own type, so what the ledger writes here is what the accounting side reads.
func TestRecordCarriesContextAndCredentialClass(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	led := openLedger(t, dir)

	rec := validRecord(t)
	rec.Context = "work"
	rec.CredentialClass = billing.CredentialAPIKey

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
	if len(records) != 1 || records[0].Context != "work" ||
		records[0].CredentialClass != billing.CredentialAPIKey {
		t.Fatalf("Scan returned %+v; want the record's own context and credential class", records)
	}
}

// TestRecordSchemaCarriesNoAuthHeaders is the type-level guarantee that no
// credential-bearing header can reach the ledger: the schema has no header field
// at all, so a fully-populated record cannot serialize one.
func TestRecordSchemaCarriesNoAuthHeaders(t *testing.T) {
	t.Parallel()
	rec := ledger.Record{
		Timestamp:       testTimestamp(t),
		Host:            "gnomon",
		Context:         "work",
		Selection:       &ledger.Selection{Kind: ledger.SelectionSubagent, Model: model("claude-opus-5")},
		Seat:            "bedrock",
		Subagent:        &ledger.Subagent{AgentID: "agent-1", ParentAgentID: "parent-1"},
		Upstream:        "https://api.anthropic.com",
		Model:           model("claude-fable-5"),
		RequestModel:    model("claude-opus-5"),
		AuthMode:        "inject",
		CredentialClass: billing.CredentialOAuth,
		ConfigSource:    "OPENROUTER_API_KEY_FILE",
		Status:          500,
		InputTokens:     11,
		OutputTokens:    22,
		DurationMS:      33,
		UpstreamMS:      44,
		BlockShape:      []string{"thinking", "thinking"},
		DropSignature:   true,
		StopReason:      "end_turn",
		ErrorBody:       "upstream exploded",
		RequestBody:     `{"model":"claude-opus-5"}`,
		ResponseBody:    `{"content":[]}`,
	}

	// Every field must be populated, so this test fails loudly if someone adds a
	// field — a header map above all — without extending the fixture.
	value := reflect.ValueOf(rec)
	for index := range value.NumField() {
		if value.Field(index).IsZero() {
			t.Fatalf("Record field %q is unset; populate it so this test covers it",
				value.Type().Field(index).Name)
		}
	}

	encoded, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	lower := strings.ToLower(string(encoded))
	for _, banned := range []string{"authorization", "x-api-key", "bearer ", "sk-"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("marshaled record contains %q: %s", banned, encoded)
		}
	}
}

// TestOptionalRecordFieldsVanishWhenUnset keeps the body and error fields from
// writing empty strings into every line of a file that gets shipped.
func TestOptionalRecordFieldsVanishWhenUnset(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(validRecord(t))
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	var fields map[string]json.RawMessage
	if unmarshalErr := json.Unmarshal(encoded, &fields); unmarshalErr != nil {
		t.Fatalf("unmarshal record into a field map: %v", unmarshalErr)
	}
	for _, name := range []string{"error_body", "request_body", "response_body", "subagent"} {
		if _, present := fields[name]; present {
			t.Errorf("optional field %q must be omitted when empty: %s", name, encoded)
		}
	}
	for _, name := range []string{"status", "input_tokens", "output_tokens", "duration_ms", "upstream_ms", "block_shape", "drop_signature", "credential_class"} {
		if _, present := fields[name]; !present {
			t.Errorf("required field %q is missing: %s", name, encoded)
		}
	}
}

// TestCredentialClassVocabularyIsClosed names the only three classifications a
// record may carry, so no future caller can smuggle a credential in as one.
func TestCredentialClassVocabularyIsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		got  billing.CredentialClass
		want string
	}{
		{billing.CredentialOAuth, "oauth"},
		{billing.CredentialAPIKey, "api_key"},
		{billing.CredentialNone, "none"},
	} {
		if tc.got.String() != tc.want {
			t.Errorf("credential class = %q; want %q", tc.got, tc.want)
		}
	}
	for _, class := range []billing.CredentialClass{"", "bearer", "x-api-key", "sk-live"} {
		rec := validRecord(t)
		rec.CredentialClass = class
		if err := rec.Validate(); err == nil {
			t.Errorf("credential_class %q accepted", class)
		}
	}
}
