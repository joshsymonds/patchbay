package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/accounting"
	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/registry"
)

type observedDoneContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (ctx *observedDoneContext) Done() <-chan struct{} {
	ctx.once.Do(func() {
		close(ctx.observed)
	})
	return ctx.Context.Done()
}

type storedRequest struct {
	id                         int64
	startedAt                  int64
	completedAt                sql.NullInt64
	upstreamDispatchedAt       sql.NullInt64
	requestKind                string
	lifecycleState             string
	outcome                    sql.NullString
	upstreamContacted          int
	routeHost                  string
	routeContext               string
	routeAlias                 sql.NullString
	routeUpstream              string
	requestModel               string
	resolvedModel              string
	authMode                   string
	credentialClass            string
	billingPolicy              string
	billingClass               sql.NullString
	costStatus                 string
	responseID                 sql.NullString
	httpStatus                 sql.NullInt64
	normalizationComplete      sql.NullInt64
	inputTokens                sql.NullInt64
	outputTokens               sql.NullInt64
	cacheCreationInputTokens   sql.NullInt64
	cacheReadInputTokens       sql.NullInt64
	cacheCreation5mInputTokens sql.NullInt64
	cacheCreation1hInputTokens sql.NullInt64
	providerCostPresent        sql.NullInt64
	providerCostValid          sql.NullInt64
	providerCostNanoUSD        sql.NullInt64
	stopReason                 sql.NullString
	blockShape                 sql.NullString
	durationMS                 sql.NullInt64
	upstreamMS                 sql.NullInt64
	costBasis                  sql.NullString
	finalCostNanoUSD           sql.NullInt64
	rateCardID                 sql.NullInt64
	upstreamTarget             sql.NullString
}

func TestRequestKindClosedVocabulary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind  RequestKind
		valid bool
	}{
		{kind: RequestMessages, valid: true},
		{kind: RequestCountTokens, valid: true},
		{kind: RequestOther, valid: true},
		{kind: "", valid: false},
		{kind: "Messages", valid: false},
		{kind: "count-tokens", valid: false},
	}
	for _, test := range tests {
		if got := test.kind.Valid(); got != test.valid {
			t.Errorf("RequestKind(%q).Valid() = %t; want %t", test.kind, got, test.valid)
		}
		if got := test.kind.String(); got != string(test.kind) {
			t.Errorf("RequestKind(%q).String() = %q; want %q", test.kind, got, test.kind)
		}
	}
}

func TestInsertPendingRejectsNilContextWithoutWriting(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)

	var nilContext context.Context
	if _, err := store.InsertPending(nilContext, validPendingRequest()); err == nil {
		t.Fatal("InsertPending(nil) returned nil error")
	}
	if got := countStoredRequests(t, store.db); got != 0 {
		t.Fatalf("stored request count = %d; want 0", got)
	}
}

func TestInsertPendingRejectsInvalidRequestsWithoutWriting(t *testing.T) {
	t.Parallel()

	background := context.Background
	tests := []struct {
		name         string
		requestCtx   func() context.Context
		mutate       func(*PendingRequest)
		wantContains string
		wantIs       error
	}{
		{
			name: "canceled context",
			requestCtx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantContains: "context",
			wantIs:       context.Canceled,
		},
		{
			name: "expired context deadline",
			requestCtx: func() context.Context {
				ctx, _ := context.WithDeadline(context.Background(), time.Unix(1, 0))
				return ctx
			},
			wantContains: "context",
			wantIs:       context.DeadlineExceeded,
		},
		{
			name:         "zero start time",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.StartedAt = time.Time{} },
			wantContains: "started at is zero",
		},
		{
			name:       "start time above UnixNano range",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.StartedAt = time.Date(2500, time.January, 1, 0, 0, 0, 0, time.UTC)
			},
			wantContains: "UnixNano",
		},
		{
			name:       "start time below UnixNano range",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.StartedAt = time.Date(1600, time.January, 1, 0, 0, 0, 0, time.UTC)
			},
			wantContains: "UnixNano",
		},
		{
			name:         "empty request kind",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.Kind = "" },
			wantContains: "request kind",
		},
		{
			name:         "unknown request kind",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.Kind = "completions" },
			wantContains: "request kind",
		},
		{
			name:         "empty host",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.Host = "" },
			wantContains: "host is blank",
		},
		{
			name:         "whitespace host",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.Host = " \t" },
			wantContains: "host is blank",
		},
		{
			name:         "empty context",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.Context = "" },
			wantContains: "context is blank",
		},
		{
			name:         "whitespace context",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.Context = " \t" },
			wantContains: "context is blank",
		},
		{
			name:         "uppercase context",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.Context = "Personal" },
			wantContains: "context",
		},
		{
			name:         "path context",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.Context = "personal/dev" },
			wantContains: "context",
		},
		{
			name:         "empty request model",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.RequestModel = "" },
			wantContains: "request model is blank",
		},
		{
			name:         "whitespace request model",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.RequestModel = " \t" },
			wantContains: "request model is blank",
		},
		{
			name:       "empty route upstream",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.Route.BaseURL = ""
			},
			wantContains: "route upstream is blank",
		},
		{
			name:       "whitespace route upstream",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.Route.BaseURL = " \t"
			},
			wantContains: "route upstream is blank",
		},
		{
			name:       "whitespace resolved model",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.Route.Model = " \t"
			},
			wantContains: "resolved model is blank",
		},
		{
			name:       "whitespace route alias",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.Route.Alias = " \t"
			},
			wantContains: "route alias is blank",
		},
		{
			name:       "invalid auth mode",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.Route.Auth = registry.AuthMode("invalid")
			},
			wantContains: "billing policy",
		},
		{
			name:       "explicit route missing class declaration",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.Route.Auth = registry.AuthInject
				request.Route.BillingClass = billing.ClassSubscription
				request.Route.BillingClassSet = false
			},
			wantContains: "BillingClassSet=true",
		},
		{
			name:       "explicit route empty class",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.Route.Auth = registry.AuthInject
				request.Route.BillingClass = ""
				request.Route.BillingClassSet = true
			},
			wantContains: "valid billing class",
		},
		{
			name:       "explicit route invalid class",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.Route.Auth = registry.AuthInject
				request.Route.BillingClass = "invalid"
				request.Route.BillingClassSet = true
			},
			wantContains: "valid billing class",
		},
		{
			name:       "forward route class-set marker",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.Route.BillingClassSet = true
			},
			wantContains: "BillingClassSet=false",
		},
		{
			name:       "forward route explicit class",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.Route.BillingClass = billing.ClassMetered
			},
			wantContains: "cannot declare billing class",
		},
		{
			name:       "sigv4 route class-set marker",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.Route = registry.Route{
					Auth:            registry.AuthSigV4,
					AWSRegion:       "us-west-2",
					BillingClassSet: true,
				}
			},
			wantContains: "BillingClassSet=false",
		},
		{
			name:       "sigv4 route explicit class",
			requestCtx: background,
			mutate: func(request *PendingRequest) {
				request.Route = registry.Route{
					Auth:         registry.AuthSigV4,
					AWSRegion:    "us-west-2",
					BillingClass: billing.ClassMetered,
				}
			},
			wantContains: "cannot declare billing class",
		},
		{
			name:         "empty credential class",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.CredentialClass = "" },
			wantContains: "credential class",
		},
		{
			name:         "invalid credential class",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.CredentialClass = "bearer" },
			wantContains: "credential class",
		},
		{
			name:         "forward route without credential",
			requestCtx:   background,
			mutate:       func(request *PendingRequest) { request.CredentialClass = billing.CredentialNone },
			wantContains: "realize pending request billing class",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			store := openStore(t, path)
			request := validPendingRequest()
			if test.mutate != nil {
				test.mutate(&request)
			}

			_, err := store.InsertPending(test.requestCtx(), request)
			if err == nil {
				t.Fatal("InsertPending() returned nil error")
			}
			if !strings.Contains(err.Error(), test.wantContains) {
				t.Fatalf("InsertPending() error = %q; want substring %q", err, test.wantContains)
			}
			if test.wantIs != nil && !errors.Is(err, test.wantIs) {
				t.Fatalf("InsertPending() error = %v; want errors.Is(_, %v)", err, test.wantIs)
			}
			if got := countStoredRequests(t, store.db); got != 0 {
				t.Fatalf("stored request count = %d; want 0", got)
			}
		})
	}
}

func TestInsertPendingValidatesBeforeWaitingForWritePermit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	if err := store.acquireWritePermit(context.Background()); err != nil {
		t.Fatalf("acquireWritePermit() returned error: %v", err)
	}
	t.Cleanup(store.releaseWritePermit)

	tests := []struct {
		name         string
		mutate       func(*PendingRequest)
		wantContains string
	}{
		{
			name:         "identity validation",
			mutate:       func(request *PendingRequest) { request.Host = "" },
			wantContains: "host is blank",
		},
		{
			name: "route snapshot derivation",
			mutate: func(request *PendingRequest) {
				request.CredentialClass = billing.CredentialClass("invalid")
			},
			wantContains: "credential class",
		},
	}

	for _, test := range tests {
		request := validPendingRequest()
		test.mutate(&request)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := store.InsertPending(ctx, request)
		cancel()
		if err == nil || !strings.Contains(err.Error(), test.wantContains) {
			t.Fatalf("%s: InsertPending() error = %v; want validation error containing %q",
				test.name, err, test.wantContains)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s: InsertPending() error = %v; validation waited for write permit", test.name, err)
		}
	}

	if got := countStoredRequests(t, store.db); got != 0 {
		t.Fatalf("stored request count = %d; want 0", got)
	}
}

