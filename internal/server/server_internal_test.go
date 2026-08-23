package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/usage"
)

// trackerDeadline bounds every call that must NOT block, so a deadlock fails
// the test instead of hanging the package.
const trackerDeadline = 5 * time.Second

// staleValidRegistry is the document the older request below resolves against:
// one context bound to one forwarding seat, with nothing in it that can fail
// validation.
const staleValidRegistry = `{"default_context":"personal","seats":{` +
	`"anthropic":{"upstream":"https://api.anthropic.com","auth_mode":"forward"}},` +
	`"contexts":{"personal":{"default_seat":"anthropic","models":{}}}}`

// rejectedSecret is the distinctive value the replacement below names in a field
// validation quotes back, standing in for whatever an operator pasted into the
// wrong place. It is what tells the detail the gateway logged apart from the
// bare sentinel every caller-facing surface carries instead.
const rejectedSecret = "sk-live-secret"

// brokenRegistry is the replacement that will not validate: api_key_env_file
// names a value that is not an environment variable name at all.
const brokenRegistry = `{"default_context":"personal","seats":{` +
	`"luna":{"upstream":"https://openrouter.ai/api","model":"openai/gpt-5.6-luna",` +
	`"auth_mode":"inject","api_key_env_file":"` + rejectedSecret + `"}},` +
	`"contexts":{"personal":{"default_seat":"luna","models":{}}}}`

// await blocks until signal fires or the deadline passes, failing with reason
// rather than hanging the package.
func await(t *testing.T, signal <-chan struct{}, reason string) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(trackerDeadline):
		t.Fatalf("timed out waiting for %s", reason)
	}
}

// emission reports the next detail emitted, or fails when none arrives.
func emission(t *testing.T, emitted chan error) error {
	t.Helper()

	select {
	case err := <-emitted:
		return err
	case <-time.After(trackerDeadline):
		t.Fatal("no detail was emitted")

		return nil
	}
}

// writeRegistryFile writes body to path and stamps it at when, which is what a
// hot reload looks like from a running registry's side: new contents under a
// stamp it has not seen yet. The stamp is set explicitly rather than left to the
// clock, so a replacement is noticed whatever resolution the filesystem keeps
// modification times at.
func writeRegistryFile(t *testing.T, path, body string, when time.Time) {
	t.Helper()

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// invalidDetails returns the detail logged with every invalidRegistryMessage
// record in a JSON handler's output, in the order they were written. Every other
// record is skipped, so a count here is a count of what an operator greps for.
func invalidDetails(t *testing.T, output []byte) []string {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	found := make([]string, 0, len(lines))

	for _, line := range lines {
		if line == "" {
			continue
		}

		var record struct {
			Message string `json:"msg"`
			Error   string `json:"error"`
		}

		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode logged record %q: %v", line, err)
		}

		if record.Message == invalidRegistryMessage {
			found = append(found, record.Error)
		}
	}

	return found
}

// TestGatewayPathsObserveRegistryFreshnessOnce counts the filesystem stat that
// begins a freshness acquisition. Every request path may acquire once, while
// prefixed introspection must carry that state into document construction rather
// than asking the registry again.
type admissionAccountant struct {
	pending atomic.Int64
}

func (accountant *admissionAccountant) InsertPending(
	context.Context,
	usage.PendingRequest,
) (int64, error) {
	return accountant.pending.Add(1), nil
}

func (*admissionAccountant) MarkUpstreamDispatched(
	context.Context,
	int64,
	time.Time,
	string,
) error {
	return nil
}

func (*admissionAccountant) Finalize(
	context.Context,
	int64,
	usage.FinalizedRequest,
) error {
	return nil
}

type admissionTransport struct {
	contacts atomic.Int64
}

