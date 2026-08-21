package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/registry"
)

const testRegistry = `{"default_context":"personal","seats":{` +
	`"anthropic":{"upstream":"https://api.anthropic.com","auth_mode":"forward"},` +
	`"luna":{"upstream":"https://openrouter.ai/api","model":"openai/gpt-5.6-luna","auth_mode":"inject","api_key_env_file":"KEY"}},` +
	`"contexts":{"personal":{"default_seat":"anthropic","models":{"luna-fast":"luna"}},` +
	`"work":{"default_seat":"anthropic","models":{}}}}`

const (
	// upstreamModels is the listing the stub Anthropic answers GET /v1/models
	// with: one model no Context binds, so a merged listing can be told from a
	// synthesized one.
	upstreamModels = `{"data":[{"type":"model","id":"claude-opus-5",` +
		`"display_name":"Claude Opus 5","created_at":"2026-01-01T00:00:00Z"}],"has_more":false}`

	// upstreamMessage is the finished turn the stub answers a proxied request
	// with. The caller must get these bytes back exactly.
	upstreamMessage = `{"type":"message","id":"msg_end_to_end","model":"claude-opus-5",` +
		`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":11,"output_tokens":3}}`

	// messagesRequest is the turn these tests send. It names a model because a
	// selected record has to: a request naming none produces a record the ledger
	// refuses, which would hide the wiring this test exists to prove.
	messagesRequest = `{"model":"claude-opus-5","max_tokens":16}`

	// waitDeadline bounds waiting for a log line that should already be coming.
	// It is a hang guard rather than a timing assumption: a gateway that never
	// starts must fail this test, not stall the suite behind it.
	waitDeadline = 10 * time.Second

	// lineBuffer sizes the recorded-log and observed-request channels, wide
	// enough for a startup and a handful of requests.
	lineBuffer = 128
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// boundAttr is one attribute bound into a logger, remembered together with the
// groups that were open when it was bound. slog renders such an attribute under
// that scope and no other, so a group opened later must not reach back and
// regroup it.
type boundAttr struct {
	groups []string
	attr   slog.Attr
}

// logRecorder is a slog handler that keeps every line with its attributes, so a
// test can wait for one and read what it carried — the bound port, above all.
//
// It is one handler's view of a shared channel: the attributes bound into this
// handler, each under the scope it was bound in, and the group stack its
// records' own attributes belong to. Both are immutable — WithAttrs and
// WithGroup hand back a new recorder over the same channel rather than editing
// this one — which is what keeps a derived logger from disturbing the logger it
// came from, and what stops the gateway's logger-bound attributes from being
// silently thrown away.
type logRecorder struct {
	lines  chan map[string]string
	attrs  []boundAttr
	groups []string
}

func newLogRecorder() *logRecorder {
	return &logRecorder{lines: make(chan map[string]string, lineBuffer)}
}
func (*logRecorder) Enabled(context.Context, slog.Level) bool { return true }

// Handle records one line, dropping it rather than blocking when the buffer is
// full. The gateway logs from its own goroutines — startup, a request being
// served, a shutdown — so a blocking send here would stop the process under
// test rather than the test.
//
// The attributes bound into this handler come first, each under the scope it was
// bound in, then the record's own under the scope it was written in.
func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]string{"message": r.Message}
	for _, bound := range l.attrs {
		recordAttr(attrs, bound.groups, bound.attr)
	}
	r.Attrs(func(a slog.Attr) bool { recordAttr(attrs, l.groups, a); return true })
	select {
	case l.lines <- attrs:
	default:
	}
	return nil
}

// WithAttrs binds attrs into a new recorder, stamped with the group scope they
// were bound under.
func (l *logRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return l
	}
	bound := make([]boundAttr, 0, len(l.attrs)+len(attrs))
	bound = append(bound, l.attrs...)
	for _, attr := range attrs {
		bound = append(bound, boundAttr{groups: l.groups, attr: attr})
	}
	return &logRecorder{lines: l.lines, attrs: bound, groups: l.groups}
}

