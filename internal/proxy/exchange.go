package proxy

import (
	"net/http"
	"strings"
	"time"

	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// modelNone stands in for a model name the request never gave — a body the
// gateway could not read, or one that named nothing. The ledger refuses a
// record with an empty model, so without a stand-in the whole record would be
// dropped: a request that named no model is worth recording as exactly that.
const modelNone = "(none)"

// exchange accumulates everything one request contributes to its ledger record.
// It belongs to a single request and is never shared.
type exchange struct {
	route        registry.Route
	requestModel string
	start        time.Time
	status       int
	contentType  string
	upstreamMS   int64
	durationMS   int64
	requestBody  string
	errorBody    string
}

// newExchange starts recording a request that arrived at start asking for
// requestModel and resolved to route.
func newExchange(route registry.Route, requestModel string, start time.Time) *exchange {
	return &exchange{
		route:        route,
		requestModel: requestModel,
		start:        start,
		status:       0,
		contentType:  "",
		upstreamMS:   0,
		durationMS:   0,
		requestBody:  "",
		errorBody:    "",
	}
}

// record builds the ledger record for the finished exchange. capt is the
// captured response, or nil when there was nothing to capture; a dropped
// capture costs the summary and nothing else, because the record is written
// either way.
func (e *exchange) record(host string, capt *capture) ledger.Record {
	rec := ledger.Record{
		Timestamp:     e.start,
		Host:          host,
		Alias:         e.route.Alias,
		Upstream:      e.route.BaseURL,
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
