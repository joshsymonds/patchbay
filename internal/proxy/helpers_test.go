package proxy_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/usage"
)

const (
	testHost   = "testhost"
	keyEnvName = "PATCHBAY_TEST_KEY_FILE"
	testAPIKey = "sk-upstream-secret"

	// callerKeyEnvName is the real environment variable the inject-route caller
	// gate reads; testCallerKey is the secret the canonical inbound request
	// presents in its X-Patchbay-Key header.
	callerKeyEnvName = "PATCHBAY_CALLER_KEY_FILE"
	callerKeyHeader  = "X-Patchbay-Key"
	testCallerKey    = "sk-caller-secret"

	echoAlias = "local/echo"
	sseType   = "text/event-stream"
	jsonType  = "application/json"

	// noModel is what a record carries when the request named no model at all,
	// standing in for the empty string the ledger refuses to store.
	noModel = "(none)"

	// callBuffer sizes the recorded-call channel: wide enough that a concurrent
	// test never blocks the stub upstream.
	callBuffer = 64
)

// upstreamCall is one request as the stub upstream received it.
type upstreamCall struct {
	method string

	// path is the decoded request path; escapedPath is the path exactly as it
	// arrived on the wire, which is what a percent-escaped Bedrock ARN is judged
	// by.
	path        string
	escapedPath string

	rawQuery string
	header   http.Header
	body     []byte
}

// upstream is a stub Anthropic-format server that records every request it
// receives before handing it to the test's own handler.
type upstream struct {
	server *httptest.Server
	calls  chan upstreamCall
}

// newUpstream starts a plain http stub on 127.0.0.1 running handle.
func newUpstream(t *testing.T, handle http.HandlerFunc) *upstream {
	t.Helper()

	stub := &upstream{calls: make(chan upstreamCall, callBuffer)}
	stub.server = httptest.NewServer(stub.wrap(t, handle))
	t.Cleanup(stub.server.Close)

	return stub
}

// newTLSUpstream starts an https stub on 127.0.0.1, for exercising the built-in
// default route without leaving the machine.
func newTLSUpstream(t *testing.T, handle http.HandlerFunc) *upstream {
	t.Helper()

	stub := &upstream{calls: make(chan upstreamCall, callBuffer)}
	stub.server = httptest.NewTLSServer(stub.wrap(t, handle))
	t.Cleanup(stub.server.Close)

	return stub
}

// wrap records the inbound request, then replays its body to handle.
func (u *upstream) wrap(t *testing.T, handle http.HandlerFunc) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream read body: %v", err)

			return
		}

		u.calls <- upstreamCall{
			method:      r.Method,
			path:        r.URL.Path,
			escapedPath: r.URL.EscapedPath(),
			rawQuery:    r.URL.RawQuery,
			header:      r.Header.Clone(),
			body:        body,
		}

		r.Body = io.NopCloser(strings.NewReader(string(body)))
		handle(w, r)
	})
}

// url returns the stub's base URL, suitable for a route's base_url.
func (u *upstream) url() string {
	return u.server.URL
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

// jsonHandler answers every request with status and body.
func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", jsonType)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// sseHandler streams body as an event stream, flushing at every offset in
// splits so the proxy sees pathological chunk boundaries.
func sseHandler(t *testing.T, body string, splits []int) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", sseType)
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, isFlusher := w.(http.Flusher)
		if !isFlusher {
			t.Error("upstream response writer is not an http.Flusher")

			return
		}

		prev := 0
		for _, offset := range boundaries(body, splits) {
			io.WriteString(w, body[prev:offset])
			flusher.Flush()
			prev = offset
		}
	}
}

// boundaries sorts, deduplicates and clamps split offsets, always ending at the
// end of body.
func boundaries(body string, splits []int) []int {
	sorted := append([]int(nil), splits...)
	sort.Ints(sorted)

	offsets := make([]int, 0, len(sorted)+1)
	prev := 0

	for _, offset := range sorted {
		if offset > prev && offset < len(body) {
			offsets = append(offsets, offset)
			prev = offset
		}
	}

	return append(offsets, len(body))
}

// newRegistry writes a registry file holding the given route entries in the
// personal context and returns a Registry reading it.
func newRegistry(t *testing.T, routes string) *registry.Registry {
	t.Helper()

	return newContextRegistry(t, `"personal":{"routes":{`+routes+`}}`)
}

// newContextRegistry writes a registry file holding whole context entries.
func newContextRegistry(t *testing.T, contexts string) *registry.Registry {
	t.Helper()

	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte(`{"contexts":{`+contexts+`}}`), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	return registry.New(path)
}

// inContext returns req routed as if the HTTP surface had stripped a
// /ctx/<name> prefix off its path, which is the only way a named context ever
// reaches the pipeline.
func inContext(req *http.Request, name string) *http.Request {
	return req.WithContext(registry.WithSelection(req.Context(),
		registry.Selection{Name: name, Explicit: true}))
}

// identityRoute is an inject route with no rewrite of any kind: the identity
// case D2 protects, pointed at a local stub.
func identityRoute(baseURL string) string {
	return fmt.Sprintf(`%q:{"base_url":%q,"auth":"inject","api_key_env_file":%q}`,
		echoAlias, baseURL, keyEnvName)
}

