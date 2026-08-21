package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stampAt builds a distinct stamp per generation, so a test can say exactly
// which version of the file a stat call reported.
func stampAt(generation int) fileStamp {
	return fileStamp{
		exists:  true,
		modTime: time.Unix(int64(generation), 0),
		size:    int64(generation),
		target:  "generation",
	}
}

// scriptedFile is a registry file whose every stat and every read is dictated by
// the test, so an interleaving that is a race on a real filesystem becomes an
// ordinary deterministic sequence here.
type scriptedFile struct {
	stamps  []fileStamp
	docs    []string
	loadErr error

	stats int
	loads int
}

// stat hands out the next scripted stamp, repeating the last one once the
// script runs out.
func (f *scriptedFile) stat() fileStamp {
	stamp := f.stamps[min(f.stats, len(f.stamps)-1)]
	f.stats++

	return stamp
}

// load hands out the next scripted document, or the scripted failure.
func (f *scriptedFile) load() (document, error) {
	f.loads++

	if f.loadErr != nil {
		return document{}, f.loadErr
	}

	name := f.docs[min(f.loads-1, len(f.docs)-1)]

	return document{DefaultContext: name}, nil
}

// TestAcquireRetriesUntilOneVersionIsReadWhole pins the pairing rule: the stamp
// an acquisition keeps must describe the bytes it actually read. A file replaced
// between the read and the stat would otherwise be cached as document A under
// stamp B, and every later refresh would see stamp B as current and never look
// again.
func TestAcquireRetriesUntilOneVersionIsReadWhole(t *testing.T) {
	t.Parallel()

	// Attempt one reads generation one and stats generation two afterwards: the
	// file moved under the read, so nothing about it may be trusted. Attempt two
	// sees generation two on both sides.
	file := &scriptedFile{
		stamps: []fileStamp{stampAt(1), stampAt(2), stampAt(2), stampAt(2)},
		docs:   []string{"one", "two"},
	}

	snap, err := acquire(file.stat, file.load)
	if err != nil {
		t.Fatalf("acquire() = %v; want the settled version", err)
	}
	if snap.err != nil {
		t.Fatalf("snapshot error = %v; want a valid document", snap.err)
	}
	if snap.doc == nil || snap.doc.DefaultContext != "two" {
		t.Fatalf("snapshot document = %+v; want the version the stamp describes", snap.doc)
	}
	if !snap.stamp.equal(stampAt(2)) {
		t.Fatalf("snapshot stamp = %+v; want generation two", snap.stamp)
	}
	if file.loads != 2 {
		t.Fatalf("load calls = %d; want the one retry the change earned", file.loads)
	}
}

// TestRefreshRetriesAfterAnUnsettledAcquisition pins what an exhausted
// acquisition leaves behind. It stores the fail-closed sentinel under no trusted
// stamp, and the state it leaves has to make the next request look again — a
// registry that recorded the file's current stamp while never having read it
// would consider that unvalidated version current forever, which is the whole
// failure the consistent load exists to prevent.
func TestRefreshRetriesAfterAnUnsettledAcquisition(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "registry.json")
	body := `{"default_context":"personal","seats":{` +
		`"pin":{"upstream":"https://api.anthropic.com","auth_mode":"forward"}},` +
		`"contexts":{"personal":{"default_seat":"pin","models":{}}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	reg, err := New(path)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	// Exactly what a refresh stores when the file never settled: no snapshot at
	// all, and the detailed acquisition failure behind it.
	reg.store(snapshot{}, errors.New("changed while being read 3 times running"))

	if reg.err == nil || reg.doc != nil || reg.loaded {
		t.Fatalf(
			"stored state = doc %+v, err %v, loaded %v; want the sentinel under no trusted stamp",
			reg.doc, reg.err, reg.loaded,
		)
	}

	// The file on disk has not moved, so a registry that had recorded a stamp
	// here would call itself fresh and refuse every later request. It has to go
	// back to the file instead.
	if _, resolveErr := reg.Resolve(Selection{}, "anything", nil); resolveErr != nil {
		t.Fatalf("Resolve() = %v; want the next request to re-read the file", resolveErr)
	}
}

// TestAcquireKeepsAStableFailureWithItsOwnStamp pins the other settled outcome.
// A file that is missing, unparsable or invalid is not a reason to retry: it is
// a stable answer, and it is kept with the stamp of the version that produced it
// so refresh waits for the file to change rather than re-reading it per request.
func TestAcquireKeepsAStableFailureWithItsOwnStamp(t *testing.T) {
	t.Parallel()

	wanted := errors.New("default_context is missing")
	file := &scriptedFile{
		stamps:  []fileStamp{stampAt(7)},
		loadErr: wanted,
	}

	snap, err := acquire(file.stat, file.load)
	if err != nil {
		t.Fatalf("acquire() = %v; want a settled failure rather than an acquisition failure", err)
	}
	if !errors.Is(snap.err, wanted) {
		t.Fatalf("snapshot error = %v; want the file's own detailed error", snap.err)
	}
	if snap.doc != nil {
		t.Fatalf("snapshot document = %+v; want none", snap.doc)
	}
	if !snap.stamp.equal(stampAt(7)) {
		t.Fatalf("snapshot stamp = %+v; want the stable stamp the failure came from", snap.stamp)
	}
	if file.loads != 1 {
		t.Fatalf("load calls = %d; want a stable failure read once", file.loads)
	}
}

// TestAcquireGivesUpOnAFileThatNeverSettles pins the bound. A file being
// rewritten continuously must cost a bounded number of attempts and then fail,
// carrying no stamp at all — a stamp handed back here would be trusted as the
// current version by a refresh that never validated it.
func TestAcquireGivesUpOnAFileThatNeverSettles(t *testing.T) {
	t.Parallel()

	stamps := make([]fileStamp, 0, 2*loadAttempts)
	for generation := range 2 * loadAttempts {
		stamps = append(stamps, stampAt(generation+1))
	}

	file := &scriptedFile{stamps: stamps, docs: []string{"never-settles"}}

	snap, err := acquire(file.stat, file.load)
	if err == nil {
		t.Fatal("acquire() = nil error; want the acquisition refused")
	}
	if !strings.Contains(err.Error(), "changed while being read") {
		t.Fatalf("acquire() = %v; want it to say the file changed while being read", err)
	}
	if snap.doc != nil || snap.err != nil {
		t.Fatalf("snapshot = %+v; want nothing kept from an unsettled read", snap)
	}
	if !snap.stamp.equal(fileStamp{}) {
		t.Fatalf("snapshot stamp = %+v; want no trusted stamp at all", snap.stamp)
	}
	if file.loads != loadAttempts {
		t.Fatalf("load calls = %d; want exactly %d bounded attempts", file.loads, loadAttempts)
	}
}
