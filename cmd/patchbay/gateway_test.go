package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
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

	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/ledger"
)

const (
	// aliasName is the one foreign alias the end-to-end registry defines.
	aliasName = "luna-fast"

	// lunaRegistry is a registry file with that alias in the personal context,
	// alongside a work context that keeps the built-in Anthropic default route.
	lunaRegistry = `{"contexts":{"personal":{"routes":{"` + aliasName +
		`":{"base_url":"https://openrouter.ai/api",` +
		`"model":"openai/gpt-5.6-luna","auth":"inject",` +
		`"api_key_env_file":"PATCHBAY_TEST_KEY_FILE"}}},` +
		`"work":{"routes":{}}}}`

	// upstreamModels is the listing the stub Anthropic answers with.
	upstreamModels = `{"data":[{"type":"model","id":"claude-opus-5",` +
		`"display_name":"Claude Opus 5","created_at":"2026-01-01T00:00:00Z"}],"has_more":false}`

	// upstreamMessage is the turn the stub Anthropic answers a proxied request
	// with. The caller must get these bytes back exactly.
	upstreamMessage = `{"type":"message","id":"msg_end_to_end","model":"claude-opus-5",` +
		`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":11,"output_tokens":3}}`

	// waitDeadline bounds waiting for a log line that should already be coming.
	waitDeadline = 10 * time.Second

	// lineBuffer sizes the recorded-log channel.
	lineBuffer = 64
)

// loggedLine is one slog record reduced to what a test asserts on.
type loggedLine struct {
	message string
	attrs   map[string]string
}

// logRecorder is a slog handler that keeps every line with its attributes, so a
// test can wait for one and read what it carried — the bound port, above all.
type logRecorder struct {
	lines chan loggedLine
}

// newLogRecorder returns a recorder wide enough for a startup's worth of lines.
func newLogRecorder() *logRecorder {
	return &logRecorder{lines: make(chan loggedLine, lineBuffer)}
}

// Enabled accepts every level, so nothing the gateway logs is missed.
func (*logRecorder) Enabled(context.Context, slog.Level) bool { return true }

// Handle records the line, dropping it rather than blocking if full.
func (l *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	line := loggedLine{message: rec.Message, attrs: make(map[string]string, rec.NumAttrs())}

	rec.Attrs(func(attr slog.Attr) bool {
		line.attrs[attr.Key] = attr.Value.String()

		return true
	})

	select {
	case l.lines <- line:
	default:
	}

	return nil
}

// WithAttrs satisfies slog.Handler; the recorder keeps lines as they are.
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }

// WithGroup satisfies slog.Handler; the recorder keeps lines as they are.
func (l *logRecorder) WithGroup(string) slog.Handler { return l }

// waitFor returns the attributes of the first line logged with message.
func (l *logRecorder) waitFor(t *testing.T, message string) map[string]string {
	t.Helper()

	deadline := time.After(waitDeadline)

	for {
		select {
		case line := <-l.lines:
			if line.message == message {
				return line.attrs
			}
		case <-deadline:
			t.Fatalf("log message %q never arrived", message)

			return nil
		}
	}
}

// stubUpstream is a local stand-in for Anthropic. Every request the gateway
// makes lands here, so nothing in these tests reaches the real network.
type stubUpstream struct {
	server *httptest.Server
	paths  chan string
}

// newStubUpstream starts the stand-in: a model listing on GET /v1/models, and
// one finished turn for everything else.
func newStubUpstream(t *testing.T) *stubUpstream {
	t.Helper()

	stub := &stubUpstream{paths: make(chan string, lineBuffer)}
	stub.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.paths <- r.Method + " " + r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			io.WriteString(w, upstreamModels)

			return
		}

		io.WriteString(w, upstreamMessage)
	}))
	t.Cleanup(stub.server.Close)

	return stub
}

