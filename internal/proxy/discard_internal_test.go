package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// drainTestTimeout is how long the test waits for a bounded drain before
// calling it unbounded. It is a hang guard, not a performance target.
const drainTestTimeout = 5 * time.Second

// endlessBody is a response body that never ends, standing in for an upstream
// that answers a refusal with more bytes than patchbay will ever read.
type endlessBody struct {
	read atomic.Int64
}

// Read fills p with zeroes forever, counting what was handed out.
func (b *endlessBody) Read(p []byte) (int, error) {
	b.read.Add(int64(len(p)))

	return len(p), nil
}

// Close satisfies io.ReadCloser.
func (*endlessBody) Close() error { return nil }

// A refused response is drained so its connection can be reused, but the drain
// is bounded: an upstream that answers a 403 with an endless body must cost the
// connection, not the request.
func TestDiscardBoundsTheDrain(t *testing.T) {
	body := &endlessBody{}
	proxied := &Proxy{logger: slog.New(slog.DiscardHandler)}
	done := make(chan struct{})

	go func() {
		proxied.discard(context.Background(), &http.Response{Body: body})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(drainTestTimeout):
		t.Fatal("discard never returned; the drain of a refused body is unbounded")
	}

	if got := body.read.Load(); got > drainLimitBytes+streamChunkBytes {
		t.Fatalf("discard read %d bytes; want no more than the %d-byte ceiling", got, drainLimitBytes)
	}
}
