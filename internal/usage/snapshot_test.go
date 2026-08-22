package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func toolingStagingMatches(t *testing.T, dest string) []string {
	t.Helper()
	matches, err := filepath.Glob(
		filepath.Join(filepath.Dir(dest), toolingStagingNamePrefix(dest)+"*"),
	)
	if err != nil {
		t.Fatalf("glob tooling staging directories: %v", err)
	}
	return matches
}

func TestSnapshotCopiesConsistentDatabase(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, source)
	if _, err := store.InsertPending(t.Context(), validPendingRequest()); err != nil {
		t.Fatalf("seed request: %v", err)
	}
	if err := os.Chmod(source, 0o644); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "usage.snapshot.sqlite")
	if err := Snapshot(t.Context(), source, dest); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	assertMode(t, dest, 0o600)
	assertMode(t, source, 0o644)

	db := inspectDB(t, dest)
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity check = %q; want ok", integrity)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM accounting_requests`).Scan(&rows); err != nil {
		t.Fatalf("count snapshot rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("snapshot rows = %d; want 1", rows)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close snapshot inspection: %v", err)
	}

	reopened, err := Open(dest)
	if err != nil {
		t.Fatalf("Open(snapshot) error = %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened snapshot: %v", err)
	}

	restored := filepath.Join(t.TempDir(), "restored.sqlite")
	contents, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read snapshot for restore: %v", err)
	}
	if err := os.WriteFile(restored, contents, 0o600); err != nil {
		t.Fatalf("copy snapshot for restore: %v", err)
	}
	restoredStore, err := Open(restored)
	if err != nil {
		t.Fatalf("Open(restored snapshot) error = %v", err)
	}
	if err := restoredStore.db.QueryRow(`SELECT COUNT(*) FROM accounting_requests`).Scan(&rows); err != nil {
		t.Fatalf("count restored rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("restored rows = %d; want 1", rows)
	}
	if err := restoredStore.Close(); err != nil {
		t.Fatalf("close restored snapshot: %v", err)
	}
}

func TestSnapshotStagesPrivatelyBeforeAtomicPublish(t *testing.T) {
	source := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, source)
	if _, err := store.InsertPending(t.Context(), validPendingRequest()); err != nil {
		t.Fatalf("seed request: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "usage.snapshot.sqlite")
	staged := make(chan string, 1)
	release := make(chan struct{})
	ctx := context.WithValue(t.Context(), snapshotBeforePublishHookKey{}, func(temp string) {
		staged <- temp
		<-release
	})
	result := make(chan error, 1)
	go func() { result <- Snapshot(ctx, source, dest) }()

	var temp string
	select {
	case temp = <-staged:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot did not reach its staged artifact")
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination exists before publish: %v", err)
	}
	stageDir := filepath.Dir(temp)
	assertMode(t, stageDir, 0o700)
	info, err := os.Stat(temp)
	if err != nil {
		t.Fatalf("stat staged snapshot: %v", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		t.Fatalf("staged artifact = mode %v size %d; want a non-empty regular file", info.Mode(), info.Size())
	}

	close(release)
	if err := <-result; err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	assertMode(t, dest, 0o600)
	if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory remains after publish: %v", err)
	}
}

func TestPrepareToolingDestinationScavengesOnlyOldUnlockedStaging(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "usage.snapshot.sqlite")
	prefix := toolingStagingNamePrefix(dest)
	oldUnlocked := filepath.Join(dir, prefix+"old-unlocked")
	oldLocked := filepath.Join(dir, prefix+"old-locked")
	fresh := filepath.Join(dir, prefix+"fresh")
	for _, path := range []string{oldUnlocked, oldLocked, fresh} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "artifact"), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, toolingStagingLockName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lockedFile, err := os.OpenFile(filepath.Join(oldLocked, toolingStagingLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lockedFile.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlockErr := unix.Flock(int(lockedFile.Fd()), unix.LOCK_UN); unlockErr != nil {
			t.Errorf("unlock live staging fixture: %v", unlockErr)
		}
		if closeErr := lockedFile.Close(); closeErr != nil {
			t.Errorf("close live staging fixture: %v", closeErr)
		}
	}()

	now := time.Now()
	oldTime := now.Add(-toolingStagingOrphanAge - time.Minute)
	for _, path := range []string{oldUnlocked, oldLocked} {
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(fresh, now, now); err != nil {
		t.Fatal(err)
	}

	prepared, err := prepareToolingDestination(dest)
	if err != nil {
		t.Fatalf("prepareToolingDestination() error: %v", err)
	}
	defer func() {
		if cleanupErr := removeToolingStaging(prepared); cleanupErr != nil {
			t.Errorf("clean prepared staging: %v", cleanupErr)
		}
	}()
	if _, err := os.Stat(oldUnlocked); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old unlocked staging directory remains: %v", err)
	}
	if _, err := os.Stat(oldLocked); err != nil {
		t.Fatalf("old locked staging directory was scavenged: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh staging directory was scavenged: %v", err)
	}
	if filepath.Dir(prepared.temp) == fresh {
		t.Fatal("prepareToolingDestination() reused the possible concurrent staging directory")
	}
}

