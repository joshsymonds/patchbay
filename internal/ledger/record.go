package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// maxErrorBodyBytes caps how much of an upstream error body a record keeps. The
// body is diagnostic, not archival: a runaway upstream must not be able to make
// one ledger line unbounded.
const maxErrorBodyBytes = 4096

// The two ways patchbay presents credentials upstream: forward the client's own
// credential unchanged, or inject a configured one.
const (
	authModeForward = "forward"
	authModeInject  = "inject"
)

// Record is one proxied request, stored as a single JSONL line.
//
// The schema deliberately has no field for headers of any kind. Request and
// response headers carry Authorization and x-api-key, so the ledger cannot hold
// them: there is nowhere to put them, not merely a rule against it.
type Record struct {
	Timestamp     time.Time `json:"timestamp"`
	Host          string    `json:"host"`
	Alias         string    `json:"alias"`
	Upstream      string    `json:"upstream"`
	Model         string    `json:"model"`
	RequestModel  string    `json:"request_model"`
	AuthMode      string    `json:"auth_mode"`
	Status        int       `json:"status"`
	InputTokens   int64     `json:"input_tokens"`
	OutputTokens  int64     `json:"output_tokens"`
	DurationMS    int64     `json:"duration_ms"`
	UpstreamMS    int64     `json:"upstream_ms"`
	BlockShape    []string  `json:"block_shape"`
	DropSignature bool      `json:"drop_signature"`
	StopReason    string    `json:"stop_reason,omitempty"`
	ErrorBody     string    `json:"error_body,omitempty"`
	RequestBody   string    `json:"request_body,omitempty"`
	ResponseBody  string    `json:"response_body,omitempty"`
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
	case authModeForward, authModeInject:
	default:
		return fmt.Errorf("ledger record has invalid auth_mode %q", r.AuthMode)
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
