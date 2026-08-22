package proxy

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/ledger"
	usagepkg "github.com/joshsymonds/patchbay/internal/usage"
)

type completionAccountant struct {
	release chan struct{}
	started chan int64

	mu        sync.Mutex
	finalized []int64
	requests  []usagepkg.FinalizedRequest
}

func (accountant *completionAccountant) InsertPending(
	context.Context,
	usagepkg.PendingRequest,
) (int64, error) {
	return 1, nil
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

func newCompletionProxy(t *testing.T, accountant Accountant, recorder Recorder, logger *slog.Logger) *Proxy {
	t.Helper()
	pipeline, err := New(Config{Accountant: accountant, Recorder: recorder, Logger: logger, Host: "host"})
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

func fillCompletionQueue(t *testing.T, pipeline *Proxy, accountant *completionAccountant) chan struct{} {
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
	blocked := make(chan struct{})
	go func() {
		pipeline.record(
			context.Background(),
			completionExchange(int64(completionWorkerCount+completionQueueCapacity+1)),
			nil,
		)
		close(blocked)
	}()
	return blocked
}

func TestCompletionQueueBackpressuresWhenFull(t *testing.T) {
	accountant := &completionAccountant{
		release: make(chan struct{}),
		started: make(chan int64, completionWorkerCount),
	}
	pipeline := newCompletionProxy(t, accountant, nil, nil)
	blocked := fillCompletionQueue(t, pipeline, accountant)

	select {
	case <-blocked:
		t.Fatal("completion submission returned while every worker and queue slot were occupied")
	case <-time.After(100 * time.Millisecond):
	}
	close(accountant.release)
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("completion submission remained blocked after queue capacity became available")
	}
	pipeline.Drain(t.Context())
}

func TestDrainRemainsBoundedWithAFullCompletionQueue(t *testing.T) {
	accountant := &completionAccountant{
		release: make(chan struct{}),
		started: make(chan int64, completionWorkerCount),
	}
	sink := &completionLogSink{messages: make(chan string, 8)}
	pipeline := newCompletionProxy(t, accountant, nil, slog.New(sink))
	blocked := fillCompletionQueue(t, pipeline, accountant)

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
		if record.Status != 200 || record.RequestBody != "before" || record.ErrorBody == string(capt.buf) {
			t.Fatalf("queued ledger record = %+v; want values materialized before persistence blocked", record)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued ledger record was not appended")
	}
}