func (transport *admissionTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.contacts.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"type":"message","content":[]}`)),
	}, nil
}

func admissionRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte(staleValidRegistry), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(path)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestHandlerAdmissionPrecedesContextResolutionAndBypassesBareIntrospection(t *testing.T) {
	reg := admissionRegistry(t)
	accountant := &admissionAccountant{}
	transport := &admissionTransport{}
	pipeline, err := proxy.New(proxy.Config{
		Registry:   reg,
		Accountant: accountant,
		Client:     &http.Client{Transport: transport},
		Host:       "host",
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	release := make(chan struct{})
	entered := make(chan struct{}, 64)
	hold := pipeline.AdmissionMiddleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	for range 34 {
		go hold.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	}
	for range 34 {
		await(t, entered, "all Proxy admission permits")
	}

	var resolutions atomic.Int64
	stopObserving := reg.ObserveStatsForTest(func(string) { resolutions.Add(1) })
	defer stopObserving()

	handler := Handler(Config{
		Proxy:               pipeline,
		Registry:            reg,
		AdmissionMiddleware: pipeline.AdmissionMiddleware(),
	})
	overload := httptest.NewRecorder()
	overloadRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"test"}`),
	)
	overloadDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(overload, overloadRequest)
		close(overloadDone)
	}()
	select {
	case <-overloadDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("over-capacity request did not receive a prompt local refusal")
	}
	if overload.Code != http.StatusServiceUnavailable {
		t.Fatalf("overload status = %d; want %d", overload.Code, http.StatusServiceUnavailable)
	}
	if got := resolutions.Load(); got != 0 {
		t.Fatalf("registry resolutions = %d; want zero before overload refusal", got)
	}
	if got := accountant.pending.Load(); got != 0 {
		t.Fatalf("pending accounting inserts = %d; want zero before overload refusal", got)
	}
	if got := transport.contacts.Load(); got != 0 {
		t.Fatalf("upstream contacts = %d; want zero before overload refusal", got)
	}

	keyPath := filepath.Join(t.TempDir(), "caller.key")
	if err := os.WriteFile(keyPath, []byte("caller-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(proxy.CallerKeyEnvFile, keyPath)
	introspection := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/_patchbay/nothing", nil)
	request.Header.Set(proxy.CallerKeyHeader, "caller-key")
	handler.ServeHTTP(introspection, request)
	if introspection.Code != http.StatusNotFound {
		t.Fatalf(
			"bare introspection status = %d; want %d while routed admission is full",
			introspection.Code,
			http.StatusNotFound,
		)
	}

	close(release)
}

func TestAdmissionMiddlewareReleasesPermitAfterPanic(t *testing.T) {
	pipeline, err := proxy.New(proxy.Config{Accountant: &admissionAccountant{}, Host: "host"})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	middleware := pipeline.AdmissionMiddleware()
	panicking := middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("test panic")
	}))
	func() {
		defer func() {
			if recovered := recover(); recovered != "test panic" {
				t.Fatalf("panic = %v; want test panic", recovered)
			}
		}()
		panicking.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()

	release := make(chan struct{})
	entered := make(chan struct{}, 64)
	hold := middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	for range 33 {
		go hold.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	for range 33 {
		await(t, entered, "all but one permits after a panic")
	}

	candidate := httptest.NewRecorder()
	middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(candidate, httptest.NewRequest(http.MethodGet, "/", nil))
	if candidate.Code != http.StatusNoContent {
		t.Fatalf("request after panic status = %d; want %d", candidate.Code, http.StatusNoContent)
	}

	close(release)
}

func TestAdmissionMiddlewareDoesNotAdmitAnotherProxy(t *testing.T) {
	first, err := proxy.New(proxy.Config{Accountant: &admissionAccountant{}, Host: "first"})
	if err != nil {
		t.Fatalf("New(first) error: %v", err)
	}

	secondRegistry := admissionRegistry(t)
	secondAccountant := &admissionAccountant{}
	secondTransport := &admissionTransport{}
	second, err := proxy.New(proxy.Config{
		Registry:   secondRegistry,
		Accountant: secondAccountant,
		Client:     &http.Client{Transport: secondTransport},
		Host:       "second",
	})
	if err != nil {
		t.Fatalf("New(second) error: %v", err)
	}

	release := make(chan struct{})
	defer func() {
		close(release)
		second.Drain(t.Context())
	}()
	entered := make(chan struct{}, 64)
	hold := second.AdmissionMiddleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	for range 34 {
		go hold.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	}
	for range 34 {
		await(t, entered, "all second Proxy admission permits")
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test"}`))
	request.Header.Set("X-Api-Key", "caller-key")
	first.AdmissionMiddleware()(second).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("mismatched admission status = %d; want %d", response.Code, http.StatusServiceUnavailable)
	}
	if got := secondAccountant.pending.Load(); got != 0 {
		t.Fatalf("second pending accounting inserts = %d; want zero", got)
	}
	if got := secondTransport.contacts.Load(); got != 0 {
		t.Fatalf("second upstream contacts = %d; want zero", got)
	}
}

func TestHandlerAdmissionAcquiresOnceBeforeProxy(t *testing.T) {
	reg := admissionRegistry(t)
	accountant := &admissionAccountant{}
	transport := &admissionTransport{}
	pipeline, err := proxy.New(proxy.Config{
		Registry:   reg,
		Accountant: accountant,
		Client:     &http.Client{Transport: transport},
		Host:       "host",
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	release := make(chan struct{})
	entered := make(chan struct{}, 64)
	hold := pipeline.AdmissionMiddleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	for range 33 {
		go hold.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	}
	for range 33 {
		await(t, entered, "all but one Proxy admission permits")
	}

	handler := Handler(Config{
		Proxy:               pipeline,
		Registry:            reg,
		AdmissionMiddleware: pipeline.AdmissionMiddleware(),
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test"}`))
	request.Header.Set("X-Api-Key", "caller-key")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("admitted request status = %d; want %d", response.Code, http.StatusOK)
	}
	if got := transport.contacts.Load(); got != 1 {
		t.Fatalf("upstream contacts = %d; want 1", got)
	}
	if got := accountant.pending.Load(); got != 1 {
		t.Fatalf("pending accounting inserts = %d; want 1", got)
	}

	close(release)
	pipeline.Drain(t.Context())
}

func TestGatewayPathsObserveRegistryFreshnessOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistryFile(t, path, staleValidRegistry, time.Now().Add(-time.Hour).Truncate(time.Second))
	reg, err := registry.New(path)
	if err != nil {
		t.Fatal(err)
	}
	gw := &gateway{registry: reg, logger: slog.Default()}

	var acquisitions int
	cleanup := reg.ObserveStatsForTest(func(observedPath string) {
		if observedPath == path {
			acquisitions++
		}
	})
	t.Cleanup(cleanup)

	assertOne := func(name string, serve http.Handler, target string) {
		t.Run(name, func(t *testing.T) {
			before := acquisitions
			serve.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
			if got := acquisitions - before; got != 1 {
				t.Fatalf("freshness acquisitions = %d; want 1", got)
			}
		})
	}

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	routed := gw.selectContext(next)
	assertOne("proxy", routed, "/v1/messages")
	assertOne("models", routed, modelsPath)

	bare := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { gw.document(r.Context()) })
	assertOne("bare introspection", bare, RegistryEndpoint)

	prefixed := gw.selectContext(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gw.document(r.Context())
	}))
	assertOne("prefixed introspection", prefixed, "/ctx/personal"+RegistryEndpoint)
}

