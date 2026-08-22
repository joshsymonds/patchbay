package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/joshsymonds/patchbay/internal/registry"
)

// SummaryFilter selects the half-open time window and optional context a usage
// summary covers.
type SummaryFilter struct {
	Since   time.Time
	Until   time.Time
	Context string
}

// Summary is the durable request accounting aggregate for one filter.
type Summary struct {
	Requests RequestCounts  `json:"requests"`
	Messages MessageSummary `json:"messages"`
}

// RequestCounts counts every selected request, regardless of lifecycle state.
type RequestCounts struct {
	Messages    int64 `json:"messages"`
	CountTokens int64 `json:"count_tokens"`
	Other       int64 `json:"other"`
}

// MessageSummary aggregates selected Messages API requests.
type MessageSummary struct {
	Finalized          int64                  `json:"finalized"`
	Pending            int64                  `json:"pending"`
	KnownCostNanoUSD   int64                  `json:"known_cost_nano_usd"`
	UnknownCostRows    int64                  `json:"unknown_cost_rows"`
	InputTokens        int64                  `json:"input_tokens"`
	OutputTokens       int64                  `json:"output_tokens"`
	CacheReadTokens    int64                  `json:"cache_read_tokens"`
	CacheCreationTotal int64                  `json:"cache_creation_total"`
	ByBillingClass     map[string]CostSummary `json:"by_billing_class"`
	ByCostBasis        map[string]CostSummary `json:"by_cost_basis"`
}

// CostSummary reports message counts and finalized cost certainty for one
// billing-class or cost-basis grouping.
type CostSummary struct {
	Requests         int64 `json:"requests"`
	KnownCostNanoUSD int64 `json:"known_cost_nano_usd"`
	UnknownCostRows  int64 `json:"unknown_cost_rows"`
}

const requestCountsSummaryQuery = `SELECT request_kind, COUNT(*)
	FROM accounting_requests
	WHERE started_at >= ? AND started_at < ? AND (? = '' OR route_context = ?)
	GROUP BY request_kind`

const messagesSummaryQuery = `SELECT
	COALESCE(SUM(CASE WHEN lifecycle_state = 'finalized' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN lifecycle_state = 'pending' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN lifecycle_state = 'finalized'
		AND cost_basis IN ('provider_reported', 'rate_card', 'zero_marginal')
		THEN COALESCE(final_cost_nano_usd, 0) ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN lifecycle_state = 'finalized'
		AND (cost_basis = 'unknown' OR cost_basis IS NULL) THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN lifecycle_state = 'finalized' THEN COALESCE(input_tokens, 0) ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN lifecycle_state = 'finalized' THEN COALESCE(output_tokens, 0) ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN lifecycle_state = 'finalized' THEN COALESCE(cache_read_input_tokens, 0) ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN lifecycle_state = 'finalized' THEN
		CASE WHEN cache_creation_5m_input_tokens IS NOT NULL OR cache_creation_1h_input_tokens IS NOT NULL
		THEN COALESCE(cache_creation_5m_input_tokens, 0) + COALESCE(cache_creation_1h_input_tokens, 0)
		ELSE COALESCE(cache_creation_input_tokens, 0) END
		ELSE 0 END), 0)
	FROM accounting_requests
	WHERE request_kind = 'messages'
		AND started_at >= ? AND started_at < ? AND (? = '' OR route_context = ?)`

const billingClassSummaryQuery = `SELECT
	COALESCE(billing_class, 'unknown'),
	COUNT(*),
	COALESCE(SUM(CASE WHEN lifecycle_state = 'finalized'
		AND cost_basis IN ('provider_reported', 'rate_card', 'zero_marginal')
		THEN COALESCE(final_cost_nano_usd, 0) ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN lifecycle_state = 'finalized'
		AND (cost_basis = 'unknown' OR cost_basis IS NULL) THEN 1 ELSE 0 END), 0)
	FROM accounting_requests
	WHERE request_kind = 'messages'
		AND started_at >= ? AND started_at < ? AND (? = '' OR route_context = ?)
	GROUP BY COALESCE(billing_class, 'unknown')`

const costBasisSummaryQuery = `SELECT
	COALESCE(cost_basis, 'unknown'),
	COUNT(*),
	COALESCE(SUM(CASE WHEN lifecycle_state = 'finalized'
		AND cost_basis IN ('provider_reported', 'rate_card', 'zero_marginal')
		THEN COALESCE(final_cost_nano_usd, 0) ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN lifecycle_state = 'finalized'
		AND (cost_basis = 'unknown' OR cost_basis IS NULL) THEN 1 ELSE 0 END), 0)
	FROM accounting_requests
	WHERE request_kind = 'messages'
		AND started_at >= ? AND started_at < ? AND (? = '' OR route_context = ?)
	GROUP BY COALESCE(cost_basis, 'unknown')`

type summaryQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type summaryAfterMessagesHookKey struct{}