// client returns a client whose every connection lands on the stub, whatever
// host the gateway thought it was addressing.
func (u *stubUpstream) client(t *testing.T) *http.Client {
	t.Helper()

	addr := u.server.Listener.Addr().String()
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

// sawPath reports whether the stub received a request for method and path.
func (u *stubUpstream) sawPath(want string) bool {
	for range len(u.paths) {
		if <-u.paths == want {
			return true
		}
	}

	return false
}

// gateway is a running patchbay: where to reach it, what it logged, and how to
// stop it.
type gateway struct {
	base string
	logs *logRecorder
	stop func()
}

// startGateway runs the gateway on an ephemeral port, learning the port it
// actually bound from its own startup log rather than guessing one.
func startGateway(t *testing.T, registryPath, ledgerDir string, client *http.Client) *gateway {
	t.Helper()

	logs := newLogRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)

	// Through the subcommand a shell would type, so the whole path from the
	// command line to the bound socket is what these tests exercise.
	go func() {
		done <- dispatch(ctx, []string{"serve"}, cli{
			stdout: io.Discard,
			stderr: io.Discard,
			lookupEnv: envMap(map[string]string{
				"PATCHBAY_LISTEN":     "127.0.0.1:0",
				"PATCHBAY_REGISTRY":   registryPath,
				"PATCHBAY_LEDGER_DIR": ledgerDir,
			}),
			client: client,
			logger: slog.New(logs),
		})
	}()

	addr := logs.waitFor(t, "patchbay listening")["listen"]

	return &gateway{
		base: "http://" + addr,
		logs: logs,
		stop: func() {
			cancel()

			if code := <-done; code != exitOK {
				t.Errorf("serve exited %d; want %d on a clean shutdown", code, exitOK)
			}
		},
	}
}

// writeFile writes contents at path.
func writeFile(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// getBody performs a GET and returns the status and body.
func getBody(t *testing.T, url string) (int, string) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}

	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("read GET %s: %v", url, readErr)
	}

	return resp.StatusCode, string(body)
}

// postBody performs an authenticated POST and returns the status and body.
func postBody(t *testing.T, url, payload string) (int, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("build POST %s: %v", url, err)
	}

	req.Header.Set("Authorization", "Bearer gateway-test-caller")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}

	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("read POST %s: %v", url, readErr)
	}

	return resp.StatusCode, string(body)
}

// listedIDs returns the id of every entry in a /v1/models body, in order.
func listedIDs(t *testing.T, body string) []string {
	t.Helper()

	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode model list %q: %v", body, err)
	}

	found := make([]string, 0, len(list.Data))
	for _, entry := range list.Data {
		found = append(found, entry.ID)
	}

	return found
}

// TestGatewayServesEndToEnd runs the whole process through run: the model
// endpoint answered from the registry, an arbitrary request carried to the
// upstream by the catch-all, the record that request leaves in the ledger, and
// the same request served again by a gateway whose ledger could not be opened
// at all.
func TestGatewayServesEndToEnd(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "routes.json")
	ledgerDir := filepath.Join(dir, "state", "ledger")

	writeFile(t, registryPath, lunaRegistry)

	stub := newStubUpstream(t)
	gw := startGateway(t, registryPath, ledgerDir, stub.client(t))

	// The alias is the gateway's own knowledge; the Claude model is the
	// upstream's. A caller asking for models gets both, aliases first.
	status, body := getBody(t, gw.base+"/v1/models")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d; want %d", status, http.StatusOK)
	}

	if got := listedIDs(t, body); len(got) != 2 || got[0] != aliasName || got[1] != "claude-opus-5" {
		t.Fatalf("model ids = %v; want [%s claude-opus-5]", got, aliasName)
	}

	// A request naming no model rides the built-in default route through the
	// catch-all, and comes back as the bytes the upstream wrote.
	status, body = postBody(t, gw.base+"/v1/messages", `{"max_tokens":16}`)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/messages status = %d; want %d", status, http.StatusOK)
	}

	if body != upstreamMessage {
		t.Fatalf("proxied body = %q; want the upstream's bytes %q", body, upstreamMessage)
	}

	if !stub.sawPath("POST /v1/messages") {
		t.Fatal("the upstream never saw the proxied request")
	}

	// The same request under a context prefix reaches the same upstream with the
	// prefix stripped, and is recorded against the context that served it.
	status, body = postBody(t, gw.base+"/ctx/work/v1/messages", `{"max_tokens":16}`)
	if status != http.StatusOK {
		t.Fatalf("POST /ctx/work/v1/messages status = %d; want %d", status, http.StatusOK)
	}

	if body != upstreamMessage {
		t.Fatalf("context-prefixed body = %q; want the upstream's bytes %q", body, upstreamMessage)
	}

	// The upstream saw the path with the context prefix already gone.
	if !stub.sawPath("POST /v1/messages") {
		t.Fatal("the upstream never saw the context-prefixed request at its stripped path")
	}

	// A context nothing defines is refused outright, never served by another.
	status, body = postBody(t, gw.base+"/ctx/bogus/v1/messages", `{"model":"claude-opus-5","max_tokens":16}`)
	if status != http.StatusNotFound {
		t.Fatalf("POST /ctx/bogus/v1/messages status = %d; want %d", status, http.StatusNotFound)
	}

	if !strings.Contains(body, "not_found_error") || !strings.Contains(body, "unknown context") {
		t.Fatalf("unknown-context body = %q; want an Anthropic not_found_error naming the context", body)
	}

	gw.stop()

	assertRecorded(t, ledgerDir)
	assertServesWithoutALedger(t, dir, registryPath, stub)
}

