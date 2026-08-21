package usage

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/accounting"
	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/registry"
)

func TestLoadRateCardsInsertsNoopsAndRejectsConflictsAtomically(t *testing.T) {
	t.Parallel()

	store := openStore(t, t.TempDir()+"/usage.sqlite")
	first := testRateCard("openrouter/model", time.Unix(1_776_912_345, 0).UTC())
	second := testRateCard("openrouter/other", first.EffectiveFrom)
	if err := store.LoadRateCards(context.Background(), []RateCard{first, second}); err != nil {
		t.Fatalf("LoadRateCards() returned error: %v", err)
	}
	if err := store.LoadRateCards(context.Background(), []RateCard{first, second}); err != nil {
		t.Fatalf("idempotent LoadRateCards() returned error: %v", err)
	}

	conflict := first
	conflict.InputNanoUSDPerMillion++
	newCard := testRateCard("openrouter/new", first.EffectiveFrom)
	err := store.LoadRateCards(context.Background(), []RateCard{newCard, conflict})
	if err == nil || !strings.Contains(err.Error(), first.Model) ||
		!strings.Contains(err.Error(), "effective") {
		t.Fatalf("conflicting LoadRateCards() error = %v; want model and effective time", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM rate_cards`).Scan(&count); err != nil {
		t.Fatalf("count rate cards: %v", err)
	}
	if count != 2 {
		t.Fatalf("rate card count after conflict = %d; want 2", count)
	}
}

func TestRateCardImmutabilityAndAtomicSQLFailure(t *testing.T) {
	t.Parallel()

	store := openStore(t, t.TempDir()+"/usage.sqlite")
	first := testRateCard("openrouter/first", time.Unix(1_776_912_345, 0).UTC())
	if err := store.LoadRateCards(context.Background(), []RateCard{first}); err != nil {
		t.Fatalf("LoadRateCards() returned error: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE rate_cards SET source = 'changed' WHERE model = ?`, first.Model); err == nil {
		t.Fatal("rate-card UPDATE returned nil error")
	}
	if _, err := store.db.Exec(`DELETE FROM rate_cards WHERE model = ?`, first.Model); err == nil {
		t.Fatal("rate-card DELETE returned nil error")
	}

	blocked := testRateCard("openrouter/blocked", first.EffectiveFrom)
	if _, err := store.db.Exec(`CREATE TRIGGER reject_rate_card_insert
		BEFORE INSERT ON rate_cards WHEN NEW.model = 'openrouter/blocked'
		BEGIN SELECT RAISE(ABORT, 'test rejection'); END`); err != nil {
		t.Fatalf("create insert rejection trigger: %v", err)
	}
	newCard := testRateCard("openrouter/new", first.EffectiveFrom)
	if err := store.LoadRateCards(context.Background(), []RateCard{newCard, blocked}); err == nil {
		t.Fatal("trigger-rejected LoadRateCards() returned nil error")
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM rate_cards`).Scan(&count); err != nil {
		t.Fatalf("count rate cards: %v", err)
	}
	if count != 1 {
		t.Fatalf("rate card count after SQL failure = %d; want 1", count)
	}
}

func TestRateCardLookupMatchesExactlyAndUsesLatestEffectiveCard(t *testing.T) {
	t.Parallel()

	store := openStore(t, t.TempDir()+"/usage.sqlite")
	at := validPendingRequest().StartedAt
	older := rateCardWithRates("exact/model", at.Add(-time.Second), 1, 0, 0, 0, 0, 0)
	boundary := rateCardWithRates("exact/model", at, 2, 0, 0, 0, 0, 0)
	after := rateCardWithRates("exact/model", at.Add(time.Nanosecond), 3, 0, 0, 0, 0, 0)
	prefix := rateCardWithRates("exact/model-preview", at, 4, 0, 0, 0, 0, 0)
	if err := store.LoadRateCards(context.Background(), []RateCard{older, boundary, after, prefix}); err != nil {
		t.Fatalf("LoadRateCards() returned error: %v", err)
	}
	card, found, err := store.lookupEffectiveRateCard(context.Background(), "exact/model", at.UnixNano())
	if err != nil || !found || card.InputNanoUSDPerMillion != 2 {
		t.Fatalf("boundary lookup = %#v, found=%t, err=%v; want boundary rate 2", card, found, err)
	}
	_, found, err = store.lookupEffectiveRateCard(
		context.Background(),
		"exact/model-pre",
		at.UnixNano(),
	)
	if err != nil || found {
		t.Fatalf("non-exact lookup found=%t, err=%v; want no match", found, err)
	}
}

func TestFinalizeDerivesFinalCostByPrecedenceAndRateCard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		request    PendingRequest
		configure  func(*FinalizedRequest)
		cards      []RateCard
		wantBasis  string
		wantAmount sql.NullInt64
		wantCard   bool
	}{
		{
			name:       "provider cost beats subscription and rate card",
			request:    validPendingRequest(),
			cards:      []RateCard{testRateCard("claude-opus-5", validPendingRequest().StartedAt)},
			wantBasis:  "provider_reported",
			wantAmount: sql.NullInt64{Int64: int64(validFinalizedRequest().ProviderCost), Valid: true},
		},
		{
			name:    "zero provider cost beats subscription",
			request: validPendingRequest(),
			configure: func(finalized *FinalizedRequest) {
				finalized.ProviderCost = 0
			},
			wantBasis:  "provider_reported",
			wantAmount: sql.NullInt64{Valid: true},
		},
		{
			name:    "subscription is zero marginal without provider cost",
			request: validPendingRequest(),
			configure: func(finalized *FinalizedRequest) {
				finalized.ProviderCostPresent = false
				finalized.ProviderCostValid = false
				finalized.ProviderCost = 0
			},
			wantBasis:  "zero_marginal",
			wantAmount: sql.NullInt64{Valid: true},
		},
		{
			name:    "self-hosted is zero marginal without provider cost",
			request: selfHostedPendingRequest(),
			configure: func(finalized *FinalizedRequest) {
				finalized.ProviderCostPresent = false
				finalized.ProviderCostValid = false
				finalized.ProviderCost = 0
			},
			wantBasis:  "zero_marginal",
			wantAmount: sql.NullInt64{Valid: true},
		},
		{
			name:    "rate card uses nested cache rates instead of base creation",
			request: meteredPendingRequest(),
			cards: []RateCard{rateCardWithRates(
				"claude-opus-5", validPendingRequest().StartedAt,
				1, 2, 3, 4, 5, 6,
			)},
			configure: func(finalized *FinalizedRequest) {
				finalized.ProviderCostPresent = false
				finalized.ProviderCostValid = false
				finalized.ProviderCost = 0
				finalized.InputTokens, finalized.OutputTokens = 1_000_000, 1_000_000
				finalized.CacheReadInputTokens = 1_000_000
				finalized.CacheCreationInputTokens = 1_000_000
				finalized.CacheCreation5mInputTokens = 1_000_000
				finalized.CacheCreation1hInputTokens = 1_000_000
			},
			wantBasis:  "rate_card",
			wantAmount: sql.NullInt64{Int64: 17, Valid: true},
			wantCard:   true,
		},
		{
			name:    "missing input is unknown",
			request: meteredPendingRequest(),
			configure: func(finalized *FinalizedRequest) {
				finalized.ProviderCostPresent = false
				finalized.ProviderCostValid = false
				finalized.ProviderCost = 0
				finalized.InputTokensPresent = false
				finalized.InputTokens = 0
			},
			cards:      []RateCard{testRateCard("claude-opus-5", validPendingRequest().StartedAt)},
			wantBasis:  "unknown",
			wantAmount: sql.NullInt64{},
		},
		{
			name:    "missing output is unknown",
			request: meteredPendingRequest(),
			configure: func(finalized *FinalizedRequest) {
				finalized.ProviderCostPresent = false
				finalized.ProviderCostValid = false
				finalized.ProviderCost = 0
				finalized.OutputTokensPresent = false
				finalized.OutputTokens = 0
			},
			cards:      []RateCard{testRateCard("claude-opus-5", validPendingRequest().StartedAt)},
			wantBasis:  "unknown",
			wantAmount: sql.NullInt64{},
		},
		{
			name:    "normalization failure is unknown",
			request: meteredPendingRequest(),
			configure: func(finalized *FinalizedRequest) {
				finalized.ProviderCostPresent = false
				finalized.ProviderCostValid = false
				finalized.ProviderCost = 0
				finalized.NormalizationComplete = false
			},
			cards:      []RateCard{testRateCard("claude-opus-5", validPendingRequest().StartedAt)},
			wantBasis:  "unknown",
			wantAmount: sql.NullInt64{},
		},
		{
			name:    "unsuccessful outcome is unknown",
			request: meteredPendingRequest(),
			configure: func(finalized *FinalizedRequest) {
				finalized.ProviderCostPresent = false
				finalized.ProviderCostValid = false
				finalized.ProviderCost = 0
				finalized.Outcome = OutcomeTransportFailure
			},
			cards:      []RateCard{testRateCard("claude-opus-5", validPendingRequest().StartedAt)},
			wantBasis:  "unknown",
			wantAmount: sql.NullInt64{},
		},
		{
			name:    "no matching card is unknown",
			request: meteredPendingRequest(),
			configure: func(finalized *FinalizedRequest) {
				finalized.ProviderCostPresent = false
				finalized.ProviderCostValid = false
				finalized.ProviderCost = 0
			},
			cards:      []RateCard{testRateCard("other/model", validPendingRequest().StartedAt)},
			wantBasis:  "unknown",
			wantAmount: sql.NullInt64{},
		},
		{
			name:    "base cache creation is priced without nested cache metrics",
			request: meteredPendingRequest(),
			configure: func(finalized *FinalizedRequest) {
				finalized.ProviderCostPresent = false
				finalized.ProviderCostValid = false
				finalized.ProviderCost = 0
				finalized.InputTokens, finalized.OutputTokens = 1_000_000, 1_000_000
				finalized.CacheReadInputTokens = 1_000_000
				finalized.CacheCreationInputTokens = 1_000_000
				finalized.CacheCreation5mInputTokensPresent = false
				finalized.CacheCreation1hInputTokensPresent = false
				finalized.CacheCreation5mInputTokens = 0
				finalized.CacheCreation1hInputTokens = 0
			},
			cards: []RateCard{rateCardWithRates(
				"claude-opus-5", validPendingRequest().StartedAt,
				1, 2, 3, 4, 5, 6,
			)},
			wantBasis:  "rate_card",
			wantAmount: sql.NullInt64{Int64: 10, Valid: true},
			wantCard:   true,
		},
		{
			name:    "overflow is unknown",
			request: meteredPendingRequest(),
			configure: func(finalized *FinalizedRequest) {
				finalized.ProviderCostPresent = false
				finalized.ProviderCostValid = false
				finalized.ProviderCost = 0
				finalized.InputTokens = math.MaxInt64
			},
			cards: []RateCard{rateCardWithRates(
				"claude-opus-5", validPendingRequest().StartedAt,
				accounting.NanoUSD(math.MaxInt64), 0, 0, 0, 0, 0,
			)},
			wantBasis:  "unknown",
			wantAmount: sql.NullInt64{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store := openStore(t, t.TempDir()+"/usage.sqlite")
			if err := store.LoadRateCards(context.Background(), test.cards); err != nil {
				t.Fatalf("LoadRateCards() returned error: %v", err)
			}
			id, err := store.InsertPending(context.Background(), test.request)
			if err != nil {
				t.Fatalf("InsertPending() returned error: %v", err)
			}
			finalized := validFinalizedRequest()
			if test.configure != nil {
				test.configure(&finalized)
			}
			if err := store.Finalize(context.Background(), id, finalized); err != nil {
				t.Fatalf("Finalize() returned error: %v", err)
			}
			got := readStoredRequest(t, store.db, id)
			if got.costBasis != (sql.NullString{String: test.wantBasis, Valid: true}) ||
				got.finalCostNanoUSD != test.wantAmount {
				t.Fatalf("derived final cost = %#v/%#v; want %q/%#v",
					got.costBasis, got.finalCostNanoUSD, test.wantBasis, test.wantAmount)
			}
			if got.rateCardID.Valid != test.wantCard {
				t.Fatalf("rate card ID = %#v; want present=%t", got.rateCardID, test.wantCard)
			}
		})
	}
}

func TestFinalizeUsesEffectiveRateCardAndPreservesFirstDerivationOnReplay(t *testing.T) {
	t.Parallel()

	store := openStore(t, t.TempDir()+"/usage.sqlite")
	startedAt := validPendingRequest().StartedAt
	oldCard := rateCardWithRates("claude-opus-5", startedAt, 1, 0, 0, 0, 0, 0)
	newCard := rateCardWithRates("claude-opus-5", startedAt.Add(time.Nanosecond), 2, 0, 0, 0, 0, 0)
	wrongModel := rateCardWithRates("claude-opus-5-preview", startedAt, 9, 0, 0, 0, 0, 0)
	if err := store.LoadRateCards(context.Background(), []RateCard{oldCard, newCard, wrongModel}); err != nil {
		t.Fatalf("LoadRateCards() returned error: %v", err)
	}
	id, err := store.InsertPending(context.Background(), meteredPendingRequest())
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	finalized := validFinalizedRequest()
	finalized.ProviderCostPresent = false
	finalized.ProviderCostValid = false
	finalized.ProviderCost = 0
	finalized.InputTokens = 1_000_000
	finalized.OutputTokens = 0
	finalized.CacheCreationInputTokensPresent = false
	finalized.CacheReadInputTokensPresent = false
	finalized.CacheCreation5mInputTokensPresent = false
	finalized.CacheCreation1hInputTokensPresent = false
	finalized.CacheCreationInputTokens = 0
	finalized.CacheReadInputTokens = 0
	finalized.CacheCreation5mInputTokens = 0
	finalized.CacheCreation1hInputTokens = 0
	if err := store.Finalize(context.Background(), id, finalized); err != nil {
		t.Fatalf("Finalize() returned error: %v", err)
	}
	before := readStoredRequest(t, store.db, id)
	if before.finalCostNanoUSD != (sql.NullInt64{Int64: 1, Valid: true}) {
		t.Fatalf("boundary card price = %#v; want 1", before.finalCostNanoUSD)
	}
	lateCard := rateCardWithRates("claude-opus-5", startedAt.Add(-time.Nanosecond), 8, 0, 0, 0, 0, 0)
	if err := store.LoadRateCards(context.Background(), []RateCard{lateCard}); err != nil {
		t.Fatalf("LoadRateCards() after finalization returned error: %v", err)
	}
	if err := store.Finalize(context.Background(), id, finalized); err != nil {
		t.Fatalf("exact replay after card load returned error: %v", err)
	}
	if got := readStoredRequest(t, store.db, id); got != before {
		t.Fatalf("row changed after exact replay:\n got %#v\nwant %#v", got, before)
	}
	finalized.OutputTokens = 1
	if err := store.Finalize(context.Background(), id, finalized); err == nil {
		t.Fatal("differing replay returned nil error")
	}
	if got := readStoredRequest(t, store.db, id); got != before {
		t.Fatalf("row changed after differing replay:\n got %#v\nwant %#v", got, before)
	}
}

func TestFinalizeExactReplayDoesNotDeriveAfterRateCardAdded(t *testing.T) {
	t.Parallel()

	store := openStore(t, t.TempDir()+"/usage.sqlite")
	id, err := store.InsertPending(context.Background(), meteredPendingRequest())
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	finalized := validFinalizedRequest()
	finalized.ProviderCostPresent = false
	finalized.ProviderCostValid = false
	finalized.ProviderCost = 0
	if err := store.Finalize(context.Background(), id, finalized); err != nil {
		t.Fatalf("first Finalize() returned error: %v", err)
	}
	before := readStoredRequest(t, store.db, id)
	if before.costBasis != (sql.NullString{String: "unknown", Valid: true}) {
		t.Fatalf("first final cost basis = %#v; want unknown", before.costBasis)
	}
	if err := store.LoadRateCards(context.Background(), []RateCard{
		testRateCard("claude-opus-5", validPendingRequest().StartedAt),
	}); err != nil {
		t.Fatalf("LoadRateCards() returned error: %v", err)
	}
	if err := store.Finalize(context.Background(), id, finalized); err != nil {
		t.Fatalf("exact replay returned error: %v", err)
	}
	if got := readStoredRequest(t, store.db, id); got != before {
		t.Fatalf("row changed after rate card addition and exact replay:\n got %#v\nwant %#v", got, before)
	}
}

func meteredPendingRequest() PendingRequest {
	request := validPendingRequest()
	request.CredentialClass = billing.CredentialAPIKey
	return request
}

func selfHostedPendingRequest() PendingRequest {
	request := validPendingRequest()
	request.Route = registry.Route{
		Alias:           "self-hosted",
		BaseURL:         "http://localhost:9000",
		Model:           "local/model",
		Auth:            registry.AuthInject,
		BillingClass:    billing.ClassSelfHosted,
		BillingClassSet: true,
	}
	request.CredentialClass = billing.CredentialNone
	return request
}

func rateCardWithRates(
	model string,
	effectiveFrom time.Time,
	input, output, cacheRead, cacheCreation, cacheCreation5m, cacheCreation1h accounting.NanoUSD,
) RateCard {
	return RateCard{
		Model:                            model,
		EffectiveFrom:                    effectiveFrom,
		InputNanoUSDPerMillion:           input,
		OutputNanoUSDPerMillion:          output,
		CacheReadNanoUSDPerMillion:       cacheRead,
		CacheCreationNanoUSDPerMillion:   cacheCreation,
		CacheCreation5mNanoUSDPerMillion: cacheCreation5m,
		CacheCreation1hNanoUSDPerMillion: cacheCreation1h,
		Source:                           "test",
	}
}

func testRateCard(model string, effectiveFrom time.Time) RateCard {
	return RateCard{
		Model:                            model,
		EffectiveFrom:                    effectiveFrom,
		InputNanoUSDPerMillion:           accounting.NanoUSD(3_000_000_000),
		OutputNanoUSDPerMillion:          accounting.NanoUSD(15_000_000_000),
		CacheReadNanoUSDPerMillion:       accounting.NanoUSD(300_000_000),
		CacheCreationNanoUSDPerMillion:   accounting.NanoUSD(3_750_000_000),
		CacheCreation5mNanoUSDPerMillion: accounting.NanoUSD(3_750_000_000),
		CacheCreation1hNanoUSDPerMillion: accounting.NanoUSD(6_000_000_000),
		Source:                           "test",
	}
}
