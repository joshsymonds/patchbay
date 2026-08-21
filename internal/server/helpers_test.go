package server_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/server"
)

const (
	jsonType   = "application/json"
	callBuffer = 8
)

// upstreamCall is one request as the stub upstream received it. host is the
// destination the request was actually addressed to, which for every fetch this
// package makes must be Anthropic and nothing else.
type upstreamCall struct {
	method   string
	path     string
	escaped  string
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
	stub.server = httptest.NewTLSServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stub.calls <- upstreamCall{
				method:   r.Method,
				path:     r.URL.Path,
				escaped:  r.URL.EscapedPath(),
				rawQuery: r.URL.RawQuery,
				host:     r.Host,
				header:   r.Header.Clone(),
			}
			handle(w, r)
		}),
	)
	t.Cleanup(stub.server.Close)

	return stub
}

// call returns the single request the stub upstream received. A fetch that never
// happened is the failure this is for: the gateway answers some listings from
// its own knowledge alone, so a skipped upstream call still produces a response
// a test can read, and receiving from the channel bare would wait forever for a
// request nobody was going to make.
func (u *upstream) call(t *testing.T) upstreamCall {
	t.Helper()

	select {
	case got := <-u.calls:
		if extra := len(u.calls); extra != 0 {
			t.Fatalf("stub upstream received %d requests, want 1", extra+1)
		}

		return got
	case <-time.After(requestDeadline):
		t.Fatal("no request reached the stub upstream")

		return upstreamCall{}
	}
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

func newRegistryDocument(t *testing.T, body string) *registry.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(path)
	if err != nil {
		t.Fatalf("New() = %v\n%s", err, body)
	}
	return reg
}

// replaceRegistryFile rewrites path with body and stamps it at when, which is
// what a hot reload looks like from a running registry's side: new contents
// under a stamp it has not seen yet. The stamp is set explicitly rather than
// left to the clock, so a replacement is noticed whatever resolution the
// filesystem keeps modification times at.
func replaceRegistryFile(t *testing.T, path, body string, when time.Time) {
	t.Helper()

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
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
		escaped:  r.URL.EscapedPath(),
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

// modelMapFile writes a Bedrock model map and names it in the environment under
// envName, the same double indirection the pipeline reads it through.
func modelMapFile(t *testing.T, envName, body string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write model map: %v", err)
	}

	t.Setenv(envName, path)
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

// jsonHandler answers every request with status and body.
func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", jsonType)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// anthropicHost is the only host any fetch this package makes may address. The
// caller's own credentials ride those requests, so the destination is asserted
// rather than assumed.
const anthropicHost = "api.anthropic.com"

// upstreamList is a one-entry Anthropic listing carrying a created_at the
// gateway does not invent, so a relayed entry can be told from a synthesized one.
func upstreamList(id string) string {
	return `{"data":[{"type":"model","id":"` + id + `","display_name":"` + id +
		`","created_at":"2026-01-01T00:00:00Z"}],"has_more":false,"first_id":"` + id +
		`","last_id":"` + id + `"}`
}

// entryFields decodes one listing entry into a plain map, so a test can assert
// the entry's fields exactly rather than a subset of them.
func entryFields(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode model entry %q: %v", raw, err)
	}
	return fields
}

// unreadableCallerKey points the caller-key variable at a directory: the file is
// configured, and reading it fails, which is the server's own misconfiguration
// rather than the caller's refusal.
func unreadableCallerKey(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "caller-key-dir")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(proxy.CallerKeyEnvFile, path)
}

// keyedRequest builds an inbound GET presenting key in the caller-key header.
func keyedRequest(target, key string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if key != "" {
		req.Header.Set(proxy.CallerKeyHeader, key)
	}
	return req
}

// errUnreachableUpstream is the failure deadClient's transport answers with. It
// is named here rather than borrowed from the network so that "the upstream is
// unreachable" is a fact of the test, not of whatever this machine happens to
// have listening.
var errUnreachableUpstream = errors.New("upstream unreachable")

// deadClient returns a client whose every dial fails immediately, which is how
// "the upstream is unreachable" is produced without a socket, a timeout, or an
// assumption about the machine the suite runs on.
func deadClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, errUnreachableUpstream
			},
		},
	}
}

