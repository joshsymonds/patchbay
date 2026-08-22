package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/jsonstrict"
	"github.com/joshsymonds/patchbay/internal/registry"
)

const maxErrorBodyBytes = 4096

// Selection kinds explain which binding class selected a Seat.
const (
	SelectionModel    = "model"
	SelectionSubagent = "subagent"
	SelectionDefault  = "default"
)

// Selection is the public resolution reason persisted for a selected request.
type Selection struct {
	Kind  string  `json:"kind"`
	Model *string `json:"model"`
}

// Subagent carries only positively validated Claude Code observation IDs.
type Subagent struct {
	AgentID       string `json:"agent_id"`
	ParentAgentID string `json:"parent_agent_id,omitempty"`
}

// Record is one proxied request in the patchbay v2 JSONL schema.
//
// Model and RequestModel are nullable exact strings: nil is a model that was
// exactly absent, and a non-nil pointer names the opaque bytes a registry bound
// or a request asked with. No string is reserved to mean absence — a Seat can be
// bound to a model an operator called "(none)", and a record has to be able to
// say which of the two it holds.
//
// The field tags describe how a line is READ. Writing one goes through
// MarshalJSON, because the two record shapes spell an absent model differently
// and a tag cannot say so: a selected record always carries both model keys and
// writes absence as JSON null, while a pre-selection record has no model at all
// and leaves an absent request model out. Reading needs no such distinction —
// null and an omitted key both arrive as nil — so the tags are the whole of it,
// and decodeRecord adds the shape rules a tag cannot state either.
type Record struct {
	Timestamp time.Time `json:"timestamp"`
	Host      string    `json:"host"`

	Context      string     `json:"context,omitempty"`
	Selection    *Selection `json:"selection,omitempty"`
	Seat         string     `json:"seat,omitempty"`
	Subagent     *Subagent  `json:"subagent,omitempty"`
	Upstream     string     `json:"upstream,omitempty"`
	Model        *string    `json:"model"`
	RequestModel *string    `json:"request_model"`
	AuthMode     string     `json:"auth_mode,omitempty"`

	// CredentialClass says which kind of credential the client presented, never
	// which credential.
	CredentialClass billing.CredentialClass `json:"credential_class"`

	ConfigSource string `json:"config_source,omitempty"`

	Status        int      `json:"status"`
	InputTokens   int64    `json:"input_tokens"`
	OutputTokens  int64    `json:"output_tokens"`
	DurationMS    int64    `json:"duration_ms"`
	UpstreamMS    int64    `json:"upstream_ms"`
	BlockShape    []string `json:"block_shape"`
	DropSignature bool     `json:"drop_signature"`
	StopReason    string   `json:"stop_reason,omitempty"`
	ErrorBody     string   `json:"error_body,omitempty"`
	RequestBody   string   `json:"request_body,omitempty"`
	ResponseBody  string   `json:"response_body,omitempty"`
}

// wireRecord is the line a Record is written as.
//
// It differs from Record in the two model fields alone, which are raw values
// rather than pointers so one struct can write both record shapes exactly. An
// empty raw value is omitted, which is how a pre-selection record leaves out
// what it does not have; a raw null is a key that is present and absent, which
// is how a selected record spells a model that was not there.
type wireRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Host      string    `json:"host"`

	Context         string                  `json:"context,omitempty"`
	Selection       *Selection              `json:"selection,omitempty"`
	Seat            string                  `json:"seat,omitempty"`
	Subagent        *Subagent               `json:"subagent,omitempty"`
	Upstream        string                  `json:"upstream,omitempty"`
	Model           json.RawMessage         `json:"model,omitempty"`
	RequestModel    json.RawMessage         `json:"request_model,omitempty"`
	AuthMode        string                  `json:"auth_mode,omitempty"`
	CredentialClass billing.CredentialClass `json:"credential_class"`
	ConfigSource    string                  `json:"config_source,omitempty"`

	Status        int      `json:"status"`
	InputTokens   int64    `json:"input_tokens"`
	OutputTokens  int64    `json:"output_tokens"`
	DurationMS    int64    `json:"duration_ms"`
	UpstreamMS    int64    `json:"upstream_ms"`
	BlockShape    []string `json:"block_shape"`
	DropSignature bool     `json:"drop_signature"`
	StopReason    string   `json:"stop_reason,omitempty"`
	ErrorBody     string   `json:"error_body,omitempty"`
	RequestBody   string   `json:"request_body,omitempty"`
	ResponseBody  string   `json:"response_body,omitempty"`
}