// WithGroup opens a group in a new recorder. The stack is copied rather than
// appended to, so a recorder that derives twice does not hand both children the
// same backing array.
func (l *logRecorder) WithGroup(name string) slog.Handler {
	if name == "" {
		return l
	}
	groups := make([]string, len(l.groups)+1)
	copy(groups, l.groups)
	groups[len(l.groups)] = name
	return &logRecorder{lines: l.lines, attrs: l.attrs, groups: groups}
}

// recordAttr writes one attribute into a line under the key the groups it
// belongs to give it. A group-valued attribute is expanded into its members
// rather than kept as one opaque value — which is what leaves a test able to
// read a detail the gateway logged inside a group — and a group with an empty
// key opens no scope of its own, exactly as slog inlines such a group into the
// level around it.
func recordAttr(line map[string]string, groups []string, attr slog.Attr) {
	value := attr.Value.Resolve()
	if value.Kind() != slog.KindGroup {
		line[qualifiedKey(groups, attr.Key)] = value.String()
		return
	}
	nested := groups
	if attr.Key != "" {
		nested = make([]string, len(groups)+1)
		copy(nested, groups)
		nested[len(groups)] = attr.Key
	}
	for _, member := range value.Group() {
		recordAttr(line, nested, member)
	}
}

// qualifiedKey names an attribute by the groups it sits under, dotted the way
// slog's own handlers name one.
func qualifiedKey(groups []string, key string) string {
	if len(groups) == 0 {
		return key
	}
	return strings.Join(groups, ".") + "." + key
}

// waitFor returns the attributes of the first line logged with message, and
// fails naming the line that never came once the deadline passes. Ranging over
// the channel instead would wait forever on a gateway that stopped logging.
func (l *logRecorder) waitFor(t *testing.T, message string) map[string]string {
	t.Helper()
	deadline := time.After(waitDeadline)
	for {
		select {
		case line := <-l.lines:
			if line["message"] == message {
				return line
			}
		case <-deadline:
			t.Fatalf("log message %q never arrived within %s", message, waitDeadline)
			return nil
		}
	}
}

// stubEvent is one request as the stand-in upstream received it: the route it
// arrived on, and the exact bytes of its body. The body is what makes this a
// fidelity oracle in both directions — the response bytes prove what came back,
// and these prove what the caller's own document looked like by the time it got
// there.
type stubEvent struct {
	method string
	path   string
	body   string
}

// route is the method and path, for comparing a whole run of requests at once.
func (e stubEvent) route() string { return e.method + " " + e.path }

// stubUpstream is a local stand-in for Anthropic. Every request the gateway
// makes lands here, so nothing in these tests reaches the real network.
type stubUpstream struct {
	server *httptest.Server
	events chan stubEvent
}

// newStubUpstream starts the stand-in: a model listing on GET /v1/models, and
// one finished turn for everything else. Every request is recorded whole — the
// body included — before it is answered. Nothing here replays the body, because
// nothing here reads it: both answers are canned.
func newStubUpstream(t *testing.T) *stubUpstream {
	t.Helper()
	stub := &stubUpstream{events: make(chan stubEvent, lineBuffer)}
	stub.server = httptest.NewTLSServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("the stub upstream could not read the request body: %v", err)
			}
			stub.events <- stubEvent{
				method: r.Method,
				path:   r.URL.EscapedPath(),
				body:   string(body),
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
				io.WriteString(w, upstreamModels)
				return
			}
			io.WriteString(w, upstreamMessage)
		}),
	)
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

// seen drains every request the stub has received so far, in order. It is read
// after the responses are back, so nothing is still in flight.
func (u *stubUpstream) seen() []stubEvent {
	got := make([]stubEvent, 0, len(u.events))
	for len(u.events) > 0 {
		got = append(got, <-u.events)
	}
	return got
}

