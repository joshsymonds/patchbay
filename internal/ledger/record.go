package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// maxErrorBodyBytes caps how much of an upstream error body a record keeps. The
// body is diagnostic, not archival: a runaway upstream must not be able to make
// one ledger line unbounded.
const maxErrorBodyBytes = 4096

// The three ways patchbay presents credentials upstream: forward the client's
// own credential unchanged, inject a configured one, or sign the request with
// AWS SigV4. A mode outside this set is refused rather than stored, the same way
// a credential class is.
const (
	authModeForward = "forward"
	authModeInject  = "inject"
	authModeSigV4   = "sigv4"
)

// Record is one proxied request, stored as a single JSONL line.
//
// The schema deliberately has no field for headers of any kind. Request and
// response headers carry Authorization and x-api-key, so the ledger cannot hold
// them: there is nowhere to put them, not merely a rule against it.
type Record struct {
	Timestamp time.Time `json:"timestamp"`
	Host      string    `json:"host"`

	// Context is the name of the context that served the request, which is
	// "personal" for every request that named none.
	Context string `json:"context"`

	// CredentialClass says which kind of credential the client presented, never
	// which credential.
	CredentialClass billing.CredentialClass `json:"credential_class"`

	Alias         string   `json:"alias"`
	Upstream      string   `json:"upstream"`
	Model         string   `json:"model"`
	RequestModel  string   `json:"request_model"`
	AuthMode      string   `json:"auth_mode"`
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

// Validate reports whether the record is complete enough to persist. Append
// runs it before touching the filesystem, so a malformed record never reaches
// a ledger file — and Scan runs it per line, so one that somehow did is
// reported as corruption rather than returned as if it were sound.
func (r Record) Validate() error {
	required := []struct {
		name  string
		value string
	}{
		{name: "host", value: r.Host},
		{name: "upstream", value: r.Upstream},
		{name: "model", value: r.Model},
		{name: "request_model", value: r.RequestModel},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("ledger record %s must not be empty", field.name)
		}
	}
	switch r.AuthMode {
	case authModeForward, authModeInject, authModeSigV4:
	default:
		return fmt.Errorf("ledger record has invalid auth_mode %q", r.AuthMode)
	}
	// The closed vocabulary keeps a credential from being recorded as its own
	// classification: a value outside it is refused, never stored. Empty is
	// read-side leniency for records written before attribution existed.
	if r.CredentialClass != "" && !r.CredentialClass.Valid() {
		return errors.New("ledger record has an invalid credential_class")
	}
	if err := validateContextName(r.Context); err != nil {
		return err
	}
	if r.Timestamp.IsZero() {
		return errors.New("ledger record timestamp must not be zero")
	}
	if r.Status < 0 {
		return errors.New("ledger record status must not be negative")
	}
	if r.InputTokens < 0 || r.OutputTokens < 0 {
		return errors.New("ledger record token counts must not be negative")
	}
	if r.DurationMS < 0 || r.UpstreamMS < 0 {
		return errors.New("ledger record durations must not be negative")
	}
	return nil
}

// validateContextName checks a record's context name. An absent one is
// accepted: records written before contexts existed carry neither attribution
// field, and a ledger that refused to read its own history would be a worse
// failure than a missing field. The leniency is read-side only — the pipeline
// always populates both fields, so nothing new is written this shape.
//
// A name that IS present is held to the registry's own rule, by calling it, so
// a value that could only have come from somewhere other than a resolved
// context is refused rather than stored.
func validateContextName(name string) error {
	if name == "" {
		return nil
	}

	if err := registry.ValidContextName(name); err != nil {
		return fmt.Errorf("ledger record has an invalid context %q", name)
	}

	return nil
}

// truncateUTF8 cuts text to at most limit bytes, backing up to the nearest rune
// boundary so the stored line never contains a split rune.
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
