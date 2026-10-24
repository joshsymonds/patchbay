package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/joshsymonds/patchbay/internal/billing"
)

const (
	fieldDefaultContext    = "default_context"
	fieldSeats             = "seats"
	fieldContexts          = "contexts"
	fieldDefaultSeat       = "default_seat"
	fieldModels            = "models"
	fieldSubagents         = "subagents"
	fieldAgentTypes        = "agent_types"
	fieldUpstream          = "upstream"
	fieldModel             = "model"
	fieldAuthMode          = "auth_mode"
	fieldKeyEnvFile        = "api_key_env_file"
	fieldAWSProfile        = "aws_profile"
	fieldAWSRegion         = "aws_region"
	fieldModelMapEnvFile   = "model_map_env_file"
	fieldBilling           = "billing"
	fieldExtraBody         = "extra_body"
	fieldMaxInputTokens    = "max_input_tokens"
	fieldMaxOutputTokens   = "max_output_tokens"
	fieldInsecure          = "insecure"
	fieldStripCacheControl = "strip_cache_control"
	fieldCaptureBodies     = "capture_bodies"
	fieldSpeed             = "speed"
	fieldCacheTTL          = "cache_ttl"
	fieldStripBetas        = "strip_betas"
)

type document struct {
	DefaultContext string
	Seats          map[string]Seat
	Contexts       map[string]Context
}

func parseRegistry(data []byte) (document, error) {
	var raw json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&raw); err != nil {
		return document{}, fmt.Errorf("decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return document{}, errors.New("unexpected trailing data after the registry document")
	}

	fields, objectErr := object(raw, "registry document")
	if objectErr != nil {
		return document{}, objectErr
	}
	required := []string{fieldDefaultContext, fieldSeats, fieldContexts}
	if fieldsErr := exactFields(fields, required, required); fieldsErr != nil {
		return document{}, fieldsErr
	}

	var out document
	if decodeErr := decodeField(
		&out.DefaultContext,
		fieldDefaultContext,
		fields[fieldDefaultContext],
	); decodeErr != nil {
		return document{}, decodeErr
	}
	seats, seatsErr := parseSeats(fields[fieldSeats])
	if seatsErr != nil {
		return document{}, seatsErr
	}
	out.Seats = seats
	contexts, contextsErr := parseContexts(fields[fieldContexts])
	if contextsErr != nil {
		return document{}, contextsErr
	}
	out.Contexts = contexts
	return out, nil
}

func parseSeats(raw json.RawMessage) (map[string]Seat, error) {
	entries, err := object(raw, fieldSeats)
	if err != nil {
		return nil, err
	}
	seats := make(map[string]Seat, len(entries))
	for _, id := range sortedKeys(entries) {
		seat, parseErr := parseSeat(entries[id])
		if parseErr != nil {
			return nil, fmt.Errorf("seat %q: %w", id, parseErr)
		}
		seats[id] = seat
	}
	return seats, nil
}

func parseSeat(raw json.RawMessage) (Seat, error) {
	fields, objectErr := object(raw, "seat")
	if objectErr != nil {
		return Seat{}, objectErr
	}
	allowed := []string{
		fieldUpstream,
		fieldModel,
		fieldAuthMode,
		fieldKeyEnvFile,
		fieldAWSProfile,
		fieldAWSRegion,
		fieldModelMapEnvFile,
		fieldBilling,
		fieldExtraBody,
		fieldMaxInputTokens,
		fieldMaxOutputTokens,
		fieldInsecure,
		fieldStripCacheControl,
		fieldCaptureBodies,
		fieldSpeed,
		fieldCacheTTL,
		fieldStripBetas,
	}
	if fieldsErr := exactFields(fields, allowed, nil); fieldsErr != nil {
		return Seat{}, fieldsErr
	}

	var seat Seat
	decoders := seatFieldDecoders(&seat)
	for _, name := range sortedKeys(fields) {
		if decodeErr := decoders[name](fields[name]); decodeErr != nil {
			return Seat{}, decodeErr
		}
	}
	return seat, nil
}