// routes reduces recorded events to the method-and-path list, so a whole run of
// requests can be compared in one assertion.
func routes(events []stubEvent) []string {
	got := make([]string, 0, len(events))
	for _, event := range events {
		got = append(got, event.route())
	}
	return got
}

// runningGateway is a live patchbay: where to reach it, what it logged, the
// client every request in this file goes through, and how to stop it.
type runningGateway struct {
	base   string
	logs   *logRecorder
	client *http.Client
	stop   func()
}

// startGateway runs the gateway on an ephemeral port through the subcommand a
// shell would type, learning the port it actually bound from its own startup
// log rather than guessing one.
//
// Every wait it hands back is bounded by waitDeadline: the startup log, each
// request the caller makes, and the shutdown. A gateway that binds and then
// stops answering, or one that never returns from cancellation, has to fail
// this test rather than hang the suite behind it.
func startGateway(t *testing.T, registryPath, ledgerDir string, client *http.Client) *runningGateway {
	t.Helper()
	logs := newLogRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() {
		done <- dispatch(ctx, []string{"serve"}, cli{
			stdout: io.Discard,
			stderr: io.Discard,
			lookupEnv: envMap(map[string]string{
				envListen:    "127.0.0.1:0",
				envRegistry:  registryPath,
				envLedgerDir: ledgerDir,
			}),
			client: client,
			logger: slog.New(logs),
		})
	}()

	addr := logs.waitFor(t, "patchbay listening")["listen"]
	if addr == "" {
		t.Fatal("the listening log named no address")
	}

	// One client for this gateway, so its idle connections are released with it
	// rather than left in the shared default transport's pool.
	caller := &http.Client{Timeout: waitDeadline}
	t.Cleanup(caller.CloseIdleConnections)

	return &runningGateway{
		base:   "http://" + addr,
		logs:   logs,
		client: caller,
		stop: func() {
			cancel()

			// Cancellation is a request to stop, not a guarantee of stopping. The
			// send on done is buffered, so the serve goroutine finishes whether or
			// not anyone is still listening, and giving up here cannot deadlock the
			// remaining cleanup.
			timeout := time.NewTimer(waitDeadline)
			defer timeout.Stop()

			select {
			case code := <-done:
				if code != exitOK {
					t.Errorf("serve exited %d; want %d on a clean shutdown", code, exitOK)
				}
			case <-timeout.C:
				t.Errorf("serve did not return within %s of cancellation", waitDeadline)
			}
		},
	}
}

// get performs a GET against the gateway's path and returns the status and body.
func (g *runningGateway) get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := g.client.Get(g.base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("read GET %s: %v", path, readErr)
	}
	return resp.StatusCode, string(body)
}

