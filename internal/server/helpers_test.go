package server_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/server"
)

const (
	jsonType = "application/json"

	// epochCreatedAt is the created_at every alias entry carries: the epoch
	// sentinel Anthropic's own contract sanctions for a model whose release date
	// is unknown.
	epochCreatedAt = "1970-01-01T00:00:00Z"

	// anthropicHost is the only destination the model endpoints may address.
	// The base URL is a constant of the package and the caller's path is set on
	// the parsed URL afterwards, so nothing a caller sends can move it.
	anthropicHost = "api.anthropic.com"

	// callBuffer sizes the recorded-call channel: wide enough that a stub
	// upstream never blocks on a test that forgets to drain it.
	callBuffer = 8

	// lunaRoute is a fully populated alias: both token ceilings are set, so its
	// listing entry advertises them.
	lunaRoute = `"luna-fast":{"base_url":"https://openrouter.ai/api",` +
		`"model":"openai/gpt-5.6-luna","auth":"inject",` +
		`"api_key_env_file":"OPENROUTER_API_KEY_FILE",` +
		`"max_input_tokens":262144,"max_output_tokens":65536}`

	// barePinRoute sets no ceiling at all, so its entry carries neither.
	barePinRoute = `"anthropic/pinned":{"base_url":"https://api.anthropic.com",` +
		`"model":"claude-opus-5","auth":"forward"}`
)

// upstreamCall is one request as the stub upstream received it. host is the
// destination the request was actually addressed to, which for every fetch this
// package makes must be Anthropic and nothing else.
type upstreamCall struct {
	method   string
	path     string
	rawQuery string
	host     string
	header   http.Header
}

// upstream is a stub Anthropic that records every request it receives before
// handing it to the test's own handler.
type upstream struct {
	server *httptest.Server
	calls  chan upstreamCall
}

// newUpstream starts an https stub on 127.0.0.1 running handle. It is https
// because the only upstream this package ever calls is the fixed
// https://api.anthropic.com, reached through a pinned client.
func newUpstream(t *testing.T, handle http.HandlerFunc) *upstream {
	t.Helper()

	stub := &upstream{calls: make(chan upstreamCall, callBuffer)}
	stub.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.calls <- upstreamCall{
			method:   r.Method,
			path:     r.URL.Path,
			rawQuery: r.URL.RawQuery,
			host:     r.Host,
			header:   r.Header.Clone(),
		}
		handle(w, r)
	}))
	t.Cleanup(stub.server.Close)

	return stub
}

// call returns the single request the upstream received, failing if it saw a
// different number of them.
func (u *upstream) call(t *testing.T) upstreamCall {
	t.Helper()

	if len(u.calls) != 1 {
		t.Fatalf("upstream received %d requests, want 1", len(u.calls))
	}

	return <-u.calls
}

// hits reports how many requests the upstream received.
func (u *upstream) hits() int {
	return len(u.calls)
}

// anthropicClient returns a client whose every connection lands on the stub, so
// a fetch aimed at https://api.anthropic.com never leaves the machine.
func anthropicClient(t *testing.T, stub *upstream) *http.Client {
	t.Helper()

	addr := stub.server.Listener.Addr().String()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer

			return dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	t.Cleanup(transport.CloseIdleConnections)

	return &http.Client{Transport: transport}
}

// deadClient returns a client whose every connection fails immediately, standing
// in for an unreachable Anthropic without waiting on a timeout.
func deadClient(t *testing.T) *http.Client {
	t.Helper()

	return &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("upstream is unreachable in this test")
		},
	}}
}

// newRegistry writes a registry file holding the given route entries and
// returns a Registry reading it.
func newRegistry(t *testing.T, routes string) *registry.Registry {
	t.Helper()

	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte(`{"routes":{`+routes+`}}`), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	return registry.New(path)
}

// sentinel is a stand-in proxy handler that records every request routed to it
// and answers with a body naming the path it saw.
type sentinel struct {
	calls chan upstreamCall
}

// newSentinel returns a sentinel proxy handler.
func newSentinel() *sentinel {
	return &sentinel{calls: make(chan upstreamCall, callBuffer)}
}

