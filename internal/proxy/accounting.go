package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/joshsymonds/patchbay/internal/billing"
	usagepkg "github.com/joshsymonds/patchbay/internal/usage"
)

// Accountant durably tracks each routed request before it reaches an upstream.
// *usagepkg.Store satisfies it.
type Accountant interface {
	InsertPending(context.Context, usagepkg.PendingRequest) (int64, error)
	MarkUpstreamDispatched(context.Context, int64, time.Time) error
	Finalize(context.Context, int64, usagepkg.FinalizedRequest) error
}

func pendingRequest(exch *exchange, host string, kind usagepkg.RequestKind) usagepkg.PendingRequest {
	return usagepkg.PendingRequest{
		StartedAt:       exch.start,
		Kind:            kind,
		Host:            host,
		Context:         exch.context,
		Route:           exch.route,
		RequestModel:    exch.requestModel,
		CredentialClass: exch.credentialClass,
	}
}

func requestKind(r *http.Request) usagepkg.RequestKind {
	if r.Method != http.MethodPost {
		return usagepkg.RequestOther
	}

	switch r.URL.Path {
	case messagesPath:
		return usagepkg.RequestMessages
	case messagesCountPath:
		return usagepkg.RequestCountTokens
	default:
		return usagepkg.RequestOther
	}
}

func (e *exchange) accountingSummary(capt *capture) summary {
	if !isSuccess(e.status) || capt == nil || capt.truncated {
		return summary{}
	}

	return summarize(capt.bytes(), e.contentType)
}

func (e *exchange) accountingOutcome(capt *capture, sum summary) usagepkg.Outcome {
	if !e.accountingDispatched {
		return usagepkg.OutcomeLocalRejection
	}
	if capt == nil {
		return usagepkg.OutcomeTransportFailure
	}
	if !isSuccess(e.status) {
		return usagepkg.OutcomeUpstreamHTTPFailure
	}
	if capt.truncated {
		return usagepkg.OutcomeResponseInterrupted
	}
	if !sum.complete {
		return usagepkg.OutcomeNormalizationFailure
	}

	return usagepkg.OutcomeSuccess
}

func (e *exchange) finalizedRequest(capt *capture) usagepkg.FinalizedRequest {
	sum := e.accountingSummary(capt)
	costStatus := usagepkg.CostStatusUnknown
	class, err := billing.RealizeClass(e.route.BillingPolicy(), e.route.BillingClass, e.credentialClass)
	if err == nil && (sum.providerCostPresent && sum.providerCostValid ||
		class == billing.ClassSubscription || class == billing.ClassSelfHosted) {
		costStatus = usagepkg.CostStatusKnown
	}

	return usagepkg.FinalizedRequest{
		CompletedAt:                       time.Now(),
		Outcome:                           e.accountingOutcome(capt, sum),
		HTTPStatus:                        e.status,
		ResponseID:                        sum.responseID,
		NormalizationComplete:             sum.complete,
		InputTokens:                       sum.inputTokens,
		InputTokensPresent:                sum.inputTokensPresent,
		OutputTokens:                      sum.outputTokens,
		OutputTokensPresent:               sum.outputTokensPresent,
		CacheCreationInputTokens:          sum.cacheCreationInputTokens,
		CacheCreationInputTokensPresent:   sum.cacheCreationInputTokensPresent,
		CacheReadInputTokens:              sum.cacheReadInputTokens,
		CacheReadInputTokensPresent:       sum.cacheReadInputTokensPresent,
		CacheCreation5mInputTokens:        sum.cacheCreation5mInputTokens,
		CacheCreation5mInputTokensPresent: sum.cacheCreation5mInputTokensPresent,
		CacheCreation1hInputTokens:        sum.cacheCreation1hInputTokens,
		CacheCreation1hInputTokensPresent: sum.cacheCreation1hInputTokensPresent,
		ProviderCostPresent:               sum.providerCostPresent,
		ProviderCostValid:                 sum.providerCostValid,
		ProviderCost:                      sum.providerCost,
		StopReason:                        sum.stopReason,
		BlockShape:                        sum.blockTypes,
		DurationMS:                        e.durationMS,
		UpstreamMS:                        e.upstreamMS,
		CostStatus:                        costStatus,
	}
}

func (p *Proxy) finalizeAccounting(ctx context.Context, exch *exchange, capt *capture) {
	finalized := exch.finalizedRequest(capt)
	if err := p.accountant.Finalize(context.WithoutCancel(ctx), exch.accountingID, finalized); err != nil {
		p.logger.ErrorContext(context.WithoutCancel(ctx), "usage accounting finalization failed",
			slog.Int64("request_id", exch.accountingID), slog.Any("error", err))
	}
}