func TestInsertPendingCanceledWhileWaitingForWritePermitDoesNotWriteOrPoisonPermit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	if err := store.acquireWritePermit(context.Background()); err != nil {
		t.Fatalf("acquireWritePermit() returned error: %v", err)
	}
	permitHeld := true
	t.Cleanup(func() {
		if permitHeld {
			store.releaseWritePermit()
		}
	})

	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &observedDoneContext{
		Context:  baseCtx,
		observed: make(chan struct{}),
	}
	type insertResult struct {
		id  int64
		err error
	}
	result := make(chan insertResult, 1)
	go func() {
		id, err := store.InsertPending(ctx, validPendingRequest())
		result <- insertResult{id: id, err: err}
	}()
	select {
	case <-ctx.observed:
	case <-time.After(time.Second):
		t.Fatal("InsertPending() did not begin waiting for the held write permit")
	}
	cancel()

	select {
	case got := <-result:
		if got.id != 0 {
			t.Errorf("InsertPending() ID = %d; want 0", got.id)
		}
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("InsertPending() error = %v; want errors.Is(_, context.Canceled)", got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("InsertPending() did not return after its queued context was canceled")
	}
	if got := countStoredRequests(t, store.db); got != 0 {
		t.Fatalf("stored request count = %d; want 0", got)
	}

	store.releaseWritePermit()
	permitHeld = false
	followupCtx, cancelFollowup := context.WithTimeout(context.Background(), time.Second)
	defer cancelFollowup()
	if _, err := store.InsertPending(followupCtx, validPendingRequest()); err != nil {
		t.Fatalf("follow-up InsertPending() returned error: %v", err)
	}
	if got := countStoredRequests(t, store.db); got != 1 {
		t.Fatalf("stored request count after follow-up = %d; want 1", got)
	}
}

func TestInsertPendingOnClosedStoreReturnsSQLFailureWithoutWriting(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q) returned error: %v", path, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() returned error: %v", err)
	}

	if _, err := store.InsertPending(context.Background(), validPendingRequest()); err == nil ||
		!strings.Contains(err.Error(), "insert pending request") {
		t.Fatalf("InsertPending() error = %v; want wrapped insert failure", err)
	}

	followupCtx, cancelFollowup := context.WithTimeout(context.Background(), time.Second)
	defer cancelFollowup()
	if _, err := store.InsertPending(followupCtx, validPendingRequest()); err == nil ||
		!strings.Contains(err.Error(), "insert pending request") ||
		errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("follow-up InsertPending() error = %v; want prompt wrapped insert failure", err)
	}

	db := inspectDB(t, path)
	if got := countStoredRequests(t, db); got != 0 {
		t.Fatalf("stored request count = %d; want 0", got)
	}
}

func TestInsertPendingIsDurableAcrossCloseAndReopen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q) returned error: %v", path, err)
	}
	request := validPendingRequest()
	request.Kind = RequestOther

	id, err := store.InsertPending(context.Background(), request)
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() returned error: %v", err)
	}

	reopened := openStore(t, path)
	got := readStoredRequest(t, reopened.db, id)
	if got.id != id || got.requestKind != RequestOther.String() {
		t.Fatalf("reopened request = %#v; want ID %d and kind %q", got, id, RequestOther)
	}
	if count := countStoredRequests(t, reopened.db); count != 1 {
		t.Fatalf("reopened request count = %d; want 1", count)
	}
}