func TestSnapshotCoexistsWithLiveWriter(t *testing.T) {
	const attempts = 5
	for attempt := range attempts {
		source := filepath.Join(t.TempDir(), "usage.sqlite")
		store := openStore(t, source)
		if _, err := store.InsertPending(t.Context(), validPendingRequest()); err != nil {
			t.Fatalf("seed request: %v", err)
		}

		started := make(chan struct{})
		committed := make(chan struct{}, 1)
		stop := make(chan struct{})
		writerErrors := make(chan error, 1)
		var finalized atomic.Int64
		var writer sync.WaitGroup
		writer.Add(1)
		go func() {
			defer writer.Done()
			close(started)
			for {
				select {
				case <-stop:
					return
				default:
				}
				id, insertErr := store.InsertPending(context.Background(), validPendingRequest())
				if insertErr != nil {
					writerErrors <- insertErr
					return
				}
				request := validFinalizedRequest()
				request.ResponseID = fmt.Sprintf("msg_01snapshot%d", id)
				if finalizeErr := store.Finalize(context.Background(), id, request); finalizeErr != nil {
					writerErrors <- finalizeErr
					return
				}
				finalized.Add(1)
				select {
				case committed <- struct{}{}:
				default:
				}
			}
		}()
		<-started
		<-committed

		dest := filepath.Join(t.TempDir(), "usage.snapshot.sqlite")
		startCount := finalized.Load()
		snapshotErr := Snapshot(t.Context(), source, dest)
		endCount := finalized.Load()
		close(stop)
		writer.Wait()
		select {
		case writerErr := <-writerErrors:
			t.Fatalf("live writer: %v", writerErr)
		default:
		}
		if snapshotErr != nil {
			t.Fatalf("Snapshot() with live writer error = %v", snapshotErr)
		}
		if endCount == startCount {
			if attempt+1 == attempts {
				t.Fatalf("no writer commits overlapped any snapshot after %d attempts", attempts)
			}
			continue
		}

		db := inspectDB(t, dest)
		var integrity string
		if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
			t.Fatalf("integrity check: %v", err)
		}
		if integrity != "ok" {
			t.Fatalf("integrity check = %q; want ok", integrity)
		}
		var rows int
		if err := db.QueryRow(`SELECT COUNT(*) FROM accounting_requests`).Scan(&rows); err != nil {
			t.Fatalf("count snapshot rows: %v", err)
		}
		if rows < 1 {
			t.Fatalf("snapshot rows = %d; want at least seed row", rows)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close snapshot inspection: %v", err)
		}

		reopened, reopenErr := Open(dest)
		if reopenErr != nil {
			t.Fatalf("Open(snapshot) error = %v", reopenErr)
		}
		if closeErr := reopened.Close(); closeErr != nil {
			t.Fatalf("close reopened snapshot: %v", closeErr)
		}
		return
	}
}

