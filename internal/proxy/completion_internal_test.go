package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/registry"
	usagepkg "github.com/joshsymonds/patchbay/internal/usage"
)

type completionAccountant struct {
	release chan struct{}
	started chan int64

	nextID atomic.Int64

	mu        sync.Mutex
	finalized []int64
	requests  []usagepkg.FinalizedRequest
}

func (accountant *completionAccountant) InsertPending(
	context.Context,
	usagepkg.PendingRequest,
) (int64, error) {
	return accountant.nextID.Add(1), nil
}

func (accountant *completionAccountant) MarkUpstreamDispatched(
	context.Context,
	int64,
	time.Time,
	string,
) error {
	return nil
}

func (accountant *completionAccountant) Finalize(
	_ context.Context,
	id int64,
	request usagepkg.FinalizedRequest,
) error {
	if accountant.started != nil {
		select {
		case accountant.started <- id:
		default:
		}
	}
	if accountant.release != nil {
		<-accountant.release
	}
	accountant.mu.Lock()
	accountant.finalized = append(accountant.finalized, id)
	accountant.requests = append(accountant.requests, request)
	accountant.mu.Unlock()
	return nil
}

type completionRecorder struct {
	records chan ledger.Record
}

func (recorder *completionRecorder) Append(record ledger.Record) error {
	recorder.records <- record
	return nil
}

type completionLogSink struct {
	messages chan string
}

func (sink *completionLogSink) Enabled(context.Context, slog.Level) bool { return true }
func (sink *completionLogSink) Handle(_ context.Context, record slog.Record) error {
	sink.messages <- record.Message
	return nil
}
func (sink *completionLogSink) WithAttrs([]slog.Attr) slog.Handler { return sink }
func (sink *completionLogSink) WithGroup(string) slog.Handler      { return sink }

func newCompletionProxy(
	t *testing.T,
	accountant Accountant,
	recorder Recorder,
	logger *slog.Logger,
) *Proxy {
	t.Helper()
	pipeline, err := New(
		Config{Accountant: accountant, Recorder: recorder, Logger: logger, Host: "host"},
	)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return pipeline
}

func completionExchange(id int64) *exchange {
	return &exchange{
		accountingID:         id,
		accountingDispatched: true,
		start:                time.Now(),
		status:               200,
		durationMS:           1,
		upstreamMS:           1,
	}
}

func waitForCompletionStarts(t *testing.T, started <-chan int64, count int) {
	t.Helper()
	for range count {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only some completion workers started; want %d", count)
		}
	}
}

func fillCompletionQueue(t *testing.T, pipeline *Proxy, accountant *completionAccountant) {
	t.Helper()
	for index := range completionWorkerCount {
		pipeline.record(context.Background(), completionExchange(int64(index+1)), nil)
	}
	waitForCompletionStarts(t, accountant.started, completionWorkerCount)
	for index := range completionQueueCapacity {
		pipeline.record(
			context.Background(),
			completionExchange(int64(completionWorkerCount+index+1)),
			nil,
		)
	}
}

type completionTransport struct {
	contacts atomic.Int64
}

func (transport *completionTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.contacts.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"type":"message","content":[]}`)),
	}, nil
}

func completionRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.json")
	body := []byte(`{
		"default_context":"personal",
		"seats":{"default":{"upstream":"https://api.anthropic.com","auth_mode":"forward"}},
		"contexts":{"personal":{"default_seat":"default","models":{}}}
	}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	reg, err := registry.New(path)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return reg
}

func completionRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, messagesPath, strings.NewReader(`{"model":"test"}`))
	req.Header.Set(anthropic.HeaderClientKey, "caller-key")
	return req
}