func TestInsertPendingConcurrentWritesAreLossless(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	const writers = 64

	ids := make(chan int64, writers)
	errs := make(chan error, writers)
	var wait sync.WaitGroup
	for writer := range writers {
		wait.Add(1)
		go func(writer int) {
			defer wait.Done()
			request := validPendingRequest()
			request.StartedAt = request.StartedAt.Add(time.Duration(writer))
			id, err := store.InsertPending(context.Background(), request)
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}(writer)
	}
	wait.Wait()
	close(ids)
	close(errs)

	for err := range errs {
		t.Errorf("concurrent InsertPending() returned error: %v", err)
	}
	seen := make(map[int64]struct{}, writers)
	for id := range ids {
		if id <= 0 {
			t.Errorf("concurrent InsertPending() ID = %d; want positive", id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Errorf("concurrent InsertPending() returned duplicate ID %d", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != writers {
		t.Errorf("unique concurrent IDs = %d; want %d", len(seen), writers)
	}
	if count := countStoredRequests(t, store.db); count != writers {
		t.Errorf("stored request count = %d; want %d", count, writers)
	}
}

func TestInsertPendingPersistsForwardOAuthSnapshot(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	startedAt := time.Unix(1_776_912_345, 678_901_234).UTC()

	id, err := store.InsertPending(context.Background(), PendingRequest{
		StartedAt:       startedAt,
		Kind:            RequestMessages,
		Host:            "gnomon",
		Context:         "personal",
		Route:           registry.DefaultRoute(),
		RequestModel:    "claude-opus-5",
		CredentialClass: billing.CredentialOAuth,
	})
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	if id <= 0 {
		t.Fatalf("InsertPending() ID = %d; want positive", id)
	}

	got := readStoredRequest(t, store.db, id)
	want := storedRequest{
		id:                id,
		startedAt:         startedAt.UnixNano(),
		requestKind:       RequestMessages.String(),
		lifecycleState:    "pending",
		upstreamContacted: 0,
		routeHost:         "gnomon",
		routeContext:      "personal",
		routeUpstream:     registry.DefaultRoute().Upstream(),
		requestModel:      "claude-opus-5",
		resolvedModel:     "claude-opus-5",
		authMode:          string(registry.AuthForward),
		credentialClass:   billing.CredentialOAuth.String(),
		billingPolicy:     billing.PolicyCallerDerived.String(),
		billingClass:      sql.NullString{String: billing.ClassSubscription.String(), Valid: true},
		costStatus:        "pending",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored request mismatch:\n got %#v\nwant %#v", got, want)
	}
}

func TestInsertPendingPersistsResolvedRouteBillingMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		route           registry.Route
		credentialClass billing.CredentialClass
		wantPolicy      billing.Policy
		wantClass       billing.Class
	}{
		{
			name: "forward API key is metered",
			route: registry.Route{
				Alias:   "forward-metered",
				BaseURL: "opaque-forward-upstream",
				Model:   "opaque/forward-model",
				Auth:    registry.AuthForward,
			},
			credentialClass: billing.CredentialAPIKey,
			wantPolicy:      billing.PolicyCallerDerived,
			wantClass:       billing.ClassMetered,
		},
		{
			name: "explicit subscription",
			route: registry.Route{
				Alias:           "inject-subscription",
				BaseURL:         "opaque-subscription-upstream",
				Model:           "opaque/subscription-model",
				Auth:            registry.AuthInject,
				BillingClass:    billing.ClassSubscription,
				BillingClassSet: true,
			},
			credentialClass: billing.CredentialNone,
			wantPolicy:      billing.PolicyExplicit,
			wantClass:       billing.ClassSubscription,
		},
		{
			name: "explicit metered",
			route: registry.Route{
				Alias:           "inject-metered",
				BaseURL:         "opaque-metered-upstream",
				Model:           "opaque/metered-model",
				Auth:            registry.AuthInject,
				BillingClass:    billing.ClassMetered,
				BillingClassSet: true,
			},
			credentialClass: billing.CredentialNone,
			wantPolicy:      billing.PolicyExplicit,
			wantClass:       billing.ClassMetered,
		},
		{
			name: "explicit self hosted",
			route: registry.Route{
				Alias:           "inject-self-hosted",
				BaseURL:         "opaque-self-hosted-upstream",
				Model:           "opaque/self-hosted-model",
				Auth:            registry.AuthInject,
				BillingClass:    billing.ClassSelfHosted,
				BillingClassSet: true,
			},
			credentialClass: billing.CredentialNone,
			wantPolicy:      billing.PolicyExplicit,
			wantClass:       billing.ClassSelfHosted,
		},
		{
			name: "sigv4 is metered",
			route: registry.Route{
				Alias:     "bedrock",
				Model:     "anthropic.claude-opus-5-v1:0",
				Auth:      registry.AuthSigV4,
				AWSRegion: "us-west-2",
			},
			credentialClass: billing.CredentialNone,
			wantPolicy:      billing.PolicySigV4Metered,
			wantClass:       billing.ClassMetered,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			store := openStore(t, path)
			startedAt := time.Unix(1_776_912_345, 123_456_789).UTC()
			request := PendingRequest{
				StartedAt:       startedAt,
				Kind:            RequestCountTokens,
				Host:            "vermissian",
				Context:         "work-2",
				Route:           test.route,
				RequestModel:    "caller/model-name",
				CredentialClass: test.credentialClass,
			}

			id, err := store.InsertPending(context.Background(), request)
			if err != nil {
				t.Fatalf("InsertPending() returned error: %v", err)
			}

			got := readStoredRequest(t, store.db, id)
			want := storedRequest{
				id:                id,
				startedAt:         startedAt.UnixNano(),
				requestKind:       RequestCountTokens.String(),
				lifecycleState:    "pending",
				upstreamContacted: 0,
				routeHost:         request.Host,
				routeContext:      request.Context,
				routeAlias:        sql.NullString{String: test.route.Alias, Valid: true},
				routeUpstream:     test.route.Upstream(),
				requestModel:      request.RequestModel,
				resolvedModel:     test.route.Model,
				authMode:          string(test.route.Auth),
				credentialClass:   test.credentialClass.String(),
				billingPolicy:     test.wantPolicy.String(),
				billingClass:      sql.NullString{String: test.wantClass.String(), Valid: true},
				costStatus:        "pending",
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("stored request mismatch:\n got %#v\nwant %#v", got, want)
			}
		})
	}
}

func TestFinalizePersistsCompleteSnapshot(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id, err := store.InsertPending(context.Background(), validPendingRequest())
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	dispatchedAt := validPendingRequest().StartedAt.Add(200 * time.Millisecond)
	if err := store.MarkUpstreamDispatched(context.Background(), id, dispatchedAt, ""); err != nil {
		t.Fatalf("MarkUpstreamDispatched() returned error: %v", err)
	}

	finalized := validFinalizedRequest()
	if err := store.Finalize(context.Background(), id, finalized); err != nil {
		t.Fatalf("Finalize() returned error: %v", err)
	}

	got := readStoredRequest(t, store.db, id)
	if got.lifecycleState != "finalized" ||
		got.completedAt != (sql.NullInt64{Int64: finalized.CompletedAt.UnixNano(), Valid: true}) {
		t.Fatalf("final lifecycle = %q completed %#v; want finalized at %d",
			got.lifecycleState, got.completedAt, finalized.CompletedAt.UnixNano())
	}
	if got.outcome != (sql.NullString{String: string(finalized.Outcome), Valid: true}) ||
		got.costStatus != string(finalized.CostStatus) {
		t.Fatalf("final outcome/cost = %#v/%q; want %q/%q",
			got.outcome, got.costStatus, finalized.Outcome, finalized.CostStatus)
	}
	if got.responseID != (sql.NullString{String: finalized.ResponseID, Valid: true}) ||
		got.httpStatus != (sql.NullInt64{Int64: int64(finalized.HTTPStatus), Valid: true}) ||
		got.normalizationComplete != (sql.NullInt64{Int64: 1, Valid: true}) {
		t.Fatalf("response metadata = ID %#v HTTP %#v normalization %#v",
			got.responseID, got.httpStatus, got.normalizationComplete)
	}
	for name, pair := range map[string]struct {
		got  sql.NullInt64
		want int64
	}{
		"input":          {got.inputTokens, finalized.InputTokens},
		"output":         {got.outputTokens, finalized.OutputTokens},
		"cache creation": {got.cacheCreationInputTokens, finalized.CacheCreationInputTokens},
		"cache read":     {got.cacheReadInputTokens, finalized.CacheReadInputTokens},
		"cache 5m":       {got.cacheCreation5mInputTokens, finalized.CacheCreation5mInputTokens},
		"cache 1h":       {got.cacheCreation1hInputTokens, finalized.CacheCreation1hInputTokens},
	} {
		if pair.got != (sql.NullInt64{Int64: pair.want, Valid: true}) {
			t.Errorf("%s tokens = %#v; want %d present", name, pair.got, pair.want)
		}
	}
	if got.providerCostPresent != (sql.NullInt64{Int64: 1, Valid: true}) ||
		got.providerCostValid != (sql.NullInt64{Int64: 1, Valid: true}) ||
		got.providerCostNanoUSD != (sql.NullInt64{Int64: int64(finalized.ProviderCost), Valid: true}) {
		t.Fatalf("provider cost tuple = %#v/%#v/%#v",
			got.providerCostPresent, got.providerCostValid, got.providerCostNanoUSD)
	}
	if got.stopReason != (sql.NullString{String: finalized.StopReason, Valid: true}) ||
		got.blockShape != (sql.NullString{String: `["thinking","text"]`, Valid: true}) ||
		got.durationMS != (sql.NullInt64{Int64: finalized.DurationMS, Valid: true}) ||
		got.upstreamMS != (sql.NullInt64{Int64: finalized.UpstreamMS, Valid: true}) {
		t.Fatalf("final details = stop %#v shape %#v duration %#v upstream %#v",
			got.stopReason, got.blockShape, got.durationMS, got.upstreamMS)
	}
}

func TestFinalizationVocabularies(t *testing.T) {
	t.Parallel()

	outcomes := []Outcome{
		OutcomeSuccess,
		OutcomeLocalRejection,
		OutcomeUpstreamHTTPFailure,
		OutcomeTransportFailure,
		OutcomeResponseInterrupted,
		OutcomeNormalizationFailure,
	}
	wantOutcomes := []string{
		"success",
		"local_rejection",
		"upstream_http_failure",
		"transport_failure",
		"response_interrupted",
		"normalization_failure",
	}
	for index, outcome := range outcomes {
		if !outcome.Valid() || outcome.String() != wantOutcomes[index] {
			t.Errorf("outcome %d = %q valid=%t; want %q valid", index, outcome, outcome.Valid(), wantOutcomes[index])
		}
	}
	for _, outcome := range []Outcome{"", "Success", "unknown"} {
		if outcome.Valid() {
			t.Errorf("Outcome(%q).Valid() = true; want false", outcome)
		}
	}

	statuses := []CostStatus{CostStatusPending, CostStatusKnown, CostStatusUnknown}
	wantStatuses := []string{"pending", "known", "unknown"}
	for index, status := range statuses {
		if !status.Valid() || status.String() != wantStatuses[index] {
			t.Errorf("cost status %d = %q valid=%t; want %q valid", index, status, status.Valid(), wantStatuses[index])
		}
	}
	for _, status := range []CostStatus{"", "Known", "unavailable"} {
		if status.Valid() {
			t.Errorf("CostStatus(%q).Valid() = true; want false", status)
		}
	}
}

func TestFinalizeAcceptsEveryOutcomeAndFinalCostStatus(t *testing.T) {
	t.Parallel()

	for _, outcome := range []Outcome{
		OutcomeSuccess,
		OutcomeLocalRejection,
		OutcomeUpstreamHTTPFailure,
		OutcomeTransportFailure,
		OutcomeResponseInterrupted,
		OutcomeNormalizationFailure,
	} {
		for _, status := range []CostStatus{CostStatusKnown, CostStatusUnknown} {
			t.Run(outcome.String()+"/"+status.String(), func(t *testing.T) {
				t.Parallel()

				path := filepath.Join(t.TempDir(), "usage.sqlite")
				store := openStore(t, path)
				id := insertPendingRequest(t, store)
				finalized := validFinalizedRequest()
				finalized.Outcome = outcome
				finalized.CostStatus = status
				if err := store.Finalize(context.Background(), id, finalized); err != nil {
					t.Fatalf("Finalize() returned error: %v", err)
				}
				got := readStoredRequest(t, store.db, id)
				if got.outcome.String != outcome.String() || got.costStatus != status.String() {
					t.Fatalf("stored outcome/status = %q/%q; want %q/%q",
						got.outcome.String, got.costStatus, outcome, status)
				}
			})
		}
	}
}

func TestFinalizePreservesTokenAbsenceAndExplicitZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		set  func(*FinalizedRequest, bool)
		read func(storedRequest) sql.NullInt64
	}{
		{
			"input",
			func(value *FinalizedRequest, present bool) { value.InputTokens, value.InputTokensPresent = 0, present },
			func(row storedRequest) sql.NullInt64 { return row.inputTokens },
		},
		{"output", func(value *FinalizedRequest, present bool) {
			value.OutputTokens, value.OutputTokensPresent = 0, present
		}, func(row storedRequest) sql.NullInt64 { return row.outputTokens }},
		{"cache creation", func(value *FinalizedRequest, present bool) {
			value.CacheCreationInputTokens, value.CacheCreationInputTokensPresent = 0, present
		}, func(row storedRequest) sql.NullInt64 { return row.cacheCreationInputTokens }},
		{"cache read", func(value *FinalizedRequest, present bool) {
			value.CacheReadInputTokens, value.CacheReadInputTokensPresent = 0, present
		}, func(row storedRequest) sql.NullInt64 { return row.cacheReadInputTokens }},
		{"cache 5m", func(value *FinalizedRequest, present bool) {
			value.CacheCreation5mInputTokens, value.CacheCreation5mInputTokensPresent = 0, present
		}, func(row storedRequest) sql.NullInt64 { return row.cacheCreation5mInputTokens }},
		{"cache 1h", func(value *FinalizedRequest, present bool) {
			value.CacheCreation1hInputTokens, value.CacheCreation1hInputTokensPresent = 0, present
		}, func(row storedRequest) sql.NullInt64 { return row.cacheCreation1hInputTokens }},
	}
	for _, test := range tests {
		for _, present := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/present=%t", test.name, present), func(t *testing.T) {
				t.Parallel()

				path := filepath.Join(t.TempDir(), "usage.sqlite")
				store := openStore(t, path)
				id := insertPendingRequest(t, store)
				finalized := validFinalizedRequest()
				test.set(&finalized, present)
				if err := store.Finalize(context.Background(), id, finalized); err != nil {
					t.Fatalf("Finalize() returned error: %v", err)
				}
				got := test.read(readStoredRequest(t, store.db, id))
				want := sql.NullInt64{Int64: 0, Valid: present}
				if got != want {
					t.Fatalf("stored token = %#v; want %#v", got, want)
				}
			})
		}
	}
}

