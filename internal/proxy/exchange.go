package proxy

import (
	"net/http"
	"strings"
	"time"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// bearerScheme is the credential scheme a subscription OAuth token arrives
// under. Matching it is how the inbound credential is classified without ever
// reading the token itself.
const bearerScheme = "bearer "

// credentialShape is the complete shape of credentials a caller presented.
// Keeping ambiguous and unrecognized shapes distinct lets a resolved forward
// Seat fail closed without changing the ledger's established vocabulary.
type credentialShape uint8

const (
	credentialNone credentialShape = iota
	credentialOAuth
	credentialAPIKey
	credentialOAuthAndAPIKey
	credentialUnrecognizedAuthorization
	credentialRepeatedOAuthAuthorization
	credentialRepeatedAuthorization
	credentialRepeatedAPIKey
)

// exchange accumulates everything one request contributes to its ledger record
// and its accounting row. It belongs to a single request and is never shared.
//
// resolution is nil until a Seat is selected. Everything the record says about
// where a request went — Seat, upstream, auth mode, credential source — hangs
// off that pointer, so a refusal that happened before selection cannot describe
// a destination it never had.
type exchange struct {
	resolution      *registry.Resolution
	context         string
	subagent        *registry.SubagentObservation
	credentialClass billing.CredentialClass
	requestModel    string
	start           time.Time
	status          int
	contentType     string
	upstreamMS      int64
	durationMS      int64
	requestBody     string
	errorBody       string

	// accountingID is the durable pending row this request owns. Zero means no
	// row was inserted, which is every refusal that happened before a Seat was
	// selected; accountingDispatched records whether dispatch evidence was
	// persisted, and upstreamTarget is the resolved destination that evidence
	// names.
	accountingID         int64
	accountingDispatched bool
	upstreamTarget       string
}

// newExchange starts recording a request that has not selected a Seat: it
// arrived at start in the Context the caller's path named, under
// credentialClass, asking for requestModel. Every refusal that precedes
// selection is recorded through this.
func newExchange(
	context string,
	credentialClass billing.CredentialClass,
	requestModel string,
	subagent *registry.SubagentObservation,
	start time.Time,
) *exchange {
	return &exchange{
		context:         context,
		credentialClass: credentialClass,
		requestModel:    requestModel,
		subagent:        subagent,
		start:           start,
	}
}

// newSelectedExchange starts recording a request that selected a Seat within
// res. The Resolution is copied so the record and the accounting row read the
// same immutable snapshot the pipeline forwards on, whatever the registry file
// does next.
func newSelectedExchange(
	res registry.Resolution,
	credentialClass billing.CredentialClass,
	requestModel string,
	start time.Time,
) *exchange {
	cloned := res

	return &exchange{
		resolution:      &cloned,
		context:         res.Context,
		subagent:        res.Subagent,
		credentialClass: credentialClass,
		requestModel:    requestModel,
		start:           start,
	}
}

// seat is the Seat this exchange selected, or the zero Seat before selection.
func (e *exchange) seat() registry.Seat {
	if e.resolution == nil {
		return registry.Seat{}
	}

	return e.resolution.Seat
}

// seatID is the registry ID of the Seat this exchange selected, or empty before
// selection. Empty is the absence of a selection and never a Seat: a name is
// only ever the one the registry gave, so nothing downstream reads an invented
// ID as a row that exists.
func (e *exchange) seatID() string {
	if e.resolution == nil {
		return ""
	}

	return e.resolution.SeatID
}

// classifyCredential returns the complete credential shape the client
// presented, without retaining either credential value. Every repeated value
// matters because the upstream header copy preserves every one; Authorization
// is read only far enough to recognize the Bearer scheme.
func classifyCredential(header http.Header) credentialShape {
	authorizations := header.Values(anthropic.HeaderAuthorization)
	apiKeys := header.Values(anthropic.HeaderClientKey)

	var authorization string
	if len(authorizations) > 0 {
		authorization = authorizations[0]
	}

	hasOAuth := len(authorization) >= len(bearerScheme) &&
		strings.EqualFold(authorization[:len(bearerScheme)], bearerScheme) &&
		strings.TrimSpace(authorization[len(bearerScheme):]) != ""

	switch {
	case len(authorizations) > 1 && hasOAuth:
		return credentialRepeatedOAuthAuthorization
	case len(authorizations) > 1:
		return credentialRepeatedAuthorization
	case hasOAuth && len(apiKeys) > 0:
		return credentialOAuthAndAPIKey
	case len(apiKeys) > 1:
		return credentialRepeatedAPIKey
	case len(authorizations) == 1 && !hasOAuth:
		return credentialUnrecognizedAuthorization
	case hasOAuth:
		return credentialOAuth
	case len(apiKeys) == 1 && strings.TrimSpace(apiKeys[0]) != "":
		return credentialAPIKey
	default:
		return credentialNone
	}
}

// ledgerClass maps the complete inbound shape onto the canonical credential
// attribution vocabulary. Ambiguous OAuth-plus-key requests retain the OAuth
// classification the old precedence rule recorded; unrecognized Authorization
// remains none.
func (c credentialShape) ledgerClass() billing.CredentialClass {
	switch c {
	case credentialOAuth, credentialOAuthAndAPIKey, credentialRepeatedOAuthAuthorization:
		return billing.CredentialOAuth
	case credentialAPIKey, credentialRepeatedAPIKey:
		return billing.CredentialAPIKey
	default:
		return billing.CredentialNone
	}
}

// record builds the ledger record for the finished exchange. capt is the
// captured response, or nil when there was nothing to capture; a dropped
// capture costs the summary and nothing else, because the record is written
// either way.
func (e *exchange) record(host string, capt *capture) ledger.Record {
	rec := ledger.Record{
		Timestamp:       e.start,
		Host:            host,
		Context:         e.context,
		CredentialClass: e.credentialClass,
		RequestModel:    optionalModel(e.requestModel),
		Status:          e.status,
		DurationMS:      e.durationMS,
		UpstreamMS:      e.upstreamMS,
		BlockShape:      ledger.BlockShape(nil),
		ErrorBody:       e.errorBody,
		RequestBody:     e.requestBody,
	}
	if e.subagent != nil {
		rec.Subagent = &ledger.Subagent{
			AgentID:       e.subagent.AgentID,
			ParentAgentID: e.subagent.ParentAgentID,
		}
	}
	if e.resolution != nil {
		res := e.resolution
		rec.Selection = &ledger.Selection{
			Kind:  string(res.Match.Kind),
			Model: cloneString(res.Match.Model),
		}
		rec.Seat = res.SeatID
		rec.Upstream = res.Seat.EffectiveUpstream()
		rec.Model = optionalModel(upstreamModel(res.Seat, e.requestModel))
		rec.AuthMode = string(res.Seat.AuthMode)
		rec.ConfigSource = res.Seat.CredentialSource()
	} else {
		// Nothing selected, so the Context is whatever the caller's path named:
		// arbitrary text of any length, which is exactly why the record is worth
		// keeping and exactly why the ledger will not store all of it. A name it
		// cannot represent costs the name and not the request, so it is dropped
		// here rather than left to fail the append.
		rec.Context = ledger.SanitizeRequestedContext(e.context)
	}
	captured := capt.bytes()
	if len(captured) == 0 {
		return rec
	}
	if e.resolution != nil && e.resolution.Seat.CaptureBodies {
		rec.ResponseBody = string(captured)
	}
	if !isSuccess(e.status) {
		rec.ErrorBody = string(captured)

		return rec
	}
	sum := summarize(captured, e.contentType)
	rec.BlockShape = ledger.BlockShape(sum.blockTypes)
	rec.DropSignature = ledger.HasDropSignature(rec.BlockShape)
	rec.InputTokens = sum.inputTokens
	rec.OutputTokens = sum.outputTokens
	rec.StopReason = sum.stopReason

	return rec
}

// upstreamModel is the model actually sent upstream: the Seat's rewrite target
// when it has one, otherwise the model the caller asked for.
func upstreamModel(seat registry.Seat, requested string) string {
	if seat.Model != "" {
		return seat.Model
	}

	return requested
}

// optionalModel is the nullable model a record stores. Exactly one value is
// absent — the empty string, meaning a request that named no model or a Seat
// that rewrites to none — and it becomes nil, which the ledger writes as JSON
// null.
//
// Nothing else is absent. A selector is an opaque exact string the registry
// bound and the request named byte for byte, so one made of spaces, or one an
// operator called "(none)", is a model that exists and is stored as it was
// written. No literal stands in for absence, so none can be mistaken for it.
func optionalModel(model string) *string {
	if model == "" {
		return nil
	}

	return &model
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value

	return &copied
}

// isSuccess reports whether a status is a 2xx.
func isSuccess(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

// millis converts a measured duration to whole milliseconds.
func millis(elapsed time.Duration) int64 { return elapsed.Milliseconds() }
