package server_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/server"
)

// requestDeadline bounds a single upstream call made from a test. Every
// upstream these tests reach is a local stub that answers at once, so this is a
// hang guard rather than a timing assumption: a request that somehow escapes the
// stub must fail the test rather than stall the suite behind it.
const requestDeadline = 10 * time.Second

func twoContextRegistry(defaultContext string) string {
	return `{"default_context":"` + defaultContext + `","seats":{` +
		`"anthropic":{"upstream":"https://api.anthropic.com","auth_mode":"forward"},` +
		`"luna":{"upstream":"https://openrouter.ai/api","model":"openai/gpt-5.6-luna","auth_mode":"inject","api_key_env_file":"KEY"}},` +
		`"contexts":{"personal":{"default_seat":"anthropic","models":{"luna-fast":"luna"}},` +
		`"work":{"default_seat":"anthropic","models":{}}}}`
}

func TestBarePathUsesConfiguredDefaultContext(t *testing.T) {
	t.Parallel()
	seen := make(chan string, 1)
	pipeline := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res, ok := registry.ResolutionFrom(r.Context())
		if !ok {
			t.Fatal("missing resolution")
		}
		seen <- res.Context
		w.WriteHeader(http.StatusNoContent)
	})
	rec := serve(
		t,
		server.Config{
			Proxy:    pipeline,
			Registry: newRegistryDocument(t, twoContextRegistry("work")),
		},
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"x"}`)),
	)
	if rec.Code != http.StatusNoContent || <-seen != "work" {
		t.Fatalf("status/context = %d", rec.Code)
	}
}

func TestContextPrefixStripsBeforeProxyIncludingUnknownContexts(t *testing.T) {
	t.Parallel()
	pipeline := newSentinel()
	reg := newRegistryDocument(t, twoContextRegistry("personal"))
	rec := serve(
		t,
		server.Config{Proxy: pipeline, Registry: reg},
		httptest.NewRequest(http.MethodPost, "/ctx/work/v1/messages?q=1", nil),
	)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status=%d", rec.Code)
	}
	call := pipeline.call(t)
	if call.path != "/v1/messages" || call.rawQuery != "q=1" {
		t.Fatalf("call=%+v", call)
	}

	pipeline = newSentinel()
	rec = serve(
		t,
		server.Config{Proxy: pipeline, Registry: reg},
		httptest.NewRequest(http.MethodPost, "/ctx/missing/v1/messages", nil),
	)
	if rec.Code != http.StatusTeapot || pipeline.hits() != 1 {
		t.Fatalf("unknown=%d %s", rec.Code, rec.Body.String())
	}
	if call := pipeline.call(t); call.path != "/v1/messages" {
		t.Fatalf("unknown call=%+v", call)
	}
}

// TestContextFromURLReadsTheEscapedPathTheGatewayRoutesOn pins the one splitter
// the CLI and the gateway share. It takes a URL rather than a string because the
// decoded Path and the raw RawPath are two different answers to "which context
// is this", and a caller handing over only the decoded half would diagnose a
// base URL the gateway will refuse as one it serves.
func TestContextFromURLReadsTheEscapedPathTheGatewayRoutesOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, target, want string
	}{
		{name: "no prefix", target: "http://127.0.0.1:4100/v1/messages", want: "personal"},
		{name: "named context", target: "http://127.0.0.1:4100/ctx/work/v1/messages", want: "work"},
		{name: "bare prefix takes the next segment", target: "http://127.0.0.1:4100/ctx/v1/messages", want: "v1"},
		{name: "prefix naming nothing", target: "http://127.0.0.1:4100/ctx//v1/messages", want: ""},
		{
			name:   "escaped separator selects no context",
			target: "http://127.0.0.1:4100/ctx/work%2Feu/v1/messages",
			want:   "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := url.Parse(tc.target)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.target, err)
			}
			if got := server.ContextFromURL(parsed, "personal"); got != tc.want {
				t.Fatalf("ContextFromURL(%q) = %q; want %q", tc.target, got, tc.want)
			}
		})
	}
}

// TestEscapedContextSeparatorFailsClosedOnTheWire is the other half of that
// claim: the gateway itself refuses the escaped form, so the helper above and
// the running gateway agree about what the path means.
//
// It runs the real proxy rather than a sentinel. A sentinel answers whatever it
// is built to answer and never reads the resolution error the surface attached,
// so it can only prove which path was handed over — the refusal itself, the
// status it carries and the upstream it never reached, are the pipeline's own
// and have to come from the pipeline.
func TestEscapedContextSeparatorFailsClosedOnTheWire(t *testing.T) {
	t.Parallel()

	spy := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message"}`))
	client := anthropicClient(t, spy)
	client.Timeout = requestDeadline

	reg := newRegistryDocument(t, twoContextRegistry("personal"))
	pipeline := proxy.New(
		proxy.Config{Registry: reg, Client: client, Logger: quietLogger(), Host: "testhost"},
	)

	rec := serve(
		t,
		server.Config{Proxy: pipeline, Registry: reg, Logger: quietLogger()},
		httptest.NewRequest(
			http.MethodPost,
			"/ctx/work%2Feu/v1/messages",
			strings.NewReader(`{"model":"claude-opus-5"}`),
		),
	)

	// Refused as a context nothing serves — never served as "work", and never
	// answered by the default context either.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want %d\nbody=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}

	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode refusal %q: %v", rec.Body.String(), err)
	}
	if envelope.Type != "error" || envelope.Error.Type != "not_found_error" {
		t.Fatalf("refusal envelope = %+v; want an Anthropic not_found_error", envelope)
	}
	if !strings.Contains(envelope.Error.Message, "unknown context") {
		t.Fatalf(
			"refusal message = %q; want it to name the context as one nothing serves",
			envelope.Error.Message,
		)
	}

	// Nothing was spent on it: a context that did not resolve has no Seat, so
	// there was no upstream to carry it to.
	if hits := len(spy.calls); hits != 0 {
		t.Fatalf("a refused context reached the upstream %d times", hits)
	}
}