func TestFinalizeRequiresZeroForAbsentTokensAndReplaysThem(t *testing.T) {
	t.Parallel()

	tokenFields := []struct {
		name string
		set  func(*FinalizedRequest, int64)
	}{
		{"input", func(value *FinalizedRequest, tokens int64) {
			value.InputTokens, value.InputTokensPresent = tokens, false
		}},
		{"output", func(value *FinalizedRequest, tokens int64) {
			value.OutputTokens, value.OutputTokensPresent = tokens, false
		}},
		{"cache creation", func(value *FinalizedRequest, tokens int64) {
			value.CacheCreationInputTokens, value.CacheCreationInputTokensPresent = tokens, false
		}},
		{"cache read", func(value *FinalizedRequest, tokens int64) {
			value.CacheReadInputTokens, value.CacheReadInputTokensPresent = tokens, false
		}},
		{"cache 5m", func(value *FinalizedRequest, tokens int64) {
			value.CacheCreation5mInputTokens, value.CacheCreation5mInputTokensPresent = tokens, false
		}},
		{"cache 1h", func(value *FinalizedRequest, tokens int64) {
			value.CacheCreation1hInputTokens, value.CacheCreation1hInputTokensPresent = tokens, false
		}},
	}
	for _, field := range tokenFields {
		for _, tokens := range []int64{-1, 1} {
			t.Run(fmt.Sprintf("rejects %s=%d when absent", field.name, tokens), func(t *testing.T) {
				t.Parallel()

				path := filepath.Join(t.TempDir(), "usage.sqlite")
				store := openStore(t, path)
				id := insertPendingRequest(t, store)
				before := readStoredRequest(t, store.db, id)
				finalized := validFinalizedRequest()
				field.set(&finalized, tokens)
				if err := store.Finalize(context.Background(), id, finalized); err == nil {
					t.Fatal("Finalize() returned nil error")
				}
				if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, before) {
					t.Fatalf("row changed after rejected absent tokens:\n got %#v\nwant %#v", got, before)
				}
			})
		}
	}

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id := insertPendingRequest(t, store)
	finalized := validFinalizedRequest()
	for _, field := range tokenFields {
		field.set(&finalized, 0)
	}
	if err := store.Finalize(context.Background(), id, finalized); err != nil {
		t.Fatalf("first Finalize() returned error: %v", err)
	}
	if err := store.Finalize(context.Background(), id, finalized); err != nil {
		t.Fatalf("exact absent-token replay returned error: %v", err)
	}
	got := readStoredRequest(t, store.db, id)
	for name, value := range map[string]sql.NullInt64{
		"input":          got.inputTokens,
		"output":         got.outputTokens,
		"cache creation": got.cacheCreationInputTokens,
		"cache read":     got.cacheReadInputTokens,
		"cache 5m":       got.cacheCreation5mInputTokens,
		"cache 1h":       got.cacheCreation1hInputTokens,
	} {
		if value.Valid {
			t.Errorf("%s tokens = %#v; want NULL", name, value)
		}
	}
}

func TestFinalizeCanonicalizesResponseIDHTTPStatusAndBlockShape(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name         string
		responseID   string
		httpStatus   int
		blockShape   []string
		wantResponse sql.NullString
		wantHTTP     sql.NullInt64
		wantShape    string
	}{
		{name: "null response and HTTP with nil shape", wantShape: "[]"},
		{name: "empty shape", blockShape: []string{}, wantShape: "[]"},
		{
			name:         "HTTP lower bound and ordered shape",
			responseID:   "msg_lower",
			httpStatus:   100,
			blockShape:   []string{"thinking", "tool_use", "text"},
			wantResponse: sql.NullString{String: "msg_lower", Valid: true},
			wantHTTP:     sql.NullInt64{Int64: 100, Valid: true},
			wantShape:    `["thinking","tool_use","text"]`,
		},
		{
			name:         "HTTP upper bound",
			responseID:   "msg_upper",
			httpStatus:   599,
			wantResponse: sql.NullString{String: "msg_upper", Valid: true},
			wantHTTP:     sql.NullInt64{Int64: 599, Valid: true},
			wantShape:    "[]",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			store := openStore(t, path)
			id := insertPendingRequest(t, store)
			finalized := validFinalizedRequest()
			finalized.ResponseID = test.responseID
			finalized.HTTPStatus = test.httpStatus
			finalized.BlockShape = test.blockShape
			if err := store.Finalize(context.Background(), id, finalized); err != nil {
				t.Fatalf("Finalize() returned error: %v", err)
			}
			got := readStoredRequest(t, store.db, id)
			if got.responseID != test.wantResponse || got.httpStatus != test.wantHTTP ||
				got.blockShape != (sql.NullString{String: test.wantShape, Valid: true}) {
				t.Fatalf("stored response/http/shape = %#v/%#v/%#v; want %#v/%#v/%q",
					got.responseID, got.httpStatus, got.blockShape,
					test.wantResponse, test.wantHTTP, test.wantShape)
			}
		})
	}
}

func TestFinalizeRejectsInvalidUTF8BlockShapeBeforeMutation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id := insertPendingRequest(t, store)
	before := readStoredRequest(t, store.db, id)
	first := validFinalizedRequest()
	first.BlockShape = []string{"\xff"}
	if err := store.Finalize(context.Background(), id, first); err == nil {
		// encoding/json replaces both \xff and \xfe with U+FFFD. Requiring valid UTF-8
		// before encoding prevents differently shaped finalizations from colliding on replay.
		second := first
		second.BlockShape = []string{"\xfe"}
		if err := store.Finalize(context.Background(), id, second); err == nil {
			t.Fatal("differing invalid-UTF-8 replay returned nil error")
		}
		t.Fatal("Finalize() with invalid UTF-8 block shape returned nil error")
	}
	if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, before) {
		t.Fatalf("row changed after rejected invalid UTF-8 block shape:\n got %#v\nwant %#v", got, before)
	}
}

func TestFinalizeRejectsInvalidValuesBeforeMutation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*FinalizedRequest)
	}{
		{"blank outcome", func(value *FinalizedRequest) { value.Outcome = "" }},
		{"invalid outcome", func(value *FinalizedRequest) { value.Outcome = "unknown" }},
		{"pending cost status", func(value *FinalizedRequest) { value.CostStatus = CostStatusPending }},
		{"blank cost status", func(value *FinalizedRequest) { value.CostStatus = "" }},
		{"invalid cost status", func(value *FinalizedRequest) { value.CostStatus = "Known" }},
		{"HTTP below range", func(value *FinalizedRequest) { value.HTTPStatus = 99 }},
		{"HTTP above range", func(value *FinalizedRequest) { value.HTTPStatus = 600 }},
		{"negative input", func(value *FinalizedRequest) { value.InputTokens = -1 }},
		{"negative output", func(value *FinalizedRequest) { value.OutputTokens = -1 }},
		{"negative cache creation", func(value *FinalizedRequest) { value.CacheCreationInputTokens = -1 }},
		{"negative cache read", func(value *FinalizedRequest) { value.CacheReadInputTokens = -1 }},
		{"negative cache 5m", func(value *FinalizedRequest) { value.CacheCreation5mInputTokens = -1 }},
		{"negative cache 1h", func(value *FinalizedRequest) { value.CacheCreation1hInputTokens = -1 }},
		{"negative duration", func(value *FinalizedRequest) { value.DurationMS = -1 }},
		{"negative upstream duration", func(value *FinalizedRequest) { value.UpstreamMS = -1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			store := openStore(t, path)
			id := insertPendingRequest(t, store)
			before := readStoredRequest(t, store.db, id)
			finalized := validFinalizedRequest()
			test.mutate(&finalized)
			if err := store.Finalize(context.Background(), id, finalized); err == nil {
				t.Fatal("Finalize() returned nil error")
			}
			if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, before) {
				t.Fatalf("row changed after rejected finalization:\n got %#v\nwant %#v", got, before)
			}
		})
	}
}

func TestFinalizeEnforcesResponseIDUniquenessWithoutMutatingLoser(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	firstStore := openStore(t, path)
	secondStore := openStore(t, path)
	firstID := insertPendingRequest(t, firstStore)
	secondID := insertPendingRequest(t, firstStore)
	finalized := validFinalizedRequest()
	finalized.ResponseID = "msg_unique"
	if err := firstStore.Finalize(context.Background(), firstID, finalized); err != nil {
		t.Fatalf("first Finalize() returned error: %v", err)
	}
	before := readStoredRequest(t, secondStore.db, secondID)
	if err := secondStore.Finalize(context.Background(), secondID, finalized); err == nil {
		t.Fatal("duplicate response ID Finalize() returned nil error")
	}
	if got := readStoredRequest(t, secondStore.db, secondID); !reflect.DeepEqual(got, before) {
		t.Fatalf("losing row changed after uniqueness failure:\n got %#v\nwant %#v", got, before)
	}
	assertWritePermitAvailable(t, secondStore)
	finalized.ResponseID = ""
	if err := secondStore.Finalize(context.Background(), secondID, finalized); err != nil {
		t.Fatalf("Finalize() with NULL response ID returned error: %v", err)
	}
	thirdID := insertPendingRequest(t, firstStore)
	if err := firstStore.Finalize(context.Background(), thirdID, finalized); err != nil {
		t.Fatalf("second Finalize() with NULL response ID returned error: %v", err)
	}
}

func TestFinalizeProviderCostTuples(t *testing.T) {
	t.Parallel()

	valid := []struct {
		name    string
		present bool
		valid   bool
		amount  accounting.NanoUSD
	}{
		{name: "absent", present: false, valid: false, amount: 0},
		{name: "present invalid", present: true, valid: false, amount: 0},
		{name: "present valid zero", present: true, valid: true, amount: 0},
		{name: "present valid maximum", present: true, valid: true, amount: accounting.NanoUSD(math.MaxInt64)},
	}
	for _, test := range valid {
		t.Run("valid/"+test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			store := openStore(t, path)
			id := insertPendingRequest(t, store)
			finalized := validFinalizedRequest()
			finalized.ProviderCostPresent = test.present
			finalized.ProviderCostValid = test.valid
			finalized.ProviderCost = test.amount
			if err := store.Finalize(context.Background(), id, finalized); err != nil {
				t.Fatalf("Finalize() returned error: %v", err)
			}
			got := readStoredRequest(t, store.db, id)
			if got.providerCostPresent.Int64 != boolInt64(test.present) ||
				got.providerCostValid.Int64 != boolInt64(test.valid) ||
				got.providerCostNanoUSD.Int64 != int64(test.amount) {
				t.Fatalf("stored tuple = %d/%d/%d; want %t/%t/%d",
					got.providerCostPresent.Int64, got.providerCostValid.Int64,
					got.providerCostNanoUSD.Int64, test.present, test.valid, test.amount)
			}
		})
	}

	invalid := []struct {
		name    string
		present bool
		valid   bool
		amount  accounting.NanoUSD
	}{
		{name: "absent positive", amount: 1},
		{name: "absent negative", amount: -1},
		{name: "absent marked valid", valid: true},
		{name: "absent marked valid positive", valid: true, amount: 1},
		{name: "present invalid positive", present: true, amount: 1},
		{name: "present invalid negative", present: true, amount: -1},
		{name: "present valid negative", present: true, valid: true, amount: -1},
	}
	for _, test := range invalid {
		t.Run("invalid/"+test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			store := openStore(t, path)
			id := insertPendingRequest(t, store)
			before := readStoredRequest(t, store.db, id)
			finalized := validFinalizedRequest()
			finalized.ProviderCostPresent = test.present
			finalized.ProviderCostValid = test.valid
			finalized.ProviderCost = test.amount
			if err := store.Finalize(context.Background(), id, finalized); err == nil {
				t.Fatal("Finalize() returned nil error")
			}
			if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, before) {
				t.Fatalf("row changed after rejected tuple:\n got %#v\nwant %#v", got, before)
			}
		})
	}
}