func TestSnapshotCanceledAfterPartialLeavesNoOutput(t *testing.T) {
	const (
		attempts    = 3
		payloadSize = 32 << 20
	)
	for attempt := range attempts {
		source := filepath.Join(t.TempDir(), "usage.sqlite")
		store, openErr := Open(source)
		if openErr != nil {
			t.Fatalf("open source: %v", openErr)
		}
		if closeErr := store.Close(); closeErr != nil {
			t.Fatalf("close source: %v", closeErr)
		}
		db, openErr := sql.Open("sqlite", source)
		if openErr != nil {
			t.Fatal(openErr)
		}
		if _, execErr := db.Exec(`CREATE TABLE snapshot_payload (payload BLOB NOT NULL)`); execErr != nil {
			t.Fatal(execErr)
		}
		const insertPayload = `INSERT INTO snapshot_payload(payload) VALUES (zeroblob(?))`
		if _, execErr := db.Exec(insertPayload, payloadSize); execErr != nil {
			t.Fatal(execErr)
		}
		if closeErr := db.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}

		dest := filepath.Join(t.TempDir(), "canceled.sqlite")
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() { result <- Snapshot(ctx, source, dest) }()

		ticker := time.NewTicker(time.Millisecond)
		deadline := time.NewTimer(5 * time.Second)
		partialObserved := false
		finished := false
		var snapshotErr error
		for !partialObserved && !finished {
			select {
			case snapshotErr = <-result:
				finished = true
			case <-ticker.C:
				partials := toolingStagingMatches(t, dest)
				if len(partials) == 1 {
					artifact := filepath.Join(partials[0], filepath.Base(dest))
					info, statErr := os.Stat(artifact)
					if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
						t.Fatal(statErr)
					}
					if statErr == nil && info.Size() > 0 {
						cancel()
						partialObserved = true
						snapshotErr = <-result
					}
				}
			case <-deadline.C:
				cancel()
				snapshotErr = <-result
				finished = true
			}
		}
		ticker.Stop()
		deadline.Stop()
		cancel()
		if !partialObserved {
			if attempt+1 == attempts {
				t.Fatalf("could not observe a partial snapshot before cancellation after %d attempts", attempts)
			}
			continue
		}
		if snapshotErr == nil {
			t.Fatal("snapshot succeeded after cancellation of a partial output")
		}
		if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
			t.Fatalf("canceled snapshot destination stat error = %v; want absent destination", statErr)
		}
		partials := toolingStagingMatches(t, dest)
		if len(partials) != 0 {
			t.Fatalf("partial snapshots = %v; want none", partials)
		}
		return
	}
	t.Fatal("could not observe a partial snapshot before cancellation")
}

func TestSnapshotRefusesUnsafeSourcesAndDestinations(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	dest := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := Snapshot(t.Context(), missing, dest); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Snapshot(missing) error = %v; want missing-source refusal", err)
	}
	partials := toolingStagingMatches(t, dest)
	if len(partials) != 0 {
		t.Fatalf("partial snapshots = %v; want none", partials)
	}

	source := filepath.Join(t.TempDir(), "usage.sqlite")
	_ = openStore(t, source)
	existing := filepath.Join(t.TempDir(), "existing.sqlite")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Snapshot(t.Context(), source, existing); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Snapshot(existing destination) error = %v; want destination refusal", err)
	}
	contents, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "keep" {
		t.Fatalf("existing destination = %q; want untouched contents", contents)
	}

	missingDest := filepath.Join(t.TempDir(), "missing", "snapshot.sqlite")
	snapshotErr := Snapshot(t.Context(), source, missingDest)
	if snapshotErr == nil || !strings.Contains(snapshotErr.Error(), "destination directory") {
		t.Fatalf("Snapshot(missing destination directory) error = %v; want refusal", snapshotErr)
	}
}

func TestRecheckToolingSourceDetectsReplacement(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "usage.sqlite")
	if err := os.WriteFile(source, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, probeErr := probeToolingSource(source)
	if probeErr != nil {
		t.Fatalf("probe source: %v", probeErr)
	}
	replacement := filepath.Join(dir, "replacement.sqlite")
	if err := os.WriteFile(replacement, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, source); err != nil {
		t.Fatal(err)
	}
	recheckErr := recheckToolingSource(source, identity)
	if recheckErr == nil || !strings.Contains(recheckErr.Error(), "changed while opening") {
		t.Fatalf("recheckToolingSource() error = %v; want replacement refusal", recheckErr)
	}
}

