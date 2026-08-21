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

// modelNone stands in for a model name the request never gave — a body the
// gateway could not read, or one that named nothing. The ledger refuses a
// record with an empty model, so without a stand-in the whole record would be
// dropped: a request that named no model is worth recording as exactly that.
const modelNone = "(none)"

// bearerScheme is the credential scheme a subscription OAuth token arrives
// under. Matching it is how the inbound credential is classified without ever
// reading the token itself.
const bearerScheme = "bearer "

// credentialShape is the complete shape of credentials a caller presented.
// Keeping ambiguous and unrecognized shapes distinct lets a resolved forward
// route fail closed without changing the ledger's established vocabulary.
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

// exchange accumulates everything one request contributes to its ledger record.
// It belongs to a single request and is never shared.
type exchange struct {
	route           registry.Route
	context         string
	credentialClass billing.CredentialClass
	requestModel    string
	start           time.Time
	status          int
	contentType     string
	upstreamMS      int64
	durationMS      int64
	requestBody     string
	errorBody       string

	accountingID         int64
	accountingDispatched bool
	upstreamTarget       string
}

// newExchange starts recording a request that arrived at start in the named
// context under credentialClass, asking for requestModel and resolved to route.
func newExchange(
	route registry.Route,
	ctxName string,
	credentialClass billing.CredentialClass,
	requestModel string,
	start time.Time,
) *exchange {
	return &exchange{
		route:           route,
		context:         ctxName,
		credentialClass: credentialClass,
		requestModel:    namedModel(requestModel),
		start:           start,
		status:          0,
		contentType:     "",
		upstreamMS:      0,
		durationMS:      0,
		requestBody:     "",
		errorBody:       "",
	}
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

		Alias:         e.route.Alias,
		Upstream:      e.route.Upstream(),
		Model:         namedModel(e.upstreamModel()),
		RequestModel:  namedModel(e.requestModel),
		AuthMode:      string(e.route.Auth),
		Status:        e.status,
		InputTokens:   0,
		OutputTokens:  0,
		DurationMS:    e.durationMS,
		UpstreamMS:    e.upstreamMS,
		BlockShape:    ledger.BlockShape(nil),
		DropSignature: false,
		StopReason:    "",
		ErrorBody:     e.errorBody,
		RequestBody:   e.requestBody,
		ResponseBody:  "",
	}

	captured := capt.bytes()
	if len(captured) == 0 {
		return rec
	}

	if e.route.CaptureBodies {
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

// upstreamModel is the model actually sent upstream: the route's rewrite target
// when it has one, otherwise the model the caller asked for.
func (e *exchange) upstreamModel() string {
	if e.route.Model != "" {
		return e.route.Model
	}

	return e.requestModel
}

// namedModel returns model, or the stand-in when the request named none. Blank
// counts as none: that is the ledger's own test for a missing field, and a
// record it would refuse is a record lost.
func namedModel(model string) string {
	if strings.TrimSpace(model) == "" {
		return modelNone
	}

	return model
}

// isSuccess reports whether a status is a 2xx.
func isSuccess(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

// millis converts a measured duration to whole milliseconds.
func millis(elapsed time.Duration) int64 {
	return elapsed.Milliseconds()
}