func TestFinalizeChronology(t *testing.T) {
	t.Parallel()

	startedAt := validPendingRequest().StartedAt
	for _, test := range []struct {
		name         string
		dispatchedAt *time.Time
		completedAt  time.Time
		wantError    bool
	}{
		{name: "completion equals start", completedAt: startedAt},
		{name: "completion precedes start", completedAt: startedAt.Add(-time.Nanosecond), wantError: true},
		{name: "completion equals dispatch", dispatchedAt: timePointer(startedAt.Add(time.Second)), completedAt: startedAt.Add(time.Second)},
		{name: "completion precedes dispatch", dispatchedAt: timePointer(startedAt.Add(time.Second)), completedAt: startedAt.Add(time.Second - time.Nanosecond), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			store := openStore(t, path)
			id := insertPendingRequest(t, store)
			if test.dispatchedAt != nil {
				if err := store.MarkUpstreamDispatched(context.Background(), id, *test.dispatchedAt, ""); err != nil {
					t.Fatalf("MarkUpstreamDispatched() returned error: %v", err)
				}
			}
			before := readStoredRequest(t, store.db, id)
			finalized := validFinalizedRequest()
			finalized.CompletedAt = test.completedAt
			err := store.Finalize(context.Background(), id, finalized)
			if test.wantError {
				if err == nil {
					t.Fatal("Finalize() returned nil error")
				}
				if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, before) {
					t.Fatalf("row changed after chronology failure:\n got %#v\nwant %#v", got, before)
				}
				return
			}
			if err != nil {
				t.Fatalf("Finalize() returned error: %v", err)
			}
		})
	}
}

func TestFinalizeRejectsInvalidCompletionAndStoredDispatchState(t *testing.T) {
	t.Parallel()

	for _, completedAt := range []time.Time{
		{},
		time.Date(2500, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1600, time.January, 1, 0, 0, 0, 0, time.UTC),
	} {
		path := filepath.Join(t.TempDir(), "usage.sqlite")
		store := openStore(t, path)
		id := insertPendingRequest(t, store)
		before := readStoredRequest(t, store.db, id)
		finalized := validFinalizedRequest()
		finalized.CompletedAt = completedAt
		if err := store.Finalize(context.Background(), id, finalized); err == nil {
			t.Fatalf("Finalize() with completion %v returned nil error", completedAt)
		}
		if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, before) {
			t.Fatalf("row changed after invalid completion:\n got %#v\nwant %#v", got, before)
		}
	}

	for _, test := range []struct {
		name       string
		contacted  int
		dispatched any
	}{
		{name: "uncontacted with timestamp", contacted: 0, dispatched: validPendingRequest().StartedAt.UnixNano()},
		{name: "contacted without timestamp", contacted: 1},
		{name: "invalid contacted marker", contacted: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			store := openStore(t, path)
			id := insertPendingRequest(t, store)
			setStoredDispatchState(t, store.db, id, "pending", test.contacted, test.dispatched)
			before := readStoredRequest(t, store.db, id)
			if err := store.Finalize(context.Background(), id, validFinalizedRequest()); err == nil {
				t.Fatal("Finalize() returned nil error")
			}
			if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, before) {
				t.Fatalf("row changed after dispatch-state failure:\n got %#v\nwant %#v", got, before)
			}
		})
	}
}

func TestFinalizeIsExactlyIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id := insertPendingRequest(t, store)
	finalized := validFinalizedRequest()
	finalized.BlockShape = nil
	if err := store.Finalize(context.Background(), id, finalized); err != nil {
		t.Fatalf("first Finalize() returned error: %v", err)
	}
	want := readStoredRequest(t, store.db, id)
	finalized.BlockShape = []string{}
	if err := store.Finalize(context.Background(), id, finalized); err != nil {
		t.Fatalf("canonical exact replay returned error: %v", err)
	}
	if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("row changed after exact replay:\n got %#v\nwant %#v", got, want)
	}
}

func TestFinalizeRejectsEveryDifferingReplayField(t *testing.T) {
	t.Parallel()

	mutations := []struct {
		name   string
		mutate func(*FinalizedRequest)
	}{
		{"completed at", func(value *FinalizedRequest) { value.CompletedAt = value.CompletedAt.Add(time.Nanosecond) }},
		{"outcome", func(value *FinalizedRequest) { value.Outcome = OutcomeTransportFailure }},
		{"HTTP status", func(value *FinalizedRequest) { value.HTTPStatus = 201 }},
		{"response ID", func(value *FinalizedRequest) { value.ResponseID = "msg_different" }},
		{"normalization", func(value *FinalizedRequest) { value.NormalizationComplete = false }},
		{"input tokens", func(value *FinalizedRequest) { value.InputTokens++ }},
		{"input presence", func(value *FinalizedRequest) { value.InputTokensPresent = false }},
		{"output tokens", func(value *FinalizedRequest) { value.OutputTokens++ }},
		{"output presence", func(value *FinalizedRequest) { value.OutputTokensPresent = false }},
		{"cache creation tokens", func(value *FinalizedRequest) { value.CacheCreationInputTokens++ }},
		{"cache creation presence", func(value *FinalizedRequest) { value.CacheCreationInputTokensPresent = false }},
		{"cache read tokens", func(value *FinalizedRequest) { value.CacheReadInputTokens++ }},
		{"cache read presence", func(value *FinalizedRequest) { value.CacheReadInputTokensPresent = false }},
		{"cache 5m tokens", func(value *FinalizedRequest) { value.CacheCreation5mInputTokens++ }},
		{"cache 5m presence", func(value *FinalizedRequest) { value.CacheCreation5mInputTokensPresent = false }},
		{"cache 1h tokens", func(value *FinalizedRequest) { value.CacheCreation1hInputTokens++ }},
		{"cache 1h presence", func(value *FinalizedRequest) { value.CacheCreation1hInputTokensPresent = false }},
		{"provider absent", func(value *FinalizedRequest) {
			value.ProviderCostPresent, value.ProviderCostValid, value.ProviderCost = false, false, 0
		}},
		{"provider invalid", func(value *FinalizedRequest) { value.ProviderCostValid, value.ProviderCost = false, 0 }},
		{"provider amount", func(value *FinalizedRequest) { value.ProviderCost++ }},
		{"stop reason", func(value *FinalizedRequest) { value.StopReason = "max_tokens" }},
		{"block shape", func(value *FinalizedRequest) { value.BlockShape = []string{"text", "thinking"} }},
		{"duration", func(value *FinalizedRequest) { value.DurationMS++ }},
		{"upstream duration", func(value *FinalizedRequest) { value.UpstreamMS++ }},
		{"cost status", func(value *FinalizedRequest) { value.CostStatus = CostStatusUnknown }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			store := openStore(t, path)
			id := insertPendingRequest(t, store)
			finalized := validFinalizedRequest()
			if err := store.Finalize(context.Background(), id, finalized); err != nil {
				t.Fatalf("first Finalize() returned error: %v", err)
			}
			before := readStoredRequest(t, store.db, id)
			test.mutate(&finalized)
			if err := store.Finalize(context.Background(), id, finalized); err == nil {
				t.Fatal("differing replay returned nil error")
			}
			if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, before) {
				t.Fatalf("row changed after differing replay:\n got %#v\nwant %#v", got, before)
			}
		})
	}
}

func TestFinalizeRejectsUnknownIDsAndPreservesImmutableColumns(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id := insertPendingRequest(t, store)
	dispatchedAt := validPendingRequest().StartedAt.Add(time.Millisecond)
	if err := store.MarkUpstreamDispatched(context.Background(), id, dispatchedAt, ""); err != nil {
		t.Fatalf("MarkUpstreamDispatched() returned error: %v", err)
	}
	before := readStoredRequest(t, store.db, id)
	for _, unknownID := range []int64{-1, 0, id + 1000} {
		if err := store.Finalize(context.Background(), unknownID, validFinalizedRequest()); err == nil {
			t.Errorf("Finalize(%d) returned nil error", unknownID)
		}
	}
	if err := store.Finalize(context.Background(), id, validFinalizedRequest()); err != nil {
		t.Fatalf("Finalize() returned error: %v", err)
	}
	got := readStoredRequest(t, store.db, id)
	if got.id != before.id || got.startedAt != before.startedAt ||
		got.upstreamDispatchedAt != before.upstreamDispatchedAt ||
		got.upstreamContacted != before.upstreamContacted ||
		got.requestKind != before.requestKind || got.routeHost != before.routeHost ||
		got.routeContext != before.routeContext || got.routeAlias != before.routeAlias ||
		got.routeUpstream != before.routeUpstream || got.requestModel != before.requestModel ||
		got.resolvedModel != before.resolvedModel || got.authMode != before.authMode ||
		got.credentialClass != before.credentialClass || got.billingPolicy != before.billingPolicy ||
		got.billingClass != before.billingClass {
		t.Fatalf("immutable columns changed:\n before %#v\n after %#v", before, got)
	}
}

