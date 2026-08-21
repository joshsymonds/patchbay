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
