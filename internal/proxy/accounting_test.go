package proxy_test

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/usage"
)

//go:embed testdata/openrouter-deepseek-null-usage.sse
var nullUsageAccountingFixture []byte

// stubAccountant records lifecycle calls and can fail either synchronous gate.
type stubAccountant struct {
	insertErr   error
	markErr     error
	finalizeErr error

	mu        sync.Mutex
	pending   []usage.PendingRequest
	marked    []int64
	finalized []usage.FinalizedRequest
	done      chan struct{}
}

func newAccountant() *stubAccountant {
	return &stubAccountant{done: make(chan struct{}, 16)}
}

func (s *stubAccountant) InsertPending(_ context.Context, request usage.PendingRequest) (int64, error) {
	if s.insertErr != nil {
		return 0, s.insertErr
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, request)

	return int64(len(s.pending)), nil
}

func (s *stubAccountant) MarkUpstreamDispatched(_ context.Context, id int64, _ time.Time, _ string) error {
	if s.markErr != nil {
		return s.markErr
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.marked = append(s.marked, id)

	return nil
}

func (s *stubAccountant) Finalize(_ context.Context, _ int64, request usage.FinalizedRequest) error {
	if s.finalizeErr != nil {
		return s.finalizeErr
	}

	s.mu.Lock()
	s.finalized = append(s.finalized, request)
	s.mu.Unlock()
	s.done <- struct{}{}

	return nil
}

func (s *stubAccountant) wait(t *testing.T) usage.FinalizedRequest {
	t.Helper()

	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.finalized[len(s.finalized)-1]
	case <-time.After(10 * time.Second):
		t.Fatal("accounting request was not finalized")
		return usage.FinalizedRequest{}
	}
}

func (s *stubAccountant) snapshot() (pending []usage.PendingRequest, marked []int64, finalized []usage.FinalizedRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]usage.PendingRequest(nil), s.pending...),
		append([]int64(nil), s.marked...),
		append([]usage.FinalizedRequest(nil), s.finalized...)
}

func TestAccountingLifecycleOutcomes(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	partial := partialSSETurn()
	tests := []struct {
		name    string
		handler http.HandlerFunc
		request *http.Request
		outcome usage.Outcome
	}{
		{
			name:    "success",
			handler: jsonHandler(http.StatusOK, jsonTurn),
			request: messagesRequest(fidelityBody),
			outcome: usage.OutcomeSuccess,
		},
		{
			name:    "upstream HTTP failure",
			handler: jsonHandler(http.StatusTooManyRequests, `{"type":"error"}`),
			request: messagesRequest(fidelityBody),
			outcome: usage.OutcomeUpstreamHTTPFailure,
		},
		{
			name: "response interrupted",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", sseType)
				w.Header().Set("Content-Length", "99999")
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, partial)
			},
			request: messagesRequest(fidelityBody),
			outcome: usage.OutcomeResponseInterrupted,
		},
		{
			name: "normalization failure",
			handler: jsonHandler(
				http.StatusOK,
				`{"type":"message","id":"msg_incomplete","usage":{"input_tokens":"invalid"}}`,
			),
			request: messagesRequest(fidelityBody),
			outcome: usage.OutcomeNormalizationFailure,
		},
		{
			name:    "local rejection",
			handler: jsonHandler(http.StatusOK, jsonTurn),
			request: func() *http.Request {
				req := messagesRequest(fidelityBody)
				req.Method = http.MethodGet
				req.URL.Path = "/v1/other"
				return req
			}(),
			outcome: usage.OutcomeLocalRejection,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := newUpstream(t, tc.handler)
			accountant := newAccountant()

			answer := serve(t, proxy.Config{
				Registry:   newRegistry(t, identitySeat(stub.url())),
				Host:       testHost,
				Accountant: accountant,
			}, tc.request)

			got := accountant.wait(t)
			if got.Outcome != tc.outcome {
				t.Fatalf("outcome = %q, want %q", got.Outcome, tc.outcome)
			}

			pending, marked, _ := accountant.snapshot()
			if len(pending) != 1 {
				t.Fatalf("pending rows = %d, want 1", len(pending))
			}
			if tc.outcome == usage.OutcomeLocalRejection {
				if len(marked) != 0 {
					t.Fatalf("dispatch marks = %d, want none", len(marked))
				}
			} else if len(marked) != 1 {
				t.Fatalf("dispatch marks = %d, want 1", len(marked))
			}
			if answer.Code == 0 {
				t.Fatal("gateway did not write a response")
			}
		})
	}
}

