package usage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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
	id                   int64
	startedAt            int64
	completedAt          sql.NullInt64
	upstreamDispatchedAt sql.NullInt64
	requestKind          string
	lifecycleState       string
	outcome              sql.NullString
	upstreamContacted    int
	routeHost            string
	routeContext         string
	routeAlias           sql.NullString
	routeUpstream        string
	requestModel         string
	resolvedModel        string
	authMode             string
	credentialClass      string
	billingPolicy        string
	billingClass         sql.NullString
	costStatus           string
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
		cost_status
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
	); err != nil {
		t.Fatalf("read accounting request %d: %v", id, err)
	}
	return request
}