func TestFinalizeValidatesBeforeWaitingForWritePermit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id := insertPendingRequest(t, store)
	before := readStoredRequest(t, store.db, id)
	if err := store.acquireWritePermit(context.Background()); err != nil {
		t.Fatalf("acquireWritePermit() returned error: %v", err)
	}
	t.Cleanup(store.releaseWritePermit)

	var nilContext context.Context
	if err := store.Finalize(nilContext, id, validFinalizedRequest()); err == nil {
		t.Fatal("Finalize(nil) returned nil error")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Finalize(canceled, id, validFinalizedRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Finalize(canceled) error = %v; want context.Canceled", err)
	}
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancelExpired()
	if err := store.Finalize(expired, id, validFinalizedRequest()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Finalize(expired) error = %v; want context.DeadlineExceeded", err)
	}
	invalid := validFinalizedRequest()
	invalid.Outcome = ""
	ctx, cancelInvalid := context.WithTimeout(context.Background(), time.Second)
	defer cancelInvalid()
	if err := store.Finalize(ctx, id, invalid); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Finalize(invalid) error = %v; want prompt validation error", err)
	}
	if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, before) {
		t.Fatalf("row changed after validation failures:\n got %#v\nwant %#v", got, before)
	}
}

func TestFinalizeCanceledWhileQueuedDoesNotMutateOrPoisonPermit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id := insertPendingRequest(t, store)
	before := readStoredRequest(t, store.db, id)
	if err := store.acquireWritePermit(context.Background()); err != nil {
		t.Fatalf("acquireWritePermit() returned error: %v", err)
	}
	permitHeld := true
	t.Cleanup(func() {
		if permitHeld {
			store.releaseWritePermit()
		}
	})

	baseCtx, cancel := context.WithCancel(context.Background())
	ctx := &observedDoneContext{Context: baseCtx, observed: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		result <- store.Finalize(ctx, id, validFinalizedRequest())
	}()
	select {
	case <-ctx.observed:
	case <-time.After(time.Second):
		t.Fatal("Finalize() did not begin waiting for the held write permit")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued Finalize() error = %v; want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Finalize() did not return after context cancellation")
	}
	if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, before) {
		t.Fatalf("row changed after queued cancellation:\n got %#v\nwant %#v", got, before)
	}

	store.releaseWritePermit()
	permitHeld = false
	if err := store.Finalize(context.Background(), id, validFinalizedRequest()); err != nil {
		t.Fatalf("follow-up Finalize() returned error: %v", err)
	}
}

func TestFinalizeTriggerAndSQLFailuresPreservePendingRowAndPermit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id := insertPendingRequest(t, store)
	before := readStoredRequest(t, store.db, id)
	if _, err := store.db.Exec(fmt.Sprintf(`CREATE TRIGGER reject_test_finalization
		BEFORE UPDATE OF lifecycle_state ON accounting_requests
		WHEN NEW.id = %d AND NEW.lifecycle_state = 'finalized'
		BEGIN SELECT RAISE(ABORT, 'test finalization rejection'); END`, id)); err != nil {
		t.Fatalf("create rejecting trigger: %v", err)
	}
	if err := store.Finalize(context.Background(), id, validFinalizedRequest()); err == nil {
		t.Fatal("trigger-rejected Finalize() returned nil error")
	}
	if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, before) {
		t.Fatalf("row changed after trigger failure:\n got %#v\nwant %#v", got, before)
	}
	assertWritePermitAvailable(t, store)
	if _, err := store.db.Exec(`DROP TRIGGER reject_test_finalization`); err != nil {
		t.Fatalf("drop rejecting trigger: %v", err)
	}
	if err := store.Finalize(context.Background(), id, validFinalizedRequest()); err != nil {
		t.Fatalf("Finalize() after trigger removal returned error: %v", err)
	}

	closedPath := filepath.Join(t.TempDir(), "closed.sqlite")
	closedStore, err := Open(closedPath)
	if err != nil {
		t.Fatalf("Open(%q) returned error: %v", closedPath, err)
	}
	closedID := insertPendingRequest(t, closedStore)
	closedBefore := readStoredRequest(t, closedStore.db, closedID)
	if err := closedStore.Close(); err != nil {
		t.Fatalf("Close() returned error: %v", err)
	}
	for range 2 {
		if err := closedStore.Finalize(context.Background(), closedID, validFinalizedRequest()); err == nil {
			t.Fatal("Finalize() on closed store returned nil error")
		}
	}
	inspection := inspectDB(t, closedPath)
	if got := readStoredRequest(t, inspection, closedID); !reflect.DeepEqual(got, closedBefore) {
		t.Fatalf("row changed after closed-store SQL failures:\n got %#v\nwant %#v", got, closedBefore)
	}
}

func TestFinalizeConcurrentSameSnapshotAcrossStoresAllSucceed(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	first := openStore(t, path)
	second := openStore(t, path)
	stores := []*Store{first, second}
	id := insertPendingRequest(t, first)
	finalized := validFinalizedRequest()
	const writers = 32
	errs := make(chan error, writers)
	var wait sync.WaitGroup
	for writer := range writers {
		wait.Add(1)
		go func(writer int) {
			defer wait.Done()
			errs <- stores[writer%len(stores)].Finalize(context.Background(), id, finalized)
		}(writer)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent exact Finalize() returned error: %v", err)
		}
	}
	got := readStoredRequest(t, first.db, id)
	if got.lifecycleState != "finalized" || got.responseID.String != finalized.ResponseID {
		t.Fatalf("stored concurrent snapshot = lifecycle %q response %#v", got.lifecycleState, got.responseID)
	}
	if other := readStoredRequest(t, second.db, id); !reflect.DeepEqual(other, got) {
		t.Fatalf("second store snapshot = %#v; want %#v", other, got)
	}
}

func TestFinalizeConcurrentDifferentSnapshotsAcrossStoresHaveOneWinner(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	first := openStore(t, path)
	second := openStore(t, path)
	stores := []*Store{first, second}
	id := insertPendingRequest(t, first)
	const writers = 32
	type finalizeResult struct {
		candidate FinalizedRequest
		err       error
	}
	results := make(chan finalizeResult, writers)
	var wait sync.WaitGroup
	for writer := range writers {
		wait.Add(1)
		go func(writer int) {
			defer wait.Done()
			candidate := validFinalizedRequest()
			candidate.ResponseID = fmt.Sprintf("msg_concurrent_%02d", writer)
			candidate.CompletedAt = candidate.CompletedAt.Add(time.Duration(writer))
			results <- finalizeResult{
				candidate: candidate,
				err:       stores[writer%len(stores)].Finalize(context.Background(), id, candidate),
			}
		}(writer)
	}
	wait.Wait()
	close(results)

	var winner FinalizedRequest
	winners := 0
	losers := 0
	for result := range results {
		if result.err == nil {
			winner = result.candidate
			winners++
		} else {
			losers++
		}
	}
	if winners != 1 || losers != writers-1 {
		t.Fatalf("concurrent finalizations = %d winners, %d losers; want 1 and %d",
			winners, losers, writers-1)
	}
	got := readStoredRequest(t, first.db, id)
	if got.responseID.String != winner.ResponseID || got.completedAt.Int64 != winner.CompletedAt.UnixNano() {
		t.Fatalf("stored snapshot = response %q completion %d; want winner %q/%d",
			got.responseID.String, got.completedAt.Int64, winner.ResponseID, winner.CompletedAt.UnixNano())
	}
	assertWritePermitAvailable(t, first)
	assertWritePermitAvailable(t, second)
}

func TestMarkUpstreamDispatchedPersistsImmutableUpstreamTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		target     string
		wantTarget sql.NullString
	}{
		{name: "non-Bedrock target is NULL", target: "", wantTarget: sql.NullString{}},
		{
			name:   "Bedrock mapped target",
			target: "arn:aws:bedrock:us-east-2:123:application-inference-profile/first",
			wantTarget: sql.NullString{
				String: "arn:aws:bedrock:us-east-2:123:application-inference-profile/first",
				Valid:  true,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
			id := insertPendingRequest(t, store)
			dispatchedAt := time.Unix(1_776_912_346, 111_222_333).UTC()
			if err := store.MarkUpstreamDispatched(context.Background(), id, dispatchedAt, test.target); err != nil {
				t.Fatalf("MarkUpstreamDispatched() returned error: %v", err)
			}
			var got sql.NullString
			if err := store.db.QueryRow(
				`SELECT upstream_target FROM accounting_requests WHERE id = ?`,
				id,
			).Scan(&got); err != nil {
				t.Fatalf("read upstream target: %v", err)
			}
			if got != test.wantTarget {
				t.Fatalf("upstream_target = %#v; want %#v", got, test.wantTarget)
			}
			if err := store.MarkUpstreamDispatched(context.Background(), id, dispatchedAt, test.target); err != nil {
				t.Fatalf("same-pair MarkUpstreamDispatched() returned error: %v", err)
			}
			if err := store.MarkUpstreamDispatched(
				context.Background(),
				id,
				dispatchedAt,
				"arn:aws:bedrock:us-east-2:123:application-inference-profile/replacement",
			); err == nil {
				t.Fatal("different target MarkUpstreamDispatched() returned nil error")
			}
			if err := store.db.QueryRow(
				`SELECT upstream_target FROM accounting_requests WHERE id = ?`,
				id,
			).Scan(&got); err != nil {
				t.Fatalf("read target after rejected replacement: %v", err)
			}
			if got != test.wantTarget {
				t.Fatalf("target after rejected replacement = %#v; want %#v", got, test.wantTarget)
			}
			if err := store.MarkUpstreamDispatched(
				context.Background(),
				id,
				dispatchedAt.Add(time.Nanosecond),
				test.target,
			); err == nil {
				t.Fatal("different timestamp MarkUpstreamDispatched() returned nil error")
			}
			if err := store.db.QueryRow(
				`SELECT upstream_target FROM accounting_requests WHERE id = ?`,
				id,
			).Scan(&got); err != nil {
				t.Fatalf("read target after rejected timestamp: %v", err)
			}
			if got != test.wantTarget {
				t.Fatalf("target after rejected timestamp = %#v; want %#v", got, test.wantTarget)
			}
		})
	}

	store := openStore(t, filepath.Join(t.TempDir(), "whitespace.sqlite"))
	id := insertPendingRequest(t, store)
	before := readStoredRequest(t, store.db, id)
	if err := store.MarkUpstreamDispatched(
		context.Background(),
		id,
		time.Unix(1_776_912_346, 111_222_333).UTC(),
		" \t",
	); err == nil || !strings.Contains(err.Error(), "target is blank") {
		t.Fatalf("whitespace-target MarkUpstreamDispatched() error = %v; want blank target rejection", err)
	}
	if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, before) {
		t.Fatalf("row changed after whitespace target rejection:\n got %#v\nwant %#v", got, before)
	}
}