func TestAccountingFailuresBlockUpstream(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	for _, tc := range []struct {
		name       string
		accountant *stubAccountant
		wantStatus int
	}{
		{name: "insert", accountant: &stubAccountant{insertErr: errors.New("disk unavailable"), done: make(chan struct{}, 1)}, wantStatus: http.StatusInternalServerError},
		{name: "dispatch", accountant: &stubAccountant{markErr: errors.New("disk unavailable"), done: make(chan struct{}, 1)}, wantStatus: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
			answer := serve(t, proxy.Config{
				Registry:   newRegistry(t, identitySeat(stub.url())),
				Host:       testHost,
				Accountant: tc.accountant,
			}, messagesRequest(fidelityBody))

			if answer.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", answer.Code, tc.wantStatus)
			}
			if stub.hits() != 0 {
				t.Fatalf("upstream received %d requests, want none", stub.hits())
			}
		})
	}
}

func TestAccountingFinalizationFailureIsLogged(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
	sink := newLogSink()
	answer := serve(t, proxy.Config{
		Registry:   newRegistry(t, identitySeat(stub.url())),
		Host:       testHost,
		Logger:     slog.New(sink),
		Accountant: &stubAccountant{finalizeErr: errors.New("disk unavailable"), done: make(chan struct{}, 1)},
	}, messagesRequest(fidelityBody))
	if answer.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", answer.Code, http.StatusOK)
	}
	sink.waitFor(t, "usage accounting finalization failed")
}

func TestAccountingFinalizesBeforeABlockingLedgerAppend(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
	recorder := newBlockingRecorder()
	accountant := newAccountant()
	t.Cleanup(func() { close(recorder.release) })

	answer := serve(t, proxy.Config{
		Registry:   newRegistry(t, identitySeat(stub.url())),
		Recorder:   recorder,
		Host:       testHost,
		Accountant: accountant,
	}, messagesRequest(fidelityBody))
	if answer.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", answer.Code, http.StatusOK)
	}

	select {
	case <-recorder.started:
	case <-time.After(time.Second):
		t.Fatal("ledger append did not start")
	}

	select {
	case <-accountant.done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("blocking ledger append prevented accounting finalization")
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("upstream unavailable")
}

func TestAccountingTransportFailure(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	accountant := newAccountant()
	stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
	answer := serve(t, proxy.Config{
		Registry:   newRegistry(t, identitySeat(stub.url())),
		Client:     &http.Client{Transport: failingTransport{}},
		Host:       testHost,
		Accountant: accountant,
	}, messagesRequest(fidelityBody))
	if answer.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", answer.Code, http.StatusBadGateway)
	}
	if got := accountant.wait(t); got.Outcome != usage.OutcomeTransportFailure {
		t.Fatalf("outcome = %q, want %q", got.Outcome, usage.OutcomeTransportFailure)
	}
}

func TestProxyRequiresAnAccountant(t *testing.T) {
	if _, err := proxy.New(proxy.Config{}); err == nil {
		t.Fatal("proxy.New() error = nil without an accountant")
	}
}

func TestAccountingRequestKinds(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	tests := []struct {
		name string
		req  func() *http.Request
		want usage.RequestKind
	}{
		{
			name: "messages",
			req:  func() *http.Request { return messagesRequest(fidelityBody) },
			want: usage.RequestMessages,
		},
		{
			name: "count tokens",
			req: func() *http.Request {
				req := messagesRequest(fidelityBody)
				req.URL.Path = "/v1/messages/count_tokens"

				return req
			},
			want: usage.RequestCountTokens,
		},
		{
			name: "other",
			req: func() *http.Request {
				req := messagesRequest(fidelityBody)
				req.URL.Path = "/v1/other"

				return req
			},
			want: usage.RequestOther,
		},
		{
			name: "messages non-post",
			req: func() *http.Request {
				req := messagesRequest(fidelityBody)
				req.Method = http.MethodGet

				return req
			},
			want: usage.RequestOther,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
			accountant := newAccountant()
			serve(t, proxy.Config{
				Registry:   newRegistry(t, identitySeat(stub.url())),
				Host:       testHost,
				Accountant: accountant,
			}, tc.req())
			accountant.wait(t)
			pending, _, _ := accountant.snapshot()
			if pending[0].Kind != tc.want {
				t.Fatalf("request kind = %q, want %q", pending[0].Kind, tc.want)
			}
		})
	}
}