func TestServeHTTPBoundsFullCompletionQueueAndRefusesOverload(t *testing.T) {
	accountant := &completionAccountant{
		release: make(chan struct{}),
		started: make(chan int64, completionWorkerCount),
	}
	transport := &completionTransport{}
	pipeline, err := New(Config{
		Registry:   completionRegistry(t),
		Accountant: accountant,
		Client:     &http.Client{Transport: transport},
		Host:       "host",
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	fillCompletionQueue(t, pipeline, accountant)

	blocked := make(chan struct{}, completionWorkerCount+completionQueueCapacity)
	for range completionWorkerCount + completionQueueCapacity {
		go func() {
			pipeline.ServeHTTP(httptest.NewRecorder(), completionRequest())
			blocked <- struct{}{}
		}()
	}

	deadline := time.Now().Add(5 * time.Second)
	for transport.contacts.Load() != completionWorkerCount+completionQueueCapacity && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := transport.contacts.Load(); got != completionWorkerCount+completionQueueCapacity {
		t.Fatalf(
			"upstream contacts before overload = %d; want %d admitted requests",
			got,
			completionWorkerCount+completionQueueCapacity,
		)
	}
	select {
	case <-blocked:
		t.Fatal("an admitted handler returned while completion persistence was stalled")
	default:
	}

	overload := httptest.NewRecorder()
	overloadDone := make(chan struct{})
	go func() {
		pipeline.ServeHTTP(overload, completionRequest())
		close(overloadDone)
	}()
	select {
	case <-overloadDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("over-capacity request did not receive a prompt local refusal")
	}
	if overload.Code != http.StatusServiceUnavailable {
		t.Fatalf("overload status = %d; want %d", overload.Code, http.StatusServiceUnavailable)
	}
	var errorBody struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(overload.Body.Bytes(), &errorBody); err != nil {
		t.Fatalf("decode overload response: %v", err)
	}
	if errorBody.Type != "error" || errorBody.Error.Type != anthropic.ErrorTypeAPI {
		t.Fatalf("overload response = %+v; want Anthropic API error shape", errorBody)
	}
	if got := transport.contacts.Load(); got != completionWorkerCount+completionQueueCapacity {
		t.Fatalf(
			"upstream contacts after overload = %d; want %d",
			got,
			completionWorkerCount+completionQueueCapacity,
		)
	}
	if got := accountant.nextID.Load(); got != completionWorkerCount+completionQueueCapacity {
		t.Fatalf(
			"pending accounting inserts after overload = %d; want %d",
			got,
			completionWorkerCount+completionQueueCapacity,
		)
	}

	close(accountant.release)
	for range completionWorkerCount + completionQueueCapacity {
		select {
		case <-blocked:
		case <-time.After(5 * time.Second):
			t.Fatal("admitted handler remained blocked after persistence resumed")
		}
	}
	pipeline.Drain(t.Context())

	accountant.mu.Lock()
	defer accountant.mu.Unlock()
	wantFinalized := completionWorkerCount + completionQueueCapacity + completionWorkerCount + completionQueueCapacity
	if got := len(accountant.finalized); got != wantFinalized {
		t.Fatalf(
			"finalized completions = %d; want %d lossless admitted completions",
			got,
			wantFinalized,
		)
	}
}

func TestServeHTTPBoundsFailuresBeforeSeatSelection(t *testing.T) {
	const pipelineCapacity = completionWorkerCount + completionQueueCapacity
	accountant := &completionAccountant{
		release: make(chan struct{}),
		started: make(chan int64, completionWorkerCount),
	}
	recorder := &completionRecorder{records: make(chan ledger.Record, pipelineCapacity*2)}
	transport := &completionTransport{}
	pipeline, err := New(Config{
		Registry:   completionRegistry(t),
		Recorder:   recorder,
		Accountant: accountant,
		Client:     &http.Client{Transport: transport},
		Host:       "host",
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	fillCompletionQueue(t, pipeline, accountant)

	blocked := make(chan struct{}, pipelineCapacity)
	for range pipelineCapacity {
		go func() {
			req := completionRequest()
			req = req.WithContext(registry.WithResolutionError(req.Context(), registry.ErrInvalid))
			pipeline.ServeHTTP(httptest.NewRecorder(), req)
			blocked <- struct{}{}
		}()
	}

	deadline := time.Now().Add(5 * time.Second)
	for pipeline.inFlight.Load() != pipelineCapacity*2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := pipeline.inFlight.Load(); got != pipelineCapacity*2 {
		t.Fatalf(
			"in-flight completions = %d; want %d including bounded pre-seat failures",
			got,
			pipelineCapacity*2,
		)
	}
	select {
	case <-blocked:
		t.Fatal("a pre-seat failure handler returned while completion persistence was stalled")
	default:
	}

	overload := httptest.NewRecorder()
	pipeline.ServeHTTP(overload, completionRequest())
	if overload.Code != http.StatusServiceUnavailable {
		t.Fatalf("overload status = %d; want %d", overload.Code, http.StatusServiceUnavailable)
	}
	if got := transport.contacts.Load(); got != 0 {
		t.Fatalf("upstream contacts = %d; want zero before-seat and overload refusals", got)
	}
	if got := accountant.nextID.Load(); got != 0 {
		t.Fatalf("pending accounting inserts = %d; want zero before seat selection", got)
	}

	close(accountant.release)
	for range pipelineCapacity {
		select {
		case <-blocked:
		case <-time.After(5 * time.Second):
			t.Fatal("pre-seat failure handler remained blocked after persistence resumed")
		}
	}
	pipeline.Drain(t.Context())
	if got := len(recorder.records); got != pipelineCapacity*2 {
		t.Fatalf(
			"ledger records = %d; want %d lossless admitted completions",
			got,
			pipelineCapacity*2,
		)
	}
}

func TestDrainRemainsBoundedWithAFullCompletionQueue(t *testing.T) {
	accountant := &completionAccountant{
		release: make(chan struct{}),
		started: make(chan int64, completionWorkerCount),
	}
	sink := &completionLogSink{messages: make(chan string, 8)}
	pipeline := newCompletionProxy(t, accountant, nil, slog.New(sink))
	fillCompletionQueue(t, pipeline, accountant)
	blocked := make(chan struct{})
	go func() {
		pipeline.record(context.Background(), completionExchange(1000), nil)
		close(blocked)
	}()

	drainCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	pipeline.Drain(drainCtx)
	select {
	case message := <-sink.messages:
		if message != "usage accounting drain timed out; finalization may be incomplete" {
			t.Fatalf("Drain() log = %q; want incomplete-finalization warning", message)
		}
	case <-time.After(time.Second):
		t.Fatal("Drain() returned without loudly logging unfinished completions")
	}
	select {
	case <-blocked:
		t.Fatal("full-queue submitter returned before a worker was released")
	default:
	}

	close(accountant.release)
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("full-queue submitter remained blocked after release")
	}
	pipeline.Drain(t.Context())
}

func TestCompletionQueueProcessesConcurrentLoadWithoutLoss(t *testing.T) {
	const completions = 512
	accountant := &completionAccountant{}
	pipeline := newCompletionProxy(t, accountant, nil, nil)

	var submissions sync.WaitGroup
	for index := range completions {
		submissions.Add(1)
		go func(id int64) {
			defer submissions.Done()
			pipeline.record(context.Background(), completionExchange(id), nil)
		}(int64(index + 1))
	}
	submissions.Wait()
	pipeline.Drain(t.Context())

	accountant.mu.Lock()
	defer accountant.mu.Unlock()
	if len(accountant.finalized) != completions {
		t.Fatalf("finalized completions = %d; want %d", len(accountant.finalized), completions)
	}
	seen := make(map[int64]bool, completions)
	for _, id := range accountant.finalized {
		if seen[id] {
			t.Fatalf("request %d finalized more than once", id)
		}
		seen[id] = true
	}
}

func TestCompletionMaterializesBeforePersistenceBlocks(t *testing.T) {
	accountant := &completionAccountant{
		release: make(chan struct{}),
		started: make(chan int64, 1),
	}
	recorder := &completionRecorder{records: make(chan ledger.Record, 1)}
	pipeline := newCompletionProxy(t, accountant, recorder, nil)
	exchange := completionExchange(1)
	exchange.requestBody = "before"
	capt := &capture{buf: []byte(`{"type":"error","error":{"message":"before"}}`)}

	pipeline.record(context.Background(), exchange, capt)
	waitForCompletionStarts(t, accountant.started, 1)
	exchange.status = 503
	exchange.requestBody = "after"
	capt.buf = []byte(`{"type":"error","error":{"message":"after"}}`)
	close(accountant.release)
	pipeline.Drain(t.Context())

	select {
	case record := <-recorder.records:
		if record.Status != 200 || record.RequestBody != "before" ||
			record.ErrorBody == string(capt.buf) {
			t.Fatalf(
				"queued ledger record = %+v; want values materialized before persistence blocked",
				record,
			)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued ledger record was not appended")
	}
}