func seatFieldDecoders(seat *Seat) map[string]func(json.RawMessage) error {
	return map[string]func(json.RawMessage) error{
		fieldUpstream:        func(raw json.RawMessage) error { return decodeField(&seat.Upstream, fieldUpstream, raw) },
		fieldModel:           func(raw json.RawMessage) error { return decodeField(&seat.Model, fieldModel, raw) },
		fieldAuthMode:        func(raw json.RawMessage) error { return decodeField(&seat.AuthMode, fieldAuthMode, raw) },
		fieldKeyEnvFile:      func(raw json.RawMessage) error { return decodeField(&seat.APIKeyEnvFile, fieldKeyEnvFile, raw) },
		fieldAWSProfile:      func(raw json.RawMessage) error { return decodeField(&seat.AWSProfile, fieldAWSProfile, raw) },
		fieldAWSRegion:       func(raw json.RawMessage) error { return decodeField(&seat.AWSRegion, fieldAWSRegion, raw) },
		fieldModelMapEnvFile: func(raw json.RawMessage) error { return decodeField(&seat.ModelMapEnvFile, fieldModelMapEnvFile, raw) },
		fieldBilling:         func(raw json.RawMessage) error { return decodeBilling(seat, raw) },
		fieldExtraBody:       func(raw json.RawMessage) error { return decodeExtraBody(&seat.ExtraBody, raw) },
		fieldMaxInputTokens:  func(raw json.RawMessage) error { return decodeField(&seat.MaxInputTokens, fieldMaxInputTokens, raw) },
		fieldMaxOutputTokens: func(raw json.RawMessage) error { return decodeField(&seat.MaxOutputTokens, fieldMaxOutputTokens, raw) },
		fieldInsecure:        func(raw json.RawMessage) error { return decodeField(&seat.Insecure, fieldInsecure, raw) },
		fieldStripCacheControl: func(raw json.RawMessage) error {
			return decodeField(&seat.StripCacheControl, fieldStripCacheControl, raw)
		},
		fieldCaptureBodies: func(raw json.RawMessage) error { return decodeField(&seat.CaptureBodies, fieldCaptureBodies, raw) },
		fieldSpeed:         func(raw json.RawMessage) error { return decodeSpeed(seat, raw) },
		fieldCacheTTL:      func(raw json.RawMessage) error { return decodeCacheTTL(seat, raw) },
		fieldStripBetas:    func(raw json.RawMessage) error { return decodeStripBetas(seat, raw) },
	}
}

// decodeStripBetas parses the beta flags a Seat withholds from its upstream.
// Each entry must name one flag: a blank or padded name fails the document
// here, because the flags a caller sends are compared by exact string and a
// name that could never match would strip nothing while looking like policy.
func decodeStripBetas(seat *Seat, raw json.RawMessage) error {
	var value []string
	if err := decodeField(&value, fieldStripBetas, raw); err != nil {
		return err
	}
	for _, name := range value {
		if name == "" || strings.TrimSpace(name) != name {
			return fmt.Errorf("field %q: entry %q must be a beta flag name with no surrounding whitespace",
				fieldStripBetas, name)
		}
	}
	seat.StripBetas = value
	return nil
}

// decodeBilling parses the explicit billing class a Seat declares and records
// that it declared one.
//
// A value outside the closed vocabulary — a blank string included — fails the
// whole document here rather than reaching validation, so the presence bit only
// ever describes a class that parsed. What it separates downstream is a Seat
// that named a valid class from one that named none at all, which is the whole
// distinction between the two refusals validateBilling can issue.
func decodeBilling(seat *Seat, raw json.RawMessage) error {
	var value string
	if err := decodeField(&value, fieldBilling, raw); err != nil {
		return err
	}
	class, err := billing.ParseClass(value)
	if err != nil {
		return fmt.Errorf("field %q: %w", fieldBilling, err)
	}
	seat.BillingClass = class
	seat.BillingClassSet = true
	return nil
}

// decodeSpeed parses the one tier a Seat may ask an upstream for. Like
// billing, a value outside the vocabulary — a blank string included — fails the
// document here, so a Seat never carries a speed that parsed but means nothing.
func decodeSpeed(seat *Seat, raw json.RawMessage) error {
	var value string
	if err := decodeField(&value, fieldSpeed, raw); err != nil {
		return err
	}
	if value != SpeedFast {
		return fmt.Errorf("field %q: must be %q or omitted", fieldSpeed, SpeedFast)
	}
	seat.Speed = value
	return nil
}

// decodeCacheTTL parses the one prompt-cache lifetime a Seat may impose. The
// five-minute default is what a caller's markers already mean, so the field
// names only the extended lifetime; anything else — a blank string included —
// fails the document here, the way speed does.
func decodeCacheTTL(seat *Seat, raw json.RawMessage) error {
	var value string
	if err := decodeField(&value, fieldCacheTTL, raw); err != nil {
		return err
	}
	if value != CacheTTLOneHour {
		return fmt.Errorf("field %q: must be %q or omitted", fieldCacheTTL, CacheTTLOneHour)
	}
	seat.CacheTTL = value
	return nil
}

func decodeExtraBody(target *json.RawMessage, raw json.RawMessage) error {
	if isNull(raw) {
		return fmt.Errorf("%s must be a JSON object, not null", fieldExtraBody)
	}
	*target = bytes.Clone(raw)
	return nil
}

func parseContexts(raw json.RawMessage) (map[string]Context, error) {
	entries, err := object(raw, fieldContexts)
	if err != nil {
		return nil, err
	}
	contexts := make(map[string]Context, len(entries))
	for _, id := range sortedKeys(entries) {
		ctx, parseErr := parseContext(entries[id])
		if parseErr != nil {
			return nil, fmt.Errorf("context %q: %w", id, parseErr)
		}
		contexts[id] = ctx
	}
	return contexts, nil
}