func TestInvalidRegistryLoggingDoesNotHoldDiagnosticLock(t *testing.T) {
	t.Parallel()

	failure := errors.New("registry rejected api_key_env_file")
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	var tracker invalidRegistry

	go func() {
		defer close(firstDone)
		tracker.report(registry.Observation{Sequence: 1, Err: failure}, func(error) {
			close(entered)
			<-release
		})
	}()
	await(t, entered, "the first report to enter its logger")

	go func() {
		defer close(secondDone)
		tracker.report(registry.Observation{Sequence: 2}, func(error) {})
	}()
	await(t, secondDone, "a newer observation to progress while logging is blocked")

	close(release)
	await(t, firstDone, "the blocked logger to return")
}

func TestObserveRegistryIgnoresAStaleValidResolution(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "registry.json")
	stamped := time.Now().Add(-time.Hour).Truncate(time.Second)
	writeRegistryFile(t, path, staleValidRegistry, stamped)

	reg, err := registry.New(path)
	if err != nil {
		t.Fatalf("New(%s) = %v", path, err)
	}

	var logged bytes.Buffer

	gw := &gateway{registry: reg, logger: slog.New(slog.NewJSONHandler(&logged, nil))}

	// The older request resolves while the file is still whole, then stalls
	// before it observes anything. Its own result says the registry is well, and
	// goes on saying so for the rest of this schedule.
	_, stale, resolveErr := reg.ResolveObserved(registry.Selection{}, "", nil)
	if resolveErr != nil {
		t.Fatalf("ResolveObserved() = %v; want the valid document the older request saw", resolveErr)
	}

	// The file breaks underneath it, and a newer request observes the
	// replacement first. That is the one line the operator needs.
	writeRegistryFile(t, path, brokenRegistry, stamped.Add(time.Hour))
	_, current := reg.InspectObserved()
	gw.observeRegistry(t.Context(), current)

	// The older request resumes with its valid observation. Its lower sequence
	// cannot clear the newer invalid marker.
	gw.observeRegistry(t.Context(), stale)

	// Repeating the current failure remains deduplicated.
	gw.observeRegistry(t.Context(), current)

	details := invalidDetails(t, logged.Bytes())
	if len(details) != 1 {
		t.Fatalf("operator log holds %d %q records; want 1\n%s",
			len(details), invalidRegistryMessage, logged.String())
	}

	for _, detail := range []string{path, rejectedSecret, "api_key_env_file"} {
		if !strings.Contains(details[0], detail) {
			t.Errorf("operator log %q keeps no %q", details[0], detail)
		}
	}
}