func TestHotInvalidRegistryReturns503BeforeAllContextLookup(t *testing.T) {
	reg := newRegistryDocument(t, twoContextRegistry("personal"))
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(reg.Path(), base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reg.Path(), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(reg.Path(), base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{"/v1/models", "/ctx/missing/v1/models", "/ctx/missing/_patchbay/registry"} {
		pipeline := newSentinel()
		rec := serve(
			t,
			server.Config{Proxy: pipeline, Registry: reg},
			httptest.NewRequest(http.MethodGet, target, nil),
		)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s status=%d body=%s", target, rec.Code, rec.Body.String())
		}
		if pipeline.hits() != 0 {
			t.Errorf("%s reached proxy", target)
		}
		var envelope struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Error.Type != "api_error" {
			t.Errorf("%s type=%q", target, envelope.Error.Type)
		}
	}
	if !errors.Is(reg.Err(), registry.ErrInvalid) {
		t.Fatalf("Err=%v", reg.Err())
	}
}

// TestHotInvalidRegistryTellsTheOperatorWhatTheCallerIsNotTold pins the two
// audiences of a hot-reload failure at the HTTP surface. The caller gets the
// sentinel and nothing else — not the value the rejected file named, not the
// path it was read from — in the response and in what the ledger keeps of it,
// while the gateway's own log carries both.
//
// It carries them once per failure rather than once per request: a registry that
// will not validate refuses everything that arrives while it stays that way, and
// repeating the same detail per refusal buries the one line an operator needs
// under the traffic it stopped. A file that is repaired and then breaks the same
// way again is news a second time, so a valid resolution forgets what was
// reported.
func TestHotInvalidRegistryTellsTheOperatorWhatTheCallerIsNotTold(t *testing.T) {
	reg := newRegistryDocument(t, twoContextRegistry("personal"))
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(reg.Path(), base, base); err != nil {
		t.Fatal(err)
	}

	sink := newLogSink()
	records := make(chan ledger.Record, callBuffer)
	pipeline := proxy.New(proxy.Config{
		Registry: reg,
		Client:   deadClient(),
		Recorder: ledgerRecorder(records),
		Logger:   quietLogger(),
		Host:     "testhost",
	})
	// One handler across every request below, because the gateway's memory of
	// what it has already reported is the thing under test.
	handler := server.Handler(server.Config{
		Proxy:    pipeline,
		Registry: reg,
		Client:   deadClient(),
		Logger:   sink.logger(),
	})

	ask := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader(body)))

		return rec
	}
	refused := func(rec *httptest.ResponseRecorder) {
		t.Helper()

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d; want 503\nbody=%s", rec.Code, rec.Body.String())
		}

		if strings.Contains(rec.Body.String(), rejectedValue) ||
			strings.Contains(rec.Body.String(), reg.Path()) {
			t.Fatalf("a refusal republished the rejected file: %s", rec.Body.String())
		}
	}
	reported := func(want int) []logEntry {
		t.Helper()

		got := sink.matching(operatorInvalidMessage)
		if len(got) != want {
			t.Fatalf("operator log holds %d %q entries; want %d\n%s",
				len(got), operatorInvalidMessage, want, sink)
		}

		return got
	}

	replaceRegistryFile(t, reg.Path(), secretRegistry, base.Add(time.Hour))

	refused(ask(http.MethodPost, "/v1/messages", `{"model":"claude-opus-5"}`))

	entry := reported(1)[0]
	if !entry.operatorVisible() {
		t.Errorf("operator log %q is quieter than a warning", entry.text())
	}
	for _, detail := range []string{reg.Path(), rejectedValue, "api_key_env_file"} {
		if !strings.Contains(entry.attrs, detail) {
			t.Errorf("operator log %q keeps no %q", entry.text(), detail)
		}
	}

	select {
	case got := <-records:
		if got.Status != http.StatusServiceUnavailable {
			t.Errorf("ledger record status = %d; want 503", got.Status)
		}
		if strings.Contains(got.ErrorBody, rejectedValue) ||
			strings.Contains(got.ErrorBody, reg.Path()) {
			t.Errorf("the ledger persisted the rejected file's detail: %q", got.ErrorBody)
		}
	case <-time.After(requestDeadline):
		t.Fatal("no ledger record was appended for the refused request")
	}

	// The same invalid registry, refusing again: the caller is told the same
	// thing and the operator is told nothing new.
	refused(ask(http.MethodPost, "/v1/messages", `{"model":"claude-opus-5"}`))
	reported(1)

	// Repaired. The listing degrades to this context's own bindings because the
	// upstream is unreachable, which is a resolution that succeeded.
	replaceRegistryFile(t, reg.Path(), twoContextRegistry("personal"), base.Add(2*time.Hour))

	repaired := ask(http.MethodGet, "/v1/models", "")
	if repaired.Code != http.StatusOK {
		t.Fatalf("repaired status = %d; want 200\nbody=%s", repaired.Code, repaired.Body.String())
	}
	if got := ids(t, decodeList(t, repaired.Body.Bytes())); !reflect.DeepEqual(got, []string{"luna-fast"}) {
		t.Fatalf("repaired ids = %v; want the context's own bindings", got)
	}
	reported(1)

	// A context this valid registry does not define is refused too, and is not the
	// same news at all: nothing is wrong with the file, so nothing is reported.
	if unknown := ask(http.MethodGet, "/ctx/missing/v1/models", ""); unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown context status = %d; want 404\nbody=%s", unknown.Code, unknown.Body.String())
	}
	reported(1)

	// Broken the same way again. Identical detail, but the failure in front of it
	// was resolved, so this one is reported rather than mistaken for the old one.
	replaceRegistryFile(t, reg.Path(), secretRegistry, base.Add(3*time.Hour))

	refused(ask(http.MethodPost, "/v1/messages", `{"model":"claude-opus-5"}`))
	reported(2)

	// Broken a different way, with no valid moment in between: a failure an
	// operator has not been told about is reported however recently another one
	// was.
	const secondRejectedValue = "sk-live-successor"

	replaceRegistryFile(
		t,
		reg.Path(),
		strings.Replace(secretRegistry, rejectedValue, secondRejectedValue, 1),
		base.Add(4*time.Hour),
	)

	refused(ask(http.MethodPost, "/v1/messages", `{"model":"claude-opus-5"}`))

	if successor := reported(3)[2]; !strings.Contains(successor.attrs, secondRejectedValue) {
		t.Errorf("operator log %q keeps no %q", successor.text(), secondRejectedValue)
	}

	// Repaired again, and this time the only request that sees the repair names a
	// context nothing defines. That 404 was produced by reading a document the
	// registry validated, which is the same evidence a resolution is: the file is
	// well. So the failure that returns after it is one nobody has been told
	// about, even though it is the failure that was reported a moment ago.
	replaceRegistryFile(t, reg.Path(), twoContextRegistry("personal"), base.Add(5*time.Hour))

	if missing := ask(http.MethodGet, "/ctx/missing/v1/models", ""); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown context status = %d; want 404\nbody=%s", missing.Code, missing.Body.String())
	}
	reported(3)

	replaceRegistryFile(
		t,
		reg.Path(),
		strings.Replace(secretRegistry, rejectedValue, secondRejectedValue, 1),
		base.Add(6*time.Hour),
	)

	refused(ask(http.MethodPost, "/v1/messages", `{"model":"claude-opus-5"}`))
	reported(4)
}

