package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// deadConnTestTimeout is how long the test waits for a request on a dead
// connection to fail before calling it hung. It is a hang guard, not a
// performance target: without the HTTP/2 health check the request waits out
// responseHeaderTimeout, ten minutes.
const deadConnTestTimeout = 5 * time.Second

// blackHole relays TCP between a client and one upstream until it is frozen.
// Frozen, it keeps both sockets open and silently drops every byte in either
// direction: the shape of a connection whose route vanished, where writes
// still succeed locally and nothing ever comes back.
type blackHole struct {
	listener net.Listener
	target   string
	frozen   atomic.Bool
}

// newBlackHole starts a relay to target on a loopback port.
func newBlackHole(t *testing.T, target string) *blackHole {
	t.Helper()

	var listen net.ListenConfig

	listener, err := listen.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	hole := &blackHole{listener: listener, target: target}
	t.Cleanup(func() { _ = listener.Close() })

	go hole.accept()

	return hole
}

// accept relays every connection until the listener closes.
func (h *blackHole) accept() {
	for {
		client, err := h.listener.Accept()
		if err != nil {
			return
		}

		var dialer net.Dialer

		upstream, err := dialer.DialContext(context.Background(), "tcp", h.target)
		if err != nil {
			_ = client.Close()

			continue
		}

		go h.pipe(upstream, client)
		go h.pipe(client, upstream)
	}
}

// pipe copies src to dst, discarding instead once the relay is frozen.
func (h *blackHole) pipe(dst, src net.Conn) {
	defer func() { _ = dst.Close() }()

	buf := make([]byte, streamChunkBytes)

	for {
		n, err := src.Read(buf)
		if n > 0 && !h.frozen.Load() {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
		}

		if err != nil {
			return
		}
	}
}

// A pooled HTTP/2 connection whose path dies must be detected and dropped, not
// handed every new request: each of those would hang behind it. This is what a
// torn-down VPN tunnel did in production — the connection stayed ESTABLISHED
// on a source address that no longer existed, and every Messages request
// multiplexed onto it hung until the caller gave up.
func TestDefaultClientDropsADeadHTTP2Connection(t *testing.T) {
	stub := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	}))
	stub.EnableHTTP2 = true
	stub.StartTLS()
	t.Cleanup(stub.Close)

	hole := newBlackHole(t, stub.Listener.Addr().String())
	url := "https://" + hole.listener.Addr().String()

	client := newClient(100*time.Millisecond, 100*time.Millisecond)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T; want *http.Transport", client.Transport)
	}
	// The stub's certificate is self-signed; trust it the way its own client does.
	stubTransport, ok := stub.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("stub transport is %T; want *http.Transport", stub.Client().Transport)
	}
	transport.TLSClientConfig = stubTransport.TLSClientConfig.Clone()

	first := get(t, client, url)
	if first.ProtoMajor != 2 {
		t.Fatalf("first request spoke %s; the test needs a pooled HTTP/2 connection", first.Proto)
	}

	hole.frozen.Store(true)

	failed := make(chan error, 1)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*deadConnTestTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			failed <- err

			return
		}

		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			err = errors.New("request on a black-holed connection succeeded")
		}
		failed <- err
	}()

	select {
	case err := <-failed:
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("request hung until its own deadline (%v); the dead connection was never dropped", err)
		}
	case <-time.After(deadConnTestTimeout):
		t.Fatal("request on a dead HTTP/2 connection still hung; the health check never dropped it")
	}

	hole.frozen.Store(false)

	// The pool must dial afresh rather than keep the corpse.
	if second := get(t, client, url); second.ProtoMajor != 2 {
		t.Fatalf("request after the drop spoke %s; want HTTP/2", second.Proto)
	}
}

// get performs one GET and reads its body to the end, failing the test on any
// error, so the connection is back in the pool when it returns.
func get(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), deadConnTestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if got := strings.TrimSpace(string(body)); got != "ok" {
		t.Fatalf("body = %q; want ok", got)
	}

	return resp
}