// quietLogger discards the warnings a degraded listing produces, so a test's
// output stays the test's own.
func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// operatorInvalidMessage is the stable line the gateway logs when the registry
// it is serving from will not validate. An operator greps for it; a caller never
// sees it.
const operatorInvalidMessage = "registry invalid; requests fail closed"

// logEntry is one record as the sink captured it: the level and message an
// operator reads, and the attributes rendered as the text they are read as.
type logEntry struct {
	level   slog.Level
	message string
	attrs   string
}

// text renders one record whole, for a failure message that has to show what was
// actually logged.
func (e logEntry) text() string {
	return e.level.String() + " " + e.message + " " + e.attrs
}

// operatorVisible reports whether a record is loud enough for an operator to
// see in a gateway's ordinary logs.
func (e logEntry) operatorVisible() bool { return e.level >= slog.LevelWarn }

// logStore is where every handler derived from one sink puts its records. It is
// held by pointer and never copied, because the lock that guards it is what lets
// the gateway log from whatever goroutine served the request. The records are
// already there when that request returns, so nothing here waits and nothing
// sleeps.
type logStore struct {
	mu      sync.Mutex
	entries []logEntry
}

// add appends one captured record.
func (l *logStore) add(entry logEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.entries = append(l.entries, entry)
}

// matching returns every captured record carrying exactly this message, in the
// order they were logged.
func (l *logStore) matching(message string) []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()

	found := make([]logEntry, 0, len(l.entries))

	for _, entry := range l.entries {
		if entry.message == message {
			found = append(found, entry)
		}
	}

	return found
}

// String renders every captured record, so a count that came out wrong can say
// what the gateway logged instead.
func (l *logStore) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out strings.Builder

	for _, entry := range l.entries {
		out.WriteString(entry.text())
		out.WriteString("\n")
	}

	return out.String()
}

// boundAttr is one attribute bound into a logger, remembered together with the
// groups that were open when it was bound. slog renders such an attribute under
// that scope and no other, so a group opened later must not reach back and
// regroup it.
type boundAttr struct {
	groups []string
	attr   slog.Attr
}

// logSink captures what a gateway logs so a test can assert on it directly. It
// is one handler's view of a shared store: the attributes bound into this
// handler and the group stack its records' own attributes belong to. Both are
// immutable — WithAttrs and WithGroup hand back a new sink over the same store
// rather than editing this one — which is what keeps a derived logger from
// disturbing the logger it came from.
type logSink struct {
	*logStore

	attrs  []boundAttr
	groups []string
}

func newLogSink() *logSink { return &logSink{logStore: &logStore{}} }

// logger returns the logger a Config hands the gateway.
func (s *logSink) logger() *slog.Logger { return slog.New(s) }

// Enabled captures every level, since a test asserting which level a record
// carries has to be handed the quiet ones too.
func (s *logSink) Enabled(context.Context, slog.Level) bool { return true }

// Handle captures one record: the attributes bound into this handler first, each
// under the scope it was bound in, then the record's own under the scope it was
// written in.
func (s *logSink) Handle(_ context.Context, record slog.Record) error {
	var attrs strings.Builder

	for _, bound := range s.attrs {
		writeAttr(&attrs, bound.groups, bound.attr)
	}

	record.Attrs(func(attr slog.Attr) bool {
		writeAttr(&attrs, s.groups, attr)

		return true
	})

	s.add(logEntry{
		level:   record.Level,
		message: record.Message,
		attrs:   attrs.String(),
	})

	return nil
}

// WithAttrs binds attrs into a new sink, stamped with the group scope they were
// bound under.
func (s *logSink) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return s
	}

	bound := make([]boundAttr, 0, len(s.attrs)+len(attrs))
	bound = append(bound, s.attrs...)

	for _, attr := range attrs {
		bound = append(bound, boundAttr{groups: s.groups, attr: attr})
	}

	return &logSink{logStore: s.logStore, attrs: bound, groups: s.groups}
}

// WithGroup opens a group in a new sink. The stack is copied rather than
// appended to, so a sink that derives twice does not hand both children the same
// backing array.
func (s *logSink) WithGroup(name string) slog.Handler {
	if name == "" {
		return s
	}

	groups := make([]string, len(s.groups)+1)
	copy(groups, s.groups)
	groups[len(s.groups)] = name

	return &logSink{logStore: s.logStore, attrs: s.attrs, groups: groups}
}