// nullLiteral is the wire spelling of a model that was exactly absent.
const nullLiteral = "null"

// MarshalJSON writes the record in the shape its selection gives it.
//
// A selected record always carries model and request_model, so a reader that
// finds neither key is looking at damage rather than at a model nobody named. A
// pre-selection record carries no model at all — it named no Seat, so there is
// nothing to have rewritten — and spells an absent request model by omission,
// which keeps one spelling per state and the round trip exact.
func (record Record) MarshalJSON() ([]byte, error) {
	wire := record.wire()

	switch {
	case record.Selection != nil:
		model, err := writtenModel(record.Model, "model")
		if err != nil {
			return nil, err
		}
		requested, err := writtenModel(record.RequestModel, "request_model")
		if err != nil {
			return nil, err
		}
		wire.Model, wire.RequestModel = model, requested
	case record.RequestModel != nil:
		requested, err := writtenModel(record.RequestModel, "request_model")
		if err != nil {
			return nil, err
		}
		wire.RequestModel = requested
	}

	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encode ledger record: %w", err)
	}
	return encoded, nil
}

// wire copies everything but the two model keys, which each record shape spells
// its own way.
func (record Record) wire() wireRecord {
	return wireRecord{
		Timestamp:       record.Timestamp,
		Host:            record.Host,
		Context:         record.Context,
		Selection:       record.Selection,
		Seat:            record.Seat,
		Subagent:        record.Subagent,
		Upstream:        record.Upstream,
		Model:           nil,
		RequestModel:    nil,
		AuthMode:        record.AuthMode,
		CredentialClass: record.CredentialClass,
		ConfigSource:    record.ConfigSource,
		Status:          record.Status,
		InputTokens:     record.InputTokens,
		OutputTokens:    record.OutputTokens,
		DurationMS:      record.DurationMS,
		UpstreamMS:      record.UpstreamMS,
		BlockShape:      record.BlockShape,
		DropSignature:   record.DropSignature,
		StopReason:      record.StopReason,
		ErrorBody:       record.ErrorBody,
		RequestBody:     record.RequestBody,
		ResponseBody:    record.ResponseBody,
	}
}

// writtenModel renders one nullable model as the value a present key carries.
func writtenModel(value *string, name string) (json.RawMessage, error) {
	if value == nil {
		return json.RawMessage(nullLiteral), nil
	}
	encoded, err := json.Marshal(*value)
	if err != nil {
		return nil, fmt.Errorf("encode ledger record %s: %w", name, err)
	}
	return encoded, nil
}

// decodeRecord reads one JSONL line as a record, holding it to the shape rules
// its own selection implies. The value rules are Validate's, and the scanner
// applies both.
//
// It is a function rather than an UnmarshalJSON method because the rules it
// enforces are about which KEYS a line carries, and by the time a decoded struct
// exists that question is already gone: an omitted model and a null one are the
// same nil. So the line is read twice — once as raw keys, once as the record —
// and the two readings are compared.
//
// Three readings, and each owns a rule the others cannot state.
//
// RejectDuplicateNames goes first because it is the only one that sees the raw
// bytes as text: it holds the line to exactly one complete JSON value with
// nothing after it, and to naming each member of each object once, at every
// depth. Both are questions neither reading below could answer — a map and the
// struct decoder each keep the last value of a repeated name, and each stops at
// the end of the first value and never looks past it.
//
// The struct decoder owns the typed decode and, with DisallowUnknownFields, the
// refusal of a field at any depth that this build has never heard of — which is
// what keeps a v1-shaped line from being read as a v2 record that merely lost
// something. The raw-key reading owns the model keys each record shape must and
// must not carry.
func decodeRecord(line []byte) (Record, error) {
	if err := jsonstrict.RejectDuplicateNames(line); err != nil {
		return Record{}, fmt.Errorf("read ledger record: %w", err)
	}

	keys := map[string]json.RawMessage{}
	if err := json.Unmarshal(line, &keys); err != nil {
		return Record{}, fmt.Errorf("read ledger record keys: %w", err)
	}

	var record Record

	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("decode ledger record: %w", err)
	}

	if err := checkModelKeys(record, keys); err != nil {
		return Record{}, err
	}

	return record, nil
}