// TestObserveRegistryReportsARepairedRegistryBreakingAgain is the other end of
// the same single entry point. A registry that is valid when it is observed
// clears the marker through that one operation, so the same failure returning
// afterwards is news nobody has been told.
func TestObserveRegistryReportsARepairedRegistryBreakingAgain(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "registry.json")
	stamped := time.Now().Add(-time.Hour).Truncate(time.Second)
	writeRegistryFile(t, path, staleValidRegistry, stamped)

	reg, err := registry.New(path)
	if err != nil {
		t.Fatalf("New(%s) = %v", path, err)
	}

	var logged bytes.Buffer

	gw := &gateway{registry: reg, logger: slog.New(slog.NewJSONHandler(&logged, nil))}

	writeRegistryFile(t, path, brokenRegistry, stamped.Add(time.Hour))
	_, observed := reg.InspectObserved()
	gw.observeRegistry(t.Context(), observed)

	writeRegistryFile(t, path, staleValidRegistry, stamped.Add(2*time.Hour))
	_, observed = reg.InspectObserved()
	gw.observeRegistry(t.Context(), observed)

	writeRegistryFile(t, path, brokenRegistry, stamped.Add(3*time.Hour))
	_, observed = reg.InspectObserved()
	gw.observeRegistry(t.Context(), observed)

	if details := invalidDetails(t, logged.Bytes()); len(details) != 2 {
		t.Fatalf("operator log holds %d %q records; want 2, the same failure twice\n%s",
			len(details), invalidRegistryMessage, logged.String())
	}
}