func TestProxyBoundContextFailuresProducePreselectionLedgerRecords(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		invalidate  bool
		wantStatus  int
		wantContext string
	}{
		{
			name:       "invalid registry",
			target:     "/v1/messages",
			invalidate: true,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:        "unknown context",
			target:      "/ctx/missing/v1/messages",
			wantStatus:  http.StatusNotFound,
			wantContext: "missing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := newRegistryDocument(t, twoContextRegistry("personal"))
			if tc.invalidate {
				base := time.Now().Add(-time.Hour).Truncate(time.Second)
				if err := os.Chtimes(reg.Path(), base, base); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(reg.Path(), []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(
					reg.Path(),
					base.Add(time.Hour),
					base.Add(time.Hour),
				); err != nil {
					t.Fatal(err)
				}
			}

			records := make(chan ledger.Record, 1)
			pipeline := proxy.New(
				proxy.Config{Registry: reg, Recorder: ledgerRecorder(records), Host: "testhost"},
			)
			rec := serve(
				t,
				server.Config{Proxy: pipeline, Registry: reg},
				httptest.NewRequest(
					http.MethodPost,
					tc.target,
					strings.NewReader(`{"model":"claude-opus-5"}`),
				),
			)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}

			select {
			case got := <-records:
				if got.Context != tc.wantContext || got.Status != tc.wantStatus {
					t.Fatalf("record context/status=%q/%d", got.Context, got.Status)
				}
				if got.Selection != nil || got.Seat != "" || got.Upstream != "" || got.Model != nil {
					t.Fatalf("preselection routing fields populated: %+v", got)
				}
				if err := got.Validate(); err != nil {
					t.Fatalf("record invalid: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("no ledger record was appended")
			}
		})
	}
}