// post sends the Messages turn these tests share to the gateway's path and
// returns the status and body.
func (g *runningGateway) post(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := g.client.Post(
		g.base+path,
		"application/json",
		strings.NewReader(messagesRequest),
	)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("read POST %s: %v", path, readErr)
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

// TestGatewayServesEndToEnd runs the whole process through the serve
// subcommand: the listener it binds, the model listing it merges from the
// registry and the upstream, the Messages turns the pipeline carries with and
// without a Context prefix, the records they leave in a real ledger, the
// Context nothing defines being refused, and the same turn served again by a
// gateway whose ledger could not be opened at all.
func TestGatewayServesEndToEnd(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "routes.json")
	ledgerDir := filepath.Join(dir, "state", "ledger")
	writeFile(t, registryPath, testRegistry)

	stub := newStubUpstream(t)
	gw := startGateway(t, registryPath, ledgerDir, stub.client(t))

	// A record lands in the day file its own timestamp names, and that timestamp
	// is taken when the request starts. Bracketing the requests bounds which day
	// files can hold them to these two, so a run crossing UTC midnight is read
	// whole rather than half-missed.
	firstDay := utcDay()

	// The public binding is the gateway's own knowledge; the Claude model is the
	// upstream's. A caller asking for models gets both, bindings first — and
	// never the Seat IDs behind them, which are the registry's private
	// vocabulary.
	status, body := gw.get(t, "/v1/models")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d; want %d", status, http.StatusOK)
	}
	if got, want := listedIDs(t, body), []string{"luna-fast", "claude-opus-5"}; !slices.Equal(
		got,
		want,
	) {
		t.Fatalf("model ids = %v; want %v", got, want)
	}
	for _, seatID := range []string{"anthropic", "luna"} {
		if strings.Contains(body, `"`+seatID+`"`) {
			t.Fatalf("the listing published the private Seat ID %q: %s", seatID, body)
		}
	}

	// A request naming no Context rides the configured default_context's own
	// default Seat through the catch-all, and comes back as the bytes the
	// upstream wrote.
	status, body = gw.post(t, "/v1/messages")
	if status != http.StatusOK {
		t.Fatalf("POST /v1/messages status = %d; want %d", status, http.StatusOK)
	}
	if body != upstreamMessage {
		t.Fatalf("proxied body = %q; want the upstream's bytes %q", body, upstreamMessage)
	}

	// The same request under a Context prefix reaches the same upstream with the
	// prefix stripped.
	status, body = gw.post(t, "/ctx/work/v1/messages")
	if status != http.StatusOK {
		t.Fatalf("POST /ctx/work/v1/messages status = %d; want %d", status, http.StatusOK)
	}
	if body != upstreamMessage {
		t.Fatalf("context-prefixed body = %q; want the upstream's bytes %q", body, upstreamMessage)
	}

	events := stub.seen()
	want := []string{"GET /v1/models", "POST /v1/messages", "POST /v1/messages"}
	if got := routes(events); !slices.Equal(got, want) {
		t.Fatalf("the upstream saw %v; want %v, the prefix already stripped", got, want)
	}

	// Fidelity in the other direction. The responses above prove the upstream's
	// bytes reached the caller; these prove the caller's own request document
	// reached the upstream — on the bare path and the Context-prefixed one alike,
	// byte for byte, since the Seat carrying both rewrites nothing.
	//
	// The listing GET is the first event and is held to carrying no body at all,
	// so it can never be mistaken for one of the Messages turns below it.
	if listing := events[0]; listing.body != "" {
		t.Fatalf("the model-listing %s carried a body: %q", listing.route(), listing.body)
	}
	for _, turn := range events[1:] {
		if turn.body != messagesRequest {
			t.Fatalf(
				"the upstream received %q on %s; want the caller's own bytes %q",
				turn.body,
				turn.route(),
				messagesRequest,
			)
		}
	}

	// A Context nothing defines is refused outright, never served by another and
	// never carried upstream.
	status, body = gw.post(t, "/ctx/bogus/v1/messages")
	if status != http.StatusNotFound {
		t.Fatalf("POST /ctx/bogus/v1/messages status = %d; want %d", status, http.StatusNotFound)
	}
	if !strings.Contains(body, "not_found_error") || !strings.Contains(body, "unknown context") {
		t.Fatalf("unknown-context body = %q; want an Anthropic not_found_error naming the context", body)
	}
	if got := stub.seen(); len(got) != 0 {
		t.Fatalf("a refused Context reached the upstream: %v", got)
	}

	gw.stop()

	assertRecorded(t, ledgerDir, firstDay, utcDay())
	assertServesWithoutALedger(t, dir, registryPath, stub)
}

// utcDay is the ledger day file's own name for now: the UTC calendar day, which
// is what Append rotates on whatever zone the machine keeps.
func utcDay() string { return time.Now().UTC().Format(time.DateOnly) }

