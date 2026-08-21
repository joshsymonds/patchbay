package usage

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSummarizeUsageAggregatesRequestedWindow(t *testing.T) {
	t.Parallel()

	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	windowStart := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(time.Hour)

	rows := []summaryFixtureRow{
		{
			startedAt:       windowStart,
			kind:            "messages",
			state:           "finalized",
			context:         "personal",
			billingClass:    "metered",
			costBasis:       "provider_reported",
			finalCost:       11,
			input:           10,
			output:          2,
			cacheRead:       3,
			cacheCreation:   99,
			cacheCreation5m: 4,
			cacheCreation1h: 5,
		},
		{
			startedAt:     windowStart.Add(time.Nanosecond),
			kind:          "messages",
			state:         "finalized",
			context:       "personal",
			billingClass:  "subscription",
			costBasis:     "zero_marginal",
			finalCost:     0,
			input:         20,
			output:        4,
			cacheRead:     6,
			cacheCreation: 7,
		},
		{
			startedAt:       windowStart.Add(2 * time.Nanosecond),
			kind:            "messages",
			state:           "finalized",
			context:         "work",
			billingClass:    "metered",
			costBasis:       "rate_card",
			finalCost:       13,
			input:           30,
			output:          6,
			cacheRead:       9,
			cacheCreation5m: 8,
		},
		{
			startedAt:       windowStart.Add(3 * time.Nanosecond),
			kind:            "messages",
			state:           "finalized",
			context:         "work",
			costBasis:       "unknown",
			input:           40,
			output:          8,
			cacheRead:       12,
			cacheCreation1h: 9,
		},
		{
			startedAt:     windowStart.Add(4 * time.Nanosecond),
			kind:          "messages",
			state:         "finalized",
			context:       "work",
			input:         50,
			output:        10,
			cacheRead:     15,
			cacheCreation: 11,
		},
		{
			startedAt:    windowStart.Add(5 * time.Nanosecond),
			kind:         "messages",
			state:        "pending",
			context:      "personal",
			billingClass: "metered",
		},
		{
			startedAt: windowStart.Add(6 * time.Nanosecond),
			kind:      "count_tokens",
			state:     "pending",
			context:   "personal",
		},
		{
			startedAt: windowStart.Add(7 * time.Nanosecond),
			kind:      "other",
			state:     "finalized",
			context:   "work",
		},
		{
			startedAt:    windowStart.Add(-time.Nanosecond),
			kind:         "messages",
			state:        "finalized",
			context:      "personal",
			billingClass: "metered",
			costBasis:    "provider_reported",
			finalCost:    100,
		},
		{
			startedAt:    windowEnd,
			kind:         "messages",
			state:        "finalized",
			context:      "personal",
			billingClass: "metered",
			costBasis:    "provider_reported",
			finalCost:    100,
		},
	}
	for _, row := range rows {
		insertSummaryFixtureRow(t, store.db, row)
	}

	got, err := store.SummarizeUsage(
		t.Context(),
		SummaryFilter{Since: windowStart, Until: windowEnd},
	)
	if err != nil {
		t.Fatalf("SummarizeUsage() error = %v", err)
	}

	want := Summary{
		Requests: RequestCounts{Messages: 6, CountTokens: 1, Other: 1},
		Messages: MessageSummary{
			Finalized:          5,
			Pending:            1,
			KnownCostNanoUSD:   24,
			UnknownCostRows:    2,
			InputTokens:        150,
			OutputTokens:       30,
			CacheReadTokens:    45,
			CacheCreationTotal: 44,
			ByBillingClass: map[string]CostSummary{
				"metered":      {Requests: 3, KnownCostNanoUSD: 24, UnknownCostRows: 0},
				"subscription": {Requests: 1, KnownCostNanoUSD: 0, UnknownCostRows: 0},
				"unknown":      {Requests: 2, KnownCostNanoUSD: 0, UnknownCostRows: 2},
			},
			ByCostBasis: map[string]CostSummary{
				"provider_reported": {Requests: 1, KnownCostNanoUSD: 11, UnknownCostRows: 0},
				"rate_card":         {Requests: 1, KnownCostNanoUSD: 13, UnknownCostRows: 0},
				"zero_marginal":     {Requests: 1, KnownCostNanoUSD: 0, UnknownCostRows: 0},
				"unknown":           {Requests: 3, KnownCostNanoUSD: 0, UnknownCostRows: 2},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SummarizeUsage() = %#v; want %#v", got, want)
	}
}

func TestSummarizeUsageFiltersContextAndEmptyResult(t *testing.T) {
	t.Parallel()

	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	start := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	insertSummaryFixtureRow(
		t,
		store.db,
		summaryFixtureRow{
			startedAt:    start,
			kind:         "messages",
			state:        "finalized",
			context:      "personal",
			billingClass: "metered",
			costBasis:    "provider_reported",
			finalCost:    9,
		},
	)
	insertSummaryFixtureRow(
		t,
		store.db,
		summaryFixtureRow{
			startedAt:    start,
			kind:         "messages",
			state:        "pending",
			context:      "work",
			billingClass: "subscription",
		},
	)

	got, err := store.SummarizeUsage(
		t.Context(),
		SummaryFilter{Since: start, Until: start.Add(time.Hour), Context: "work"},
	)
	if err != nil {
		t.Fatalf("SummarizeUsage(work) error = %v", err)
	}
	want := Summary{
		Requests: RequestCounts{Messages: 1},
		Messages: MessageSummary{
			Pending: 1,
			ByBillingClass: map[string]CostSummary{
				"subscription": {Requests: 1},
			},
			ByCostBasis: map[string]CostSummary{
				"unknown": {Requests: 1},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SummarizeUsage(work) = %#v; want %#v", got, want)
	}

	empty, err := store.SummarizeUsage(
		t.Context(),
		SummaryFilter{Since: start.Add(2 * time.Hour), Until: start.Add(3 * time.Hour)},
	)
	if err != nil {
		t.Fatalf("SummarizeUsage(empty) error = %v", err)
	}
	if !reflect.DeepEqual(
		empty,
		Summary{
			Requests: RequestCounts{},
			Messages: MessageSummary{
				ByBillingClass: map[string]CostSummary{},
				ByCostBasis:    map[string]CostSummary{},
			},
		},
	) {
		t.Fatalf("SummarizeUsage(empty) = %#v; want an empty, JSON-stable summary", empty)
	}
}

func TestSummarizeUsageReadsOneSnapshotAcrossFinalization(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	request := validPendingRequest()
	id, err := store.InsertPending(t.Context(), request)
	if err != nil {
		t.Fatalf("InsertPending() error = %v", err)
	}

	afterMessages := make(chan struct{})
	continueSummary := make(chan struct{})
	ctx := context.WithValue(t.Context(), summaryAfterMessagesHookKey{}, func() {
		close(afterMessages)
		<-continueSummary
	})
	type result struct {
		summary Summary
		err     error
	}
	finished := make(chan result, 1)
	go func() {
		summary, summaryErr := store.SummarizeUsage(ctx, SummaryFilter{
			Since: request.StartedAt.Add(-time.Second),
			Until: request.StartedAt.Add(2 * time.Second),
		})
		finished <- result{summary: summary, err: summaryErr}
	}()

	select {
	case <-afterMessages:
	case <-time.After(time.Second):
		t.Fatal("summary did not read its message totals")
	}
	if err := store.Finalize(t.Context(), id, validFinalizedRequest()); err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	close(continueSummary)

	got := <-finished
	if got.err != nil {
		t.Fatalf("SummarizeUsage() error = %v", got.err)
	}
	if got.summary.Messages.Finalized != 0 || got.summary.Messages.Pending != 1 ||
		got.summary.Messages.KnownCostNanoUSD != 0 || got.summary.Messages.UnknownCostRows != 0 {
		t.Fatalf(
			"message totals = %#v; want the complete pre-finalization snapshot",
			got.summary.Messages,
		)
	}
	if billing := got.summary.Messages.ByBillingClass; !reflect.DeepEqual(
		billing,
		map[string]CostSummary{"subscription": {Requests: 1}},
	) {
		t.Fatalf("billing-class groups = %#v; want the complete pre-finalization snapshot", billing)
	}
	if basis := got.summary.Messages.ByCostBasis; !reflect.DeepEqual(
		basis,
		map[string]CostSummary{"unknown": {Requests: 1}},
	) {
		t.Fatalf("cost-basis groups = %#v; want the complete pre-finalization snapshot", basis)
	}
}

func TestSummarizeUsageRejectsInvalidFilters(t *testing.T) {
	t.Parallel()

	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	at := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	for _, filter := range []SummaryFilter{
		{Since: at, Until: at},
		{Since: at.Add(time.Nanosecond), Until: at},
		{Since: at, Until: at.Add(time.Hour), Context: "Work"},
		{Since: at, Until: at.Add(time.Hour), Context: "work/dev"},
	} {
		if _, err := store.SummarizeUsage(t.Context(), filter); err == nil {
			t.Fatalf("SummarizeUsage(%+v) returned nil error", filter)
		}
	}
}

type summaryFixtureRow struct {
	startedAt       time.Time
	kind            string
	state           string
	context         string
	billingClass    string
	costBasis       string
	finalCost       int64
	input           int64
	output          int64
	cacheRead       int64
	cacheCreation   int64
	cacheCreation5m int64
	cacheCreation1h int64
}

func insertSummaryFixtureRow(t *testing.T, db *sql.DB, row summaryFixtureRow) {
	t.Helper()

	var billingClass, costBasis, finalCost any
	if row.billingClass != "" {
		billingClass = row.billingClass
	}
	if row.costBasis != "" {
		costBasis = row.costBasis
	}
	if row.costBasis == "provider_reported" || row.costBasis == "rate_card" ||
		row.costBasis == "zero_marginal" {
		finalCost = row.finalCost
	}
	if row.state == "" {
		row.state = "pending"
	}
	if row.context == "" {
		row.context = "personal"
	}

	_, err := db.ExecContext(
		t.Context(),
		`INSERT INTO accounting_requests (
		started_at, request_kind, lifecycle_state, upstream_contacted, route_host,
		route_context, route_upstream, request_model, resolved_model, auth_mode,
		credential_class, billing_policy, billing_class, cost_status, cost_basis,
		final_cost_nano_usd, input_tokens, output_tokens, cache_read_input_tokens,
		cache_creation_input_tokens, cache_creation_5m_input_tokens, cache_creation_1h_input_tokens
	) VALUES (?, ?, ?, 0, 'host', ?, 'upstream', 'model', 'model', 'forward',
		'oauth', 'credential_derived', ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.startedAt.UnixNano(),
		row.kind,
		row.state,
		row.context,
		billingClass,
		costBasis,
		finalCost,
		nullableSummaryToken(
			row.input,
		),
		nullableSummaryToken(row.output),
		nullableSummaryToken(row.cacheRead),
		nullableSummaryToken(
			row.cacheCreation,
		),
		nullableSummaryToken(row.cacheCreation5m),
		nullableSummaryToken(row.cacheCreation1h),
	)
	if err != nil {
		t.Fatalf("insert summary fixture row: %v", err)
	}
}

func nullableSummaryToken(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func TestSummaryFilterInvalidContextDoesNotReachSQLite(t *testing.T) {
	t.Parallel()

	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	if err := store.db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	_, err := store.SummarizeUsage(context.Background(), SummaryFilter{
		Since:   time.Now().UTC(),
		Until:   time.Now().UTC().Add(time.Hour),
		Context: "invalid/context",
	})
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("SummarizeUsage() error = %v; want context validation error", err)
	}
}
