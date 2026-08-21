package usage

import (
	"context"
	"fmt"
	"strings"
	"time"

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
