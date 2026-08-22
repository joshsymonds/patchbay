package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/proxy"
)

// settleWindow is how long a test waits before it is satisfied that Drain is
// still blocked. It bounds scheduling, not timing: a Drain that does not wait
// at all returns immediately, far inside this.
const settleWindow = 100 * time.Millisecond

// drainDeadline bounds waiting for a Drain that should have given up.
const drainDeadline = 10 * time.Second

// blockedRecord runs one request through a proxy whose recorder announces the
// append and then blocks, and returns the proxy with that record outstanding.
func blockedRecord(t *testing.T, recorder *stubRecorder) *proxy.Proxy {
	t.Helper()

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","id":"msg_1"}`))

	pxy := newProxy(t, proxy.Config{
		Registry:   newRegistry(t, identitySeat(stub.url())),
		Recorder:   recorder,
		Client:     anthropicClient(t, stub),
		Host:       testHost,
		Accountant: permissiveAccountant{},
	})

	pxy.ServeHTTP(httptest.NewRecorder(), messagesRequest(`{"model":"`+echoSelector+`"}`))

	<-recorder.started

	return pxy
}

// TestDrainWaitsForAnOutstandingRecord holds the shutdown contract: a record is
// written off the request path, so the last request can be answered and the
// process torn down while its record is still being appended. Closing the
// ledger under that write loses the record — silently, since the pipeline
// treats a recorder failure as costing nothing else.
func TestDrainWaitsForAnOutstandingRecord(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	recorder := newBlockingRecorder()
	pxy := blockedRecord(t, recorder)

	drained := make(chan struct{})

	go func() {
		defer close(drained)

		pxy.Drain(t.Context())
	}()

	select {
	case <-drained:
		t.Fatal("Drain returned while a record was still being written")
	case <-time.After(settleWindow):
	}

	close(recorder.release)
	<-drained

	if got := len(recorder.records); got != 1 {
		t.Fatalf("records written = %d; want the outstanding one to have landed before Drain returned", got)
	}
}

// TestDrainGivesUpWhenItsContextExpires is the other half: a Recorder that
// never returns must cost the records, not the shutdown.
func TestDrainGivesUpWhenItsContextExpires(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	recorder := newBlockingRecorder()

	// Released at teardown so the stuck append is not left holding the test
	// binary open.
	t.Cleanup(func() { close(recorder.release) })

	pxy := blockedRecord(t, recorder)

	ctx, cancel := context.WithCancel(t.Context())
	drained := make(chan struct{})

	go func() {
		defer close(drained)

		pxy.Drain(ctx)
	}()

	select {
	case <-drained:
		t.Fatal("Drain returned while a record was still being written")
	case <-time.After(settleWindow):
	}

	cancel()

	select {
	case <-drained:
	case <-time.After(drainDeadline):
		t.Fatal("Drain never gave up after its context expired")
	}

	if got := len(recorder.records); got != 0 {
		t.Fatalf("records written = %d; want none — the recorder never returned", got)
	}
}