type ledgerRecorder chan<- ledger.Record

func (r ledgerRecorder) Append(rec ledger.Record) error {
	r <- rec
	return nil
}

// utcDay is the ledger day file's own name for now: the UTC calendar day, which
// is what Append rotates on whatever zone the machine keeps.
func utcDay() string { return time.Now().UTC().Format(time.DateOnly) }

// TestInvalidContextNamesReachTheRealLedger pins the pre-selection record end to
// end against a real ledger rather than a stub that accepts anything. A path
// segment is caller-supplied text, so the context a refused request names is
// routinely not a configured ID — and that request is exactly the one an
// operator needs to find in the ledger afterwards.
func TestInvalidContextNamesReachTheRealLedger(t *testing.T) {
	dir := t.TempDir()
	led, err := ledger.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := led.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})

	reg := newRegistryDocument(t, twoContextRegistry("personal"))
	// The last name is one byte past the bound the record's Context field can
	// represent. A path segment has no length limit, so this is an ordinary thing
	// for a caller to send — and the request behind it is exactly as worth
	// recording as the others, which is why the name is what gets dropped rather
	// than the record.
	oversized := strings.Repeat("a", 257)
	names := []string{"Bad_Name", "UPPER", "not-a-real-context", oversized}

	pipeline := proxy.New(proxy.Config{Registry: reg, Recorder: led, Host: "testhost"})

	// A record lands in the day file its own timestamp names, and that timestamp
	// is taken when the request starts. Bracketing the requests bounds which day
	// files can hold them to these two, so a run crossing UTC midnight is read
	// whole rather than half-missed.
	firstDay := utcDay()

	for _, name := range names {
		rec := serve(
			t,
			server.Config{Proxy: pipeline, Registry: reg},
			httptest.NewRequest(
				http.MethodPost,
				"/ctx/"+name+"/v1/messages",
				strings.NewReader(`{"model":"claude-opus-5"}`),
			),
		)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d body=%s", name, rec.Code, rec.Body.String())
		}
	}

	pipeline.Drain(t.Context())

	// The drain is the last moment a record can be written, so firstDay and the
	// day it finished on bracket every one of them: the same day almost always,
	// two adjacent days when the run crossed midnight, and never a day nothing
	// here wrote.
	days := []string{firstDay}
	if lastDay := utcDay(); lastDay != firstDay {
		days = append(days, lastDay)
	}

	records := make([]ledger.Record, 0, len(names))
	for _, day := range days {
		got, scanErr := led.Scan(day)
		if scanErr != nil {
			t.Fatalf("scan ledger day %s: %v", day, scanErr)
		}
		records = append(records, got...)
	}
	if len(records) != len(names) {
		t.Fatalf(
			"ledger days %v hold %d records, want %d: %+v",
			days,
			len(records),
			len(names),
			records,
		)
	}
	got := make(map[string]bool, len(records))
	for _, record := range records {
		if record.Selection != nil || record.Seat != "" {
			t.Errorf("pre-selection record carries selection fields: %+v", record)
		}
		if record.Status != http.StatusNotFound {
			t.Errorf("pre-selection record status = %d; want 404: %+v", record.Status, record)
		}
		if record.CredentialClass != ledger.CredentialNone {
			t.Errorf("pre-selection record credential_class = %q; want %q",
				record.CredentialClass, ledger.CredentialNone)
		}
		if record.ErrorBody == "" || !strings.Contains(record.ErrorBody, "not_found_error") {
			t.Errorf("pre-selection record error_body = %q; want the refusal patchbay answered",
				record.ErrorBody)
		}
		// An unknown Context is refused before the body is read at all, so there
		// is no parsed request model — and absence is spelled by omitting the key,
		// never by a literal standing in for one.
		if record.RequestModel != nil {
			t.Errorf("pre-selection record request_model = %q; want it absent, since the "+
				"refusal happened before the body was parsed", *record.RequestModel)
		}
		got[record.Context] = true
	}
	for _, name := range names[:len(names)-1] {
		if !got[name] {
			t.Errorf("no ledger record kept the requested context %q", name)
		}
	}

	// The name the record could not hold is the only thing lost. The request it
	// named is still there, under an omitted Context — a dropped record would
	// have made the whole refusal invisible to the operator looking for it.
	if got[oversized] {
		t.Errorf("a ledger record stored an unrepresentable requested context")
	}
	if !got[""] {
		t.Errorf("the oversized requested context cost the whole record: %+v", records)
	}
}

