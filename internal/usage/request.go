package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/joshsymonds/patchbay/internal/accounting"
	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// RequestKind classifies the upstream API operation being accounted for.
type RequestKind string

const (
	// RequestMessages is a Messages API request.
	RequestMessages RequestKind = "messages"
	// RequestCountTokens is a token-counting request.
	RequestCountTokens RequestKind = "count_tokens"
	// RequestOther is any other upstream-eligible request.
	RequestOther RequestKind = "other"
)

// Valid reports whether kind is in the closed request-kind vocabulary.
func (kind RequestKind) Valid() bool {
	switch kind {
	case RequestMessages, RequestCountTokens, RequestOther:
		return true
	default:
		return false
	}
}

// String returns the wire form of kind.
func (kind RequestKind) String() string {
	return string(kind)
}

// Outcome classifies how a request completed.
type Outcome string

const (
	// OutcomeSuccess means the request completed successfully.
	OutcomeSuccess Outcome = "success"
	// OutcomeLocalRejection means Patchbay rejected the request before upstream completion.
	OutcomeLocalRejection Outcome = "local_rejection"
	// OutcomeUpstreamHTTPFailure means upstream returned a failing HTTP response.
	OutcomeUpstreamHTTPFailure Outcome = "upstream_http_failure"
	// OutcomeTransportFailure means upstream transport failed.
	OutcomeTransportFailure Outcome = "transport_failure"
	// OutcomeResponseInterrupted means the response ended before normal completion.
	OutcomeResponseInterrupted Outcome = "response_interrupted"
	// OutcomeNormalizationFailure means response normalization failed.
	OutcomeNormalizationFailure Outcome = "normalization_failure"
)

// Valid reports whether outcome is in the closed outcome vocabulary.
func (outcome Outcome) Valid() bool {
	switch outcome {
	case OutcomeSuccess,
		OutcomeLocalRejection,
		OutcomeUpstreamHTTPFailure,
		OutcomeTransportFailure,
		OutcomeResponseInterrupted,
		OutcomeNormalizationFailure:
		return true
	default:
		return false
	}
}

// String returns the wire form of outcome.
func (outcome Outcome) String() string {
	return string(outcome)
}

// CostStatus classifies whether the finalized cost is known.
type CostStatus string

const (
	// CostStatusPending means accounting has not been finalized.
	CostStatusPending CostStatus = "pending"
	// CostStatusKnown means the finalized cost is known.
	CostStatusKnown CostStatus = "known"
	// CostStatusUnknown means the finalized cost is unknown.
	CostStatusUnknown CostStatus = "unknown"
)

// Valid reports whether status is in the closed cost-status vocabulary.
func (status CostStatus) Valid() bool {
	switch status {
	case CostStatusPending, CostStatusKnown, CostStatusUnknown:
		return true
	default:
		return false
	}
}

// String returns the wire form of status.
func (status CostStatus) String() string {
	return string(status)
}

// FinalizedRequest is the completed response and accounting snapshot for one request.
type FinalizedRequest struct {
	CompletedAt                       time.Time
	Outcome                           Outcome
	HTTPStatus                        int
	ResponseID                        string
	NormalizationComplete             bool
	InputTokens                       int64
	InputTokensPresent                bool
	OutputTokens                      int64
	OutputTokensPresent               bool
	ReasoningOutputTokens             int64
	ReasoningOutputTokensPresent      bool
	CacheCreationInputTokens          int64
	CacheCreationInputTokensPresent   bool
	CacheReadInputTokens              int64
	CacheReadInputTokensPresent       bool
	CacheCreation5mInputTokens        int64
	CacheCreation5mInputTokensPresent bool
	CacheCreation1hInputTokens        int64
	CacheCreation1hInputTokensPresent bool
	ProviderCostPresent               bool
	ProviderCostValid                 bool
	ProviderCost                      accounting.NanoUSD
	StopReason                        string
	BlockShape                        []string
	DurationMS                        int64
	UpstreamMS                        int64
	FirstContentAt                    time.Time
}

// PendingRequest is the immutable request identity captured before upstream contact.
//
// SeatID and Seat are the Seat the request actually rides, taken from the one
// Resolution the forward path and the ledger record read. They are recorded
// together because neither answers for the other: the ID names the row an
// operator can look up, and the Seat carries the upstream, the rewrite and the
// billing policy that decide what the request costs.
//
// RequestModel is the exact string the caller asked with, and may be empty —
// that is a request that named no model, not a model named "". A Seat that
// binds no rewrite leaves the resolved model equally empty, and both are stored
// as they stand.
type PendingRequest struct {
	StartedAt       time.Time
	Kind            RequestKind
	Host            string
	Context         string
	SeatID          string
	Seat            registry.Seat
	RequestModel    string
	CredentialClass billing.CredentialClass
}

// pendingSnapshot is the derived row one pending request inserts as.
//
// Its seatID and upstream land in the route_alias and route_upstream columns,
// which keep their names so already-imported history stays readable. The names
// are the schema's; the values are the Seat's.
type pendingSnapshot struct {
	startedAt     int64
	seatID        string
	upstream      string
	resolvedModel string
	billingPolicy billing.Policy
	billingClass  any
}