// writeKeyFile provisions an inject route's server-side secrets: it points the
// upstream key environment variable at a file holding key (returning that path)
// and, so a request carrying the canonical X-Patchbay-Key is accepted, also
// provisions a valid caller key file. A test that needs a bad or absent caller
// key overrides callerKeyEnvName after calling this.
func writeKeyFile(t *testing.T, key string) string {
	t.Helper()

	writeCallerKeyFile(t)

	path := filepath.Join(t.TempDir(), "api.key")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	t.Setenv(keyEnvName, path)

	return path
}

// writeCallerKeyFile points the caller key environment variable at a file
// holding the canonical caller secret, returning that path.
func writeCallerKeyFile(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "caller.key")
	if err := os.WriteFile(path, []byte(testCallerKey), 0o600); err != nil {
		t.Fatalf("write caller key file: %v", err)
	}

	t.Setenv(callerKeyEnvName, path)

	return path
}

// anthropicClient returns a client whose every connection lands on srv, so a
// request to the default route's https://api.anthropic.com never leaves the
// machine.
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

// stubRecorder collects the records the proxy appends. A recorder with started
// and release channels announces when an append is in flight and holds it there
// until the test lets go, which is how shutdown behavior gets pinned without a
// clock.
type stubRecorder struct {
	records chan ledger.Record
	err     error
	started chan struct{}
	release chan struct{}
}

// newRecorder returns a recorder that accepts records without failing.
func newRecorder() *stubRecorder {
	return &stubRecorder{records: make(chan ledger.Record, 8)}
}

// newBlockingRecorder returns a recorder whose Append announces itself on
// started and then blocks until release is closed.
func newBlockingRecorder() *stubRecorder {
	return &stubRecorder{
		records: make(chan ledger.Record, 8),
		started: make(chan struct{}, 8),
		release: make(chan struct{}),
	}
}

// Append satisfies proxy.Recorder.
func (s *stubRecorder) Append(rec ledger.Record) error {
	if s.started != nil {
		s.started <- struct{}{}
	}

	if s.release != nil {
		<-s.release
	}

	s.records <- rec

	return s.err
}

// wait returns the next appended record, failing if none arrives.
func (s *stubRecorder) wait(t *testing.T) ledger.Record {
	t.Helper()

	select {
	case rec := <-s.records:
		return rec
	case <-time.After(10 * time.Second):
		t.Fatal("no ledger record was appended")

		return ledger.Record{}
	}
}

// logSink is a slog handler that hands every record it is given to a channel,
// so a test can wait for a log line instead of racing a shared buffer.
type logSink struct {
	messages chan string
}

// newLogSink returns a sink wide enough to hold a request's warnings.
func newLogSink() *logSink {
	return &logSink{messages: make(chan string, callBuffer)}
}

// Enabled accepts every level, so nothing the pipeline logs is missed.
func (*logSink) Enabled(context.Context, slog.Level) bool {
	return true
}

// Handle records the message, dropping it rather than blocking if the sink is
// somehow full.
func (s *logSink) Handle(_ context.Context, rec slog.Record) error {
	select {
	case s.messages <- rec.Message:
	default:
	}

	return nil
}

// WithAttrs satisfies slog.Handler; the sink keeps only messages.
func (s *logSink) WithAttrs([]slog.Attr) slog.Handler {
	return s
}

// WithGroup satisfies slog.Handler; the sink keeps only messages.
func (s *logSink) WithGroup(string) slog.Handler {
	return s
}

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

// permissiveAccountant lets existing proxy tests exercise their own behavior
// without coupling to the durable-store assertions in accounting_test.go.
type permissiveAccountant struct{}

func (permissiveAccountant) InsertPending(context.Context, usage.PendingRequest) (int64, error) {
	return 1, nil
}

func (permissiveAccountant) MarkUpstreamDispatched(context.Context, int64, time.Time, string) error {
	return nil
}

func (permissiveAccountant) Finalize(context.Context, int64, usage.FinalizedRequest) error {
	return nil
}

func newProxy(t *testing.T, cfg proxy.Config) *proxy.Proxy {
	t.Helper()

	if cfg.Accountant == nil {
		cfg.Accountant = permissiveAccountant{}
	}

	pipeline, err := proxy.New(cfg)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}

	return pipeline
}

// serve runs one request through a proxy built from cfg and returns the
// recorder holding the response.
func serve(t *testing.T, cfg proxy.Config, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	newProxy(t, cfg).ServeHTTP(rec, req)

	return rec
}

// messagesRequest builds an inbound /v1/messages request carrying body. It
// presents the canonical caller secret in X-Patchbay-Key, matching the file
// writeKeyFile provisions, so an inject route accepts it.
func messagesRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", jsonType)
	req.Header.Set(callerKeyHeader, testCallerKey)

	return req
}

// decodeJSON decodes data into a generic value for order-independent
// comparison.
func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()

	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode json %q: %v", data, err)
	}

	return value
}

// hash returns the SHA-256 of data, so byte equality failures print short.
func hash(data []byte) string {
	sum := sha256.Sum256(data)

	return fmt.Sprintf("%x", sum)
}

// resolveRoute resolves model the way a bare, unprefixed request does.
func resolveRoute(t *testing.T, reg *registry.Registry, model string) registry.Route {
	t.Helper()

	res, err := reg.Resolve(registry.Selection{}, model)
	if err != nil {
		t.Fatalf("Resolve(%q) = %v; want no error", model, err)
	}

	return res.Route
}