// writeAttr renders one attribute in the "key=value " form this sink has always
// used, qualified by the groups it belongs to. A group-valued attribute is
// expanded into its members rather than printed as one opaque value — which is
// what keeps a test able to read a detail the gateway logged inside a group —
// and a group with an empty key opens no scope of its own, exactly as slog
// inlines such a group into the level around it.
func writeAttr(out *strings.Builder, groups []string, attr slog.Attr) {
	value := attr.Value.Resolve()

	if value.Kind() != slog.KindGroup {
		fmt.Fprintf(out, "%s=%v ", qualify(groups, attr.Key), value)

		return
	}

	nested := groups
	if attr.Key != "" {
		nested = make([]string, len(groups)+1)
		copy(nested, groups)
		nested[len(groups)] = attr.Key
	}

	for _, member := range value.Group() {
		writeAttr(out, nested, member)
	}
}

// qualify names an attribute by the groups it sits under, dotted the way slog's
// own text handler names one.
func qualify(groups []string, key string) string {
	if len(groups) == 0 {
		return key
	}

	return strings.Join(groups, ".") + "." + key
}

// TestLogSinkHonorsHandlerDerivation pins the sink's own contract, because every
// assertion this package makes about what a gateway logged is only as true as
// the handler underneath it. slog hands a handler its logger-bound attributes
// through WithAttrs and its group scope through WithGroup, and a sink that
// answers either by returning itself silently throws both away — after which a
// gateway that stopped logging half of what it logs still looks correct here.
//
// The grouping is point-in-time: an attribute is rendered under the groups that
// were open when it was bound, so a WithGroup opened afterwards must not reach
// back and regroup it. A record's own attributes belong to the group stack in
// effect where it was written. And every handler derived from one sink captures
// into that sink, which is what lets a test read what a logger three derivations
// down wrote.
func TestLogSinkHonorsHandlerDerivation(t *testing.T) {
	t.Parallel()

	sink := newLogSink()
	logger := sink.logger().
		With(slog.String("host", "testhost")).
		WithGroup("request").
		With(
			slog.String("method", "GET"),
			slog.Group("route", slog.String("context", "personal"), slog.Int("seats", 2)),
		).
		WithGroup("upstream")

	logger.Warn("gateway error", slog.Int("status", 502), slog.String("host", "api.anthropic.com"))

	got := sink.matching("gateway error")
	if len(got) != 1 {
		t.Fatalf("sink holds %d %q entries; want 1\n%s", len(got), "gateway error", sink)
	}

	// host stays ungrouped because it was bound before any group opened; method
	// and route sit under request and not under upstream, which opened after
	// them; the record's own attrs sit under the whole stack.
	const want = "host=testhost request.method=GET request.route.context=personal " +
		"request.route.seats=2 request.upstream.status=502 " +
		"request.upstream.host=api.anthropic.com "

	if got[0].attrs != want {
		t.Errorf("attrs = %q; want %q", got[0].attrs, want)
	}
	if !got[0].operatorVisible() {
		t.Errorf("entry %q is quieter than a warning", got[0].text())
	}

	// A sibling derived from the same sink captures into it too, and inherits
	// nothing from the logger above: derivation copies a handler, it does not
	// edit the one it came from.
	sink.logger().With(slog.String("seat", "luna")).Info("other", slog.String("model", "luna-fast"))

	if sibling := sink.matching("other"); len(sibling) != 1 {
		t.Fatalf("sink holds %d %q entries; want 1\n%s", len(sibling), "other", sink)
	} else if sibling[0].attrs != "seat=luna model=luna-fast " {
		t.Errorf("sibling attrs = %q; want %q", sibling[0].attrs, "seat=luna model=luna-fast ")
	}

	// The original logger is undisturbed by everything derived from it.
	sink.logger().Warn("bare")

	if bare := sink.matching("bare"); len(bare) != 1 {
		t.Fatalf("sink holds %d %q entries; want 1\n%s", len(bare), "bare", sink)
	} else if bare[0].attrs != "" {
		t.Errorf("bare attrs = %q; want none", bare[0].attrs)
	}
}
