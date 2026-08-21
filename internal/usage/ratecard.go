package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/joshsymonds/patchbay/internal/accounting"
)

// RateCard is an immutable per-model price schedule effective from one instant.
type RateCard struct {
	ID                               int64
	Model                            string
	EffectiveFrom                    time.Time
	InputNanoUSDPerMillion           accounting.NanoUSD
	OutputNanoUSDPerMillion          accounting.NanoUSD
	CacheReadNanoUSDPerMillion       accounting.NanoUSD
	CacheCreationNanoUSDPerMillion   accounting.NanoUSD
	CacheCreation5mNanoUSDPerMillion accounting.NanoUSD
	CacheCreation1hNanoUSDPerMillion accounting.NanoUSD
	Source                           string
	CreatedAt                        time.Time
}

type storedRateCard struct {
	id int64
	RateCard
	effectiveFrom int64
	createdAt     int64
}

// LoadRateCards records immutable price schedules, rejecting any conflict with
// a schedule already recorded for the same model and effective instant.
func (store *Store) LoadRateCards(ctx context.Context, cards []RateCard) error {
	if err := validateRateCardLoad(ctx, cards); err != nil {
		return err
	}
	if err := store.acquireWritePermit(ctx); err != nil {
		return fmt.Errorf("load rate cards: %w", err)
	}
	defer store.releaseWritePermit()

	return store.withRateCardTransaction(ctx, func(conn *sql.Conn) error {
		toInsert, err := collectRateCards(ctx, conn, cards)
		if err != nil {
			return err
		}
		return insertRateCards(ctx, conn, toInsert)
	})
}

func (store *Store) withRateCardTransaction(
	ctx context.Context,
	action func(*sql.Conn) error,
) (result error) {
	conn, connErr := store.db.Conn(ctx)
	if connErr != nil {
		return fmt.Errorf("load rate cards: acquire connection: %w", connErr)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("load rate cards: close connection: %w", closeErr))
		}
	}()
	if _, beginErr := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); beginErr != nil {
		return fmt.Errorf("load rate cards: begin transaction: %w", beginErr)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if _, rollbackErr := conn.ExecContext(ctx, `ROLLBACK`); rollbackErr != nil {
			result = errors.Join(result, fmt.Errorf("load rate cards: roll back transaction: %w", rollbackErr))
		}
	}()
	if actionErr := action(conn); actionErr != nil {
		return actionErr
	}
	if _, commitErr := conn.ExecContext(ctx, `COMMIT`); commitErr != nil {
		return fmt.Errorf("load rate cards: commit transaction: %w", commitErr)
	}
	committed = true
	return nil
}

func collectRateCards(
	ctx context.Context,
	conn *sql.Conn,
	cards []RateCard,
) ([]storedRateCard, error) {
	toInsert := make([]storedRateCard, 0, len(cards))
	seen := make(map[string]storedRateCard, len(cards))
	for _, card := range cards {
		candidate := makeStoredRateCard(card)
		key := rateCardKey(candidate.Model, candidate.effectiveFrom)
		if prior, exists := seen[key]; exists {
			if !sameRateCard(prior, candidate) {
				return nil, rateCardConflictError(candidate)
			}
			continue
		}
		seen[key] = candidate
		present, err := rateCardAlreadyPresent(ctx, conn, candidate)
		if err != nil {
			return nil, err
		}
		if !present {
			toInsert = append(toInsert, candidate)
		}
	}
	return toInsert, nil
}

func rateCardAlreadyPresent(
	ctx context.Context,
	conn *sql.Conn,
	candidate storedRateCard,
) (bool, error) {
	existing, found, queryErr := readRateCard(ctx, conn, candidate.Model, candidate.effectiveFrom)
	if queryErr != nil {
		return false, fmt.Errorf(
			"load rate cards: inspect %q at %d: %w",
			candidate.Model,
			candidate.effectiveFrom,
			queryErr,
		)
	}
	if found && !sameRateCard(existing, candidate) {
		return false, rateCardConflictError(candidate)
	}
	return found, nil
}

func insertRateCards(ctx context.Context, conn *sql.Conn, cards []storedRateCard) error {
	for _, card := range cards {
		if err := insertRateCard(ctx, conn, card); err != nil {
			return err
		}
	}
	return nil
}

func insertRateCard(ctx context.Context, conn *sql.Conn, card storedRateCard) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO rate_cards (
		model, effective_from,
		input_nano_usd_per_million, output_nano_usd_per_million,
		cache_read_nano_usd_per_million, cache_creation_nano_usd_per_million,
		cache_creation_5m_nano_usd_per_million, cache_creation_1h_nano_usd_per_million,
		source, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		card.Model, card.effectiveFrom,
		card.InputNanoUSDPerMillion, card.OutputNanoUSDPerMillion,
		card.CacheReadNanoUSDPerMillion, card.CacheCreationNanoUSDPerMillion,
		card.CacheCreation5mNanoUSDPerMillion, card.CacheCreation1hNanoUSDPerMillion,
		card.Source, card.createdAt,
	)
	if err != nil {
		return fmt.Errorf("load rate cards: insert %q at %d: %w", card.Model, card.effectiveFrom, err)
	}
	return nil
}