// assertRecorded checks that the proxied request left one valid record behind,
// which is the whole ledger wiring — recorder, host name and drain — in one
// assertion.
func assertRecorded(t *testing.T, ledgerDir string) {
	t.Helper()

	led, err := ledger.Open(ledgerDir)
	if err != nil {
		t.Fatalf("open ledger %s: %v", ledgerDir, err)
	}

	defer led.Close()

	records, scanErr := led.Scan(time.Now().UTC().Format(time.DateOnly))
	if scanErr != nil {
		t.Fatalf("scan ledger: %v", scanErr)
	}

	// Two records, not three: the unknown-context refusal is answered by the
	// HTTP surface before the pipeline, and the surface writes no records.
	if len(records) != 2 {
		t.Fatalf("ledger holds %d records; want the two proxied requests", len(records))
	}

	if records[0].Status != http.StatusOK || records[0].Upstream != "https://api.anthropic.com" {
		t.Fatalf("record = %+v; want the default route's answer", records[0])
	}

	wantContexts := []string{"personal", "work"}
	for i, want := range wantContexts {
		if records[i].Context != want {
			t.Fatalf("record %d context = %q; want %q", i, records[i].Context, want)
		}

		if records[i].CredentialClass != billing.CredentialOAuth {
			t.Fatalf("record %d credential_class = %q; want %q",
				i, records[i].CredentialClass, billing.CredentialOAuth)
		}
	}
}

// assertServesWithoutALedger runs the same request against a gateway whose
// ledger directory cannot be created. The pipeline must skip recording rather
// than panic on a Recorder that is nil only underneath its interface.
func assertServesWithoutALedger(t *testing.T, dir, registryPath string, stub *stubUpstream) {
	t.Helper()

	blocker := filepath.Join(dir, "not-a-directory")
	writeFile(t, blocker, "in the way")

	gw := startGateway(t, registryPath, filepath.Join(blocker, "ledger"), stub.client(t))
	defer gw.stop()

	status, body := postBody(t, gw.base+"/v1/messages", `{"max_tokens":16}`)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/messages without a ledger: status = %d; want %d", status, http.StatusOK)
	}

	if body != upstreamMessage {
		t.Fatalf("proxied body without a ledger = %q; want the upstream's bytes %q", body, upstreamMessage)
	}
}

// TestGatewayReportsABrokenRegistryAtStartup covers the other way a gateway
// starts diminished: a registry file it cannot read costs every alias, and
// nothing about serving requests would ever say so.
func TestGatewayReportsABrokenRegistryAtStartup(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "routes.json")

	writeFile(t, registryPath, `{"contexts":{"personal":{"routes":{"broken":{"auth":"nonsense"}}}}}`)

	stub := newStubUpstream(t)
	gw := startGateway(t, registryPath, filepath.Join(dir, "ledger"), stub.client(t))

	defer gw.stop()

	attrs := gw.logs.waitFor(t, "registry file is broken; foreign aliases are unavailable")
	if attrs["registry"] != registryPath {
		t.Fatalf("log registry = %q; want the file it could not read %q", attrs["registry"], registryPath)
	}

	// The aliases are gone, and the listing is still served.
	status, body := getBody(t, gw.base+"/v1/models")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d; want %d", status, http.StatusOK)
	}

	if got := listedIDs(t, body); len(got) != 1 || got[0] != "claude-opus-5" {
		t.Fatalf("model ids = %v; want the upstream alone", got)
	}
}