func TestAccountingPreservesInvalidProviderCost(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	accountant := newAccountant()
	stub := newUpstream(t, jsonHandler(http.StatusOK,
		`{"type":"message","id":"msg_invalid_cost","content":[],"usage":{"cost":"not-a-cost"}}`))
	serve(t, proxy.Config{
		Registry:   newRegistry(t, identitySeat(stub.url())),
		Host:       testHost,
		Accountant: accountant,
	}, messagesRequest(fidelityBody))

	got := accountant.wait(t)
	if !got.ProviderCostPresent || got.ProviderCostValid || got.ProviderCost != 0 {
		t.Fatalf("provider cost = present=%t valid=%t amount=%d; want present invalid zero",
			got.ProviderCostPresent, got.ProviderCostValid, got.ProviderCost)
	}
}

func TestAccountingSkipsPreRouteRejections(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	tests := []struct {
		name string
		cfg  func(t *testing.T) proxy.Config
		req  func() *http.Request
	}{
		{
			name: "body read failure",
			cfg: func(t *testing.T) proxy.Config {
				t.Helper()

				return proxy.Config{Registry: newRegistry(t, identitySeat("http://127.0.0.1:1")), Host: testHost}
			},
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/messages", iotest.ErrReader(errors.New("body failed")))
			},
		},
		{
			name: "route rejection",
			cfg: func(t *testing.T) proxy.Config {
				t.Helper()

				return proxy.Config{Registry: newRegistry(t, identitySeat("http://127.0.0.1:1")), Host: testHost}
			},
			req: func() *http.Request { return inContext(messagesRequest(fidelityBody), "vanished") },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			accountant := newAccountant()
			cfg := tc.cfg(t)
			cfg.Accountant = accountant
			serve(t, cfg, tc.req())
			pending, marked, finalized := accountant.snapshot()
			if len(pending) != 0 || len(marked) != 0 || len(finalized) != 0 {
				t.Fatalf("accounting lifecycle = %d pending, %d dispatched, %d finalized; want none",
					len(pending), len(marked), len(finalized))
			}
		})
	}
}

func TestAccountingFinalizesCredentialRejections(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T) (*upstream, proxy.Config, *http.Request)
		wantStatus int
	}{
		{
			name: "forward credential",
			setup: func(t *testing.T) (*upstream, proxy.Config, *http.Request) {
				t.Helper()

				stub := newTLSUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
				return stub, proxy.Config{
					Registry: newRegistry(t, ""),
					Client:   anthropicClient(t, stub),
					Host:     testHost,
				}, messagesRequest(`{"model":"claude-sonnet-5"}`)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "inject caller key",
			setup: func(t *testing.T) (*upstream, proxy.Config, *http.Request) {
				t.Helper()
				writeKeyFile(t, testAPIKey)

				stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
				req := messagesRequest(fidelityBody)
				req.Header.Del(callerKeyHeader)

				return stub, proxy.Config{
					Registry: newRegistry(t, identitySeat(stub.url())),
					Host:     testHost,
				}, req
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "sigv4 caller key",
			setup: func(t *testing.T) (*upstream, proxy.Config, *http.Request) {
				t.Helper()

				stub, client, _ := bedrockSetup(t, jsonHandler(http.StatusOK, jsonTurn))
				req := messagesRequest(`{"model":"bedrock/sonnet","max_tokens":4}`)
				req.Header.Del(callerKeyHeader)

				return stub, bedrockConfig(t, client, nil), req
			},
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub, cfg, req := tc.setup(t)
			accountant := newAccountant()
			cfg.Accountant = accountant

			answer := serve(t, cfg, req)
			if answer.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", answer.Code, tc.wantStatus)
			}
			if got := stub.hits(); got != 0 {
				t.Fatalf("upstream received %d requests, want none", got)
			}

			if got := accountant.wait(t); got.Outcome != usage.OutcomeLocalRejection {
				t.Fatalf("outcome = %q, want %q", got.Outcome, usage.OutcomeLocalRejection)
			}
			pending, marked, finalized := accountant.snapshot()
			if len(pending) != 1 || len(marked) != 0 || len(finalized) != 1 {
				t.Fatalf("accounting lifecycle = %d pending, %d dispatched, %d finalized; want 1, 0, 1",
					len(pending), len(marked), len(finalized))
			}
		})
	}
}

type delayedStoreAccountant struct {
	store   *usage.Store
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *delayedStoreAccountant) InsertPending(ctx context.Context, request usage.PendingRequest) (int64, error) {
	return a.store.InsertPending(ctx, request)
}