func TestMarkUpstreamDispatchedTransitionsPendingRequestDurablyAndIdempotently(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id, err := store.InsertPending(context.Background(), validPendingRequest())
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	before := readStoredRequest(t, store.db, id)
	dispatchedAt := time.Unix(1_776_912_346, 987_654_321).UTC()

	if err := store.MarkUpstreamDispatched(context.Background(), id, dispatchedAt, ""); err != nil {
		t.Fatalf("MarkUpstreamDispatched() returned error: %v", err)
	}
	want := before
	want.upstreamContacted = 1
	want.upstreamDispatchedAt = sql.NullInt64{Int64: dispatchedAt.UnixNano(), Valid: true}
	if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored request after first dispatch mismatch:\n got %#v\nwant %#v", got, want)
	}

	if err := store.MarkUpstreamDispatched(context.Background(), id, dispatchedAt, ""); err != nil {
		t.Fatalf("idempotent MarkUpstreamDispatched() returned error: %v", err)
	}
	if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored request after idempotent dispatch mismatch:\n got %#v\nwant %#v", got, want)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close() returned error: %v", err)
	}
	reopened := openStore(t, path)
	if got := readStoredRequest(t, reopened.db, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened stored request mismatch:\n got %#v\nwant %#v", got, want)
	}
	if err := reopened.MarkUpstreamDispatched(context.Background(), id, dispatchedAt, ""); err != nil {
		t.Fatalf("reopened idempotent MarkUpstreamDispatched() returned error: %v", err)
	}
	if got := readStoredRequest(t, reopened.db, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened stored request after idempotent dispatch mismatch:\n got %#v\nwant %#v", got, want)
	}
}

func TestMarkUpstreamDispatchedRejectsDifferentTimestampWithoutMutation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id, err := store.InsertPending(context.Background(), validPendingRequest())
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	first := time.Unix(1_776_912_346, 111_222_333).UTC()
	if err := store.MarkUpstreamDispatched(context.Background(), id, first, ""); err != nil {
		t.Fatalf("first MarkUpstreamDispatched() returned error: %v", err)
	}
	want := readStoredRequest(t, store.db, id)

	second := first.Add(time.Nanosecond)
	err = store.MarkUpstreamDispatched(context.Background(), id, second, "")
	if err == nil || !strings.Contains(err.Error(), "already dispatched at") ||
		!strings.Contains(err.Error(), "cannot replace with") {
		t.Fatalf("different-time MarkUpstreamDispatched() error = %v; want immutable timestamp error", err)
	}
	if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored request after rejected timestamp mismatch:\n got %#v\nwant %#v", got, want)
	}
	assertWritePermitAvailable(t, store)
}

func TestMarkUpstreamDispatchedRejectsUnknownIDWithoutMutation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id, err := store.InsertPending(context.Background(), validPendingRequest())
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	want := readStoredRequest(t, store.db, id)

	err = store.MarkUpstreamDispatched(
		context.Background(),
		id+1_000,
		time.Unix(1_776_912_346, 111_222_333).UTC(),
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unknown-ID MarkUpstreamDispatched() error = %v; want missing request error", err)
	}
	if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("existing request after unknown ID mismatch:\n got %#v\nwant %#v", got, want)
	}
	assertWritePermitAvailable(t, store)
}

func TestMarkUpstreamDispatchedRejectsInvalidInputsBeforeWritePermit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id, err := store.InsertPending(context.Background(), validPendingRequest())
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	want := readStoredRequest(t, store.db, id)
	validTime := time.Unix(1_776_912_346, 111_222_333).UTC()
	if err := store.acquireWritePermit(context.Background()); err != nil {
		t.Fatalf("acquireWritePermit() returned error: %v", err)
	}
	t.Cleanup(store.releaseWritePermit)

	var nilContext context.Context
	if err := store.MarkUpstreamDispatched(nilContext, id, validTime, ""); err == nil ||
		!strings.Contains(err.Error(), "context is nil") {
		t.Fatalf("nil-context MarkUpstreamDispatched() error = %v; want nil context error", err)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	err = store.MarkUpstreamDispatched(canceledCtx, 0, time.Time{}, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled invalid MarkUpstreamDispatched() error = %v; want context.Canceled", err)
	}
	expiredCtx, cancelExpired := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancelExpired()
	err = store.MarkUpstreamDispatched(expiredCtx, 0, time.Time{}, "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired invalid MarkUpstreamDispatched() error = %v; want context.DeadlineExceeded", err)
	}

	tests := []struct {
		name         string
		id           int64
		dispatchedAt time.Time
		wantContains string
	}{
		{name: "zero ID", id: 0, dispatchedAt: validTime, wantContains: "ID must be positive"},
		{name: "negative ID", id: -1, dispatchedAt: validTime, wantContains: "ID must be positive"},
		{name: "zero time", id: id, dispatchedAt: time.Time{}, wantContains: "time is zero"},
		{
			name:         "time above UnixNano range",
			id:           id,
			dispatchedAt: time.Date(2500, time.January, 1, 0, 0, 0, 0, time.UTC),
			wantContains: "UnixNano",
		},
		{
			name:         "time below UnixNano range",
			id:           id,
			dispatchedAt: time.Date(1600, time.January, 1, 0, 0, 0, 0, time.UTC),
			wantContains: "UnixNano",
		},
	}
	for _, test := range tests {
		ctx, cancelCall := context.WithTimeout(context.Background(), time.Second)
		err := store.MarkUpstreamDispatched(ctx, test.id, test.dispatchedAt, "")
		cancelCall()
		if err == nil || !strings.Contains(err.Error(), test.wantContains) {
			t.Errorf("%s: MarkUpstreamDispatched() error = %v; want substring %q",
				test.name, err, test.wantContains)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: validation waited for write permit: %v", test.name, err)
		}
	}

	if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored request after invalid calls mismatch:\n got %#v\nwant %#v", got, want)
	}
}

func TestMarkUpstreamDispatchedRejectsIneligibleStoredStatesWithoutMutation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		lifecycleState    string
		upstreamContacted int
		dispatchedAt      any
		wantContains      string
	}{
		{
			name:              "non-pending lifecycle",
			lifecycleState:    "completed",
			upstreamContacted: 0,
			wantContains:      `lifecycle state "completed" is not pending`,
		},
		{
			name:              "uncontacted with timestamp",
			lifecycleState:    "pending",
			upstreamContacted: 0,
			dispatchedAt:      int64(1_776_912_346_111_222_333),
			wantContains:      "inconsistent upstream dispatch evidence",
		},
		{
			name:              "contacted without timestamp",
			lifecycleState:    "pending",
			upstreamContacted: 1,
			wantContains:      "inconsistent upstream dispatch evidence",
		},
		{
			name:              "invalid contacted marker without timestamp",
			lifecycleState:    "pending",
			upstreamContacted: 2,
			wantContains:      "inconsistent upstream dispatch evidence",
		},
		{
			name:              "invalid contacted marker with timestamp",
			lifecycleState:    "pending",
			upstreamContacted: 2,
			dispatchedAt:      int64(1_776_912_346_111_222_333),
			wantContains:      "inconsistent upstream dispatch evidence",
		},
	}

	for _, test := range tests {
		name := fmt.Sprintf(
			"%s/contacted=%d/time=%v",
			test.name,
			test.upstreamContacted,
			test.dispatchedAt,
		)
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			store := openStore(t, path)
			id, err := store.InsertPending(context.Background(), validPendingRequest())
			if err != nil {
				t.Fatalf("InsertPending() returned error: %v", err)
			}
			setStoredDispatchState(
				t,
				store.db,
				id,
				test.lifecycleState,
				test.upstreamContacted,
				test.dispatchedAt,
			)
			want := readStoredRequest(t, store.db, id)

			err = store.MarkUpstreamDispatched(
				context.Background(),
				id,
				time.Unix(1_776_912_347, 444_555_666).UTC(),
				"",
			)
			if err == nil || !strings.Contains(err.Error(), test.wantContains) {
				t.Fatalf("MarkUpstreamDispatched() error = %v; want substring %q", err, test.wantContains)
			}
			if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, want) {
				t.Fatalf("stored request after rejected state mismatch:\n got %#v\nwant %#v", got, want)
			}
			assertWritePermitAvailable(t, store)
		})
	}
}