// InsertPending durably records an upstream-eligible request before contact.
func (store *Store) InsertPending(ctx context.Context, request PendingRequest) (int64, error) {
	snapshot, err := derivePendingSnapshot(ctx, request)
	if err != nil {
		return 0, err
	}

	if permitErr := store.acquireWritePermit(ctx); permitErr != nil {
		return 0, fmt.Errorf("insert pending request: %w", permitErr)
	}
	defer store.releaseWritePermit()

	result, err := store.db.ExecContext(ctx, `INSERT INTO accounting_requests (
		started_at,
		completed_at,
		upstream_dispatched_at,
		request_kind,
		lifecycle_state,
		outcome,
		upstream_contacted,
		route_host,
		route_context,
		route_alias,
		route_upstream,
		request_model,
		resolved_model,
		auth_mode,
		credential_class,
		billing_policy,
		billing_class,
		cost_status
	) VALUES (?, NULL, NULL, ?, 'pending', NULL, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`,
		snapshot.startedAt,
		request.Kind.String(),
		request.Host,
		request.Context,
		snapshot.seatID,
		snapshot.upstream,
		request.RequestModel,
		snapshot.resolvedModel,
		string(request.Seat.AuthMode),
		request.CredentialClass.String(),
		snapshot.billingPolicy.String(),
		snapshot.billingClass,
	)
	if err != nil {
		return 0, fmt.Errorf("insert pending request: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read inserted pending request ID: %w", err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("inserted pending request ID is not positive: %d", id)
	}
	return id, nil
}

const (
	finalizeUpdateArgumentCount = 47
	finalizedLifecycleState     = "finalized"
)

type finalizedSnapshot struct {
	completedAt                int64
	outcome                    string
	httpStatus                 sql.NullInt64
	responseID                 sql.NullString
	normalizationComplete      int64
	inputTokens                sql.NullInt64
	outputTokens               sql.NullInt64
	reasoningOutputTokens      sql.NullInt64
	cacheCreationInputTokens   sql.NullInt64
	cacheReadInputTokens       sql.NullInt64
	cacheCreation5mInputTokens sql.NullInt64
	cacheCreation1hInputTokens sql.NullInt64
	providerCostPresent        int64
	providerCostValid          int64
	providerCostNanoUSD        int64
	stopReason                 string
	blockShape                 string
	durationMS                 int64
	upstreamMS                 int64
	firstContentAt             sql.NullInt64
}

type finalCost struct {
	basis      sql.NullString
	amount     sql.NullInt64
	rateCardID sql.NullInt64
}

func (cost finalCost) status() string {
	if !cost.basis.Valid {
		return CostStatusUnknown.String()
	}
	return costStatusForBasis(cost.basis.String)
}

func costStatusForBasis(basis string) string {
	if basis == CostStatusUnknown.String() {
		return CostStatusUnknown.String()
	}
	return CostStatusKnown.String()
}

type finalizationState struct {
	startedAt            int64
	resolvedModel        string
	billingClass         sql.NullString
	upstreamDispatchedAt sql.NullInt64
	upstreamContacted    int64
	lifecycleState       string
	snapshot             finalizedSnapshot
}

// Finalize atomically transitions one pending request to its durable finalized snapshot.
func (store *Store) Finalize(ctx context.Context, id int64, finalized FinalizedRequest) error {
	snapshot, err := deriveFinalizedSnapshot(ctx, id, finalized)
	if err != nil {
		return err
	}
	if err = store.acquireWritePermit(ctx); err != nil {
		return fmt.Errorf("finalize request %d: %w", id, err)
	}
	defer store.releaseWritePermit()

	state, err := store.readFinalizationState(ctx, id)
	if err != nil {
		return err
	}
	if validationErr := validateFinalizationState(id, state, snapshot); validationErr != nil {
		return validationErr
	}
	if state.lifecycleState == finalizedLifecycleState {
		return nil
	}
	return store.finalizeAfterPendingRead(ctx, id, state, snapshot)
}

// finalizeAfterPendingRead completes a finalization whose caller already read and
// validated a pending row while holding this Store's write permit. Another Store
// can finalize the row before this UPDATE; the exact-replay branch then preserves
// that winner's derived pricing while accepting the identical caller snapshot.
func (store *Store) finalizeAfterPendingRead(
	ctx context.Context,
	id int64,
	state finalizationState,
	snapshot finalizedSnapshot,
) error {
	cost, err := store.deriveFinalCost(ctx, state, snapshot)
	if err != nil {
		return fmt.Errorf("finalize request %d: derive final cost: %w", id, err)
	}

	result, err := store.db.ExecContext(ctx, `UPDATE accounting_requests SET
		completed_at = ?,
		lifecycle_state = 'finalized',
		outcome = ?,
		cost_status = CASE WHEN lifecycle_state = 'pending' THEN ? ELSE cost_status END,
		cost_basis = CASE WHEN lifecycle_state = 'pending' THEN ? ELSE cost_basis END,
		final_cost_nano_usd = CASE WHEN lifecycle_state = 'pending' THEN ? ELSE final_cost_nano_usd END,
		rate_card_id = CASE WHEN lifecycle_state = 'pending' THEN ? ELSE rate_card_id END,
		response_id = ?,
		http_status = ?,
		normalization_complete = ?,
		input_tokens = ?,
		output_tokens = ?,
		reasoning_output_tokens = ?,
		cache_creation_input_tokens = ?,
		cache_read_input_tokens = ?,
		cache_creation_5m_input_tokens = ?,
		cache_creation_1h_input_tokens = ?,
		provider_cost_present = ?,
		provider_cost_valid = ?,
		provider_cost_nano_usd = ?,
		stop_reason = ?,
		block_shape = ?,
		duration_ms = ?,
		upstream_ms = ?,
		first_content_at = ?
	WHERE id = ? AND (
		(lifecycle_state = 'pending'
			AND started_at <= ?
			AND ((upstream_contacted = 0 AND upstream_dispatched_at IS NULL)
				OR (upstream_contacted = 1 AND upstream_dispatched_at IS NOT NULL
					AND upstream_dispatched_at <= ?)))
		OR (lifecycle_state = 'finalized'
			AND completed_at IS ?
			AND outcome IS ?
			AND response_id IS ?
			AND http_status IS ?
			AND normalization_complete IS ?
			AND input_tokens IS ?
			AND output_tokens IS ?
			AND reasoning_output_tokens IS ?
			AND cache_creation_input_tokens IS ?
			AND cache_read_input_tokens IS ?
			AND cache_creation_5m_input_tokens IS ?
			AND cache_creation_1h_input_tokens IS ?
			AND provider_cost_present IS ?
			AND provider_cost_valid IS ?
			AND provider_cost_nano_usd IS ?
			AND stop_reason IS ?
			AND block_shape IS ?
			AND duration_ms IS ?
			AND upstream_ms IS ?
			AND first_content_at IS ?)
	)`, snapshot.updateArguments(id, cost)...)
	if err != nil {
		return fmt.Errorf("finalize request %d: update finalized snapshot: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("finalize request %d: read updated row count: %w", id, err)
	}
	if rows == 1 {
		return nil
	}
	if rows != 0 {
		return fmt.Errorf("finalize request %d: update affected %d rows", id, rows)
	}
	return store.diagnoseFinalizeNoMatch(ctx, id, snapshot)
}

func deriveFinalizedSnapshot(
	ctx context.Context,
	id int64,
	finalized FinalizedRequest,
) (finalizedSnapshot, error) {
	completedAt, validationErr := validateFinalizedRequest(ctx, id, finalized)
	if validationErr != nil {
		return finalizedSnapshot{}, validationErr
	}

	shape := finalized.BlockShape
	if len(shape) == 0 {
		shape = []string{}
	}
	encodedShape, err := json.Marshal(shape)
	if err != nil {
		return finalizedSnapshot{}, fmt.Errorf("finalize request block shape: %w", err)
	}

	return finalizedSnapshot{
		completedAt:           completedAt,
		outcome:               finalized.Outcome.String(),
		httpStatus:            nullableInt64(int64(finalized.HTTPStatus), finalized.HTTPStatus != 0),
		responseID:            nullableString(finalized.ResponseID, finalized.ResponseID != ""),
		normalizationComplete: boolInteger(finalized.NormalizationComplete),
		inputTokens:           nullableInt64(finalized.InputTokens, finalized.InputTokensPresent),
		outputTokens:          nullableInt64(finalized.OutputTokens, finalized.OutputTokensPresent),
		reasoningOutputTokens: nullableInt64(finalized.ReasoningOutputTokens, finalized.ReasoningOutputTokensPresent),
		cacheCreationInputTokens: nullableInt64(
			finalized.CacheCreationInputTokens,
			finalized.CacheCreationInputTokensPresent,
		),
		cacheReadInputTokens: nullableInt64(
			finalized.CacheReadInputTokens,
			finalized.CacheReadInputTokensPresent,
		),
		cacheCreation5mInputTokens: nullableInt64(
			finalized.CacheCreation5mInputTokens,
			finalized.CacheCreation5mInputTokensPresent,
		),
		cacheCreation1hInputTokens: nullableInt64(
			finalized.CacheCreation1hInputTokens,
			finalized.CacheCreation1hInputTokensPresent,
		),
		providerCostPresent: boolInteger(finalized.ProviderCostPresent),
		providerCostValid:   boolInteger(finalized.ProviderCostValid),
		providerCostNanoUSD: int64(finalized.ProviderCost),
		stopReason:          finalized.StopReason,
		blockShape:          string(encodedShape),
		durationMS:          finalized.DurationMS,
		upstreamMS:          finalized.UpstreamMS,
		firstContentAt:      nullableInt64(finalized.FirstContentAt.UnixNano(), !finalized.FirstContentAt.IsZero()),
	}, nil
}

func validateFinalizedRequest(
	ctx context.Context,
	id int64,
	finalized FinalizedRequest,
) (int64, error) {
	if ctx == nil {
		return 0, fmt.Errorf("finalize request: context is nil")
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return 0, fmt.Errorf("finalize request context: %w", contextErr)
	}
	if id <= 0 {
		return 0, fmt.Errorf("finalize request ID must be positive: %d", id)
	}
	if finalized.CompletedAt.IsZero() {
		return 0, fmt.Errorf("finalize request completed at is zero")
	}
	completedAt := finalized.CompletedAt.UnixNano()
	if !time.Unix(0, completedAt).Equal(finalized.CompletedAt) {
		return 0, fmt.Errorf("finalize request completed at cannot round-trip through UnixNano")
	}
	if !finalized.FirstContentAt.IsZero() {
		firstContentAt := finalized.FirstContentAt.UnixNano()
		if !time.Unix(0, firstContentAt).Equal(finalized.FirstContentAt) {
			return 0, fmt.Errorf("finalize request first content at cannot round-trip through UnixNano")
		}
	}
	if !finalized.Outcome.Valid() {
		return 0, fmt.Errorf("finalize request outcome %q is invalid", finalized.Outcome)
	}
	if finalized.HTTPStatus != 0 && (finalized.HTTPStatus < 100 || finalized.HTTPStatus > 599) {
		return 0, fmt.Errorf("finalize request HTTP status %d is outside 100-599", finalized.HTTPStatus)
	}
	if metricErr := validateFinalizedMetrics(finalized); metricErr != nil {
		return 0, metricErr
	}
	if providerCostErr := validateProviderCostTuple(finalized); providerCostErr != nil {
		return 0, providerCostErr
	}
	if blockShapeErr := validateBlockShape(finalized.BlockShape); blockShapeErr != nil {
		return 0, blockShapeErr
	}
	return completedAt, nil
}

func validateFinalizedMetrics(finalized FinalizedRequest) error {
	for _, token := range []struct {
		name    string
		value   int64
		present bool
	}{
		{"input tokens", finalized.InputTokens, finalized.InputTokensPresent},
		{"output tokens", finalized.OutputTokens, finalized.OutputTokensPresent},
		{"reasoning output tokens", finalized.ReasoningOutputTokens, finalized.ReasoningOutputTokensPresent},
		{
			"cache creation input tokens",
			finalized.CacheCreationInputTokens,
			finalized.CacheCreationInputTokensPresent,
		},
		{"cache read input tokens", finalized.CacheReadInputTokens, finalized.CacheReadInputTokensPresent},
		{
			"cache creation 5m input tokens",
			finalized.CacheCreation5mInputTokens,
			finalized.CacheCreation5mInputTokensPresent,
		},
		{
			"cache creation 1h input tokens",
			finalized.CacheCreation1hInputTokens,
			finalized.CacheCreation1hInputTokensPresent,
		},
	} {
		if token.value < 0 {
			return fmt.Errorf("finalize request %s must be nonnegative: %d", token.name, token.value)
		}
		if !token.present && token.value != 0 {
			return fmt.Errorf("finalize request %s must be zero when absent: %d", token.name, token.value)
		}
	}
	for _, metric := range []struct {
		name  string
		value int64
	}{
		{"duration milliseconds", finalized.DurationMS},
		{"upstream milliseconds", finalized.UpstreamMS},
	} {
		if metric.value < 0 {
			return fmt.Errorf("finalize request %s must be nonnegative: %d", metric.name, metric.value)
		}
	}
	return nil
}

func validateBlockShape(shape []string) error {
	for index, blockType := range shape {
		// Valid UTF-8 makes encoding/json's block-shape representation canonical and injective.
		if !utf8.ValidString(blockType) {
			return fmt.Errorf("finalize request block shape entry %d is not valid UTF-8", index)
		}
	}
	return nil
}

func validateProviderCostTuple(finalized FinalizedRequest) error {
	amount := finalized.ProviderCost
	switch {
	case !finalized.ProviderCostPresent && !finalized.ProviderCostValid && amount == 0:
		return nil
	case finalized.ProviderCostPresent && !finalized.ProviderCostValid && amount == 0:
		return nil
	case finalized.ProviderCostPresent && finalized.ProviderCostValid && amount >= 0:
		return nil
	default:
		return fmt.Errorf(
			"finalize request provider cost tuple is invalid: present=%t valid=%t amount=%d",
			finalized.ProviderCostPresent,
			finalized.ProviderCostValid,
			amount,
		)
	}
}

func nullableInt64(value int64, present bool) sql.NullInt64 {
	if !present {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: value, Valid: true}
}

func nullableString(value string, present bool) sql.NullString {
	return sql.NullString{String: value, Valid: present}
}

func boolInteger(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func (snapshot finalizedSnapshot) updateArguments(id int64, cost finalCost) []any {
	values := make([]any, 0, finalizeUpdateArgumentCount)
	values = append(values,
		snapshot.completedAt,
		snapshot.outcome,
		cost.status(),
		nullableValue(cost.basis),
		nullableValue(cost.amount),
		nullableValue(cost.rateCardID),
		nullableValue(snapshot.responseID),
		nullableValue(snapshot.httpStatus),
		snapshot.normalizationComplete,
		nullableValue(snapshot.inputTokens),
		nullableValue(snapshot.outputTokens),
		nullableValue(snapshot.reasoningOutputTokens),
		nullableValue(snapshot.cacheCreationInputTokens),
		nullableValue(snapshot.cacheReadInputTokens),
		nullableValue(snapshot.cacheCreation5mInputTokens),
		nullableValue(snapshot.cacheCreation1hInputTokens),
		snapshot.providerCostPresent,
		snapshot.providerCostValid,
		snapshot.providerCostNanoUSD,
		snapshot.stopReason,
		snapshot.blockShape,
		snapshot.durationMS,
		snapshot.upstreamMS,
		nullableValue(snapshot.firstContentAt),
		id,
		snapshot.completedAt,
		snapshot.completedAt,
	)
	return append(values,
		snapshot.completedAt,
		snapshot.outcome,
		nullableValue(snapshot.responseID),
		nullableValue(snapshot.httpStatus),
		snapshot.normalizationComplete,
		nullableValue(snapshot.inputTokens),
		nullableValue(snapshot.outputTokens),
		nullableValue(snapshot.reasoningOutputTokens),
		nullableValue(snapshot.cacheCreationInputTokens),
		nullableValue(snapshot.cacheReadInputTokens),
		nullableValue(snapshot.cacheCreation5mInputTokens),
		nullableValue(snapshot.cacheCreation1hInputTokens),
		snapshot.providerCostPresent,
		snapshot.providerCostValid,
		snapshot.providerCostNanoUSD,
		snapshot.stopReason,
		snapshot.blockShape,
		snapshot.durationMS,
		snapshot.upstreamMS,
		nullableValue(snapshot.firstContentAt),
	)
}

func nullableValue(value any) any {
	switch value := value.(type) {
	case sql.NullInt64:
		if value.Valid {
			return value.Int64
		}
	case sql.NullString:
		if value.Valid {
			return value.String
		}
	}
	return nil
}

func (store *Store) readFinalizationState(ctx context.Context, id int64) (finalizationState, error) {
	var state finalizationState
	var completedAt, normalizationComplete sql.NullInt64
	var providerCostPresent, providerCostValid, providerCostNanoUSD sql.NullInt64
	var durationMS, upstreamMS sql.NullInt64
	var outcome, stopReason, blockShape sql.NullString
	err := store.db.QueryRowContext(ctx, `SELECT
		started_at,
		resolved_model,
		billing_class,
		upstream_dispatched_at,
		upstream_contacted,
		lifecycle_state,
		completed_at,
		outcome,
		http_status,
		response_id,
		normalization_complete,
		input_tokens,
		output_tokens,
		reasoning_output_tokens,
		cache_creation_input_tokens,
		cache_read_input_tokens,
		cache_creation_5m_input_tokens,
		cache_creation_1h_input_tokens,
		provider_cost_present,
		provider_cost_valid,
		provider_cost_nano_usd,
		stop_reason,
		block_shape,
		duration_ms,
		upstream_ms,
		first_content_at
	FROM accounting_requests WHERE id = ?`, id).Scan(
		&state.startedAt,
		&state.resolvedModel,
		&state.billingClass,
		&state.upstreamDispatchedAt,
		&state.upstreamContacted,
		&state.lifecycleState,
		&completedAt,
		&outcome,
		&state.snapshot.httpStatus,
		&state.snapshot.responseID,
		&normalizationComplete,
		&state.snapshot.inputTokens,
		&state.snapshot.outputTokens,
		&state.snapshot.reasoningOutputTokens,
		&state.snapshot.cacheCreationInputTokens,
		&state.snapshot.cacheReadInputTokens,
		&state.snapshot.cacheCreation5mInputTokens,
		&state.snapshot.cacheCreation1hInputTokens,
		&providerCostPresent,
		&providerCostValid,
		&providerCostNanoUSD,
		&stopReason,
		&blockShape,
		&durationMS,
		&upstreamMS,
		&state.snapshot.firstContentAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return finalizationState{}, fmt.Errorf("finalize request %d: request does not exist", id)
	}
	if err != nil {
		return finalizationState{}, fmt.Errorf("finalize request %d: inspect request state: %w", id, err)
	}
	if state.lifecycleState != finalizedLifecycleState {
		return state, nil
	}
	if !completedAt.Valid || !outcome.Valid || !normalizationComplete.Valid ||
		!providerCostPresent.Valid || !providerCostValid.Valid || !providerCostNanoUSD.Valid ||
		!stopReason.Valid || !blockShape.Valid || !durationMS.Valid || !upstreamMS.Valid {
		return finalizationState{}, fmt.Errorf("finalize request %d: stored finalized snapshot is incomplete", id)
	}
	state.snapshot.completedAt = completedAt.Int64
	state.snapshot.outcome = outcome.String
	state.snapshot.normalizationComplete = normalizationComplete.Int64
	state.snapshot.providerCostPresent = providerCostPresent.Int64
	state.snapshot.providerCostValid = providerCostValid.Int64
	state.snapshot.providerCostNanoUSD = providerCostNanoUSD.Int64
	state.snapshot.stopReason = stopReason.String
	state.snapshot.blockShape = blockShape.String
	state.snapshot.durationMS = durationMS.Int64
	state.snapshot.upstreamMS = upstreamMS.Int64
	return state, nil
}

func (store *Store) deriveFinalCost(
	ctx context.Context,
	state finalizationState,
	snapshot finalizedSnapshot,
) (finalCost, error) {
	if snapshot.providerCostPresent == 1 && snapshot.providerCostValid == 1 {
		return finalCost{
			basis:  sql.NullString{String: "provider_reported", Valid: true},
			amount: sql.NullInt64{Int64: snapshot.providerCostNanoUSD, Valid: true},
		}, nil
	}
	if state.billingClass.Valid &&
		(state.billingClass.String == billing.ClassSubscription.String() ||
			state.billingClass.String == billing.ClassSelfHosted.String()) {
		return finalCost{
			basis:  sql.NullString{String: "zero_marginal", Valid: true},
			amount: sql.NullInt64{Int64: 0, Valid: true},
		}, nil
	}
	if snapshot.outcome != OutcomeSuccess.String() || snapshot.normalizationComplete != 1 ||
		!snapshot.inputTokens.Valid || !snapshot.outputTokens.Valid {
		return unknownFinalCost(), nil
	}
	card, found, err := store.lookupEffectiveRateCard(ctx, state.resolvedModel, state.startedAt)
	if err != nil {
		return finalCost{}, err
	}
	if !found {
		return unknownFinalCost(), nil
	}
	amount, priced := priceRateCard(card, snapshot)
	if !priced {
		return unknownFinalCost(), nil
	}
	return finalCost{
		basis:      sql.NullString{String: "rate_card", Valid: true},
		amount:     sql.NullInt64{Int64: int64(amount), Valid: true},
		rateCardID: sql.NullInt64{Int64: card.id, Valid: true},
	}, nil
}

func unknownFinalCost() finalCost {
	return finalCost{basis: sql.NullString{String: CostStatusUnknown.String(), Valid: true}}
}

type pricedTokens struct {
	rate   accounting.NanoUSD
	tokens sql.NullInt64
}

func priceRateCard(card storedRateCard, snapshot finalizedSnapshot) (accounting.NanoUSD, bool) {
	amount, priced := addPricedTokens(0, []pricedTokens{
		{card.InputNanoUSDPerMillion, snapshot.inputTokens},
		{card.OutputNanoUSDPerMillion, snapshot.outputTokens},
		{card.CacheReadNanoUSDPerMillion, snapshot.cacheReadInputTokens},
	})
	if !priced {
		return 0, false
	}
	if snapshot.cacheCreation5mInputTokens.Valid || snapshot.cacheCreation1hInputTokens.Valid {
		return addPricedTokens(amount, []pricedTokens{
			{card.CacheCreation5mNanoUSDPerMillion, snapshot.cacheCreation5mInputTokens},
			{card.CacheCreation1hNanoUSDPerMillion, snapshot.cacheCreation1hInputTokens},
		})
	}
	return addPricedTokens(amount, []pricedTokens{
		{card.CacheCreationNanoUSDPerMillion, snapshot.cacheCreationInputTokens},
	})
}

func addPricedTokens(amount accounting.NanoUSD, entries []pricedTokens) (accounting.NanoUSD, bool) {
	for _, entry := range entries {
		if !entry.tokens.Valid {
			continue
		}
		priced, multiplyErr := accounting.MultiplyPerMillion(entry.rate, entry.tokens.Int64)
		if multiplyErr != nil {
			return 0, false
		}
		var addErr error
		amount, addErr = accounting.Add(amount, priced)
		if addErr != nil {
			return 0, false
		}
	}
	return amount, true
}

func validateFinalizationState(id int64, state finalizationState, snapshot finalizedSnapshot) error {
	if snapshot.completedAt < state.startedAt {
		return fmt.Errorf(
			"finalize request %d: completion %d precedes start %d",
			id,
			snapshot.completedAt,
			state.startedAt,
		)
	}
	if state.upstreamContacted == 0 && state.upstreamDispatchedAt.Valid ||
		state.upstreamContacted == 1 && !state.upstreamDispatchedAt.Valid ||
		state.upstreamContacted != 0 && state.upstreamContacted != 1 {
		return fmt.Errorf(
			"finalize request %d: inconsistent upstream dispatch evidence: contacted=%d dispatched_at=%v",
			id,
			state.upstreamContacted,
			state.upstreamDispatchedAt,
		)
	}
	if state.upstreamDispatchedAt.Valid && snapshot.completedAt < state.upstreamDispatchedAt.Int64 {
		return fmt.Errorf(
			"finalize request %d: completion %d precedes upstream dispatch %d",
			id,
			snapshot.completedAt,
			state.upstreamDispatchedAt.Int64,
		)
	}
	switch state.lifecycleState {
	case "pending":
		return nil
	case finalizedLifecycleState:
		if state.snapshot == snapshot {
			return nil
		}
		return fmt.Errorf("finalize request %d: finalized snapshot differs", id)
	default:
		return fmt.Errorf("finalize request %d: lifecycle state %q is not pending", id, state.lifecycleState)
	}
}

func (store *Store) diagnoseFinalizeNoMatch(
	ctx context.Context,
	id int64,
	snapshot finalizedSnapshot,
) error {
	state, err := store.readFinalizationState(ctx, id)
	if err != nil {
		return err
	}
	if validationErr := validateFinalizationState(id, state, snapshot); validationErr != nil {
		return validationErr
	}
	return fmt.Errorf("finalize request %d: eligible request was not updated", id)
}

// MarkUpstreamDispatched durably records the first provider HTTP dispatch.
func (store *Store) MarkUpstreamDispatched(
	ctx context.Context,
	id int64,
	dispatchedAt time.Time,
	upstreamTarget string,
) error {
	dispatchedUnixNano, err := validateUpstreamDispatch(ctx, id, dispatchedAt)
	if err != nil {
		return err
	}
	target, err := validateUpstreamTarget(upstreamTarget)
	if err != nil {
		return err
	}

	if err = store.acquireWritePermit(ctx); err != nil {
		return fmt.Errorf("mark upstream dispatched request %d: %w", id, err)
	}
	defer store.releaseWritePermit()

	result, err := store.db.ExecContext(ctx, `UPDATE accounting_requests
		SET upstream_contacted = 1, upstream_dispatched_at = ?, upstream_target = ?
		WHERE id = ?
			AND lifecycle_state = 'pending'
			AND (
				(upstream_contacted = 0 AND upstream_dispatched_at IS NULL AND upstream_target IS NULL)
				OR (upstream_contacted = 1 AND upstream_dispatched_at = ? AND upstream_target IS ?)
			)`, dispatchedUnixNano, nullableValue(target), id, dispatchedUnixNano, nullableValue(target))
	if err != nil {
		return fmt.Errorf("mark upstream dispatched request %d: update dispatch evidence: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark upstream dispatched request %d: read updated row count: %w", id, err)
	}
	if rows == 1 {
		return nil
	}
	if rows != 0 {
		return fmt.Errorf("mark upstream dispatched request %d: update affected %d rows", id, rows)
	}
	return store.diagnoseUpstreamDispatchNoMatch(ctx, id, dispatchedUnixNano, target)
}

func validateUpstreamDispatch(ctx context.Context, id int64, dispatchedAt time.Time) (int64, error) {
	if ctx == nil {
		return 0, fmt.Errorf("mark upstream dispatched: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("mark upstream dispatched context: %w", err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("mark upstream dispatched request ID must be positive: %d", id)
	}
	if dispatchedAt.IsZero() {
		return 0, fmt.Errorf("mark upstream dispatched time is zero")
	}
	dispatchedUnixNano := dispatchedAt.UnixNano()
	if !time.Unix(0, dispatchedUnixNano).Equal(dispatchedAt) {
		return 0, fmt.Errorf("mark upstream dispatched time cannot round-trip through UnixNano")
	}
	return dispatchedUnixNano, nil
}

func validateUpstreamTarget(upstreamTarget string) (sql.NullString, error) {
	if upstreamTarget == "" {
		return sql.NullString{}, nil
	}
	if strings.TrimSpace(upstreamTarget) == "" {
		return sql.NullString{}, errors.New("mark upstream dispatched target is blank")
	}
	return sql.NullString{String: upstreamTarget, Valid: true}, nil
}

func diagnoseExistingUpstreamDispatch(
	id int64,
	existingDispatchedAt sql.NullInt64,
	existingTarget sql.NullString,
	dispatchedUnixNano int64,
	upstreamTarget sql.NullString,
) error {
	if existingDispatchedAt.Int64 == dispatchedUnixNano && existingTarget == upstreamTarget {
		return nil
	}
	return fmt.Errorf(
		"mark upstream dispatched request %d: already dispatched at %d to %q; cannot replace with %d to %q",
		id,
		existingDispatchedAt.Int64,
		existingTarget.String,
		dispatchedUnixNano,
		upstreamTarget.String,
	)
}

func (store *Store) diagnoseUpstreamDispatchNoMatch(
	ctx context.Context,
	id int64,
	dispatchedUnixNano int64,
	upstreamTarget sql.NullString,
) error {
	var lifecycleState string
	var upstreamContacted int
	var existingDispatchedAt sql.NullInt64
	var existingTarget sql.NullString
	err := store.db.QueryRowContext(ctx, `SELECT
		lifecycle_state,
		upstream_contacted,
		upstream_dispatched_at,
		upstream_target
	FROM accounting_requests
	WHERE id = ?`, id).Scan(
		&lifecycleState,
		&upstreamContacted,
		&existingDispatchedAt,
		&existingTarget,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("mark upstream dispatched request %d: request does not exist", id)
	}
	if err != nil {
		return fmt.Errorf("mark upstream dispatched request %d: inspect dispatch evidence: %w", id, err)
	}
	if lifecycleState != "pending" {
		return fmt.Errorf(
			"mark upstream dispatched request %d: lifecycle state %q is not pending",
			id,
			lifecycleState,
		)
	}
	if upstreamContacted == 1 && existingDispatchedAt.Valid {
		return diagnoseExistingUpstreamDispatch(
			id,
			existingDispatchedAt,
			existingTarget,
			dispatchedUnixNano,
			upstreamTarget,
		)
	}
	if upstreamContacted == 0 && !existingDispatchedAt.Valid && !existingTarget.Valid {
		return fmt.Errorf(
			"mark upstream dispatched request %d: eligible dispatch evidence was not updated",
			id,
		)
	}

	storedDispatchTime := "NULL"
	if existingDispatchedAt.Valid {
		storedDispatchTime = fmt.Sprintf("%d", existingDispatchedAt.Int64)
	}
	storedTarget := "NULL"
	if existingTarget.Valid {
		storedTarget = fmt.Sprintf("%q", existingTarget.String)
	}
	return fmt.Errorf(
		"mark upstream dispatched request %d: inconsistent upstream dispatch evidence: contacted=%d, dispatched_at=%s, upstream_target=%s",
		id,
		upstreamContacted,
		storedDispatchTime,
		storedTarget,
	)
}

func derivePendingSnapshot(ctx context.Context, request PendingRequest) (pendingSnapshot, error) {
	if ctx == nil {
		return pendingSnapshot{}, fmt.Errorf("insert pending request: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return pendingSnapshot{}, fmt.Errorf("insert pending request context: %w", err)
	}

	startedAt, err := validatePendingIdentity(request)
	if err != nil {
		return pendingSnapshot{}, err
	}
	seatSnapshot, err := derivePendingSeat(request)
	if err != nil {
		return pendingSnapshot{}, err
	}
	seatSnapshot.startedAt = startedAt
	return seatSnapshot, nil
}

func validatePendingIdentity(request PendingRequest) (int64, error) {
	if request.StartedAt.IsZero() {
		return 0, fmt.Errorf("pending request started at is zero")
	}
	startedAt := request.StartedAt.UnixNano()
	if !time.Unix(0, startedAt).Equal(request.StartedAt) {
		return 0, fmt.Errorf("pending request started at cannot round-trip through UnixNano")
	}
	if !request.Kind.Valid() {
		return 0, fmt.Errorf("pending request kind %q is invalid", request.Kind)
	}
	if strings.TrimSpace(request.Host) == "" {
		return 0, fmt.Errorf("pending request host is blank")
	}
	if strings.TrimSpace(request.Context) == "" {
		return 0, fmt.Errorf("pending request context is blank")
	}
	// A stored Context and Seat ID are the two names an operator later looks a
	// row up by, and both came out of the same registry document. They are held
	// to that document's own grammar, by calling it, so a row can never name a
	// Context or a Seat no registry could have admitted.
	if err := registry.ValidID(request.Context); err != nil {
		return 0, fmt.Errorf("pending request context %q: %w", request.Context, err)
	}
	if strings.TrimSpace(request.SeatID) == "" {
		return 0, fmt.Errorf("pending request seat is blank")
	}
	if err := registry.ValidID(request.SeatID); err != nil {
		return 0, fmt.Errorf("pending request seat %q: %w", request.SeatID, err)
	}
	return startedAt, nil
}

// derivePendingSeat reads the accounting facts off the Seat the request rides.
//
// Neither model is judged. A model is an opaque exact string the registry bound
// and the caller asked with, so a Seat that rewrites nothing leaves the resolved
// model to the request's own — and a request that named no model leaves both
// empty. Empty is stored as empty: a default-Seat request with no model is a
// request that happened, and rounding it up to a model nobody named would be
// the row lying about what was sent.
func derivePendingSeat(request PendingRequest) (pendingSnapshot, error) {
	upstream := request.Seat.EffectiveUpstream()
	if strings.TrimSpace(upstream) == "" {
		return pendingSnapshot{}, fmt.Errorf("pending request seat upstream is blank")
	}
	resolvedModel := request.Seat.Model
	if resolvedModel == "" {
		resolvedModel = request.RequestModel
	}

	policy := request.Seat.BillingPolicy()
	if !policy.Valid() {
		return pendingSnapshot{}, fmt.Errorf(
			"pending request seat auth mode %q yields invalid billing policy %q",
			request.Seat.AuthMode,
			policy,
		)
	}
	if err := validateBillingDeclaration(request.Seat, policy); err != nil {
		return pendingSnapshot{}, err
	}
	if !request.CredentialClass.Valid() {
		return pendingSnapshot{}, fmt.Errorf("pending request credential class %q is invalid", request.CredentialClass)
	}
	var storedClass any
	class, err := billing.RealizeClass(policy, request.Seat.BillingClass, request.CredentialClass)
	if err != nil {
		// A forward Seat with no recognized credential is a selected local
		// rejection, not an upstream-eligible request. Its pending row preserves the
		// caller-derived policy and an unknown payer so the proxy can finalize the
		// refusal as a 401 without inventing a billing class.
		if policy != billing.PolicyCallerDerived || request.CredentialClass != billing.CredentialNone {
			return pendingSnapshot{}, fmt.Errorf("realize pending request billing class: %w", err)
		}
	} else {
		if class == "" {
			return pendingSnapshot{}, fmt.Errorf("realize pending request billing class returned an empty class")
		}
		storedClass = class.String()
	}

	return pendingSnapshot{
		seatID:        request.SeatID,
		upstream:      upstream,
		resolvedModel: resolvedModel,
		billingPolicy: policy,
		billingClass:  storedClass,
	}, nil
}

// validateBillingDeclaration holds a Seat to exactly one answer about who pays
// for it, which is the same invariant registry admission enforces on the Seat
// document. It is checked again here because a Seat can reach accounting
// without passing through a file — a caller's own struct, a resolved state
// built in process — and a row that realized its class from a declaration the
// registry would have refused is a cost attributed to the wrong payer.
func validateBillingDeclaration(seat registry.Seat, policy billing.Policy) error {
	switch policy {
	case billing.PolicyExplicit:
		if !seat.BillingClassSet {
			return fmt.Errorf("explicit billing policy requires BillingClassSet=true")
		}
		if !seat.BillingClass.Valid() {
			return fmt.Errorf(
				"explicit billing policy requires a valid billing class, got %q",
				seat.BillingClass,
			)
		}
	case billing.PolicyCallerDerived:
		if seat.BillingClassSet {
			return fmt.Errorf("caller-derived billing policy requires BillingClassSet=false")
		}
		if seat.BillingClass != "" {
			return fmt.Errorf(
				"caller-derived billing policy cannot declare billing class %q",
				seat.BillingClass,
			)
		}
	case billing.PolicySigV4Metered:
		if seat.BillingClassSet {
			return fmt.Errorf("sigv4-metered billing policy requires BillingClassSet=false")
		}
		if seat.BillingClass != "" {
			return fmt.Errorf(
				"sigv4-metered billing policy cannot declare billing class %q",
				seat.BillingClass,
			)
		}
	}
	return nil
}