func (a *delayedStoreAccountant) MarkUpstreamDispatched(
	ctx context.Context,
	id int64,
	sent time.Time,
	target string,
) error {
	return a.store.MarkUpstreamDispatched(ctx, id, sent, target)
}

func (a *delayedStoreAccountant) Finalize(ctx context.Context, id int64, request usage.FinalizedRequest) error {
	a.once.Do(func() { close(a.started) })
	<-a.release

	return a.store.Finalize(ctx, id, request)
}

func TestAccountingStoreLifecycle(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store, err := usage.Open(path)
	if err != nil {
		t.Fatalf("open usage store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	db, err := sql.Open("sqlite", "file:"+path+"?_foreign_keys=1")
	if err != nil {
		t.Fatalf("open usage database for inspection: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	const accountingTurn = `{"id":"msg_usage","type":"message","model":"vendor/model",` +
		`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":11,"output_tokens":22,"cache_creation_input_tokens":3,` +
		`"cache_read_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":5,` +
		`"ephemeral_1h_input_tokens":6},"cost":0.000000002}}`
	dispatched := make(chan struct{})
	release := make(chan struct{})
	stub := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		close(dispatched)
		<-release
		time.Sleep(5 * time.Millisecond)
		jsonHandler(http.StatusOK, accountingTurn)(w, nil)
	})
	seat := billedSeat(stub.url(), "self_hosted")
	pipeline := newProxy(t, proxy.Config{
		Registry:   newRegistry(t, seat),
		Host:       testHost,
		Accountant: store,
	})
	answer := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		pipeline.ServeHTTP(recorder, messagesRequest(fidelityBody))
		answer <- recorder
	}()

	select {
	case <-dispatched:
	case <-time.After(time.Second):
		t.Fatal("upstream was not dispatched")
	}

	var lifecycle string
	var contacted int
	var dispatchedAt sql.NullInt64
	var upstreamTarget sql.NullString
	err = db.QueryRow(`SELECT lifecycle_state, upstream_contacted, upstream_dispatched_at, upstream_target
		FROM accounting_requests`).Scan(&lifecycle, &contacted, &dispatchedAt, &upstreamTarget)
	if err != nil {
		t.Fatalf("query pending accounting lifecycle: %v", err)
	}
	if lifecycle != "pending" || contacted != 1 || !dispatchedAt.Valid || upstreamTarget.Valid {
		t.Fatalf(
			"mid-flight row = lifecycle=%q contacted=%d dispatched_at=%v upstream_target=%v; want pending, 1, set, NULL",
			lifecycle,
			contacted,
			dispatchedAt,
			upstreamTarget,
		)
	}

	close(release)
	if got := <-answer; got.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", got.Code, http.StatusOK)
	}
	pipeline.Drain(t.Context())

	var outcome, costStatus, stopReason, blockShape string
	var complete, status int
	var responseID string
	var completedAt, input, output, cacheCreation, cacheRead, cache5m, cache1h, providerCost sql.NullInt64
	var providerPresent, providerValid int
	var duration, upstream int64
	err = db.QueryRow(`SELECT lifecycle_state, outcome, cost_status, upstream_contacted,
		completed_at, upstream_dispatched_at, normalization_complete, response_id, http_status, input_tokens, output_tokens,
		cache_creation_input_tokens, cache_read_input_tokens, cache_creation_5m_input_tokens,
		cache_creation_1h_input_tokens, provider_cost_present, provider_cost_valid,
		provider_cost_nano_usd, stop_reason, block_shape, duration_ms, upstream_ms
		FROM accounting_requests`).Scan(&lifecycle, &outcome, &costStatus, &contacted, &completedAt,
		&dispatchedAt, &complete, &responseID, &status, &input, &output, &cacheCreation, &cacheRead, &cache5m,
		&cache1h, &providerPresent, &providerValid, &providerCost, &stopReason, &blockShape,
		&duration, &upstream)
	if err != nil {
		t.Fatalf("query finalized accounting lifecycle: %v", err)
	}
	if lifecycle != "finalized" || outcome != string(usage.OutcomeSuccess) ||
		costStatus != string(usage.CostStatusKnown) {
		t.Fatalf("state/outcome/cost = %q/%q/%q, want finalized/success/known", lifecycle, outcome, costStatus)
	}
	if contacted != 1 || !dispatchedAt.Valid || !completedAt.Valid || completedAt.Int64 < dispatchedAt.Int64 ||
		complete != 1 || responseID != "msg_usage" || status != http.StatusOK ||
		!input.Valid || input.Int64 != 11 || !output.Valid || output.Int64 != 22 ||
		!cacheCreation.Valid || cacheCreation.Int64 != 3 || !cacheRead.Valid || cacheRead.Int64 != 4 ||
		!cache5m.Valid || cache5m.Int64 != 5 || !cache1h.Valid || cache1h.Int64 != 6 ||
		providerPresent != 1 || providerValid != 1 || !providerCost.Valid || providerCost.Int64 != 2 ||
		stopReason != "end_turn" || blockShape != `["text"]` || duration < upstream || upstream <= 0 {
		t.Fatalf("accounting row = contacted=%d completed=%v dispatched=%v complete=%d response=%q status=%d "+
			"input=%v output=%v cache=%v/%v/%v/%v provider=%d/%d/%v "+
			"stop=%q shape=%q duration=%d upstream=%d",
			contacted, completedAt, dispatchedAt, complete, responseID, status, input, output,
			cacheCreation, cacheRead, cache5m, cache1h, providerPresent,
			providerValid, providerCost, stopReason, blockShape, duration, upstream)
	}
}