func TestMarkUpstreamDispatchedCanceledWhileQueuedDoesNotMutateOrPoisonPermit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id, err := store.InsertPending(context.Background(), validPendingRequest())
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	want := readStoredRequest(t, store.db, id)
	if err := store.acquireWritePermit(context.Background()); err != nil {
		t.Fatalf("acquireWritePermit() returned error: %v", err)
	}
	permitHeld := true
	t.Cleanup(func() {
		if permitHeld {
			store.releaseWritePermit()
		}
	})

	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &observedDoneContext{
		Context:  baseCtx,
		observed: make(chan struct{}),
	}
	result := make(chan error, 1)
	dispatchedAt := time.Unix(1_776_912_346, 111_222_333).UTC()
	go func() {
		result <- store.MarkUpstreamDispatched(ctx, id, dispatchedAt, "")
	}()
	select {
	case <-ctx.observed:
	case <-time.After(time.Second):
		t.Fatal("MarkUpstreamDispatched() did not begin waiting for the held write permit")
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued MarkUpstreamDispatched() error = %v; want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("MarkUpstreamDispatched() did not return after queued context cancellation")
	}
	if got := readStoredRequest(t, store.db, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored request after queued cancellation mismatch:\n got %#v\nwant %#v", got, want)
	}

	store.releaseWritePermit()
	permitHeld = false
	followupCtx, cancelFollowup := context.WithTimeout(context.Background(), time.Second)
	defer cancelFollowup()
	if err := store.MarkUpstreamDispatched(followupCtx, id, dispatchedAt, ""); err != nil {
		t.Fatalf("follow-up MarkUpstreamDispatched() returned error: %v", err)
	}
}

func TestMarkUpstreamDispatchedOnClosedStoreReleasesPermit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id, err := store.InsertPending(context.Background(), validPendingRequest())
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	want := readStoredRequest(t, store.db, id)
	if err := store.Close(); err != nil {
		t.Fatalf("Close() returned error: %v", err)
	}
	dispatchedAt := time.Unix(1_776_912_346, 111_222_333).UTC()

	if err := store.MarkUpstreamDispatched(context.Background(), id, dispatchedAt, ""); err == nil ||
		!strings.Contains(err.Error(), "update dispatch evidence") {
		t.Fatalf("closed-store MarkUpstreamDispatched() error = %v; want wrapped update failure", err)
	}
	followupCtx, cancelFollowup := context.WithTimeout(context.Background(), time.Second)
	defer cancelFollowup()
	if err := store.MarkUpstreamDispatched(followupCtx, id, dispatchedAt, ""); err == nil ||
		!strings.Contains(err.Error(), "update dispatch evidence") ||
		errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("closed-store follow-up error = %v; want prompt wrapped update failure", err)
	}

	db := inspectDB(t, path)
	if got := readStoredRequest(t, db, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored request after closed-store calls mismatch:\n got %#v\nwant %#v", got, want)
	}
}

func TestMarkUpstreamDispatchedConcurrentSameTimestampAllSucceed(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id, err := store.InsertPending(context.Background(), validPendingRequest())
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	dispatchedAt := time.Unix(1_776_912_346, 111_222_333).UTC()
	const writers = 64

	errs := make(chan error, writers)
	var wait sync.WaitGroup
	for range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs <- store.MarkUpstreamDispatched(context.Background(), id, dispatchedAt, "")
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent same-time MarkUpstreamDispatched() returned error: %v", err)
		}
	}

	got := readStoredRequest(t, store.db, id)
	if got.upstreamContacted != 1 ||
		got.upstreamDispatchedAt != (sql.NullInt64{Int64: dispatchedAt.UnixNano(), Valid: true}) {
		t.Fatalf("stored dispatch evidence = contacted %d, time %#v; want 1 and %d",
			got.upstreamContacted, got.upstreamDispatchedAt, dispatchedAt.UnixNano())
	}
}

func TestMarkUpstreamDispatchedConcurrentDistinctTimestampsHaveOneWinner(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id, err := store.InsertPending(context.Background(), validPendingRequest())
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	const writers = 64
	base := time.Unix(1_776_912_346, 111_222_000).UTC()
	type dispatchResult struct {
		dispatchedAt time.Time
		err          error
	}
	results := make(chan dispatchResult, writers)
	var wait sync.WaitGroup
	for writer := range writers {
		wait.Add(1)
		go func(writer int) {
			defer wait.Done()
			dispatchedAt := base.Add(time.Duration(writer))
			results <- dispatchResult{
				dispatchedAt: dispatchedAt,
				err:          store.MarkUpstreamDispatched(context.Background(), id, dispatchedAt, ""),
			}
		}(writer)
	}
	wait.Wait()
	close(results)

	var winner time.Time
	winnerCount := 0
	loserCount := 0
	for result := range results {
		if result.err == nil {
			winner = result.dispatchedAt
			winnerCount++
			continue
		}
		loserCount++
		if !strings.Contains(result.err.Error(), "already dispatched at") ||
			!strings.Contains(result.err.Error(), "cannot replace with") {
			t.Errorf("concurrent distinct-time error = %v; want immutable timestamp error", result.err)
		}
	}
	if winnerCount != 1 || loserCount != writers-1 {
		t.Fatalf("concurrent results = %d winners, %d losers; want 1 and %d",
			winnerCount, loserCount, writers-1)
	}

	got := readStoredRequest(t, store.db, id)
	if got.upstreamContacted != 1 ||
		got.upstreamDispatchedAt != (sql.NullInt64{Int64: winner.UnixNano(), Valid: true}) {
		t.Fatalf("stored dispatch evidence = contacted %d, time %#v; want winning time %d",
			got.upstreamContacted, got.upstreamDispatchedAt, winner.UnixNano())
	}
}

func assertWritePermitAvailable(t *testing.T, store *Store) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := store.acquireWritePermit(ctx); err != nil {
		t.Fatalf("acquireWritePermit() after rejected dispatch returned error: %v", err)
	}
	store.releaseWritePermit()
}

func setStoredDispatchState(
	t *testing.T,
	db *sql.DB,
	id int64,
	lifecycleState string,
	upstreamContacted int,
	dispatchedAt any,
) {
	t.Helper()

	result, err := db.Exec(`UPDATE accounting_requests
		SET lifecycle_state = ?, upstream_contacted = ?, upstream_dispatched_at = ?
		WHERE id = ?`, lifecycleState, upstreamContacted, dispatchedAt, id)
	if err != nil {
		t.Fatalf("set stored dispatch state: %v", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		t.Fatalf("read affected rows after setting stored dispatch state: %v", err)
	}
	if rows != 1 {
		t.Fatalf("set stored dispatch state affected %d rows; want 1", rows)
	}
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func insertPendingRequest(t *testing.T, store *Store) int64 {
	t.Helper()
	id, err := store.InsertPending(context.Background(), validPendingRequest())
	if err != nil {
		t.Fatalf("InsertPending() returned error: %v", err)
	}
	return id
}

func validFinalizedRequest() FinalizedRequest {
	return FinalizedRequest{
		CompletedAt:                       validPendingRequest().StartedAt.Add(time.Second),
		Outcome:                           OutcomeSuccess,
		HTTPStatus:                        200,
		ResponseID:                        "msg_01durable",
		NormalizationComplete:             true,
		InputTokens:                       101,
		InputTokensPresent:                true,
		OutputTokens:                      202,
		OutputTokensPresent:               true,
		CacheCreationInputTokens:          303,
		CacheCreationInputTokensPresent:   true,
		CacheReadInputTokens:              404,
		CacheReadInputTokensPresent:       true,
		CacheCreation5mInputTokens:        505,
		CacheCreation5mInputTokensPresent: true,
		CacheCreation1hInputTokens:        606,
		CacheCreation1hInputTokensPresent: true,
		ProviderCostPresent:               true,
		ProviderCostValid:                 true,
		ProviderCost:                      accounting.NanoUSD(7_890_123_456),
		StopReason:                        "end_turn",
		BlockShape:                        []string{"thinking", "text"},
		DurationMS:                        1_000,
		UpstreamMS:                        800,
		CostStatus:                        CostStatusKnown,
	}
}

func validPendingRequest() PendingRequest {
	return PendingRequest{
		StartedAt:       time.Unix(1_776_912_345, 678_901_234).UTC(),
		Kind:            RequestMessages,
		Host:            "gnomon",
		Context:         "personal",
		Route:           registry.DefaultRoute(),
		RequestModel:    "claude-opus-5",
		CredentialClass: billing.CredentialOAuth,
	}
}

func countStoredRequests(t *testing.T, db *sql.DB) int {
	t.Helper()

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM accounting_requests`).Scan(&count); err != nil {
		t.Fatalf("count accounting requests: %v", err)
	}
	return count
}

func readStoredRequest(t *testing.T, db *sql.DB, id int64) storedRequest {
	t.Helper()

	var request storedRequest
	if err := db.QueryRow(`SELECT
		id,
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
		cost_status,
		response_id,
		http_status,
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
		cost_basis,
		final_cost_nano_usd,
		rate_card_id,
		upstream_target
	FROM accounting_requests
	WHERE id = ?`, id).Scan(
		&request.id,
		&request.startedAt,
		&request.completedAt,
		&request.upstreamDispatchedAt,
		&request.requestKind,
		&request.lifecycleState,
		&request.outcome,
		&request.upstreamContacted,
		&request.routeHost,
		&request.routeContext,
		&request.routeAlias,
		&request.routeUpstream,
		&request.requestModel,
		&request.resolvedModel,
		&request.authMode,
		&request.credentialClass,
		&request.billingPolicy,
		&request.billingClass,
		&request.costStatus,
		&request.responseID,
		&request.httpStatus,
		&request.normalizationComplete,
		&request.inputTokens,
		&request.outputTokens,
		&request.cacheCreationInputTokens,
		&request.cacheReadInputTokens,
		&request.cacheCreation5mInputTokens,
		&request.cacheCreation1hInputTokens,
		&request.providerCostPresent,
		&request.providerCostValid,
		&request.providerCostNanoUSD,
		&request.stopReason,
		&request.blockShape,
		&request.durationMS,
		&request.upstreamMS,
		&request.costBasis,
		&request.finalCostNanoUSD,
		&request.rateCardID,
		&request.upstreamTarget,
	); err != nil {
		t.Fatalf("read accounting request %d: %v", id, err)
	}
	return request
}