// assertRecorded checks what the served requests left behind: the whole ledger
// wiring — recorder, host name and drain — plus the v2 shapes the two record
// kinds are held to.
//
// firstDay and lastDay bracket the requests. They are the same day almost
// always and two adjacent days when the run crossed UTC midnight, and no record
// written by this test can land outside them — so the two named scans read every
// record there is, without reaching for a day nothing here wrote.
func assertRecorded(t *testing.T, ledgerDir, firstDay, lastDay string) {
	t.Helper()

	led, err := ledger.Open(ledgerDir)
	if err != nil {
		t.Fatalf("open ledger %s: %v", ledgerDir, err)
	}
	defer led.Close()

	days := []string{firstDay}
	if lastDay != firstDay {
		days = append(days, lastDay)
	}

	// recordedModelIs reports whether a record's nullable model is exactly want.
	// A nil model is one that was absent, which no string equals.
	recordedModelIs := func(value *string, want string) bool {
		return value != nil && *value == want
	}

	records := make([]ledger.Record, 0, 3)
	for _, day := range days {
		got, scanErr := led.Scan(day)
		if scanErr != nil {
			t.Fatalf("scan ledger day %s: %v", day, scanErr)
		}
		records = append(records, got...)
	}

	// Three: the two proxied turns, and the refusal the pipeline answered for a
	// Context that never resolved.
	if len(records) != 3 {
		t.Fatalf("ledger days %v hold %d records; want 3: %+v", days, len(records), records)
	}

	// Records are appended off the request path, so they are matched by the
	// Context that produced them rather than by arrival order.
	byContext := make(map[string]ledger.Record, len(records))
	for _, record := range records {
		if validateErr := record.Validate(); validateErr != nil {
			t.Fatalf("stored record is not a valid v2 record: %v (%+v)", validateErr, record)
		}
		byContext[record.Context] = record
	}

	for _, name := range []string{"personal", "work"} {
		record, ok := byContext[name]
		if !ok {
			t.Fatalf("no record names the context %q: %+v", name, records)
		}
		if record.Seat != "anthropic" {
			t.Errorf("%s record seat = %q; want the Context's default Seat", name, record.Seat)
		}
		if record.Selection == nil || record.Selection.Kind != ledger.SelectionDefault ||
			record.Selection.Model != nil {
			t.Errorf("%s record selection = %+v; want an unnamed default", name, record.Selection)
		}
		if record.Status != http.StatusOK || record.Upstream != "https://api.anthropic.com" {
			t.Errorf("%s record = %+v; want the default Seat's own answer", name, record)
		}
		if !recordedModelIs(record.Model, "claude-opus-5") ||
			!recordedModelIs(record.RequestModel, "claude-opus-5") {
			t.Errorf("%s record models = %+v/%+v", name, record.Model, record.RequestModel)
		}
		if record.AuthMode != "forward" || record.ConfigSource != "caller" {
			t.Errorf("%s record auth = %q/%q", name, record.AuthMode, record.ConfigSource)
		}
		if record.CredentialClass != ledger.CredentialNone {
			t.Errorf("%s record credential_class = %q", name, record.CredentialClass)
		}
		// The tee really read the upstream's body rather than counting bytes.
		if record.InputTokens != 11 || record.OutputTokens != 3 ||
			record.StopReason != "end_turn" {
			t.Errorf("%s record summary = %+v; want the upstream's own usage", name, record)
		}
	}

	refusal, ok := byContext["bogus"]
	if !ok {
		t.Fatalf("the refused Context left no record: %+v", records)
	}
	if refusal.Status != http.StatusNotFound || refusal.Selection != nil || refusal.Seat != "" {
		t.Errorf("refusal record = %+v; want a pre-selection record naming no Seat", refusal)
	}
}