func TestAccountingStreamRecordsFirstContentAtAndReasoningTokens(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store, err := usage.Open(path)
	if err != nil {
		t.Fatalf("open usage store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	db, err := sql.Open("sqlite", "file:"+path+"?_foreign_keys=1")
	if err != nil {
		t.Fatalf("open usage database for inspection: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	stub := newUpstream(t, sseHandler(t, string(nullUsageAccountingFixture), nil))
	pipeline := newProxy(t, proxy.Config{
		Registry:   newRegistry(t, identitySeat(stub.url())),
		Host:       testHost,
		Accountant: store,
	})
	answer := httptest.NewRecorder()
	pipeline.ServeHTTP(answer, messagesRequest(fidelityBody))
	if answer.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", answer.Code, http.StatusOK)
	}
	pipeline.Drain(t.Context())

	var dispatchedAt, firstContentAt sql.NullInt64
	var reasoning sql.NullInt64
	if err := db.QueryRow(`SELECT upstream_dispatched_at, first_content_at, reasoning_output_tokens
		FROM accounting_requests`).Scan(&dispatchedAt, &firstContentAt, &reasoning); err != nil {
		t.Fatalf("query finalized stream accounting: %v", err)
	}
	if !dispatchedAt.Valid || !firstContentAt.Valid || firstContentAt.Int64 <= dispatchedAt.Int64 {
		t.Fatalf(
			"timestamps = dispatched=%v first_content=%v; want first content after dispatch",
			dispatchedAt,
			firstContentAt,
		)
	}
	if !reasoning.Valid || reasoning.Int64 != 20 {
		t.Fatalf("reasoning_output_tokens = %v; want 20", reasoning)
	}
}

func TestAccountingDrainTimeoutLeavesPendingRow(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store, err := usage.Open(path)
	if err != nil {
		t.Fatalf("open usage store: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+path+"?_foreign_keys=1")
	if err != nil {
		t.Fatalf("open usage database for inspection: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	accountant := &delayedStoreAccountant{
		store:   store,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	sink := newLogSink()
	stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
	seat := billedSeat(stub.url(), "self_hosted")
	pipeline := newProxy(t, proxy.Config{
		Registry:   newRegistry(t, seat),
		Host:       testHost,
		Logger:     slog.New(sink),
		Accountant: accountant,
	})

	answer := httptest.NewRecorder()
	pipeline.ServeHTTP(answer, messagesRequest(fidelityBody))
	if answer.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", answer.Code, http.StatusOK)
	}

	select {
	case <-accountant.started:
	case <-time.After(time.Second):
		t.Fatal("accounting finalization did not start")
	}

	drainCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	pipeline.Drain(drainCtx)

	var lifecycle string
	if err := db.QueryRow(`SELECT lifecycle_state FROM accounting_requests`).Scan(&lifecycle); err != nil {
		t.Fatalf("query pending accounting row: %v", err)
	}
	if lifecycle != "pending" {
		t.Fatalf("lifecycle_state = %q, want pending after bounded drain timeout", lifecycle)
	}

	select {
	case message := <-sink.messages:
		if message != "usage accounting drain timed out; finalization may be incomplete" {
			t.Fatalf("drain log = %q, want finalization warning", message)
		}
	case <-time.After(time.Second):
		t.Fatal("drain timeout did not log incomplete accounting finalization")
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close usage store after drain: %v", err)
	}
	close(accountant.release)
	pipeline.Drain(t.Context())
	sink.waitFor(t, "usage accounting finalization failed")
}