func parseContext(raw json.RawMessage) (Context, error) {
	fields, objectErr := object(raw, "context")
	if objectErr != nil {
		return Context{}, objectErr
	}
	if fieldsErr := exactFields(
		fields,
		[]string{fieldDefaultSeat, fieldModels, fieldSubagents},
		[]string{fieldDefaultSeat, fieldModels},
	); fieldsErr != nil {
		return Context{}, fieldsErr
	}
	var ctx Context
	if decodeErr := decodeField(
		&ctx.DefaultSeat,
		fieldDefaultSeat,
		fields[fieldDefaultSeat],
	); decodeErr != nil {
		return Context{}, decodeErr
	}
	models, modelsErr := parseBindings(fieldModels, fields[fieldModels])
	if modelsErr != nil {
		return Context{}, modelsErr
	}
	ctx.Models = models
	if rawSubagents, ok := fields[fieldSubagents]; ok {
		if isNull(rawSubagents) {
			return Context{}, errors.New("subagents must be an object, not null")
		}
		parsed, parseErr := parseSubagents(rawSubagents)
		if parseErr != nil {
			return Context{}, fmt.Errorf("subagents: %w", parseErr)
		}
		ctx.Subagents = &parsed
	}
	return ctx, nil
}

func parseSubagents(raw json.RawMessage) (Subagents, error) {
	fields, objectErr := object(raw, fieldSubagents)
	if objectErr != nil {
		return Subagents{}, objectErr
	}
	if fieldsErr := exactFields(
		fields,
		[]string{fieldDefaultSeat, fieldAgentTypes, fieldModels},
		[]string{fieldDefaultSeat},
	); fieldsErr != nil {
		return Subagents{}, fieldsErr
	}
	var out Subagents
	if decodeErr := decodeField(
		&out.DefaultSeat,
		fieldDefaultSeat,
		fields[fieldDefaultSeat],
	); decodeErr != nil {
		return Subagents{}, decodeErr
	}
	out.AgentTypes = map[string]string{}
	if rawAgentTypes, ok := fields[fieldAgentTypes]; ok {
		agentTypes, agentTypesErr := parseBindings(fieldAgentTypes, rawAgentTypes)
		if agentTypesErr != nil {
			return Subagents{}, agentTypesErr
		}
		out.AgentTypes = agentTypes
	}
	out.Models = map[string]string{}
	if rawModels, ok := fields[fieldModels]; ok {
		models, modelsErr := parseBindings(fieldModels, rawModels)
		if modelsErr != nil {
			return Subagents{}, modelsErr
		}
		out.Models = models
	}
	return out, nil
}

func parseBindings(name string, raw json.RawMessage) (map[string]string, error) {
	entries, objectErr := object(raw, name)
	if objectErr != nil {
		return nil, objectErr
	}
	bindings := make(map[string]string, len(entries))
	for _, selector := range sortedKeys(entries) {
		var seatID string
		decodeErr := decodeField(
			&seatID,
			name+" selector "+fmt.Sprintf("%q", selector),
			entries[selector],
		)
		if decodeErr != nil {
			return nil, decodeErr
		}
		bindings[selector] = seatID
	}
	return bindings, nil
}

// object decodes one JSON object into its members, refusing a repeated member
// name outright.
//
// encoding/json keeps the last value of a repeated name, so a document that
// says a thing twice loads as whatever came second — the reviewed value
// silently shadowed by one further down the file. Every object the registry
// grammar owns is read through here, so the refusal reaches the root, the seats
// and contexts maps, each Seat and Context, each binding map and the subagents
// object alike.
func object(raw json.RawMessage, name string) (map[string]json.RawMessage, error) {
	if isNull(raw) {
		return nil, fmt.Errorf("%s must be an object, not null", name)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("%s must be an object", name)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}

	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", name, err)
		}
		member, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("decode %s: member name %v is not a string", name, token)
		}
		if _, repeated := fields[member]; repeated {
			return nil, fmt.Errorf("%s has a duplicate member %q", name, member)
		}
		var value json.RawMessage
		if decodeErr := decoder.Decode(&value); decodeErr != nil {
			return nil, fmt.Errorf("decode %s: %w", name, decodeErr)
		}
		fields[member] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	return fields, nil
}

func exactFields(fields map[string]json.RawMessage, allowed, required []string) error {
	allow := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		allow[name] = true
	}
	for _, name := range sortedKeys(fields) {
		if !allow[name] {
			return fmt.Errorf("unknown field %q", name)
		}
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return fmt.Errorf("missing required field %q", name)
		}
	}
	return nil
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func decodeField[Val any](target *Val, name string, value json.RawMessage) error {
	if err := json.Unmarshal(value, target); err != nil {
		return fmt.Errorf("field %q: %w", name, err)
	}
	return nil
}

func sortedKeys[Val any](values map[string]Val) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
