package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// importRecord is the lenient union of the v1 and v2 ledger wire shapes.
//
// Import reads files written by older builds, so unknown members are ignored
// rather than rejected. The fields whose names changed are retained together:
// v1 wrote alias and v2 writes seat, while both model fields became nullable in
// v2. Version-specific validation still applies after this decode, selected by
// the filename that identifies the schema.
type importRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Host      string    `json:"host"`

	Context         string                  `json:"context"`
	CredentialClass billing.CredentialClass `json:"credential_class"`
	Alias           string                  `json:"alias"`
	Selection       json.RawMessage         `json:"selection"`
	Seat            string                  `json:"seat"`
	Subagent        json.RawMessage         `json:"subagent"`
	Upstream        string                  `json:"upstream"`
	Model           *string                 `json:"model"`
	RequestModel    *string                 `json:"request_model"`
	AuthMode        string                  `json:"auth_mode"`
	ConfigSource    string                  `json:"config_source"`

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

// validateV1 applies v1's own record rules, including its read-side leniency
// for absent context and credential attribution.
func (record importRecord) validateV1() error {
	if err := record.validateV1RequiredFields(); err != nil {
		return err
	}
	if err := record.validateV1Vocabulary(); err != nil {
		return err
	}
	return record.validateMetrics("legacy")
}

func (record importRecord) validateV1RequiredFields() error {
	for _, field := range []struct {
		name  string
		value *string
	}{
		{name: "model", value: record.Model},
		{name: "request_model", value: record.RequestModel},
	} {
		if field.value == nil || strings.TrimSpace(*field.value) == "" {
			return fmt.Errorf("legacy ledger record %s must not be empty", field.name)
		}
	}
	for _, field := range []struct{ name, value string }{
		{name: "host", value: record.Host},
		{name: "upstream", value: record.Upstream},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("legacy ledger record %s must not be empty", field.name)
		}
	}
	return nil
}

func (record importRecord) validateV1Vocabulary() error {
	switch record.AuthMode {
	case string(registry.AuthForward), string(registry.AuthInject), string(registry.AuthSigV4):
	default:
		return fmt.Errorf("legacy ledger record has invalid auth_mode %q", record.AuthMode)
	}
	if record.CredentialClass != "" && !record.CredentialClass.Valid() {
		return errors.New("legacy ledger record has an invalid credential_class")
	}
	return validLegacyContext(record.Context)
}

func (record importRecord) validateMetrics(prefix string) error {
	if record.Timestamp.IsZero() {
		return fmt.Errorf("%s ledger record timestamp must not be zero", prefix)
	}
	if record.Status < 0 {
		return fmt.Errorf("%s ledger record status must not be negative", prefix)
	}
	if record.InputTokens < 0 || record.OutputTokens < 0 {
		return fmt.Errorf("%s ledger record token counts must not be negative", prefix)
	}
	if record.DurationMS < 0 || record.UpstreamMS < 0 {
		return fmt.Errorf("%s ledger record durations must not be negative", prefix)
	}
	return nil
}

// validLegacyContext checks a v1 context name against v1's own grammar:
// lowercase letters, digits and dashes, with an absent name accepted.
func validLegacyContext(name string) error {
	for _, char := range name {
		if !isLegacyContextChar(char) {
			return fmt.Errorf(
				"legacy ledger record has an invalid context %q: must use only lowercase letters, digits and dashes; found %q",
				name,
				char,
			)
		}
	}
	return nil
}

func isLegacyContextChar(char rune) bool {
	return char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-'
}