// checkModelKeys enforces which model keys each record shape must and must not
// carry, which is a question about the keys rather than about their values.
func checkModelKeys(record Record, keys map[string]json.RawMessage) error {
	_, model := keys["model"]
	requested, requestedPresent := keys["request_model"]

	if record.Selection != nil {
		if !model || !requestedPresent {
			return errors.New(
				"selected ledger record must carry both model and request_model, null when absent",
			)
		}
		return nil
	}

	if model {
		return errors.New("pre-selection ledger record must omit model")
	}
	if requestedPresent && string(requested) == nullLiteral {
		return errors.New(
			"pre-selection ledger record must omit an absent request_model rather than null it",
		)
	}
	return nil
}

// SanitizeRequestedContext returns value when a pre-selection record may keep it
// and the empty string when it may not.
//
// A requested Context is a path segment: caller-controlled text of any length
// carrying any bytes, and the record keeps it because a request naming a Context
// no registry can produce is exactly the failure worth recording. What it cannot
// do is cost the whole record. An unbounded or non-printable name is refused by
// validation — a ledger file gets shipped off the machine — so a producer that
// passed one straight through would lose the request as well as the name.
//
// It is exported because the proxy builds those records and must not carry a
// second copy of this grammar: two copies is how a name one of them admits
// becomes a record the other one drops.
func SanitizeRequestedContext(value string) string {
	if value == "" || validObservedID(value) {
		return value
	}
	return ""
}

// Validate enforces the strict v2 selected and pre-selection record shapes.
func (record Record) Validate() error {
	if err := validateIdentity(record); err != nil {
		return err
	}
	if err := validateRouting(record); err != nil {
		return err
	}
	return validateMetrics(record)
}

func validateIdentity(record Record) error {
	if strings.TrimSpace(record.Host) == "" {
		return errors.New("ledger record host must not be empty")
	}
	if record.Timestamp.IsZero() {
		return errors.New("ledger record timestamp must not be zero")
	}
	// The closed vocabulary keeps a credential from being recorded as its own
	// classification: a value outside it is refused, never stored. The rule is
	// billing's own, called rather than restated, so a class the accounting side
	// would reject can never reach a ledger file through this one.
	if !record.CredentialClass.Valid() {
		return errors.New("ledger record has an invalid credential_class")
	}
	return validateSubagent(record.Subagent)
}

func validateSubagent(subagent *Subagent) error {
	if subagent == nil {
		return nil
	}
	if !validObservedID(subagent.AgentID) {
		return errors.New("ledger record has invalid subagent agent_id")
	}
	if subagent.ParentAgentID != "" && !validObservedID(subagent.ParentAgentID) {
		return errors.New("ledger record has invalid subagent parent_agent_id")
	}
	return nil
}

// validateRouting enforces the two record shapes, which differ in what a
// context name is allowed to be.
//
// A pre-selection record exists precisely because selection did not happen —
// most often because the request named a context no registry can produce. That
// requested name is the whole point of the record, so it is retained as the
// caller asked for it, bounded rather than judged against the configured-ID
// grammar. A selected record names a context that did resolve, so it is held to
// the exact ID rule — and so is the Seat it selected, because both names came
// out of the same registry and are the same kind of ID. A stored Seat no
// registry could have admitted names something that does not exist.
func validateRouting(record Record) error {
	if record.Selection == nil {
		return validatePreSelection(record)
	}
	return validateSelected(record)
}

func validatePreSelection(record Record) error {
	if record.Seat != "" || record.Upstream != "" || record.Model != nil || record.AuthMode != "" ||
		record.ConfigSource != "" {
		return errors.New("pre-selection ledger record must omit selection-dependent fields")
	}
	if err := validateOptionalModel(record.RequestModel, "request_model"); err != nil {
		return err
	}
	if record.Context != "" && !validObservedID(record.Context) {
		return fmt.Errorf(
			"pre-selection ledger record requested context %q is not a bounded printable name",
			record.Context,
		)
	}
	return nil
}

func validateSelected(record Record) error {
	if record.Context == "" {
		return errors.New("selected ledger record context must not be empty")
	}
	if err := registry.ValidID(record.Context); err != nil {
		return fmt.Errorf("selected ledger record has invalid context %q", record.Context)
	}
	if err := validateSelection(*record.Selection); err != nil {
		return err
	}
	if err := validateSelectedFields(record); err != nil {
		return err
	}
	if err := registry.ValidID(record.Seat); err != nil {
		return fmt.Errorf("selected ledger record has invalid seat %q", record.Seat)
	}
	if err := validateAuthMode(record.AuthMode); err != nil {
		return err
	}
	if !validConfigSource(record.ConfigSource) {
		return errors.New("ledger record has invalid config_source")
	}
	return nil
}

