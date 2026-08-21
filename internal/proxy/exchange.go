package proxy

import (
	"net/http"
	"strings"
	"time"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/registry"
)

const bearerScheme = "bearer "

type exchange struct {
	resolution      *registry.Resolution
	context         string
	subagent        *registry.SubagentObservation
	credentialClass string
	requestModel    string
	start           time.Time
	status          int
	contentType     string
	upstreamMS      int64
	durationMS      int64
	requestBody     string
	errorBody       string
}

func newExchange(
	context, credentialClass, requestModel string,
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

func newSelectedExchange(
	res registry.Resolution,
	credentialClass, requestModel string,
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

func credentialClass(header http.Header) string {
	if authorization := header.Get(
		anthropic.HeaderAuthorization,
	); authorization != "" &&
		strings.HasPrefix(strings.ToLower(authorization), bearerScheme) {
		return ledger.CredentialOAuth
	}
	if header.Get(anthropic.HeaderClientKey) != "" {
		return ledger.CredentialAPIKey
	}
	return ledger.CredentialNone
}

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
func isSuccess(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}
func millis(elapsed time.Duration) int64 { return elapsed.Milliseconds() }