func TestEverythingExceptGETModelsAndIntrospectionRidesProxy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ method, target string }{{http.MethodPost, "/v1/messages"}, {http.MethodPost, "/v1/models"}, {http.MethodDelete, "/v1/models/x"}, {http.MethodGet, "/other"}} {
		pipeline := newSentinel()
		rec := serve(
			t,
			server.Config{
				Proxy:    pipeline,
				Registry: newRegistryDocument(t, twoContextRegistry("personal")),
			},
			httptest.NewRequest(tc.method, tc.target, nil),
		)
		if rec.Code != http.StatusTeapot || pipeline.hits() != 1 {
			t.Errorf("%s %s status=%d hits=%d", tc.method, tc.target, rec.Code, pipeline.hits())
		}
	}
}

// TestModelEndpointsNeverReachTheProxy pins the two paths the gateway answers
// itself. Answering them locally is the whole point: a listing merged from the
// registry cannot be produced by forwarding.
func TestModelEndpointsNeverReachTheProxy(t *testing.T) {
	callerKeyFile(t)
	for _, target := range []string{"/v1/models", "/v1/models/luna-fast", "/ctx/work/v1/models"} {
		pipeline := newSentinel()
		reg := newRegistryDocument(t, twoContextRegistry("personal"))
		serve(
			t,
			server.Config{Proxy: pipeline, Registry: reg, Client: deadClient(), Logger: quietLogger()},
			keyedRequest(target, callerKey),
		)
		if pipeline.hits() != 0 {
			t.Errorf("%s reached the proxy", target)
		}
	}
}