// SummarizeUsage reads the selected durable accounting rows without taking the
// write permit, so a long summary cannot block recording a request.
func (store *Store) SummarizeUsage(
	ctx context.Context,
	filter SummaryFilter,
) (result Summary, err error) {
	if validationErr := validateSummaryFilter(ctx, filter); validationErr != nil {
		return Summary{}, validationErr
	}

	tx, beginErr := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if beginErr != nil {
		return Summary{}, fmt.Errorf("summarize usage: begin read transaction: %w", beginErr)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil &&
			!errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(
				err,
				fmt.Errorf("summarize usage: roll back read transaction: %w", rollbackErr),
			)
			result = Summary{}
		}
	}()

	summary := Summary{Messages: MessageSummary{
		ByBillingClass: make(map[string]CostSummary),
		ByCostBasis:    make(map[string]CostSummary),
	}}
	args := summaryArguments(filter)

	if summaryErr := summarizeRequestCounts(ctx, tx, args, &summary.Requests); summaryErr != nil {
		return Summary{}, summaryErr
	}
	if summaryErr := summarizeMessages(ctx, tx, args, &summary.Messages); summaryErr != nil {
		return Summary{}, summaryErr
	}
	if hook, ok := ctx.Value(summaryAfterMessagesHookKey{}).(func()); ok {
		hook()
	}
	if summaryErr := summarizeCostGroups(
		ctx,
		tx,
		billingClassSummaryQuery,
		args,
		summary.Messages.ByBillingClass,
	); summaryErr != nil {
		return Summary{}, summaryErr
	}
	if summaryErr := summarizeCostGroups(
		ctx,
		tx,
		costBasisSummaryQuery,
		args,
		summary.Messages.ByCostBasis,
	); summaryErr != nil {
		return Summary{}, summaryErr
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return Summary{}, fmt.Errorf("summarize usage: commit read transaction: %w", commitErr)
	}

	return summary, nil
}

func validateSummaryFilter(ctx context.Context, filter SummaryFilter) error {
	if ctx == nil {
		return errors.New("summarize usage: context is nil")
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("summarize usage context: %w", contextErr)
	}
	if !filter.Until.After(filter.Since) {
		return errors.New("summarize usage: until must be after since")
	}
	if err := validateSummaryTime("since", filter.Since); err != nil {
		return err
	}
	if err := validateSummaryTime("until", filter.Until); err != nil {
		return err
	}
	if filter.Context != "" {
		if contextErr := registry.ValidID(filter.Context); contextErr != nil {
			return fmt.Errorf("summarize usage: context %q: %w", filter.Context, contextErr)
		}
	}
	return nil
}

func validateSummaryTime(name string, value time.Time) error {
	nanos := value.UnixNano()
	if !time.Unix(0, nanos).Equal(value) {
		return fmt.Errorf("summarize usage: %s cannot round-trip through UnixNano", name)
	}
	return nil
}

func summaryArguments(filter SummaryFilter) []any {
	return []any{filter.Since.UnixNano(), filter.Until.UnixNano(), filter.Context, filter.Context}
}

func summarizeRequestCounts(
	ctx context.Context,
	queryer summaryQueryer,
	args []any,
	counts *RequestCounts,
) (result error) {
	rows, queryErr := queryer.QueryContext(ctx, requestCountsSummaryQuery, args...)
	if queryErr != nil {
		return fmt.Errorf("summarize usage request counts: %w", queryErr)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			result = errors.Join(
				result,
				fmt.Errorf("summarize usage request counts: close rows: %w", closeErr),
			)
		}
	}()

	for rows.Next() {
		var kind string
		var count int64
		if scanErr := rows.Scan(&kind, &count); scanErr != nil {
			return fmt.Errorf("summarize usage request counts: scan row: %w", scanErr)
		}
		switch kind {
		case RequestMessages.String():
			counts.Messages += count
		case RequestCountTokens.String():
			counts.CountTokens += count
		default:
			counts.Other += count
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("summarize usage request counts: read rows: %w", rowsErr)
	}
	return nil
}

func summarizeMessages(
	ctx context.Context,
	queryer summaryQueryer,
	args []any,
	messages *MessageSummary,
) error {
	queryErr := queryer.QueryRowContext(ctx, messagesSummaryQuery, args...).Scan(
		&messages.Finalized,
		&messages.Pending,
		&messages.KnownCostNanoUSD,
		&messages.UnknownCostRows,
		&messages.InputTokens,
		&messages.OutputTokens,
		&messages.CacheReadTokens,
		&messages.CacheCreationTotal,
	)
	if queryErr != nil {
		return fmt.Errorf("summarize usage messages: %w", queryErr)
	}
	return nil
}

func summarizeCostGroups(
	ctx context.Context,
	queryer summaryQueryer,
	query string,
	args []any,
	groups map[string]CostSummary,
) (result error) {
	rows, queryErr := queryer.QueryContext(ctx, query, args...)
	if queryErr != nil {
		return fmt.Errorf("summarize usage cost groups: %w", queryErr)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			result = errors.Join(
				result,
				fmt.Errorf("summarize usage cost groups: close rows: %w", closeErr),
			)
		}
	}()

	for rows.Next() {
		var name string
		var summary CostSummary
		if scanErr := rows.Scan(
			&name,
			&summary.Requests,
			&summary.KnownCostNanoUSD,
			&summary.UnknownCostRows,
		); scanErr != nil {
			return fmt.Errorf("summarize usage cost groups: scan row: %w", scanErr)
		}
		groups[name] = summary
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("summarize usage cost groups: read rows: %w", rowsErr)
	}
	return nil
}
