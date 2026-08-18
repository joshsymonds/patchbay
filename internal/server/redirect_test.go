package server

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/joshsymonds/patchbay/internal/registry"
)

// anthropicAddr is the host:port a fetch at https://api.anthropic.com dials.
const anthropicAddr = "api.anthropic.com:443"

// TestDefaultClientRelaysARedirectRatherThanFollowingIt pins the redirect
// policy of the client this package builds when the host process supplies none
// — the client production actually runs on, since main passes a nil Client.
//
// Go's default policy follows up to ten hops, and crossing to another host it
// strips only Authorization, Www-Authenticate, Cookie and the Proxy-* headers —
// never x-api-key, anthropic-version or anthropic-beta. Following a redirect
// would hand the caller's key to whatever host the upstream named, and would
// break the single-model relay's contract besides: it answers with the
// upstream's status and body, and a 3xx is one of them.
//
// The test is in-package because the policy under test belongs to
// defaultClient, and the exported surface only reaches it by leaving the
// machine: every fetch here goes to the fixed https://api.anthropic.com. Only
// that host is pinned to the stub, so a followed redirect really does reach the
// attacker.
func TestDefaultClientRelaysARedirectRatherThanFollowingIt(t *testing.T) {
	t.Parallel()

	const redirectBody = `{"type":"error","error":{"type":"moved","message":"elsewhere"}}`

	attacker := countingServer(t)

	stub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", attacker.URL+"/v1/models/claude-opus-5")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusFound)
		io.WriteString(w, redirectBody)
	}))
	t.Cleanup(stub.Close)

	client := defaultClient()
	client.Transport = pinnedTransport(t, stub.Listener.Addr().String())

	req := httptest.NewRequest(http.MethodGet, "/v1/models/claude-opus-5", nil)
	req.Header.Set("X-Api-Key", "sk-ant-caller")

	rec := httptest.NewRecorder()
	Handler(Config{
		Proxy:    http.NotFoundHandler(),
		Registry: emptyRegistry(t),
		Client:   client,
		Logger:   slog.New(slog.DiscardHandler),
	}).ServeHTTP(rec, req)

	if hits := attackerHits(attacker); hits != 0 {
		t.Fatalf("the redirect target saw %d requests; want 0 — following one leaks the caller's credentials", hits)
	}

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d; want the upstream's own %d", rec.Code, http.StatusFound)
	}

	if got := rec.Body.String(); got != redirectBody {
		t.Fatalf("body = %q; want the upstream's bytes %q", got, redirectBody)
	}
}

// hitCounter counts the requests a stub received, over a channel so the test
// reads it without racing the server's own goroutines.
type hitCounter struct {
	*httptest.Server

	hits chan struct{}
}

// countingServer starts a plain-http stub that records every request it gets.
func countingServer(t *testing.T) *hitCounter {
	t.Helper()

	counter := &hitCounter{hits: make(chan struct{}, 8)}
	counter.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		counter.hits <- struct{}{}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"credentials":"stolen"}`)
	}))
	t.Cleanup(counter.Close)

	return counter
}

// attackerHits reports how many requests reached the redirect target.
func attackerHits(counter *hitCounter) int {
	return len(counter.hits)
}

// pinnedTransport sends connections for api.anthropic.com to addr and dials
// every other host for real, so a followed redirect to a loopback stub is
// observable while nothing ever leaves the machine.
func pinnedTransport(t *testing.T, addr string) *http.Transport {
	t.Helper()

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, requested string) (net.Conn, error) {
			var dialer net.Dialer

			if requested == anthropicAddr {
				requested = addr
			}

			return dialer.DialContext(ctx, network, requested)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	t.Cleanup(transport.CloseIdleConnections)

	return transport
}

// emptyRegistry returns a registry with no aliases, so every model lookup is
// the upstream's to answer.
func emptyRegistry(t *testing.T) *registry.Registry {
	t.Helper()

	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte(`{"routes":{}}`), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	return registry.New(path)
}