// assertServesWithoutALedger runs the same request against a gateway whose
// ledger directory cannot be created, because a regular file sits where its
// parent belongs. The pipeline must skip recording rather than panic on a
// Recorder that is nil only underneath its interface, and must still shut down
// cleanly with nothing to drain or close.
func assertServesWithoutALedger(t *testing.T, dir, registryPath string, stub *stubUpstream) {
	t.Helper()

	blocker := filepath.Join(dir, "not-a-directory")
	writeFile(t, blocker, "in the way")

	gw := startGateway(t, registryPath, filepath.Join(blocker, "ledger"), stub.client(t))
	defer gw.stop()

	gw.logs.waitFor(t, "ledger unavailable; requests will not be recorded")

	status, body := gw.post(t, "/v1/messages")
	if status != http.StatusOK {
		t.Fatalf("POST /v1/messages without a ledger: status = %d; want %d", status, http.StatusOK)
	}
	if body != upstreamMessage {
		t.Fatalf("proxied body without a ledger = %q; want the upstream's bytes %q", body, upstreamMessage)
	}

	events := stub.seen()
	if got := routes(events); !slices.Equal(got, []string{"POST /v1/messages"}) {
		t.Fatalf("the upstream saw %v; want the one proxied turn", got)
	}
	if events[0].body != messagesRequest {
		t.Fatalf(
			"the upstream received %q without a ledger; want the caller's own bytes %q",
			events[0].body,
			messagesRequest,
		)
	}
}

func TestShippedExamplesUseStrictSeatRegistries(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		filepath.Join("..", "..", "examples", "routes.json"),
		filepath.Join("..", "..", "examples", "docker", "routes.json"),
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			if _, err := registry.New(path); err != nil {
				t.Fatalf("registry.New(%q) = %v", path, err)
			}
		})
	}
}

func TestServeSubcommandBindsOnlyAfterValidRegistry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	writeFile(t, path, testRegistry)
	gw := startGateway(t, path, filepath.Join(dir, "ledger"), nil)
	gw.stop()
}

// TestLogRecorderHonorsHandlerDerivation pins the recorder's own contract,
// because every assertion this file makes about what a running gateway logged is
// only as true as the handler underneath it. slog hands a handler its
// logger-bound attributes through WithAttrs and its group scope through
// WithGroup, and a recorder that answers either by returning itself throws both
// away — after which a gateway that stopped logging half of what it logs still
// looks correct here.
//
// The grouping is point-in-time: an attribute is recorded under the groups that
// were open when it was bound, so a group opened afterwards must not reach back
// and regroup it. A record's own attributes belong to the group stack in effect
// where it was written, a group-valued attribute keeps its members rather than
// collapsing into one opaque value, and every recorder derived from one shares
// that one's channel — which is what lets a test read what a logger three
// derivations down wrote.
func TestLogRecorderHonorsHandlerDerivation(t *testing.T) {
	t.Parallel()

	logs := newLogRecorder()
	logger := slog.New(logs).
		With(slog.String("version", "test")).
		WithGroup("request").
		With(
			slog.String("method", "GET"),
			slog.Group("route", slog.String("context", "personal"), slog.Int("seats", 2)),
		).
		WithGroup("upstream")

	logger.Info("derived", slog.String("listen", "127.0.0.1:8080"))

	// version stays unqualified because it was bound before any group opened;
	// method and route sit under request and not under upstream, which opened
	// after them; the record's own attribute sits under the whole stack.
	want := map[string]string{
		"message":                 "derived",
		"version":                 "test",
		"request.method":          "GET",
		"request.route.context":   "personal",
		"request.route.seats":     "2",
		"request.upstream.listen": "127.0.0.1:8080",
	}
	if got := logs.waitFor(t, "derived"); !maps.Equal(got, want) {
		t.Errorf("derived line = %v; want %v", got, want)
	}

	// A sibling derived from the same recorder writes into the same channel and
	// inherits nothing from the logger above: derivation copies a handler, it
	// does not edit the one it came from.
	slog.New(logs).With(slog.String("seat", "luna")).Info("sibling")

	sibling := map[string]string{"message": "sibling", "seat": "luna"}
	if got := logs.waitFor(t, "sibling"); !maps.Equal(got, sibling) {
		t.Errorf("sibling line = %v; want %v", got, sibling)
	}

	// And the recorder itself is undisturbed by everything derived from it.
	slog.New(logs).Info("bare")

	if got := logs.waitFor(t, "bare"); !maps.Equal(got, map[string]string{"message": "bare"}) {
		t.Errorf("bare line = %v; want only its message", got)
	}
}