// TestTheSurfaceResolvesOncePerRequest pins the in-flight snapshot. The context
// is resolved once, before anything dispatches on it, so a registry file
// replaced mid-request cannot serve the first half of a request from one
// document and the second half from the next.
func TestTheSurfaceResolvesOncePerRequest(t *testing.T) {
	reg := newRegistryDocument(t, twoContextRegistry("personal"))
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(reg.Path(), base, base); err != nil {
		t.Fatal(err)
	}

	seen := make(chan string, 2)
	pipeline := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res, ok := registry.ResolutionFrom(r.Context())
		if !ok {
			t.Error("missing attached resolution")
			return
		}

		// Replace the registry underneath the request with a document that binds
		// the same selector somewhere else.
		replacement := strings.Replace(
			twoContextRegistry("personal"),
			`"models":{"luna-fast":"luna"}`,
			`"models":{"luna-fast":"anthropic"}`,
			1,
		)
		if err := os.WriteFile(reg.Path(), []byte(replacement), 0o600); err != nil {
			t.Error(err)
			return
		}
		if err := os.Chtimes(reg.Path(), base.Add(time.Hour), base.Add(time.Hour)); err != nil {
			t.Error(err)
			return
		}

		selected, err := res.Select("luna-fast", nil)
		if err != nil {
			t.Error(err)
			return
		}
		seen <- selected.SeatID

		fresh, freshErr := reg.Resolve(registry.Selection{}, "luna-fast", nil)
		if freshErr != nil {
			t.Error(freshErr)
			return
		}
		seen <- fresh.SeatID

		w.WriteHeader(http.StatusNoContent)
	})

	rec := serve(
		t,
		server.Config{Proxy: pipeline, Registry: reg},
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"luna-fast"}`)),
	)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if attached := <-seen; attached != "luna" {
		t.Fatalf("the attached snapshot resolved to %q; want the document the request started on", attached)
	}
	if reloaded := <-seen; reloaded != "anthropic" {
		t.Fatalf("a fresh Resolve returned %q; want the replaced document", reloaded)
	}
}