func validateRateCardLoad(ctx context.Context, cards []RateCard) error {
	if ctx == nil {
		return errors.New("load rate cards: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("load rate cards context: %w", err)
	}
	for _, card := range cards {
		if strings.TrimSpace(card.Model) == "" {
			return errors.New("load rate cards: model is blank")
		}
		if card.EffectiveFrom.IsZero() {
			return fmt.Errorf("load rate cards: effective time for %q is zero", card.Model)
		}
		effectiveFrom := card.EffectiveFrom.UnixNano()
		if !time.Unix(0, effectiveFrom).Equal(card.EffectiveFrom) {
			return fmt.Errorf("load rate cards: effective time for %q cannot round-trip through UnixNano", card.Model)
		}
		if strings.TrimSpace(card.Source) == "" {
			return fmt.Errorf("load rate cards: source for %q is blank", card.Model)
		}
		for _, rate := range []accounting.NanoUSD{
			card.InputNanoUSDPerMillion,
			card.OutputNanoUSDPerMillion,
			card.CacheReadNanoUSDPerMillion,
			card.CacheCreationNanoUSDPerMillion,
			card.CacheCreation5mNanoUSDPerMillion,
			card.CacheCreation1hNanoUSDPerMillion,
		} {
			if rate < 0 {
				return fmt.Errorf("load rate cards: rate for %q is negative: %d", card.Model, rate)
			}
		}
	}
	return nil
}

func makeStoredRateCard(card RateCard) storedRateCard {
	createdAt := card.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return storedRateCard{
		RateCard:      card,
		effectiveFrom: card.EffectiveFrom.UnixNano(),
		createdAt:     createdAt.UnixNano(),
	}
}

func readRateCard(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, model string, effectiveFrom int64) (storedRateCard, bool, error) {
	var card storedRateCard
	err := queryer.QueryRowContext(ctx, `SELECT
		id, model, effective_from,
		input_nano_usd_per_million, output_nano_usd_per_million,
		cache_read_nano_usd_per_million, cache_creation_nano_usd_per_million,
		cache_creation_5m_nano_usd_per_million, cache_creation_1h_nano_usd_per_million,
		source, created_at
	FROM rate_cards WHERE model = ? AND effective_from = ?`, model, effectiveFrom).Scan(
		&card.id, &card.Model, &card.effectiveFrom,
		&card.InputNanoUSDPerMillion, &card.OutputNanoUSDPerMillion,
		&card.CacheReadNanoUSDPerMillion, &card.CacheCreationNanoUSDPerMillion,
		&card.CacheCreation5mNanoUSDPerMillion, &card.CacheCreation1hNanoUSDPerMillion,
		&card.Source, &card.createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return storedRateCard{}, false, nil
	}
	if err != nil {
		return storedRateCard{}, false, fmt.Errorf("scan rate card: %w", err)
	}
	return card, true, nil
}

func (store *Store) lookupEffectiveRateCard(
	ctx context.Context,
	model string,
	startedAt int64,
) (storedRateCard, bool, error) {
	var card storedRateCard
	err := store.db.QueryRowContext(ctx, `SELECT
		id, model, effective_from,
		input_nano_usd_per_million, output_nano_usd_per_million,
		cache_read_nano_usd_per_million, cache_creation_nano_usd_per_million,
		cache_creation_5m_nano_usd_per_million, cache_creation_1h_nano_usd_per_million,
		source, created_at
	FROM rate_cards
	WHERE model = ? AND effective_from <= ?
	ORDER BY effective_from DESC
	LIMIT 1`, model, startedAt).Scan(
		&card.id, &card.Model, &card.effectiveFrom,
		&card.InputNanoUSDPerMillion, &card.OutputNanoUSDPerMillion,
		&card.CacheReadNanoUSDPerMillion, &card.CacheCreationNanoUSDPerMillion,
		&card.CacheCreation5mNanoUSDPerMillion, &card.CacheCreation1hNanoUSDPerMillion,
		&card.Source, &card.createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return storedRateCard{}, false, nil
	}
	if err != nil {
		return storedRateCard{}, false, fmt.Errorf("scan rate card: %w", err)
	}
	return card, true, nil
}

func sameRateCard(left, right storedRateCard) bool {
	return left.Model == right.Model &&
		left.effectiveFrom == right.effectiveFrom &&
		left.InputNanoUSDPerMillion == right.InputNanoUSDPerMillion &&
		left.OutputNanoUSDPerMillion == right.OutputNanoUSDPerMillion &&
		left.CacheReadNanoUSDPerMillion == right.CacheReadNanoUSDPerMillion &&
		left.CacheCreationNanoUSDPerMillion == right.CacheCreationNanoUSDPerMillion &&
		left.CacheCreation5mNanoUSDPerMillion == right.CacheCreation5mNanoUSDPerMillion &&
		left.CacheCreation1hNanoUSDPerMillion == right.CacheCreation1hNanoUSDPerMillion &&
		left.Source == right.Source
}

func rateCardConflictError(card storedRateCard) error {
	return fmt.Errorf(
		"load rate cards: conflicting card for model %q at effective time %d",
		card.Model,
		card.effectiveFrom,
	)
}

func rateCardKey(model string, effectiveFrom int64) string {
	return model + "\x00" + fmt.Sprintf("%d", effectiveFrom)
}
