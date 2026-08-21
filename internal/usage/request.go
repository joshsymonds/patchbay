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
	CostStatus                        CostStatus
}

// PendingRequest is the immutable request identity captured before upstream contact.
type PendingRequest struct {
	StartedAt       time.Time
	Kind            RequestKind
	Host            string
	Context         string
	Route           registry.Route
	RequestModel    string
	CredentialClass billing.CredentialClass
}

type pendingSnapshot struct {
	startedAt     int64
	routeAlias    any
	routeUpstream string
	resolvedModel string
	billingPolicy billing.Policy
	billingClass  billing.Class
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
		snapshot.routeAlias,
		snapshot.routeUpstream,
		request.RequestModel,
		snapshot.resolvedModel,
		string(request.Route.Auth),
		request.CredentialClass.String(),
		snapshot.billingPolicy.String(),
		snapshot.billingClass.String(),
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

const finalizeUpdateArgumentCount = 41

type finalizedSnapshot struct {
	completedAt                int64
	outcome                    string
	httpStatus                 sql.NullInt64
	responseID                 sql.NullString
	normalizationComplete      int64
	inputTokens                sql.NullInt64
	outputTokens               sql.NullInt64
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
	costStatus                 string
}

type finalizationState struct {
	startedAt            int64
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

	result, err := store.db.ExecContext(ctx, `UPDATE accounting_requests SET
		completed_at = ?,
		lifecycle_state = 'finalized',
		outcome = ?,
		cost_status = ?,
		response_id = ?,
		http_status = ?,
		normalization_complete = ?,
		input_tokens = ?,
		output_tokens = ?,
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
		upstream_ms = ?
	WHERE id = ? AND (
		(lifecycle_state = 'pending'
			AND started_at <= ?
			AND ((upstream_contacted = 0 AND upstream_dispatched_at IS NULL)
				OR (upstream_contacted = 1 AND upstream_dispatched_at IS NOT NULL
					AND upstream_dispatched_at <= ?)))
		OR (lifecycle_state = 'finalized'
			AND completed_at IS ?
			AND outcome IS ?
			AND cost_status IS ?
			AND response_id IS ?
			AND http_status IS ?
			AND normalization_complete IS ?
			AND input_tokens IS ?
			AND output_tokens IS ?
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
			AND upstream_ms IS ?)
	)`, snapshot.updateArguments(id)...)
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
		costStatus:          finalized.CostStatus.String(),
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
	if !finalized.Outcome.Valid() {
		return 0, fmt.Errorf("finalize request outcome %q is invalid", finalized.Outcome)
	}
	if finalized.HTTPStatus != 0 && (finalized.HTTPStatus < 100 || finalized.HTTPStatus > 599) {
		return 0, fmt.Errorf("finalize request HTTP status %d is outside 100-599", finalized.HTTPStatus)
	}
	if finalized.CostStatus != CostStatusKnown && finalized.CostStatus != CostStatusUnknown {
		return 0, fmt.Errorf("finalize request cost status %q is invalid", finalized.CostStatus)
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

func (snapshot finalizedSnapshot) updateArguments(id int64) []any {
	values := make([]any, 0, finalizeUpdateArgumentCount)
	values = append(values,
		snapshot.completedAt,
		snapshot.outcome,
		snapshot.costStatus,
		nullableValue(snapshot.responseID),
		nullableValue(snapshot.httpStatus),
		snapshot.normalizationComplete,
		nullableValue(snapshot.inputTokens),
		nullableValue(snapshot.outputTokens),
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
		id,
		snapshot.completedAt,
		snapshot.completedAt,
	)
	return append(values,
		snapshot.completedAt,
		snapshot.outcome,
		snapshot.costStatus,
		nullableValue(snapshot.responseID),
		nullableValue(snapshot.httpStatus),
		snapshot.normalizationComplete,
		nullableValue(snapshot.inputTokens),
		nullableValue(snapshot.outputTokens),
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
		cost_status
	FROM accounting_requests WHERE id = ?`, id).Scan(
		&state.startedAt,
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
		&state.snapshot.costStatus,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return finalizationState{}, fmt.Errorf("finalize request %d: request does not exist", id)
	}
	if err != nil {
		return finalizationState{}, fmt.Errorf("finalize request %d: inspect request state: %w", id, err)
	}
	if state.lifecycleState != "finalized" {
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
	case "finalized":
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
) error {
	dispatchedUnixNano, err := validateUpstreamDispatch(ctx, id, dispatchedAt)
	if err != nil {
		return err
	}

	if err = store.acquireWritePermit(ctx); err != nil {
		return fmt.Errorf("mark upstream dispatched request %d: %w", id, err)
	}
	defer store.releaseWritePermit()

	result, err := store.db.ExecContext(ctx, `UPDATE accounting_requests
		SET upstream_contacted = 1, upstream_dispatched_at = ?
		WHERE id = ?
			AND lifecycle_state = 'pending'
			AND (
				(upstream_contacted = 0 AND upstream_dispatched_at IS NULL)
				OR (upstream_contacted = 1 AND upstream_dispatched_at = ?)
			)`, dispatchedUnixNano, id, dispatchedUnixNano)
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
	return store.diagnoseUpstreamDispatchNoMatch(ctx, id, dispatchedUnixNano)
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

func (store *Store) diagnoseUpstreamDispatchNoMatch(
	ctx context.Context,
	id int64,
	dispatchedUnixNano int64,
) error {
	var lifecycleState string
	var upstreamContacted int
	var existingDispatchedAt sql.NullInt64
	err := store.db.QueryRowContext(ctx, `SELECT
		lifecycle_state,
		upstream_contacted,
		upstream_dispatched_at
	FROM accounting_requests
	WHERE id = ?`, id).Scan(
		&lifecycleState,
		&upstreamContacted,
		&existingDispatchedAt,
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
		if existingDispatchedAt.Int64 == dispatchedUnixNano {
			return nil
		}
		return fmt.Errorf(
			"mark upstream dispatched request %d: already dispatched at %d; cannot replace with %d",
			id,
			existingDispatchedAt.Int64,
			dispatchedUnixNano,
		)
	}
	if upstreamContacted == 0 && !existingDispatchedAt.Valid {
		return fmt.Errorf(
			"mark upstream dispatched request %d: eligible dispatch evidence was not updated",
			id,
		)
	}

	storedDispatchTime := "NULL"
	if existingDispatchedAt.Valid {
		storedDispatchTime = fmt.Sprintf("%d", existingDispatchedAt.Int64)
	}
	return fmt.Errorf(
		"mark upstream dispatched request %d: inconsistent upstream dispatch evidence: contacted=%d, dispatched_at=%s",
		id,
		upstreamContacted,
		storedDispatchTime,
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
	routeSnapshot, err := derivePendingRoute(request)
	if err != nil {
		return pendingSnapshot{}, err
	}
	routeSnapshot.startedAt = startedAt
	return routeSnapshot, nil
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
	if err := registry.ValidContextName(request.Context); err != nil {
		return 0, fmt.Errorf("pending request context %q: %w", request.Context, err)
	}
	if strings.TrimSpace(request.RequestModel) == "" {
		return 0, fmt.Errorf("pending request model is blank")
	}
	return startedAt, nil
}

func derivePendingRoute(request PendingRequest) (pendingSnapshot, error) {
	upstream := request.Route.Upstream()
	if strings.TrimSpace(upstream) == "" {
		return pendingSnapshot{}, fmt.Errorf("pending request route upstream is blank")
	}
	resolvedModel := request.Route.Model
	if resolvedModel == "" {
		resolvedModel = request.RequestModel
	}
	if strings.TrimSpace(resolvedModel) == "" {
		return pendingSnapshot{}, fmt.Errorf("pending request resolved model is blank")
	}
	if request.Route.Alias != "" && strings.TrimSpace(request.Route.Alias) == "" {
		return pendingSnapshot{}, fmt.Errorf("pending request route alias is blank")
	}

	policy := request.Route.BillingPolicy()
	if !policy.Valid() {
		return pendingSnapshot{}, fmt.Errorf(
			"pending request route auth mode %q yields invalid billing policy %q",
			request.Route.Auth,
			policy,
		)
	}
	if err := validateBillingDeclaration(request.Route, policy); err != nil {
		return pendingSnapshot{}, err
	}
	if !request.CredentialClass.Valid() {
		return pendingSnapshot{}, fmt.Errorf("pending request credential class %q is invalid", request.CredentialClass)
	}
	class, err := billing.RealizeClass(policy, request.Route.BillingClass, request.CredentialClass)
	if err != nil {
		return pendingSnapshot{}, fmt.Errorf("realize pending request billing class: %w", err)
	}
	if class == "" {
		return pendingSnapshot{}, fmt.Errorf("realize pending request billing class returned an empty class")
	}

	var alias any
	if request.Route.Alias != "" {
		alias = request.Route.Alias
	}
	return pendingSnapshot{
		routeAlias:    alias,
		routeUpstream: upstream,
		resolvedModel: resolvedModel,
		billingPolicy: policy,
		billingClass:  class,
	}, nil
}

func validateBillingDeclaration(route registry.Route, policy billing.Policy) error {
	switch policy {
	case billing.PolicyExplicit:
		if !route.BillingClassSet {
			return fmt.Errorf("explicit billing policy requires BillingClassSet=true")
		}
		if !route.BillingClass.Valid() {
			return fmt.Errorf(
				"explicit billing policy requires a valid billing class, got %q",
				route.BillingClass,
			)
		}
	case billing.PolicyCallerDerived:
		if route.BillingClassSet {
			return fmt.Errorf("caller-derived billing policy requires BillingClassSet=false")
		}
		if route.BillingClass != "" {
			return fmt.Errorf(
				"caller-derived billing policy cannot declare billing class %q",
				route.BillingClass,
			)
		}
	case billing.PolicySigV4Metered:
		if route.BillingClassSet {
			return fmt.Errorf("sigv4-metered billing policy requires BillingClassSet=false")
		}
		if route.BillingClass != "" {
			return fmt.Errorf(
				"sigv4-metered billing policy cannot declare billing class %q",
				route.BillingClass,
			)
		}
	}
	return nil
}