// validateSelectedFields enforces the two rules a selected record carries, which
// are not the same rule.
//
// The Seat, the upstream it reached, the mode it authenticated with and the
// credential descriptor it named are patchbay's own vocabulary, checked against
// their own grammars further down; blank is never one of them, so a value that
// is nothing but whitespace is refused here.
//
// A model is not patchbay's vocabulary. It is an opaque exact string: the
// registry binds the bytes an operator wrote and a request selects them byte for
// byte, with no trimming or folding anywhere between. So a model of nothing but
// spaces is a model that exists and is kept as written, and absence is not a
// string at all — it is nil, which is why nothing here refuses one.
func validateSelectedFields(record Record) error {
	for _, field := range []struct{ name, value string }{
		{"seat", record.Seat},
		{"upstream", record.Upstream},
		{"auth_mode", record.AuthMode},
		{"config_source", record.ConfigSource},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("selected ledger record %s must not be empty", field.name)
		}
	}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"model", record.Model},
		{"request_model", record.RequestModel},
	} {
		if err := validateOptionalModel(field.value, field.name); err != nil {
			return err
		}
	}
	return nil
}

// validateOptionalModel keeps one spelling per state. Absence is nil; a pointer
// at the empty string is a producer claiming a model that is not one, and it
// would persist as `""` — a value no reader could distinguish from a Seat bound
// to the empty selector, which no registry admits.
func validateOptionalModel(value *string, name string) error {
	if value != nil && *value == "" {
		return fmt.Errorf(
			"ledger record %s must be absent rather than an empty string",
			name,
		)
	}
	return nil
}

func validateMetrics(record Record) error {
	if record.Status < 0 {
		return errors.New("ledger record status must not be negative")
	}
	if record.InputTokens < 0 || record.OutputTokens < 0 {
		return errors.New("ledger record token counts must not be negative")
	}
	if record.DurationMS < 0 || record.UpstreamMS < 0 {
		return errors.New("ledger record durations must not be negative")
	}
	// drop_signature exists so a later reader can select on it without
	// re-deriving it from block_shape. A record whose flag disagrees with its own
	// shape is that reader being lied to, so neither direction is stored.
	if record.DropSignature != HasDropSignature(record.BlockShape) {
		return fmt.Errorf(
			"ledger record drop_signature %v disagrees with block_shape %v",
			record.DropSignature,
			record.BlockShape,
		)
	}
	return nil
}

// validateSelection enforces the selection vocabulary and the model nullability
// each kind carries.
//
// A model selection names the public selector that matched. A subagent
// selection names the exact marked binding that matched, or nothing at all when
// marked traffic fell through to the marked-subagent default — the nil model is
// the only thing distinguishing the unnamed fallback from an exact hit. An
// ordinary Context default names no model either, and never may.
func validateSelection(sel Selection) error {
	switch sel.Kind {
	case SelectionModel:
		if sel.Model == nil || *sel.Model == "" {
			return fmt.Errorf("selection %s requires model", sel.Kind)
		}
	case SelectionSubagent:
		if sel.Model != nil && *sel.Model == "" {
			return errors.New("selection subagent requires a named or null model")
		}
	case SelectionDefault:
		if sel.Model != nil {
			return errors.New("selection default requires null model")
		}
	default:
		return fmt.Errorf("ledger record has invalid selection kind %q", sel.Kind)
	}
	return nil
}

func validateAuthMode(mode string) error {
	switch mode {
	case string(registry.AuthForward), string(registry.AuthInject), string(registry.AuthSigV4):
		return nil
	}
	return fmt.Errorf("ledger record has invalid auth_mode %q", mode)
}

// validConfigSource accepts the three non-secret credential descriptors a Seat
// can report. The environment-variable case defers to the registry's own
// exported grammar — the same rule that admitted the Seat — so a Seat patchbay
// loads can never produce a record patchbay refuses to store.
func validConfigSource(source string) bool {
	if source == "caller" {
		return true
	}
	if strings.HasPrefix(source, "aws_profile:") {
		return validAWSProfile(strings.TrimPrefix(source, "aws_profile:"))
	}
	return registry.ValidEnvName(source) == nil
}

func validAWSProfile(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if !isAWSProfileChar(char) {
			return false
		}
	}
	return true
}

func isAWSProfileChar(char rune) bool {
	return char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '.' ||
		char == '_'
}

func validObservedID(value string) bool {
	if len(value) < 1 || len(value) > 256 {
		return false
	}
	for _, char := range []byte(value) {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
