package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/registry"
)

// blockedGrace bounds how long a call that must block is watched before it is
// accepted as blocked. It is a deadline, not a delay: the call it guards would
// finish in microseconds if it were not blocked, so overrunning the grace is
// the only way past this check.
const blockedGrace = 100 * time.Millisecond

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

// reportRun drives one concurrent invalidRegistry.reportCurrent, exposing the
// two moments the ordering rule is about: the instant the current-detail
// callback runs inside the tracker's critical section, and the instant the
// whole report has returned.
type reportRun struct {
	entered chan struct{}
	release chan struct{}
	done    chan struct{}
	emitted chan error
}

// startReport begins a report whose current-detail callback blocks until the
// test releases it, so the test can act while the tracker's lock is held from
// inside the report.
func startReport(tracker *invalidRegistry, detail error, emitted chan error) *reportRun {
	run := &reportRun{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		done:    make(chan struct{}),
		emitted: emitted,
	}

	go func() {
		defer close(run.done)

		tracker.reportCurrent(func() error {
			close(run.entered)
			<-run.release

			return detail
		}, func(err error) { run.emitted <- err })
	}()

	return run
}

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

// TestObserveRegistryIgnoresAStaleValidResolution is the schedule a split
// report/clear API cannot survive, run in the order that breaks it.
//
// A request resolves the registry once and carries that result for the rest of
// its life. Deciding the diagnostic from that result means a request which
// resolved a valid document — and then did anything at all before it got to the
// diagnostic — clears a failure a newer request already reported. The marker is
// then empty while the registry is still broken, and the very next refusal
// reports the same failure a second time: an operator reading the log sees one
// failure logged twice, with a false recovery between them.
//
// So there is one operation and it asks the registry, not the caller. Whatever
// the request resolved, what gets reported is what is wrong right now.
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
	if _, resolveErr := reg.Resolve(registry.Selection{}, "", nil); resolveErr != nil {
		t.Fatalf("Resolve() = %v; want the valid document the older request saw", resolveErr)
	}

	// The file breaks underneath it, and a newer request observes the
	// replacement first. That is the one line the operator needs.
	writeRegistryFile(t, path, brokenRegistry, stamped.Add(time.Hour))
	gw.observeRegistry(t.Context())

	// The older request resumes and makes the same observation every request
	// makes. It observes the registry as it is now rather than as its own
	// resolution found it, so the reported failure stands.
	gw.observeRegistry(t.Context())

	// Which a third observation of that same failure proves: still nothing new
	// to tell. Had the resumed request cleared the marker, this would say the
	// same thing over again.
	gw.observeRegistry(t.Context())

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
	gw.observeRegistry(t.Context())

	writeRegistryFile(t, path, staleValidRegistry, stamped.Add(2*time.Hour))
	gw.observeRegistry(t.Context())

	writeRegistryFile(t, path, brokenRegistry, stamped.Add(3*time.Hour))
	gw.observeRegistry(t.Context())

	if details := invalidDetails(t, logged.Bytes()); len(details) != 2 {
		t.Fatalf("operator log holds %d %q records; want 2, the same failure twice\n%s",
			len(details), invalidRegistryMessage, logged.String())
	}
}

// TestReportCurrentOrdersRecoveryAgainstAnEarlierObservation is the whole point
// of folding the current-detail read into the tracker's own critical section.
//
// Reading the registry's error outside the lock lets a recovery land between the
// observation and the report that describes it. The stale report then wins the
// lock, writes the old detail back over the cleared marker, and every later
// refusal for that same failure is suppressed as already-told — so the failure
// an operator is looking at is the one failure the log will not carry.
//
// Held under one lock there is one total order and only two arrangements of it.
// A recovery that arrives while a report is mid-observation waits for that
// report and lands after it, leaving the marker cleared; a recovery that got
// there first is simply overwritten by a report that observed the registry after
// it.
func TestReportCurrentOrdersRecoveryAgainstAnEarlierObservation(t *testing.T) {
	t.Parallel()

	failure := errors.New("registry rejected api_key_env_file")
	emitted := make(chan error, 4)

	var tracker invalidRegistry

	emit := func(err error) { emitted <- err }

	run := startReport(&tracker, failure, emitted)
	await(t, run.entered, "the report to reach its current-detail callback")

	// The report holds the tracker's lock from inside the callback, so a
	// recovery racing it cannot complete until the report has finished with it.
	recovered := make(chan struct{})
	go func() {
		defer close(recovered)

		tracker.reportCurrent(func() error { return nil }, emit)
	}()

	select {
	case <-recovered:
		t.Fatal("a recovery completed while a report held the tracker lock; the two are not ordered")
	case <-time.After(blockedGrace):
	}

	close(run.release)
	await(t, run.done, "the report to return")
	await(t, recovered, "the recovery to return once the report released the lock")

	if got := emission(t, emitted); !errors.Is(got, failure) {
		t.Fatalf("emitted %v; want the observed failure %v", got, failure)
	}

	// The recovery ran last, so the marker it cleared stays cleared: the same
	// failure arriving afterwards is news nobody has been told.
	tracker.reportCurrent(func() error { return failure }, emit)

	if got := emission(t, emitted); !errors.Is(got, failure) {
		t.Fatalf("the repeat emitted %v; want the same failure %v reported again", got, failure)
	}

	if len(emitted) != 0 {
		t.Fatalf("%d unexpected extra emissions", len(emitted))
	}
}

// TestReportCurrentDedupsAndClearsUnderOneLock covers the rest of the marker's
// behavior through the single entry point: a repeat of the outstanding failure
// says nothing, a different failure always does, and a nil current detail is a
// recovery that clears the marker in the same critical section it was read in.
func TestReportCurrentDedupsAndClearsUnderOneLock(t *testing.T) {
	t.Parallel()

	first := errors.New("registry rejected api_key_env_file")
	second := errors.New("registry rejected default_context")
	emitted := make(chan error, 8)

	var tracker invalidRegistry

	emit := func(err error) { emitted <- err }
	report := func(detail error) {
		tracker.reportCurrent(func() error { return detail }, emit)
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

	tracker.reportCurrent(func() error { return failure }, emit)
	if got := emission(t, emitted); !errors.Is(got, failure) {
		t.Fatalf("emitted %v; want %v", got, failure)
	}

	tracker.reportCurrent(func() error { return nil }, emit)

	// Cleared first, observed second: the report speaks even though its detail
	// is the one that was just forgotten.
	tracker.reportCurrent(func() error { return failure }, emit)
	if got := emission(t, emitted); !errors.Is(got, failure) {
		t.Fatalf("after a recovery, emitted %v; want %v reported again", got, failure)
	}

	// And a report whose observation finds nothing wrong clears rather than
	// marks, so it leaves no marker for the next failure to be mistaken for.
	tracker.reportCurrent(func() error { return nil }, emit)
	tracker.reportCurrent(func() error { return failure }, emit)

	if got := emission(t, emitted); !errors.Is(got, failure) {
		t.Fatalf("after a nil observation, emitted %v; want %v reported again", got, failure)
	}

	if len(emitted) != 0 {
		t.Fatalf("%d unexpected extra emissions", len(emitted))
	}
}