func TestToolingOpenRejectsInvalidSources(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.sqlite")
	_, missingErr := openToolingDatabase(t.Context(), missing)
	if missingErr == nil || !strings.Contains(missingErr.Error(), "does not exist") {
		t.Fatalf("openToolingDatabase(missing) error = %v; want missing-source refusal", missingErr)
	}

	target := filepath.Join(dir, "target.sqlite")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "source.sqlite")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	_, symlinkErr := openToolingDatabase(t.Context(), symlink)
	if symlinkErr == nil || !strings.Contains(symlinkErr.Error(), "symbolic link") {
		t.Fatalf("openToolingDatabase(symlink) error = %v; want symlink refusal", symlinkErr)
	}
	_, directoryErr := openToolingDatabase(t.Context(), dir)
	if directoryErr == nil || !strings.Contains(directoryErr.Error(), "not a regular file") {
		t.Fatalf("openToolingDatabase(directory) error = %v; want regular-file refusal", directoryErr)
	}

	notDB := filepath.Join(dir, "not-db.sqlite")
	if err := os.WriteFile(notDB, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, notDBErr := openToolingDatabase(t.Context(), notDB)
	if notDBErr == nil {
		t.Fatal("openToolingDatabase(non-database) error = nil; want refusal")
	}

	notPatchbay := filepath.Join(dir, "not-patchbay.sqlite")
	db, err := sql.Open("sqlite", notPatchbay)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, foreignDatabaseErr := openToolingDatabase(t.Context(), notPatchbay)
	if foreignDatabaseErr == nil || !strings.Contains(foreignDatabaseErr.Error(), "not a patchbay usage database") {
		t.Fatalf("foreign database error = %v; want patchbay-database refusal", foreignDatabaseErr)
	}

	future := filepath.Join(dir, "future.sqlite")
	store, err := Open(future)
	if err != nil {
		t.Fatalf("open future source: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close future source: %v", err)
	}
	db, err = sql.Open("sqlite", future)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, name) VALUES (99, 'future')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, futureErr := openToolingDatabase(t.Context(), future)
	if futureErr == nil || !strings.Contains(futureErr.Error(), "unknown future schema version 99") {
		t.Fatalf("openToolingDatabase(future) error = %v; want future-schema refusal", futureErr)
	}

	spoofed := filepath.Join(dir, "spoofed.sqlite")
	spoofedStore, openErr := Open(spoofed)
	if openErr != nil {
		t.Fatalf("open spoofed source: %v", openErr)
	}
	if closeErr := spoofedStore.Close(); closeErr != nil {
		t.Fatalf("close spoofed source: %v", closeErr)
	}
	db, err = sql.Open("sqlite", spoofed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_migrations SET name = 'spoofed' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, spoofedErr := openToolingDatabase(t.Context(), spoofed)
	if spoofedErr == nil || !strings.Contains(spoofedErr.Error(), "not a patchbay usage database") {
		t.Fatalf("spoofed database error = %v; want patchbay-database refusal", spoofedErr)
	}

	missingRequests := filepath.Join(dir, "missing-requests.sqlite")
	requestsStore, openErr := Open(missingRequests)
	if openErr != nil {
		t.Fatalf("open missing-requests source: %v", openErr)
	}
	if closeErr := requestsStore.Close(); closeErr != nil {
		t.Fatalf("close missing-requests source: %v", closeErr)
	}
	db, err = sql.Open("sqlite", missingRequests)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE accounting_requests`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, missingRequestsErr := openToolingDatabase(t.Context(), missingRequests)
	if missingRequestsErr == nil || !strings.Contains(missingRequestsErr.Error(), "not a patchbay usage database") {
		t.Fatalf("missing accounting_requests error = %v; want patchbay-database refusal", missingRequestsErr)
	}
}