// ServeHTTP records the request and answers with a marker body.
func (s *sentinel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.calls <- upstreamCall{
		method:   r.Method,
		path:     r.URL.Path,
		rawQuery: r.URL.RawQuery,
		host:     r.Host,
		header:   r.Header.Clone(),
	}

	w.Header().Set("Content-Type", jsonType)
	w.WriteHeader(http.StatusTeapot)
	io.WriteString(w, `{"proxied":true}`)
}

// hits reports how many requests reached the proxy.
func (s *sentinel) hits() int {
	return len(s.calls)
}

// call returns the single request the proxy received.
func (s *sentinel) call(t *testing.T) upstreamCall {
	t.Helper()

	if len(s.calls) != 1 {
		t.Fatalf("proxy received %d requests, want 1", len(s.calls))
	}

	return <-s.calls
}

// logSink is a slog handler that hands every message to a channel, so a test
// can wait for a warning instead of racing a shared buffer.
type logSink struct {
	messages chan string
}

// newLogSink returns a sink wide enough to hold a request's warnings.
func newLogSink() *logSink {
	return &logSink{messages: make(chan string, callBuffer)}
}

// Enabled accepts every level, so nothing the gateway logs is missed.
func (*logSink) Enabled(context.Context, slog.Level) bool { return true }

// Handle records the message, dropping it rather than blocking if full.
func (s *logSink) Handle(_ context.Context, rec slog.Record) error {
	select {
	case s.messages <- rec.Message:
	default:
	}

	return nil
}

// WithAttrs satisfies slog.Handler; the sink keeps only messages.
func (s *logSink) WithAttrs([]slog.Attr) slog.Handler { return s }

// WithGroup satisfies slog.Handler; the sink keeps only messages.
func (s *logSink) WithGroup(string) slog.Handler { return s }

// waitFor fails unless want is logged before the deadline.
func (s *logSink) waitFor(t *testing.T, want string) {
	t.Helper()

	deadline := time.After(10 * time.Second)

	for {
		select {
		case message := <-s.messages:
			if message == want {
				return
			}
		case <-deadline:
			t.Fatalf("log message %q never arrived", want)

			return
		}
	}
}

// serve runs one request through a handler built from cfg.
func serve(t *testing.T, cfg server.Config, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	server.Handler(cfg).ServeHTTP(rec, req)

	return rec
}

// modelsRequest builds an inbound GET carrying no credentials.
func modelsRequest(target string) *http.Request {
	return httptest.NewRequest(http.MethodGet, target, nil)
}

// listResponse is the decoded /v1/models envelope, with each entry left as the
// bytes it was written as so upstream fields are compared exactly.
type listResponse struct {
	Data    []json.RawMessage `json:"data"`
	HasMore bool              `json:"has_more"`
	FirstID *string           `json:"first_id"`
	LastID  *string           `json:"last_id"`
}

// decodeList decodes a /v1/models response body.
func decodeList(t *testing.T, body []byte) listResponse {
	t.Helper()

	var list listResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode model list %q: %v", body, err)
	}

	return list
}

// ids returns the id of every entry in a decoded listing, in order.
func ids(t *testing.T, list listResponse) []string {
	t.Helper()

	found := make([]string, 0, len(list.Data))

	for _, raw := range list.Data {
		var entry struct {
			ID string `json:"id"`
		}

		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("decode model entry %q: %v", raw, err)
		}

		found = append(found, entry.ID)
	}

	return found
}

// entryFields decodes one listing entry into a generic map, so a test can
// assert on exactly which keys it carries.
func entryFields(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()

	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode model entry %q: %v", raw, err)
	}

	return fields
}

// upstreamList is a stub upstream listing carrying the given ids, each with a
// created_at patchbay must relay untouched and never invent.
func upstreamList(modelIDs ...string) string {
	entries := make([]string, 0, len(modelIDs))
	for _, id := range modelIDs {
		entries = append(entries, `{"type":"model","id":"`+id+`","display_name":"`+id+
			`","created_at":"2026-01-01T00:00:00Z"}`)
	}

	return `{"data":[` + strings.Join(entries, ",") + `],"has_more":false}`
}

// jsonHandler answers every request with status and body.
func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", jsonType)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}