// TestReportIgnoresOlderObservations pins the sequence ordering that lets the
// registry perform filesystem work before entering the diagnostic critical section.
func TestReportIgnoresOlderObservations(t *testing.T) {
	t.Parallel()

	failure := errors.New("registry rejected api_key_env_file")
	emitted := make(chan error, 4)
	var tracker invalidRegistry
	emit := func(err error) { emitted <- err }

	tracker.report(registry.Observation{Sequence: 2, Err: failure}, emit)
	if got := emission(t, emitted); !errors.Is(got, failure) {
		t.Fatalf("emitted %v; want %v", got, failure)
	}

	tracker.report(registry.Observation{Sequence: 1}, emit)
	tracker.report(registry.Observation{Sequence: 3, Err: failure}, emit)
	if len(emitted) != 0 {
		t.Fatalf("stale recovery or duplicate failure emitted %d times", len(emitted))
	}
}

// TestReportDedupsAndClearsUnderOneLock covers the rest of the marker's
// behavior through the single entry point: a repeat of the outstanding failure
// says nothing, a different failure always does, and a nil detail is a recovery
// that clears the marker in the same critical section.
func TestReportDedupsAndClearsUnderOneLock(t *testing.T) {
	t.Parallel()

	first := errors.New("registry rejected api_key_env_file")
	second := errors.New("registry rejected default_context")
	emitted := make(chan error, 8)

	var tracker invalidRegistry

	emit := func(err error) { emitted <- err }
	var sequence uint64
	report := func(detail error) {
		sequence++
		tracker.report(registry.Observation{Sequence: sequence, Err: detail}, emit)
	}

	report(first)
	if got := emission(t, emitted); !errors.Is(got, first) {
		t.Fatalf("emitted %v; want %v", got, first)
	}

	// The same outstanding failure, said again: nothing new to tell.
	report(first)
	if len(emitted) != 0 {
		t.Fatalf("a repeat of the outstanding failure emitted %d times; want 0", len(emitted))
	}

	// A different failure is always news.
	report(second)
	if got := emission(t, emitted); !errors.Is(got, second) {
		t.Fatalf("emitted %v; want %v", got, second)
	}

	// A nil current detail is a recovery: it emits nothing and forgets the
	// marker, so the same failure returning is news a second time.
	report(nil)
	if len(emitted) != 0 {
		t.Fatalf("a recovery emitted %d times; want 0", len(emitted))
	}

	report(second)
	if got := emission(t, emitted); !errors.Is(got, second) {
		t.Fatalf("after a recovery, emitted %v; want %v reported again", got, second)
	}
}

// TestRecoveryBeforeAReportLeavesTheLaterObservationStanding is the other
// arrangement of the same total order. A recovery that completes before a report
// begins is not overtaken by it: the report observes the registry afterwards, so
// whatever it finds is the current truth and is what the marker ends up holding.
func TestRecoveryBeforeAReportLeavesTheLaterObservationStanding(t *testing.T) {
	t.Parallel()

	failure := errors.New("registry rejected api_key_env_file")
	emitted := make(chan error, 4)

	var tracker invalidRegistry

	emit := func(err error) { emitted <- err }

	tracker.report(registry.Observation{Sequence: 1, Err: failure}, emit)
	if got := emission(t, emitted); !errors.Is(got, failure) {
		t.Fatalf("emitted %v; want %v", got, failure)
	}

	tracker.report(registry.Observation{Sequence: 2}, emit)

	// Cleared first, observed second: the report speaks even though its detail
	// is the one that was just forgotten.
	tracker.report(registry.Observation{Sequence: 3, Err: failure}, emit)
	if got := emission(t, emitted); !errors.Is(got, failure) {
		t.Fatalf("after a recovery, emitted %v; want %v reported again", got, failure)
	}

	// And a report whose observation finds nothing wrong clears rather than
	// marks, so it leaves no marker for the next failure to be mistaken for.
	tracker.report(registry.Observation{Sequence: 4}, emit)
	tracker.report(registry.Observation{Sequence: 5, Err: failure}, emit)

	if got := emission(t, emitted); !errors.Is(got, failure) {
		t.Fatalf("after a nil observation, emitted %v; want %v reported again", got, failure)
	}

	if len(emitted) != 0 {
		t.Fatalf("%d unexpected extra emissions", len(emitted))
	}
}
