package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// TestMain builds the migrated template every openStore seeds from, runs the
// package's tests, and removes it.
//
// Migrating a fresh database is most of what opening one costs, and under the
// race detector the transpiled SQLite engine runs that work nearly serially
// across every parallel test. So the migrations run once per test process, and
// a test that only needs a current store starts from a copy of their result.
// Open still validates the copied schema against the canon on the way in.
func TestMain(m *testing.M) {
	dir := filepath.Dir(migratedTemplatePath())
	if err := buildMigratedTemplate(dir); err != nil {
		fmt.Fprintf(os.Stderr, "build migrated template: %v\n", err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr, "remove migrated template: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

// migratedTemplatePath names this test process's template. The process ID
// keeps concurrent test binaries apart without any state to share.
func migratedTemplatePath() string {
	return filepath.Join(
		os.TempDir(),
		fmt.Sprintf("patchbay-usage-template-%d", os.Getpid()),
		"usage.sqlite",
	)
}

// buildMigratedTemplate creates dir under the same private mode Open demands of
// a database's parent and migrates a database inside it. Creating dir fails if
// anything already holds the name, so a stale or planted directory is refused
// rather than trusted.
func buildMigratedTemplate(dir string) error {
	if err := os.Mkdir(dir, parentMode); err != nil {
		return fmt.Errorf("create template directory: %w", err)
	}
	path := migratedTemplatePath()
	store, err := Open(path)
	if err != nil {
		return fmt.Errorf("open template: %w", err)
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("close template: %w", err)
	}
	// Closing the last connection checkpoints the write-ahead log into the main
	// file. A sidecar left behind would hold schema the copied file lacks.
	for _, suffix := range databaseSidecarSuffixes() {
		_, statErr := os.Lstat(path + suffix)
		if statErr == nil {
			return fmt.Errorf("template left %s behind after close", suffix)
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect template %s: %w", suffix, statErr)
		}
	}
	return nil
}

// seedMigratedDatabase writes the migrated template to path when nothing is
// there and its directory already exists. Any other path — an existing
// database being reopened, or a missing directory Open must create under its
// own policy — is left for Open to handle exactly as it would in production.
func seedMigratedDatabase(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return
	}
	template, err := os.ReadFile(migratedTemplatePath())
	if err != nil {
		t.Fatalf("read migrated template: %v", err)
	}
	if err := os.WriteFile(path, template, databaseMode); err != nil {
		t.Fatalf("seed migrated database %q: %v", path, err)
	}
}

func openStore(t *testing.T, path string) *Store {
	t.Helper()

	seedMigratedDatabase(t, path)
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q) returned error: %v", path, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() returned error: %v", err)
		}
	})
	return store
}

func inspectDB(t *testing.T, path string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("inspection DB Close() returned error: %v", err)
		}
	})
	return db
}

func TestOpenCreatesSecureMigratedDatabase(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	parent := filepath.Join(root, "state", "patchbay")
	path := filepath.Join(parent, "usage.sqlite")
	store := openStore(t, path)

	assertMode(t, parent, 0o700)
	assertMode(t, path, 0o600)

	db := store.db
	assertPragma(t, db, "journal_mode", "wal")
	assertPragma(t, db, "foreign_keys", "1")
	assertPragma(t, db, "busy_timeout", "5000")
	assertConnectionPragmas(t, db)

	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read migration version: %v", err)
	}
	if version != responseDetailTimingMigrationVersion {
		t.Fatalf("migration version = %d; want %d", version, responseDetailTimingMigrationVersion)
	}

	var table string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'accounting_requests'`,
	).Scan(&table); err != nil {
		t.Fatalf("find accounting_requests table: %v", err)
	}
}

func TestOpenAcceptsRootOwnedStickyTemporaryDirectory(t *testing.T) {
	tempRoot := assertSharedStickyTemporaryRoot(t)
	t.Setenv("TMPDIR", tempRoot)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatalf("secure temporary usage parent: %v", err)
	}

	path := filepath.Join(parent, "state", "usage.sqlite")
	_ = openStore(t, path)
	assertMode(t, parent, 0o700)
	assertMode(t, filepath.Dir(path), parentMode)
	assertMode(t, path, databaseMode)
}

func TestOpenRejectsSharedWritableImmediateParentBeforeCreatingSidecars(t *testing.T) {
	tempRoot := assertSharedStickyTemporaryRoot(t)
	path := filepath.Join(tempRoot, uniqueTemporaryName(t)+".sqlite")
	t.Cleanup(func() {
		for _, candidate := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
			if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove rejected shared-directory artifact %q: %v", candidate, err)
			}
		}
	})

	store, err := Open(path)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "shared writable immediate parent") {
		t.Fatalf("Open(%q) error = %v; want shared-writable-immediate-parent rejection", path, err)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if _, statErr := os.Lstat(candidate); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("SQLite artifact appeared beneath shared writable parent %q: %v", candidate, statErr)
		}
	}
}

// TestOpenRefusesPreplantedSidecarInSharedWritableParent covers the attack the
// immediate-parent rule exists for: another local UID drops a hostile `-wal`
// beside the database path in a sticky shared directory before Patchbay starts.
// Sticky bits stop a rename of the database itself but not a preplanted sidecar,
// so Patchbay must refuse the parent outright and leave the plant untouched.
func TestOpenRefusesPreplantedSidecarInSharedWritableParent(t *testing.T) {
	tempRoot := assertSharedStickyTemporaryRoot(t)
	path := filepath.Join(tempRoot, uniqueTemporaryName(t)+".sqlite")
	sidecar := path + "-wal"
	plant := []byte("hostile preplanted write-ahead log")
	if err := os.WriteFile(sidecar, plant, 0o666); err != nil {
		t.Fatalf("preplant sidecar %q: %v", sidecar, err)
	}
	if err := os.Chmod(sidecar, 0o666); err != nil {
		t.Fatalf("expose preplanted sidecar %q: %v", sidecar, err)
	}
	t.Cleanup(func() {
		for _, candidate := range []string{path, sidecar, path + "-shm", path + "-journal"} {
			if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove preplant fixture %q: %v", candidate, err)
			}
		}
	})

	store, err := Open(path)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "shared writable immediate parent") {
		t.Fatalf("Open(%q) error = %v; want shared-writable-immediate-parent rejection", path, err)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("database adopted a preplanted sidecar in %q: %v", tempRoot, statErr)
	}
	got, readErr := os.ReadFile(sidecar)
	if readErr != nil {
		t.Fatalf("read preplanted sidecar after rejection: %v", readErr)
	}
	if string(got) != string(plant) {
		t.Errorf("preplanted sidecar contents = %q; want %q", got, plant)
	}
	assertMode(t, sidecar, 0o666)
}

// TestOpenRejectsUnsafeExistingSidecarInPrivateParent covers the hole the
// immediate-parent rule leaves open. A private 0700 parent stops another local UID
// from creating a sidecar now, but says nothing about one already sitting there,
// planted while the directory was briefly exposed. SQLite opens `-wal`, `-shm`,
// and `-journal` by pathname during Ping and hot-journal recovery, so Patchbay
// must inspect every existing sidecar before SQLite ever sees the database path,
// and must leave a rejected plant exactly as it found it.
//
// The inspection reads current state, so these cases are all sidecars that are
// unsafe right now. A sidecar that passes every check can still be held open for
// writing by a UID that opened it during that earlier exposure; nothing here
// detects that, and the README documents it as a residual with a fresh-inode
// remediation.
func TestOpenRejectsUnsafeExistingSidecarInPrivateParent(t *testing.T) {
	t.Parallel()

	plants := []struct {
		name    string
		plant   func(t *testing.T, sidecar, spare string)
		wantErr string
		verify  func(t *testing.T, sidecar string)
	}{
		{
			name: "writable by another local user",
			plant: func(t *testing.T, sidecar, _ string) {
				t.Helper()
				if err := os.WriteFile(sidecar, []byte("hostile preplanted sidecar"), 0o600); err != nil {
					t.Fatalf("preplant sidecar %q: %v", sidecar, err)
				}
				if err := os.Chmod(sidecar, 0o666); err != nil {
					t.Fatalf("expose preplanted sidecar %q: %v", sidecar, err)
				}
			},
			wantErr: "writable by another local user",
			verify: func(t *testing.T, sidecar string) {
				t.Helper()
				got, readErr := os.ReadFile(sidecar)
				if readErr != nil {
					t.Fatalf("read preplanted sidecar after rejection: %v", readErr)
				}
				if string(got) != "hostile preplanted sidecar" {
					t.Errorf("preplanted sidecar contents = %q; want it untouched", got)
				}
				assertMode(t, sidecar, 0o666)
			},
		},
		{
			name: "symbolic link",
			plant: func(t *testing.T, sidecar, spare string) {
				t.Helper()
				if err := os.WriteFile(spare, []byte("redirect target"), 0o600); err != nil {
					t.Fatalf("create sidecar link target %q: %v", spare, err)
				}
				if err := os.Symlink(spare, sidecar); err != nil {
					t.Fatalf("preplant sidecar symlink %q: %v", sidecar, err)
				}
			},
			wantErr: "symbolic link",
			verify: func(t *testing.T, sidecar string) {
				t.Helper()
				info, err := os.Lstat(sidecar)
				if err != nil {
					t.Fatalf("lstat preplanted sidecar symlink: %v", err)
				}
				if info.Mode()&os.ModeSymlink == 0 {
					t.Errorf("preplanted sidecar %q is no longer a symbolic link: mode %v", sidecar, info.Mode())
				}
			},
		},
		{
			name: "non-regular file",
			plant: func(t *testing.T, sidecar, _ string) {
				t.Helper()
				if err := os.Mkdir(sidecar, 0o700); err != nil {
					t.Fatalf("preplant sidecar directory %q: %v", sidecar, err)
				}
			},
			wantErr: "not a regular file",
			verify: func(t *testing.T, sidecar string) {
				t.Helper()
				info, err := os.Lstat(sidecar)
				if err != nil {
					t.Fatalf("lstat preplanted sidecar directory: %v", err)
				}
				if !info.IsDir() {
					t.Errorf("preplanted sidecar %q is no longer a directory: mode %v", sidecar, info.Mode())
				}
			},
		},
	}

	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		for _, plant := range plants {
			t.Run(suffix+" "+plant.name, func(t *testing.T) {
				t.Parallel()

				parent := t.TempDir()
				path := filepath.Join(parent, "usage.sqlite")
				closedToolingSource(t, path)
				// A safe-but-unsecured leaf mode proves the sidecar verdict lands
				// before the leaf is opened: reaching openSecureDatabaseFile would
				// chmod this back to 0600.
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatalf("relax database leaf mode: %v", err)
				}
				sidecar := path + suffix
				plant.plant(t, sidecar, filepath.Join(parent, "spare"))

				store, err := Open(path)
				if store != nil {
					_ = store.Close()
				}
				if err == nil || !strings.Contains(err.Error(), plant.wantErr) {
					t.Fatalf("Open(%q) error = %v; want sidecar %q rejection", path, err, plant.wantErr)
				}
				if !strings.Contains(err.Error(), sidecar) {
					t.Errorf("Open(%q) error = %v; want it to name the rejected sidecar %q", path, err, sidecar)
				}
				assertMode(t, path, 0o644)
				plant.verify(t, sidecar)
				for _, other := range []string{"-wal", "-shm", "-journal"} {
					if other == suffix {
						continue
					}
					if _, statErr := os.Lstat(path + other); !errors.Is(statErr, os.ErrNotExist) {
						t.Errorf("SQLite sidecar %q appeared for a rejected database: %v", path+other, statErr)
					}
				}
			})
		}
	}
}

// TestOpenAcceptsMissingAndSQLiteOwnedSidecars guards the other direction: sidecar
// validation must not reject the ordinary shapes. A quiescent database has none,
// and a live one has `-wal` and `-shm` that SQLite created from the database's own
// private mode.
func TestOpenAcceptsMissingAndSQLiteOwnedSidecars(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	closedToolingSource(t, path)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("quiescent source has sidecar %q: %v", path+suffix, err)
		}
	}

	live := openStore(t, path)
	if _, err := live.InsertPending(t.Context(), validPendingRequest()); err != nil {
		t.Fatalf("seed request: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		info, err := os.Lstat(path + suffix)
		if err != nil {
			t.Fatalf("live database is missing sidecar %q: %v", path+suffix, err)
		}
		if !databaseFileModeSafe(info.Mode()) {
			t.Fatalf("SQLite sidecar %q mode = %#o; want no group or world write", path+suffix, info.Mode().Perm())
		}
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q) with SQLite-owned sidecars present error = %v; want acceptance", path, err)
	}
	if closeErr := second.Close(); closeErr != nil {
		t.Fatalf("close second store: %v", closeErr)
	}
}

// assertReturnsPromptly runs work on its own goroutine and fails if it has not
// returned within the deadline. A blocking open has no other symptom: the process
// simply never comes back, so only a timeout can express the assertion.
func assertReturnsPromptly(t *testing.T, deadline time.Duration, work func() error) error {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- work() }()
	select {
	case err := <-done:
		return err
	case <-time.After(deadline):
		t.Fatalf("call did not return within %s; the open blocked on a special file", deadline)
		return nil
	}
}

// TestOpenRejectsBlockingSpecialFileDatabase covers the shapes that make an
// unvalidated open a denial of service rather than a refusal. A FIFO planted at
// the database path, or at a sidecar path, parks a plain blocking open until a
// peer appears — which for an attacker-planted pipe is never. The no-follow opens
// carry O_NONBLOCK so the descriptor comes back immediately and fstat can refuse
// it; O_NONBLOCK is ignored for the regular files that are actually accepted.
func TestOpenRejectsBlockingSpecialFileDatabase(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		build   func(t *testing.T, parent string) (path, blocking string)
		wantErr string
	}{
		{
			name: "database leaf is a named pipe",
			build: func(t *testing.T, parent string) (string, string) {
				t.Helper()
				path := filepath.Join(parent, "usage.sqlite")
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatalf("create FIFO database %q: %v", path, err)
				}
				return path, path
			},
			wantErr: "not a regular file",
		},
		{
			name: "database leaf is a socket",
			build: func(t *testing.T, parent string) (string, string) {
				t.Helper()
				path := filepath.Join(parent, "usage.sqlite")
				plantUnixSocket(t, path)
				return path, path
			},
			wantErr: "open usage database",
		},
		{
			name: "sidecar is a named pipe",
			build: func(t *testing.T, parent string) (string, string) {
				t.Helper()
				path := filepath.Join(parent, "usage.sqlite")
				closedToolingSource(t, path)
				sidecar := path + "-wal"
				if err := unix.Mkfifo(sidecar, 0o600); err != nil {
					t.Fatalf("create FIFO sidecar %q: %v", sidecar, err)
				}
				return path, sidecar
			},
			wantErr: "sidecar",
		},
		{
			name: "sidecar is a socket",
			build: func(t *testing.T, parent string) (string, string) {
				t.Helper()
				path := filepath.Join(parent, "usage.sqlite")
				closedToolingSource(t, path)
				sidecar := path + "-shm"
				plantUnixSocket(t, sidecar)
				return path, sidecar
			},
			wantErr: "sidecar",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			parent := t.TempDir()
			path, blocking := test.build(t, parent)
			before := assertSpecialFileMode(t, blocking)

			err := assertReturnsPromptly(t, 10*time.Second, func() error {
				store, openErr := Open(path)
				if store != nil {
					_ = store.Close()
				}
				return openErr
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Open(%q) error = %v; want %q refusal", path, err, test.wantErr)
			}

			assertSpecialFileUnchanged(t, blocking, before)
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				sidecar := path + suffix
				if sidecar == blocking {
					continue
				}
				if _, sidecarErr := os.Lstat(sidecar); !errors.Is(sidecarErr, os.ErrNotExist) {
					t.Errorf("SQLite sidecar %q appeared for a rejected database: %v", sidecar, sidecarErr)
				}
			}
		})
	}
}

// plantUnixSocket leaves a bound socket at path. open(2) refuses a socket with
// ENXIO rather than blocking, so it exercises the other half of the special-file
// question: the open/fstat path must reject every non-regular shape it meets.
func plantUnixSocket(t *testing.T, path string) {
	t.Helper()

	listener, listenErr := net.Listen("unix", path)
	if listenErr != nil {
		t.Fatalf("create socket %q: %v", path, listenErr)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil {
			t.Errorf("close socket %q: %v", path, closeErr)
		}
	})
}

func assertSpecialFileMode(t *testing.T, path string) os.FileMode {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat planted special file %q: %v", path, err)
	}
	if info.Mode()&os.ModeType == 0 {
		t.Fatalf("planted fixture %q is a regular file: mode %v", path, info.Mode())
	}
	return info.Mode()
}

// assertSpecialFileUnchanged proves a rejected special file was neither replaced
// nor chmodded: comparing the whole FileMode catches a type change and a
// permission change alike.
func assertSpecialFileUnchanged(t *testing.T, path string, before os.FileMode) {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat rejected special file %q: %v", path, err)
	}
	if info.Mode() != before {
		t.Errorf("rejected special file %q mode changed from %v to %v", path, before, info.Mode())
	}
}

func TestPrepareDatabasePathPrecreatesSecureDatabase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "usage.sqlite")
	if err := prepareDatabasePath(path); err != nil {
		t.Fatalf("prepareDatabasePath(%q): %v", path, err)
	}
	assertMode(t, filepath.Dir(path), parentMode)
	assertMode(t, path, databaseMode)
}

func TestPrepareDatabasePathFailsClosedOnPathologicalUmask(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "usage.sqlite")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPrepareDatabasePathFailsClosedOnPathologicalUmaskHelper$")
	cmd.Env = append(os.Environ(), "PATCHBAY_USAGE_RESTRICTIVE_UMASK_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prepareDatabasePath pathological-umask helper: %v\n%s", err, output)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore pathological-umask fixture mode: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database created under pathological umask: %v", err)
	}
}

func TestPrepareDatabasePathFailsClosedOnPathologicalUmaskHelper(t *testing.T) {
	path := os.Getenv("PATCHBAY_USAGE_RESTRICTIVE_UMASK_PATH")
	if path == "" {
		return
	}

	previousUmask := syscall.Umask(0o777)
	defer syscall.Umask(previousUmask)

	err := prepareDatabasePath(path)
	if err == nil || !strings.Contains(err.Error(), "umask") {
		t.Fatalf("prepareDatabasePath(%q) error = %v; want pathological-umask refusal", path, err)
	}
}

func TestSecureCreatedDatabaseReportsChmodAndCloseFailures(t *testing.T) {
	t.Parallel()

	chmodFailure := errors.New("chmod failed")
	closeFailure := errors.New("close failed")
	for _, tc := range []struct {
		name     string
		chmodErr error
		closeErr error
	}{
		{name: "chmod", chmodErr: chmodFailure},
		{name: "close", closeErr: closeFailure},
		{name: "both", chmodErr: chmodFailure, closeErr: closeFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var calls []string
			err := secureCreatedDatabase(
				"usage.sqlite",
				123,
				func(int, uint32) error {
					calls = append(calls, "chmod")
					return tc.chmodErr
				},
				func(int) error {
					calls = append(calls, "close")
					return tc.closeErr
				},
			)
			if err == nil {
				t.Fatal("secureCreatedDatabase returned nil error")
			}
			if tc.chmodErr != nil && !errors.Is(err, tc.chmodErr) {
				t.Errorf("error = %v; want chmod error %v", err, tc.chmodErr)
			}
			if tc.closeErr != nil && !errors.Is(err, tc.closeErr) {
				t.Errorf("error = %v; want close error %v", err, tc.closeErr)
			}
			if got := strings.Join(calls, ","); got != "chmod,close" {
				t.Errorf("operations = %q; want chmod,close", got)
			}
		})
	}
}

func TestOpenRejectsDatabaseSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	targetParent := filepath.Join(root, "target")
	if err := os.Mkdir(targetParent, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetParent, "target.sqlite")
	contents := []byte("must remain unopened")
	if err := os.WriteFile(target, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "private", "usage.sqlite")
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Open(%q) error = %v; want symbolic-link rejection", path, err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(contents) {
		t.Fatalf("symlink target contents = %q; want %q", got, contents)
	}
	assertMode(t, targetParent, 0o755)
	assertMode(t, target, 0o644)
}

func TestDatabaseParentDescriptorSafety(t *testing.T) {
	t.Parallel()

	const effectiveUID = 1000
	for _, test := range []struct {
		name string
		stat unix.Stat_t
		want bool
	}{
		{
			name: "root-owned ordinary ancestor",
			stat: unix.Stat_t{Mode: unix.S_IFDIR | 0o755, Uid: 0},
			want: true,
		},
		{
			name: "effective-UID-owned private ancestor",
			stat: unix.Stat_t{Mode: unix.S_IFDIR | 0o700, Uid: effectiveUID},
			want: true,
		},
		{
			name: "root-owned sticky shared ancestor",
			stat: unix.Stat_t{Mode: unix.S_IFDIR | unix.S_ISVTX | 0o777, Uid: 0},
			want: true,
		},
		{
			name: "root-owned replaceable ancestor",
			stat: unix.Stat_t{Mode: unix.S_IFDIR | 0o777, Uid: 0},
			want: false,
		},
		{
			name: "effective-UID-owned group-writable ancestor",
			stat: unix.Stat_t{Mode: unix.S_IFDIR | 0o770, Uid: effectiveUID},
			want: false,
		},
		{
			name: "foreign-owned private ancestor",
			stat: unix.Stat_t{Mode: unix.S_IFDIR | 0o700, Uid: effectiveUID + 1},
			want: false,
		},
		{
			name: "foreign-owned sticky ancestor",
			stat: unix.Stat_t{Mode: unix.S_IFDIR | unix.S_ISVTX | 0o777, Uid: effectiveUID + 1},
			want: false,
		},
		{
			name: "non-directory",
			stat: unix.Stat_t{Mode: unix.S_IFREG | 0o600, Uid: effectiveUID},
			want: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := databaseParentDescriptorSafe(test.stat, effectiveUID); got != test.want {
				t.Fatalf("databaseParentDescriptorSafe(%#v, %d) = %t; want %t", test.stat, effectiveUID, got, test.want)
			}
		})
	}
}

func TestVerifyDescriptorACL(t *testing.T) {
	t.Parallel()

	inspectionFailure := errors.New("ACL inspection failed")
	for _, test := range []struct {
		name       string
		hasACL     bool
		inspectErr error
		wantErr    string
	}{
		{name: "no extended ACL"},
		{name: "extended ACL", hasACL: true, wantErr: "extended ACL"},
		{name: "inspection failure", inspectErr: inspectionFailure, wantErr: inspectionFailure.Error()},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := verifyDescriptorACL(
				"usage database",
				"usage.sqlite",
				123,
				func(fd int) (bool, error) {
					if fd != 123 {
						t.Fatalf("ACL inspection fd = %d; want 123", fd)
					}
					return test.hasACL, test.inspectErr
				},
			)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyDescriptorACL() error = %v; want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("verifyDescriptorACL() error = %v; want %q", err, test.wantErr)
			}
		})
	}
}

func TestDatabaseFileOwnerSafety(t *testing.T) {
	t.Parallel()

	const effectiveUID = 1000
	for _, test := range []struct {
		name    string
		owner   uint32
		created bool
		want    bool
	}{
		{name: "existing root-owned leaf", owner: 0, want: true},
		{name: "existing effective-UID-owned leaf", owner: effectiveUID, want: true},
		{name: "existing foreign-owned leaf", owner: effectiveUID + 1, want: false},
		{name: "created root-owned leaf", owner: 0, created: true, want: false},
		{name: "created effective-UID-owned leaf", owner: effectiveUID, created: true, want: true},
		{name: "created foreign-owned leaf", owner: effectiveUID + 1, created: true, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := databaseFileOwnerSafe(test.owner, effectiveUID, test.created); got != test.want {
				t.Fatalf(
					"databaseFileOwnerSafe(%d, %d, %t) = %t; want %t",
					test.owner,
					effectiveUID,
					test.created,
					got,
					test.want,
				)
			}
		})
	}
}

func TestDatabaseFileModeSafety(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		mode os.FileMode
		want bool
	}{
		{name: "private", mode: 0o600, want: true},
		{name: "group and world readable", mode: 0o644, want: true},
		{name: "group writable", mode: 0o660, want: false},
		{name: "world writable", mode: 0o602, want: false},
		{name: "group and world writable", mode: 0o666, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := databaseFileModeSafe(test.mode); got != test.want {
				t.Fatalf("databaseFileModeSafe(%#o) = %t; want %t", test.mode, got, test.want)
			}
		})
	}
}

func TestSecureExistingDatabaseDescriptorRejectsForeignOwnerWithoutMutation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	contents := []byte("must remain untouched")
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}

	const effectiveUID = 1000
	var calls []string
	err = secureExistingDatabaseDescriptor(
		path,
		fd,
		effectiveUID+1,
		effectiveUID,
		func(fd int, mode uint32) error {
			calls = append(calls, "chmod")
			return syscall.Fchmod(fd, mode)
		},
		func(fd int) error {
			calls = append(calls, "close")
			return syscall.Close(fd)
		},
	)
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("secureExistingDatabaseDescriptor() error = %v; want foreign-owner rejection", err)
	}
	if got := strings.Join(calls, ","); got != "close" {
		t.Errorf("operations = %q; want close without chmod", got)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Errorf("read database after rejection: %v", readErr)
	} else if string(got) != string(contents) {
		t.Errorf("database contents after rejection = %q; want %q", got, contents)
	}
	assertMode(t, path, 0o644)
}

// TestSecureExistingDatabaseDescriptorJoinsForeignOwnerAndCloseFailures proves
// the owner rejection still runs the descriptor close, reports both causes
// through errors.Is, and never reaches the chmod that would mutate the file.
func TestSecureExistingDatabaseDescriptorJoinsForeignOwnerAndCloseFailures(t *testing.T) {
	t.Parallel()

	closeFailure := errors.New("close failed")
	const effectiveUID = 1000
	var calls []string
	err := secureExistingDatabaseDescriptor(
		"usage.sqlite",
		123,
		effectiveUID+1,
		effectiveUID,
		func(int, uint32) error {
			calls = append(calls, "chmod")
			return nil
		},
		func(int) error {
			calls = append(calls, "close")
			return closeFailure
		},
	)
	if err == nil {
		t.Fatal("secureExistingDatabaseDescriptor() error = nil; want foreign-owner rejection")
	}
	if !errors.Is(err, closeFailure) {
		t.Errorf("errors.Is(err, closeFailure) = false; error = %v", err)
	}
	if !strings.Contains(err.Error(), "unsafe owner uid") {
		t.Errorf("error = %v; want the foreign-owner cause preserved alongside the close failure", err)
	}
	if got := strings.Join(calls, ","); got != "close" {
		t.Errorf("operations = %q; want close without chmod", got)
	}
}

func TestCreatedDatabaseParentRejectsSwappedWrongModeDirectoryWithoutMutation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "swapped")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(
		path,
		syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := syscall.Close(fd); closeErr != nil {
			t.Errorf("close swapped directory descriptor: %v", closeErr)
		}
	}()

	err = verifyCreatedDatabaseParentDescriptor(path, fd, os.Geteuid())
	if err == nil || !strings.Contains(err.Error(), "umask") {
		t.Fatalf("verifyCreatedDatabaseParentDescriptor() error = %v; want mode/umask refusal", err)
	}
	assertMode(t, path, 0o755)
}

func TestOpenLeavesPreexistingParentModeUntouched(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	parent := filepath.Join(root, "state")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "usage.sqlite")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	_ = openStore(t, path)
	assertMode(t, parent, 0o755)
	assertMode(t, path, 0o600)
}

func TestOpenRejectsWritableExistingDatabaseWithoutMutation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	contents := []byte("must remain untouched")
	if err := os.WriteFile(path, contents, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "writable by another local user") {
		t.Errorf("Open(%q) error = %v; want writable-existing-database rejection", path, err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Errorf("read database after rejection: %v", readErr)
	} else if string(got) != string(contents) {
		t.Errorf("database contents after rejection = %q; want %q", got, contents)
	}
	assertMode(t, path, 0o666)
}

func TestOpenRejectsReplaceablePreexistingParentBeforeCreatingDatabase(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "usage.sqlite")

	store, err := Open(path)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "replace") {
		t.Fatalf("Open(%q) error = %v; want replaceable-parent rejection", path, err)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("database created beneath replaceable parent: %v", statErr)
	}
	assertMode(t, parent, 0o777)
}

func TestOpenRejectsUnsafeEarlierAncestorBeforeCreatingDatabase(t *testing.T) {
	t.Parallel()

	unsafeAncestor := t.TempDir()
	if err := os.Chmod(unsafeAncestor, 0o777); err != nil {
		t.Fatal(err)
	}
	immediateParent := filepath.Join(unsafeAncestor, "private")
	if err := os.Mkdir(immediateParent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(immediateParent, "usage.sqlite")

	store, err := Open(path)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "replace") {
		t.Fatalf("Open(%q) error = %v; want unsafe-earlier-ancestor rejection", path, err)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("database created beneath unsafe earlier ancestor: %v", statErr)
	}
	assertMode(t, unsafeAncestor, 0o777)
	assertMode(t, immediateParent, 0o700)
}

func TestOpenRejectsReplaceablePreexistingParentWithoutMutatingDatabase(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "usage.sqlite")
	contents := []byte("must remain untouched")
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "replace") {
		t.Errorf("Open(%q) error = %v; want replaceable-parent rejection", path, err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Errorf("read database after rejection: %v", readErr)
	} else if string(got) != string(contents) {
		t.Errorf("database contents after rejection = %q; want %q", got, contents)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("stat database after rejection: %v", statErr)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o644 {
		t.Errorf("database mode after rejection = %#o; want %#o", gotMode, os.FileMode(0o644))
	}
	assertMode(t, parent, 0o777)
}

func TestOpenRejectsParentPathSymlinkWithoutChangingTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "state")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(link, "usage.sqlite")
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Open(%q) error = %v; want parent symbolic-link rejection", path, err)
	}
	assertMode(t, target, 0o755)
	if _, err := os.Lstat(filepath.Join(target, "usage.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database appeared through parent symlink: %v", err)
	}
}

func TestOpenDatabaseRechecksPreparedFileIdentity(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "usage.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("create original database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close original database: %v", err)
	}
	identity, err := prepareDatabasePathIdentity(path)
	if err != nil {
		t.Fatalf("prepare original database: %v", err)
	}

	replacement := filepath.Join(dir, "replacement.sqlite")
	replacementStore, err := Open(replacement)
	if err != nil {
		t.Fatalf("create replacement database: %v", err)
	}
	if err := replacementStore.Close(); err != nil {
		t.Fatalf("close replacement database: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replace prepared database: %v", err)
	}

	db, err := openPreparedDatabase(path, identity)
	if db != nil {
		_ = db.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("openPreparedDatabase() error = %v; want identity-change refusal", err)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db := inspectDB(t, path)
	var count, version int
	if err := db.QueryRow(`SELECT COUNT(*), MAX(version) FROM schema_migrations`).
		Scan(&count, &version); err != nil {
		t.Fatal(err)
	}
	if count != responseDetailTimingMigrationVersion ||
		version != responseDetailTimingMigrationVersion {
		t.Fatalf("migration rows = %d, max version = %d; want %d, %d",
			count,
			version,
			responseDetailTimingMigrationVersion,
			responseDetailTimingMigrationVersion,
		)
	}
}

func TestAccountingRequestsSchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	_ = openStore(t, path)
	db := inspectDB(t, path)

	type column struct {
		name     string
		typeName string
		notNull  int
		pk       int
	}
	want := []column{
		{"id", "INTEGER", 0, 1},
		{"started_at", "INTEGER", 1, 0},
		{"completed_at", "INTEGER", 0, 0},
		{"upstream_dispatched_at", "INTEGER", 0, 0},
		{"request_kind", "TEXT", 1, 0},
		{"lifecycle_state", "TEXT", 1, 0},
		{"outcome", "TEXT", 0, 0},
		{"upstream_contacted", "INTEGER", 1, 0},
		{"route_host", "TEXT", 1, 0},
		{"route_context", "TEXT", 1, 0},
		{"route_alias", "TEXT", 0, 0},
		{"route_upstream", "TEXT", 1, 0},
		{"request_model", "TEXT", 1, 0},
		{"resolved_model", "TEXT", 1, 0},
		{"auth_mode", "TEXT", 1, 0},
		{"credential_class", "TEXT", 1, 0},
		{"billing_policy", "TEXT", 1, 0},
		{"billing_class", "TEXT", 0, 0},
		{"cost_status", "TEXT", 1, 0},
		{"response_id", "TEXT", 0, 0},
		{"http_status", "INTEGER", 0, 0},
		{"normalization_complete", "INTEGER", 0, 0},
		{"input_tokens", "INTEGER", 0, 0},
		{"output_tokens", "INTEGER", 0, 0},
		{"cache_creation_input_tokens", "INTEGER", 0, 0},
		{"cache_read_input_tokens", "INTEGER", 0, 0},
		{"cache_creation_5m_input_tokens", "INTEGER", 0, 0},
		{"cache_creation_1h_input_tokens", "INTEGER", 0, 0},
		{"provider_cost_present", "INTEGER", 0, 0},
		{"provider_cost_valid", "INTEGER", 0, 0},
		{"provider_cost_nano_usd", "INTEGER", 0, 0},
		{"stop_reason", "TEXT", 0, 0},
		{"block_shape", "TEXT", 0, 0},
		{"duration_ms", "INTEGER", 0, 0},
		{"upstream_ms", "INTEGER", 0, 0},
		{"cost_basis", "TEXT", 0, 0},
		{"final_cost_nano_usd", "INTEGER", 0, 0},
		{"rate_card_id", "INTEGER", 0, 0},
		{"import_source", "TEXT", 0, 0},
		{"import_line", "INTEGER", 0, 0},
		{"upstream_target", "TEXT", 0, 0},
		{"reasoning_output_tokens", "INTEGER", 0, 0},
		{"first_content_at", "INTEGER", 0, 0},
	}

	rows, err := db.Query(`PRAGMA table_info(accounting_requests)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []column
	for rows.Next() {
		var cid int
		var col column
		var defaultValue any
		if err := rows.Scan(
			&cid,
			&col.name,
			&col.typeName,
			&col.notNull,
			&defaultValue,
			&col.pk,
		); err != nil {
			t.Fatal(err)
		}
		got = append(got, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema mismatch:\n got %#v\nwant %#v", got, want)
	}
}

func TestFinalizedResponseAccountingMigrationDeclarationAndIndex(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)

	var name string
	if err := store.db.QueryRow(
		`SELECT name FROM schema_migrations WHERE version = 2`,
	).Scan(&name); err != nil {
		t.Fatalf("read migration 2 name: %v", err)
	}
	if name != "finalized_response_accounting" {
		t.Fatalf("migration 2 name = %q; want %q", name, "finalized_response_accounting")
	}

	var indexSQL string
	if err := store.db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type = 'index' AND name = 'accounting_requests_finalized_response_id'`).Scan(&indexSQL); err != nil {
		t.Fatalf("read finalized response ID index: %v", err)
	}
	want := "CREATE UNIQUE INDEX accounting_requests_finalized_response_id ON accounting_requests(response_id) WHERE lifecycle_state = 'finalized' AND response_id IS NOT NULL AND response_id <> ''"
	if indexSQL != want {
		t.Fatalf("finalized response ID index SQL = %q; want %q", indexSQL, want)
	}
}

func TestImportedLedgerProvenanceMigrationAndInsertTrigger(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	first := insertPendingRequest(t, store)
	second := insertPendingRequest(t, store)
	if _, err := store.db.Exec(
		`UPDATE accounting_requests SET import_source = 'source', import_line = 1 WHERE id = ?`,
		first,
	); err != nil {
		t.Fatalf("set provenance: %v", err)
	}
	if _, err := store.db.Exec(
		`UPDATE accounting_requests SET import_source = 'source' WHERE id = ?`,
		second,
	); err == nil {
		t.Fatal("partial import provenance update returned nil error")
	}
	if _, err := store.db.Exec(
		`UPDATE accounting_requests SET import_source = 'source', import_line = 1 WHERE id = ?`,
		second,
	); err == nil {
		t.Fatal("duplicate import provenance update returned nil error")
	}
	if _, err := store.db.Exec(`INSERT INTO accounting_requests (
		started_at, request_kind, lifecycle_state, upstream_contacted, route_host, route_context,
		route_upstream, request_model, resolved_model, auth_mode, credential_class, billing_policy,
		cost_status
	) VALUES (1, 'other', 'finalized', 0, 'host', '', 'upstream', 'model', 'model',
		'inject', '', 'explicit', 'unknown')`); err == nil || !strings.Contains(err.Error(), "invalid finalized accounting snapshot") {
		t.Fatalf("incomplete finalized INSERT error = %v; want finalized snapshot rejection", err)
	}
}

func TestUpstreamTargetDispatchEvidenceMigrationPreservesVersionFourRows(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	db := inspectDB(t, path)
	if err := applyMigrations(t.Context(), db, declaredMigrations()[:4]); err != nil {
		t.Fatalf("apply migrations 1-4: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO accounting_requests (
		started_at, completed_at, upstream_dispatched_at, request_kind, lifecycle_state,
		outcome, upstream_contacted, route_host, route_context, route_alias, route_upstream,
		request_model, resolved_model, auth_mode, credential_class, billing_policy,
		billing_class, cost_status
	) VALUES (123, NULL, NULL, 'messages', 'pending', NULL, 0, 'gnomon', 'personal', NULL,
		'https://api.anthropic.com', 'claude-sonnet-5', 'claude-sonnet-5', 'forward', 'oauth',
		'credential_derived', 'subscription', 'pending')`); err != nil {
		t.Fatalf("insert version-4 row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close version-4 database: %v", err)
	}

	store := openStore(t, path)
	var startedAt int64
	var target sql.NullString
	if err := store.db.QueryRow(
		`SELECT started_at, upstream_target FROM accounting_requests WHERE id = 1`,
	).Scan(&startedAt, &target); err != nil {
		t.Fatalf("read upgraded row: %v", err)
	}
	if startedAt != 123 || target.Valid {
		t.Fatalf("upgraded row = start %d target %#v; want preserved start and NULL target", startedAt, target)
	}
	if _, err := store.db.Exec(`UPDATE accounting_requests SET upstream_target = '' WHERE id = 1`); err == nil {
		t.Fatal("empty upstream target update returned nil error")
	}
	if _, err := store.db.Exec(
		`UPDATE accounting_requests SET upstream_target = 'arn:example' WHERE id = 1`,
	); err == nil {
		t.Fatal("target without upstream contact update returned nil error")
	}
}

func TestSummaryIndexesMigrationPreservesVersionFiveRows(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	db := inspectDB(t, path)
	through := declaredMigrations()[:upstreamTargetDispatchEvidenceMigrationVersion]
	if err := applyMigrations(t.Context(), db, through); err != nil {
		t.Fatalf("apply migrations through version 5: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO accounting_requests (
		started_at, request_kind, lifecycle_state, upstream_contacted, route_host, route_context,
		route_upstream, request_model, resolved_model, auth_mode, credential_class, billing_policy,
		cost_status
	) VALUES (123, 'messages', 'pending', 0, 'gnomon', 'personal',
		'https://api.anthropic.com', 'claude-sonnet-5', 'claude-sonnet-5', 'forward', 'oauth',
		'credential_derived', 'pending')`); err != nil {
		t.Fatalf("insert version-5 row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close version-5 database: %v", err)
	}

	store := openStore(t, path)
	var startedAt int64
	if err := store.db.QueryRow(
		`SELECT started_at FROM accounting_requests WHERE id = 1`,
	).Scan(&startedAt); err != nil {
		t.Fatalf("read upgraded row: %v", err)
	}
	if startedAt != 123 {
		t.Fatalf("upgraded row started_at = %d; want 123", startedAt)
	}
	for _, name := range []string{summaryWindowIndexName, summaryContextWindowIndexName} {
		var found string
		if err := store.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`,
			name,
		).Scan(&found); err != nil {
			t.Fatalf("read summary index %q: %v", name, err)
		}
	}
}

func TestFinalizedAccountingColumnConstraints(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		column string
		value  int64
	}{
		{name: "normalization boolean below range", column: "normalization_complete", value: -1},
		{name: "normalization boolean above range", column: "normalization_complete", value: 2},
		{name: "provider present boolean", column: "provider_cost_present", value: 2},
		{name: "provider valid boolean", column: "provider_cost_valid", value: -1},
		{name: "input tokens", column: "input_tokens", value: -1},
		{name: "output tokens", column: "output_tokens", value: -1},
		{name: "cache creation tokens", column: "cache_creation_input_tokens", value: -1},
		{name: "cache read tokens", column: "cache_read_input_tokens", value: -1},
		{name: "cache 5m tokens", column: "cache_creation_5m_input_tokens", value: -1},
		{name: "cache 1h tokens", column: "cache_creation_1h_input_tokens", value: -1},
		{name: "provider cost", column: "provider_cost_nano_usd", value: -1},
		{name: "duration", column: "duration_ms", value: -1},
		{name: "upstream duration", column: "upstream_ms", value: -1},
		{name: "HTTP below range", column: "http_status", value: 99},
		{name: "HTTP above range", column: "http_status", value: 600},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			store := openStore(t, path)
			id, err := store.InsertPending(context.Background(), validPendingRequest())
			if err != nil {
				t.Fatalf("InsertPending() returned error: %v", err)
			}

			query := fmt.Sprintf(`UPDATE accounting_requests SET %s = ? WHERE id = ?`, test.column)
			if _, err := store.db.Exec(query, test.value, id); err == nil {
				t.Fatalf("setting %s to %d returned nil error", test.column, test.value)
			}
			var stored sql.NullInt64
			query = fmt.Sprintf(`SELECT %s FROM accounting_requests WHERE id = ?`, test.column)
			if err := store.db.QueryRow(query, id).Scan(&stored); err != nil {
				t.Fatalf("read %s: %v", test.column, err)
			}
			if stored.Valid {
				t.Fatalf("%s = %#v after rejected update; want NULL", test.column, stored)
			}
		})
	}
}

func TestFinalizedSnapshotTriggerPreservesPendingRows(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	id := insertPendingRequest(t, store)

	if _, err := store.db.Exec(`UPDATE accounting_requests
		SET lifecycle_state = 'finalized' WHERE id = ?`, id); err == nil {
		t.Fatal("incomplete finalized snapshot update returned nil error")
	}
	var lifecycle string
	if err := store.db.QueryRow(`SELECT lifecycle_state FROM accounting_requests WHERE id = ?`, id).
		Scan(&lifecycle); err != nil {
		t.Fatalf("read lifecycle after trigger failure: %v", err)
	}
	if lifecycle != "pending" {
		t.Fatalf("lifecycle after trigger failure = %q; want pending", lifecycle)
	}

	if _, err := store.db.Exec(`UPDATE accounting_requests SET
		completed_at = started_at,
		lifecycle_state = 'finalized',
		outcome = 'success',
		cost_status = 'known',
		normalization_complete = 1,
		provider_cost_present = 0,
		provider_cost_valid = 1,
		provider_cost_nano_usd = 0,
		stop_reason = '',
		block_shape = '[]',
		duration_ms = 0,
		upstream_ms = 0
		WHERE id = ?`, id); err == nil {
		t.Fatal("invalid provider-cost tuple update returned nil error")
	}
	if err := store.db.QueryRow(`SELECT lifecycle_state FROM accounting_requests WHERE id = ?`, id).
		Scan(&lifecycle); err != nil {
		t.Fatalf("read lifecycle after tuple trigger failure: %v", err)
	}
	if lifecycle != "pending" {
		t.Fatalf("lifecycle after tuple trigger failure = %q; want pending", lifecycle)
	}
}

func TestRateCardsMigrationPreservesExistingPendingData(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	db := inspectDB(t, path)
	if err := applyMigrations(t.Context(), db, declaredMigrations()[:2]); err != nil {
		t.Fatalf("apply migrations 1-2: %v", err)
	}
	const startedAt int64 = 1_776_912_345_678_901_234
	if _, err := db.Exec(`INSERT INTO accounting_requests (
		started_at, completed_at, upstream_dispatched_at, request_kind, lifecycle_state,
		outcome, upstream_contacted, route_host, route_context, route_alias, route_upstream,
		request_model, resolved_model, auth_mode, credential_class, billing_policy,
		billing_class, cost_status
	) VALUES (?, NULL, NULL, 'messages', 'pending', NULL, 0, 'gnomon', 'personal', NULL,
		'https://api.anthropic.com', 'claude-opus-5', 'claude-opus-5', 'forward', 'oauth',
		'credential_derived', 'subscription', 'pending')`, startedAt); err != nil {
		t.Fatalf("insert migration-1 pending row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close migration-1 database: %v", err)
	}

	store := openStore(t, path)
	var gotStartedAt int64
	var lifecycle, costStatus string
	var responseID, costBasis sql.NullString
	var finalCost, rateCardID sql.NullInt64
	if err := store.db.QueryRow(`SELECT started_at, lifecycle_state, cost_status, response_id,
		cost_basis, final_cost_nano_usd, rate_card_id
		FROM accounting_requests WHERE id = 1`).Scan(
		&gotStartedAt, &lifecycle, &costStatus, &responseID, &costBasis, &finalCost, &rateCardID,
	); err != nil {
		t.Fatalf("read migrated pending row: %v", err)
	}
	if gotStartedAt != startedAt || lifecycle != "pending" || costStatus != "pending" || responseID.Valid ||
		costBasis.Valid || finalCost.Valid || rateCardID.Valid {
		t.Fatalf(
			"migrated row = start %d lifecycle %q cost %q response %#v basis %#v amount %#v card %#v; want preserved pending row",
			gotStartedAt,
			lifecycle,
			costStatus,
			responseID,
			costBasis,
			finalCost,
			rateCardID,
		)
	}
}

func TestOpenRejectsInvalidPathsAndDatabases(t *testing.T) {
	t.Parallel()

	if _, err := Open("  \t"); err == nil || err.Error() != "usage database path is blank" {
		t.Fatalf("blank path error = %v", err)
	}

	dir := t.TempDir()
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("directory path error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "corrupt.sqlite")
	if err := os.WriteFile(path, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil ||
		!strings.Contains(err.Error(), "open usage database") ||
		!strings.Contains(err.Error(), "ping:") {
		t.Fatalf("corrupt database error = %v; want ping stage", err)
	}
}

func TestOpenRejectsUnknownFutureMigration(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db := inspectDB(t, path)
	if _, err := db.Exec(
		`INSERT INTO schema_migrations(version, name) VALUES (99, 'future')`,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(
		path,
	); err == nil ||
		!strings.Contains(err.Error(), "migrate:") ||
		!strings.Contains(err.Error(), "unknown future schema version 99") {
		t.Fatalf("future migration error = %v; want migration stage", err)
	}
}

// TestOpenAtCurrentSchemaLeavesAnOlderDatabaseAlone covers the one database a
// running daemon still owns: migrating it would advance a schema that daemon
// cannot reopen, so the older version has to be refused untouched, while an
// operator's own copy named with --db is still migrated by Open.
func TestOpenAtCurrentSchemaLeavesAnOlderDatabaseAlone(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	db := inspectDB(t, path)
	if err := applyMigrations(
		t.Context(),
		db,
		declaredMigrations()[:summaryQueryIndexesMigrationVersion],
	); err != nil {
		t.Fatalf("apply migrations through version %d: %v", summaryQueryIndexesMigrationVersion, err)
	}
	if _, err := db.Exec(`INSERT INTO accounting_requests (
		started_at, request_kind, lifecycle_state, upstream_contacted, route_host, route_context,
		route_upstream, request_model, resolved_model, auth_mode, credential_class, billing_policy,
		cost_status
	) VALUES (123, 'messages', 'pending', 0, 'gnomon', 'personal',
		'https://api.anthropic.com', 'claude-sonnet-5', 'claude-sonnet-5', 'forward', 'oauth',
		'credential_derived', 'pending')`); err != nil {
		t.Fatalf("insert version-6 row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close version-6 database: %v", err)
	}

	_, err := OpenAtCurrentSchema(path)
	if err == nil {
		t.Fatal("OpenAtCurrentSchema() on a version-6 database returned no error")
	}
	for _, want := range []string{
		fmt.Sprintf("schema version %d", summaryQueryIndexesMigrationVersion),
		fmt.Sprintf("version %d this build declares", responseDetailTimingMigrationVersion),
		"upgrade the running patchbay daemon",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("OpenAtCurrentSchema() error = %v; want it to name %q", err, want)
		}
	}

	refused := inspectDB(t, path)
	if version, rows := schemaVersionAndRowCount(t, refused); version != summaryQueryIndexesMigrationVersion ||
		rows != 1 {
		t.Fatalf(
			"refused database = version %d with %d rows; want version %d with 1 row",
			version, rows, summaryQueryIndexesMigrationVersion,
		)
	}
	if err := refused.Close(); err != nil {
		t.Fatalf("close refused database: %v", err)
	}

	store := openStore(t, path)
	if version, rows := schemaVersionAndRowCount(t, store.db); version != responseDetailTimingMigrationVersion ||
		rows != 1 {
		t.Fatalf(
			"migrated database = version %d with %d rows; want version %d with 1 row",
			version, rows, responseDetailTimingMigrationVersion,
		)
	}
}

func schemaVersionAndRowCount(t *testing.T, db *sql.DB) (int, int) {
	t.Helper()

	var version, rows int
	if err := db.QueryRow(`SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM accounting_requests`).Scan(&rows); err != nil {
		t.Fatalf("count accounting rows: %v", err)
	}
	return version, rows
}

func TestOpenRejectsRecordedMigrationNameMismatch(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store := openStore(t, path)
	if _, err := store.db.Exec(
		`UPDATE schema_migrations SET name = 'wrong_name' WHERE version = 1`,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(
		path,
	); err == nil ||
		!strings.Contains(err.Error(), `recorded as "wrong_name"`) {
		t.Fatalf("Open(%q) error = %v; want recorded-name mismatch", path, err)
	}
}

// TestCanonicalSchemaDefinitionsReproduceEveryMigratedVersion holds the canonical
// schema to the only standard there is: a database this build migrated itself. The
// canonical definitions are what every source is measured against, so a canonical
// build that drifts from the migrations would either wave through the divergence
// it exists to catch or refuse databases that are perfectly good.
//
// Building it in memory from the declared migrations is what keeps the two in
// step. A hand-maintained copy of the CREATE text could not: `accounting_requests`
// is created once and extended by five later migrations, and the text SQLite
// stores for it afterwards is the text those ALTERs produced.
func TestCanonicalSchemaDefinitionsReproduceEveryMigratedVersion(t *testing.T) {
	t.Parallel()

	declared := declaredMigrations()
	for through := 1; through <= len(declared); through++ {
		version := declared[through-1].version
		t.Run(fmt.Sprintf("schema version %d", version), func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			db := inspectDB(t, path)
			if err := applyMigrations(t.Context(), db, declared[:through]); err != nil {
				t.Fatalf("apply migrations through version %d: %v", version, err)
			}
			built, builtErr := readSchemaDefinitions(t.Context(), db)
			if builtErr != nil {
				t.Fatalf("read the schema migrating built at version %d: %v", version, builtErr)
			}

			canonical, canonicalErr := canonicalSchemaDefinitions(t.Context(), version)
			if canonicalErr != nil {
				t.Fatalf("canonicalSchemaDefinitions(%d) error = %v; want the migrated schema", version, canonicalErr)
			}
			if len(canonical) == 0 {
				t.Fatalf("canonicalSchemaDefinitions(%d) named no objects", version)
			}
			for identity, want := range built {
				got, ok := canonical[identity]
				if !ok {
					t.Errorf(
						"canonicalSchemaDefinitions(%d) omits the %s %q that the migrations create",
						version, identity.kind, identity.name,
					)
					continue
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf(
						"canonicalSchemaDefinitions(%d)[%s %q] = %+v; want %+v",
						version, identity.kind, identity.name, got, want,
					)
				}
			}
			for identity := range canonical {
				if _, ok := built[identity]; !ok {
					t.Errorf(
						"canonicalSchemaDefinitions(%d) holds the %s %q, which no migration through that "+
							"version creates",
						version, identity.kind, identity.name,
					)
				}
			}
		})
	}
}

// TestValidateRecordedSchemaObjectsAcceptsEverySupportedSchema pins the sweep's
// floor. Refusing an object the migrations never named must not extend to the
// indexes SQLite builds for itself: `rate_cards` declares `UNIQUE(model,
// effective_from)`, and the `sqlite_autoindex_` entry SQLite creates for that
// constraint is attached to a managed table, carries a name no migration wrote,
// and is required for the database to work at all. Every clean schema this build
// declares has to pass, or the check refuses Patchbay's own output.
func TestValidateRecordedSchemaObjectsAcceptsEverySupportedSchema(t *testing.T) {
	t.Parallel()

	declared := declaredMigrations()
	for through := 1; through <= len(declared); through++ {
		version := declared[through-1].version
		t.Run(fmt.Sprintf("schema version %d", version), func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			db := inspectDB(t, path)
			if err := applyMigrations(t.Context(), db, declared[:through]); err != nil {
				t.Fatalf("apply migrations through version %d: %v", version, err)
			}
			if err := validateRecordedSchemaObjects(t.Context(), db, version); err != nil {
				t.Fatalf("validateRecordedSchemaObjects(clean version %d) = %v; want acceptance", version, err)
			}
			if version < rateCardsAndFinalCostMigrationVersion {
				return
			}
			// Prove the acceptance above is about a schema that actually
			// holds an implicit index, rather than one that never had one.
			var implicit int
			const implicitQuery = `SELECT COUNT(*) FROM sqlite_master
				WHERE type = 'index' AND tbl_name = ? AND sql IS NULL`
			if countErr := db.QueryRow(implicitQuery, rateCardsTable).Scan(&implicit); countErr != nil {
				t.Fatalf("count the implicit indexes on %q: %v", rateCardsTable, countErr)
			}
			if implicit == 0 {
				t.Errorf(
					"version %d holds no implicit index on %q; the acceptance proves nothing",
					version,
					rateCardsTable,
				)
			}
		})
	}
}

// TestValidateRecordedSchemaObjectsJudgesExtrasByTheTableTheyActOn separates the
// two halves of the extra-object rule at the level the rule is written, so a
// change that widened or narrowed it is caught without going through a whole
// snapshot run.
func TestValidateRecordedSchemaObjectsJudgesExtrasByTheTableTheyActOn(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		extra   []string
		wantErr string
	}{
		{
			name: "a trigger on an unrelated table is accepted",
			extra: []string{
				`CREATE TABLE operator_notes (id INTEGER PRIMARY KEY, note TEXT)`,
				`CREATE TRIGGER operator_notes_stamp AFTER INSERT ON operator_notes
				BEGIN
					DELETE FROM operator_notes WHERE id < 0;
				END`,
			},
		},
		{
			name: "indexes and views off the managed tables are accepted",
			extra: []string{
				`CREATE TABLE operator_notes (id INTEGER PRIMARY KEY, note TEXT)`,
				`CREATE UNIQUE INDEX operator_notes_unique_note ON operator_notes(note)`,
				`CREATE VIEW recent_requests AS SELECT id FROM accounting_requests`,
			},
		},
		{
			name:  "a trigger on the ledger is refused",
			extra: []string{`CREATE TRIGGER ledger_purge AFTER INSERT ON accounting_requests BEGIN SELECT 1; END`},
			wantErr: `schema version 7 does not declare trigger "ledger_purge" ` +
				`on table "accounting_requests"`,
		},
		{
			name:    "an index on the rate cards table is refused",
			extra:   []string{`CREATE INDEX rate_cards_by_source ON rate_cards(source)`},
			wantErr: `schema version 7 does not declare index "rate_cards_by_source" on table "rate_cards"`,
		},
		{
			name:  "an index on the migration bookkeeping table is refused",
			extra: []string{`CREATE INDEX schema_migrations_by_name ON schema_migrations(name)`},
			wantErr: `schema version 7 does not declare index "schema_migrations_by_name" ` +
				`on table "schema_migrations"`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			db := inspectDB(t, path)
			if err := migrate(t.Context(), db); err != nil {
				t.Fatalf("migrate %q: %v", path, err)
			}
			for _, statement := range test.extra {
				if _, execErr := db.Exec(statement); execErr != nil {
					t.Fatalf("apply %q: %v", statement, execErr)
				}
			}

			err := validateRecordedSchemaObjects(t.Context(), db, latestSchemaVersion())
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("validateRecordedSchemaObjects(extra off the managed tables) = %v; want acceptance", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateRecordedSchemaObjects(managed extra) = %v; want %q", err, test.wantErr)
			}
		})
	}
}

// canonicalSummaryWindowIndex and canonicalRateCardsForbidUpdate reproduce two
// canonical definitions verbatim, so a case that drops one and puts it back is
// restoring the object the migrations declared rather than an approximation of
// it. A rebuilt object that no longer matched would be refused by the definition
// comparison, and the case would then prove nothing about the sweep.
const (
	canonicalSummaryWindowIndex = `CREATE INDEX ` + summaryWindowIndexName + `
		ON accounting_requests(started_at, request_kind)`
	canonicalRateCardsForbidUpdate = `CREATE TRIGGER rate_cards_forbid_update
		BEFORE UPDATE ON rate_cards
		BEGIN
			SELECT RAISE(ABORT, 'rate cards are immutable');
		END`
)

// TestValidateRecordedSchemaObjectsIdentifiesObjectsByKindAndName covers the
// namespaces SQLite keeps apart. A trigger and an index may hold one name in the
// same database, so a name is not an identity, and a check that keys its
// canonical and actual objects by name alone reduces to whichever `sqlite_master`
// row it reads last.
//
// That makes both verdicts wrong, in opposite directions. When the intruder's row
// comes last it displaces the canonical object of the other kind and the refusal
// names the wrong fault — reporting an index as missing or replaced while the
// index sits in the file, exact. When the canonical row comes last it displaces
// the intruder, every declared object compares equal, and the sweep skips the
// intruder as declared — so a trigger that empties the ledger on every write is
// accepted under the name of a canonical index. Each case below therefore plants
// its collision on both sides of the canonical row.
func TestValidateRecordedSchemaObjectsIdentifiesObjectsByKindAndName(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		extra   []string
		wantErr string
	}{
		{
			name:  "a destructive trigger has taken a canonical index name",
			extra: []string{destructiveLedgerTrigger(summaryWindowIndexName, "INSERT")},
			wantErr: `schema version 7 does not declare trigger "` + summaryWindowIndexName + `" ` +
				`on table "accounting_requests"`,
		},
		{
			name: "a destructive trigger has taken a canonical index name and precedes it",
			extra: []string{
				`DROP INDEX ` + summaryWindowIndexName,
				destructiveLedgerTrigger(summaryWindowIndexName, "INSERT"),
				canonicalSummaryWindowIndex,
			},
			wantErr: `schema version 7 does not declare trigger "` + summaryWindowIndexName + `" ` +
				`on table "accounting_requests"`,
		},
		{
			name:  "a destructive trigger has taken the other canonical index name",
			extra: []string{destructiveLedgerTrigger(summaryContextWindowIndexName, "UPDATE")},
			wantErr: `schema version 7 does not declare trigger "` + summaryContextWindowIndexName + `" ` +
				`on table "accounting_requests"`,
		},
		{
			name:  "an index has taken a canonical trigger name",
			extra: []string{`CREATE INDEX rate_cards_forbid_update ON rate_cards(source)`},
			wantErr: `schema version 7 does not declare index "rate_cards_forbid_update" ` +
				`on table "rate_cards"`,
		},
		{
			name: "an index has taken a canonical trigger name and precedes it",
			extra: []string{
				`DROP TRIGGER rate_cards_forbid_update`,
				`CREATE INDEX rate_cards_forbid_update ON rate_cards(source)`,
				canonicalRateCardsForbidUpdate,
			},
			wantErr: `schema version 7 does not declare index "rate_cards_forbid_update" ` +
				`on table "rate_cards"`,
		},
		{
			name:  "an index has taken a canonical trigger name on another managed table",
			extra: []string{`CREATE INDEX accounting_requests_validate_finalized_snapshot ON rate_cards(model)`},
			wantErr: `schema version 7 does not declare index "accounting_requests_validate_finalized_snapshot" ` +
				`on table "rate_cards"`,
		},
		{
			name:  "a destructive trigger has taken a canonical table name",
			extra: []string{destructiveLedgerTrigger(rateCardsTable, "DELETE")},
			wantErr: `schema version 7 does not declare trigger "` + rateCardsTable + `" ` +
				`on table "accounting_requests"`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			db := inspectDB(t, path)
			if err := migrate(t.Context(), db); err != nil {
				t.Fatalf("migrate %q: %v", path, err)
			}
			for _, statement := range test.extra {
				if _, execErr := db.Exec(statement); execErr != nil {
					t.Fatalf("apply %q: %v", statement, execErr)
				}
			}

			err := validateRecordedSchemaObjects(t.Context(), db, latestSchemaVersion())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateRecordedSchemaObjects(cross-kind name collision) = %v; want %q", err, test.wantErr)
			}
		})
	}
}

// TestValidateRecordedSchemaObjectsRefusesDuplicateSchemaRowsInTheFile pins what
// a database holding two `sqlite_master` rows under one identity does. SQLite
// itself refuses to parse such a schema, so the attack never reaches the
// comparison — but "SQLite would catch it" is a claim about a dependency, and an
// untested one would quietly become false if the read ever stopped going through
// SQLite's schema parser. What matters downstream is that no verdict of
// acceptance is ever produced for such a file.
func TestValidateRecordedSchemaObjectsRefusesDuplicateSchemaRowsInTheFile(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		kind   string
		object string
		table  string
		create string
	}{
		{
			name:   "a second row for a canonical trigger",
			kind:   "trigger",
			object: "rate_cards_forbid_update",
			table:  rateCardsTable,
			create: `CREATE TRIGGER rate_cards_forbid_update BEFORE UPDATE ON rate_cards ` +
				`BEGIN SELECT 1; END`,
		},
		{
			name:   "a second row for a canonical index",
			kind:   "index",
			object: summaryWindowIndexName,
			table:  accountingRequestsTable,
			create: `CREATE INDEX ` + summaryWindowIndexName + ` ON accounting_requests(request_kind)`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "usage.sqlite")
			seedToolingSource(t, path, 1)
			plantReservedSchemaObject(test.kind, test.object, test.table, test.create)(t, path)

			db := inspectDB(t, path)
			err := validateRecordedSchemaObjects(t.Context(), db, latestSchemaVersion())
			if err == nil {
				t.Fatalf("validateRecordedSchemaObjects(duplicate %s row) = nil; want a refusal", test.kind)
			}
			if !strings.Contains(err.Error(), "not a patchbay usage database") {
				t.Errorf("validateRecordedSchemaObjects(duplicate %s row) = %v; want the usage-database refusal",
					test.kind, err)
			}
		})
	}
}

// fixedSchemaQueryer answers every schema read with one fixed statement, so a
// `sqlite_master` shape SQLite will not hold in a file can still be put in front
// of the readers that consume it.
type fixedSchemaQueryer struct {
	db    *sql.DB
	query string
}

func (queryer fixedSchemaQueryer) QueryContext(
	ctx context.Context,
	_ string,
	_ ...any,
) (*sql.Rows, error) {
	return queryer.db.QueryContext(ctx, queryer.query)
}

func (queryer fixedSchemaQueryer) QueryRowContext(
	ctx context.Context,
	_ string,
	_ ...any,
) *sql.Row {
	return queryer.db.QueryRowContext(ctx, queryer.query)
}

// TestReadSchemaObjectsRefusesTwoRowsUnderOneIdentity holds the map build itself
// to the rule, rather than to SQLite's willingness to enforce it. Collecting rows
// into a map keyed by identity keeps whichever arrives last, so two rows under
// one identity would decide a definition comparison by row order — a gutted
// trigger planted beside the real one would win or lose depending on where it sat
// in the file. There is no honest verdict about such a schema, so the read
// refuses instead of picking one.
func TestReadSchemaObjectsRefusesTwoRowsUnderOneIdentity(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name: "two triggers under one name",
			query: `SELECT 'trigger', 'rate_cards_forbid_update', 'rate_cards',
					'CREATE TRIGGER rate_cards_forbid_update BEFORE UPDATE ON rate_cards ` +
				`BEGIN SELECT RAISE(ABORT, ''rate cards are immutable''); END'
				UNION ALL
				SELECT 'trigger', 'rate_cards_forbid_update', 'rate_cards',
					'CREATE TRIGGER rate_cards_forbid_update BEFORE UPDATE ON rate_cards BEGIN SELECT 1; END'`,
			wantErr: `holds more than one trigger named "rate_cards_forbid_update"`,
		},
		{
			name: "two indexes under one name",
			query: `SELECT 'index', '` + summaryWindowIndexName + `', 'accounting_requests',
					'CREATE INDEX ` + summaryWindowIndexName + ` ON accounting_requests(started_at, request_kind)'
				UNION ALL
				SELECT 'index', '` + summaryWindowIndexName + `', 'accounting_requests',
					'CREATE INDEX ` + summaryWindowIndexName + ` ON accounting_requests(request_kind)'`,
			wantErr: `holds more than one index named "` + summaryWindowIndexName + `"`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			db := inspectDB(t, filepath.Join(t.TempDir(), "fixture.sqlite"))
			definitions, err := readSchemaObjects(t.Context(), fixedSchemaQueryer{db: db, query: test.query})
			if err == nil {
				t.Fatalf("readSchemaObjects(two rows under one identity) = %v, nil; want a refusal", definitions)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("readSchemaObjects(two rows under one identity) error = %v; want %q", err, test.wantErr)
			}
			if definitions != nil {
				t.Errorf("readSchemaObjects(two rows under one identity) returned %v; want no definitions", definitions)
			}
		})
	}
}

// TestCanonicalSchemaDefinitionsRefuseAVersionThisBuildCannotBuild covers the
// versions no declared migration produces. A canonical schema built from a
// truncated migration list would be a weaker contract wearing the right version
// number, so there is no answer to give and the read must fail instead.
func TestCanonicalSchemaDefinitionsRefuseAVersionThisBuildCannotBuild(t *testing.T) {
	t.Parallel()

	for _, version := range []int{-1, 0, latestSchemaVersion() + 1} {
		t.Run(fmt.Sprintf("schema version %d", version), func(t *testing.T) {
			t.Parallel()

			definitions, err := canonicalSchemaDefinitions(t.Context(), version)
			if err == nil {
				t.Fatalf("canonicalSchemaDefinitions(%d) = %v, nil; want a refusal", version, definitions)
			}
			if definitions != nil {
				t.Errorf(
					"canonicalSchemaDefinitions(%d) returned %v alongside %v; want no definitions",
					version, definitions, err,
				)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("schema version %d", version)) {
				t.Errorf("canonicalSchemaDefinitions(%d) error = %v; want it to name the version", version, err)
			}
		})
	}
}

// TestCanonicalSchemaDefinitionsHonourCallerCancellation proves the canonical
// build answers to the caller's context. It runs inside a read command a user can
// interrupt, and a canceled read that returned definitions anyway would be
// comparing a source against a half-built schema.
func TestCanonicalSchemaDefinitionsHonourCallerCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	definitions, err := canonicalSchemaDefinitions(ctx, latestSchemaVersion())
	if err == nil {
		t.Fatalf("canonicalSchemaDefinitions(canceled) = %v, nil; want the cancellation", definitions)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("canonicalSchemaDefinitions(canceled) error = %v; want context.Canceled", err)
	}
	if definitions != nil {
		t.Errorf("canonicalSchemaDefinitions(canceled) returned %v; want no definitions", definitions)
	}
}

// TestReadSchemaDefinitionsRefusesWhatItCannotRead covers the reads that never
// produce a verdict. Each one is a separate statement against the source, and a
// failure treated as an empty result would be read as a database holding nothing
// that contradicts its version — the opposite of what happened.
func TestReadSchemaDefinitionsRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	if store, openErr := Open(path); openErr != nil {
		t.Fatalf("Open(%q): %v", path, openErr)
	} else if closeErr := store.Close(); closeErr != nil {
		t.Fatalf("close %q: %v", path, closeErr)
	}
	db, openErr := sql.Open("sqlite", path)
	if openErr != nil {
		t.Fatal(openErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	definitions, err := readSchemaDefinitions(t.Context(), db)
	if err == nil {
		t.Fatalf("readSchemaDefinitions(closed database) = %v, nil; want a refusal", definitions)
	}
	if definitions != nil {
		t.Errorf("readSchemaDefinitions(closed database) returned %v; want no definitions", definitions)
	}
}

// TestNormalizeSchemaSQLIgnoresFormattingAndNothingElse pins the one comparison
// every definition check rests on. Migrations are immutable, so a database's
// stored CREATE text derives from the same statements this build declares; the
// normalizer exists to absorb the formatting SQLite and past builds may have
// applied around them — never to talk two different definitions into agreeing.
func TestNormalizeSchemaSQLIgnoresFormattingAndNothingElse(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		left  string
		right string
		same  bool
	}{
		{
			name:  "indentation and line breaks",
			left:  "CREATE TABLE t (\n\tid INTEGER PRIMARY KEY,\n\tname TEXT\n)",
			right: "CREATE TABLE t ( id INTEGER PRIMARY KEY, name TEXT )",
			same:  true,
		},
		{
			name:  "keyword case",
			left:  `create unique index i on t(a) where a <> ''`,
			right: `CREATE UNIQUE INDEX i ON t(a) WHERE a <> ''`,
			same:  true,
		},
		{
			name:  "a line comment",
			left:  "CREATE TABLE t ( -- the comment\n id INTEGER)",
			right: "CREATE TABLE t ( id INTEGER)",
			same:  true,
		},
		{
			name:  "a block comment",
			left:  "CREATE TABLE t (/* why */ id INTEGER)",
			right: "CREATE TABLE t ( id INTEGER)",
			same:  true,
		},
		{
			name:  "a block comment that joins two tokens",
			left:  "CREATE TABLE/**/t (id INTEGER)",
			right: "CREATE TABLEt (id INTEGER)",
			same:  false,
		},
		{
			name:  "string literal case",
			left:  `CREATE TRIGGER g BEFORE DELETE ON t BEGIN SELECT RAISE(ABORT, 'immutable'); END`,
			right: `CREATE TRIGGER g BEFORE DELETE ON t BEGIN SELECT RAISE(ABORT, 'IMMUTABLE'); END`,
			same:  false,
		},
		{
			name:  "whitespace inside a string literal",
			left:  `SELECT 'a  b'`,
			right: `SELECT 'a b'`,
			same:  false,
		},
		{
			name:  "a comment marker inside a string literal",
			left:  `SELECT '-- not a comment', 1`,
			right: `SELECT '', 1`,
			same:  false,
		},
		{
			name:  "an escaped quote inside a string literal",
			left:  `SELECT 'it''s  fine'`,
			right: `SELECT 'it''s fine'`,
			same:  false,
		},
		{
			name:  "a quoted identifier's case",
			left:  `CREATE TABLE "T" (id INTEGER)`,
			right: `CREATE TABLE "t" (id INTEGER)`,
			same:  false,
		},
		{
			name:  "a trigger body",
			left:  `CREATE TRIGGER g BEFORE DELETE ON t BEGIN SELECT RAISE(ABORT, 'no'); END`,
			right: `CREATE TRIGGER g BEFORE DELETE ON t BEGIN SELECT 1; END`,
			same:  false,
		},
		{
			name:  "an index predicate",
			left:  `CREATE UNIQUE INDEX i ON t(a) WHERE a IS NOT NULL`,
			right: `CREATE UNIQUE INDEX i ON t(a)`,
			same:  false,
		},
		{
			name:  "an unterminated literal",
			left:  `SELECT 'open`,
			right: `SELECT 'other`,
			same:  false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			left, right := normalizeSchemaSQL(test.left), normalizeSchemaSQL(test.right)
			if (left == right) != test.same {
				t.Errorf(
					"normalizeSchemaSQL(%q) = %q and normalizeSchemaSQL(%q) = %q; want equal = %t",
					test.left, left, test.right, right, test.same,
				)
			}
			if again := normalizeSchemaSQL(test.left); again != left {
				t.Errorf(
					"normalizeSchemaSQL(%q) = %q on a second call; want the deterministic %q",
					test.left, again, left,
				)
			}
		})
	}
}

func TestMigrationDeclarationsMustBeOrderedAndUnique(t *testing.T) {
	for _, tc := range []struct {
		name       string
		migrations []migration
	}{
		{"duplicate", []migration{{version: 1, name: "one"}, {version: 1, name: "again"}}},
		{"out of order", []migration{{version: 2, name: "two"}, {version: 1, name: "one"}}},
		{"blank", []migration{{version: 1, name: ""}}},
		{"whitespace", []migration{{version: 1, name: " \t\n"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateMigrations(tc.migrations); err == nil {
				t.Fatal("validateMigrations returned nil error")
			}
		})
	}
}

func TestFailingMigrationIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	failing := migration{version: responseDetailTimingMigrationVersion + 1, name: "fail", statements: []string{
		`CREATE TABLE must_not_survive (id INTEGER)`,
		`THIS IS NOT SQL`,
	}}
	if err := applyMigrations(t.Context(), db, append(declaredMigrations(), failing)); err == nil {
		t.Fatal("applyMigrations returned nil error")
	}

	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'must_not_survive'`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration published partial schema")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, failing.version).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration published its version")
	}
}

func TestCommitFailureRollsBackMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	readerDB := inspectDB(t, path)
	writerDB := inspectDB(t, path)
	for _, db := range []*sql.DB{readerDB, writerDB} {
		if _, err := db.Exec(`PRAGMA journal_mode = DELETE`); err != nil {
			t.Fatal(err)
		}
	}
	const migrationTable = `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at INTEGER NOT NULL DEFAULT (unixepoch('subsec') * 1000000000)
	)`
	if _, err := writerDB.Exec(migrationTable); err != nil {
		t.Fatal(err)
	}
	if _, err := writerDB.Exec(`PRAGMA busy_timeout = 1`); err != nil {
		t.Fatal(err)
	}

	reader, err := readerDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.ExecContext(context.Background(), `BEGIN`); err != nil {
		t.Fatal(err)
	}
	defer reader.ExecContext(context.Background(), `ROLLBACK`)
	var version int
	row := reader.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM schema_migrations`,
	)
	if err := row.Scan(&version); err != nil {
		t.Fatal(err)
	}

	writerDB.SetMaxOpenConns(1)
	candidate := migration{
		version:    1,
		name:       "blocked_commit",
		statements: []string{`CREATE TABLE must_not_commit (id INTEGER)`},
	}
	migrationErr := applyMigration(context.Background(), writerDB, candidate)
	if migrationErr == nil || !strings.Contains(migrationErr.Error(), "commit migration") {
		t.Fatalf("applyMigration commit error = %v; want busy commit failure", migrationErr)
	}
	if _, err := reader.ExecContext(context.Background(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if err := writerDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'must_not_commit'`).
		Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("failed commit published partial schema: %d tables", version)
	}
	if err := writerDB.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).
		Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("failed commit recorded migration version: %d rows", version)
	}
	if _, err := writerDB.Exec(`CREATE TABLE connection_is_usable (id INTEGER)`); err != nil {
		t.Fatalf("connection unusable after commit failure: %v", err)
	}
}

func TestConcurrentOpenConverges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "usage.sqlite")
	const workers = 12
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			errs <- store.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Open: %v", err)
		}
	}

	db := inspectDB(t, path)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("migration 1 rows = %d; want 1", count)
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).
		Scan(&integrity); err != nil ||
		integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v; want ok", integrity, err)
	}
}

func TestInitializeWALRetriesTypedBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	blocker := inspectDB(t, path)
	contender := inspectDB(t, path)
	blockerConn, err := blocker.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blockerConn.Close()
	if _, err := blockerConn.ExecContext(t.Context(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if !locked {
			return
		}
		if _, err := blockerConn.ExecContext(t.Context(), `ROLLBACK`); err != nil {
			t.Errorf("release SQLite lock: %v", err)
		}
	}()

	attempts := 0
	var firstErr error
	err = initializeWAL(t.Context(), func(ctx context.Context) (string, error) {
		attempts++
		var mode string
		setErr := contender.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode)
		if attempts == 1 {
			firstErr = setErr
			if _, rollbackErr := blockerConn.ExecContext(ctx, `ROLLBACK`); rollbackErr != nil {
				return "", rollbackErr
			}
			locked = false
		}
		return mode, setErr
	})
	if err != nil {
		t.Fatalf("initializeWAL returned error: %v", err)
	}
	var sqliteErr *sqlite.Error
	if !errors.As(firstErr, &sqliteErr) {
		t.Fatalf("first journal_mode error = %T %v; want *sqlite.Error", firstErr, firstErr)
	}
	if sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Fatalf("first journal_mode code = %d; want primary SQLITE_BUSY", sqliteErr.Code())
	}
	if attempts < 2 {
		t.Fatalf("journal_mode attempts = %d; want at least 2 after typed SQLITE_BUSY", attempts)
	}
	assertPragma(t, contender, "journal_mode", "wal")
}

func TestInitializeWALRejectsUnexpectedResultOrError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		err     error
		wantErr string
	}{
		{name: "wrong mode", mode: "delete", wantErr: `returned "delete"; want "wal"`},
		{name: "non-busy error", err: errors.New("disk failure"), wantErr: "set journal mode to WAL: disk failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			err := initializeWAL(t.Context(), func(context.Context) (string, error) {
				attempts++
				return tc.mode, tc.err
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("initializeWAL error = %v; want %q", err, tc.wantErr)
			}
			if attempts != 1 {
				t.Fatalf("journal_mode attempts = %d; want 1", attempts)
			}
		})
	}
}

func TestInitializeWALStopsAfterContextCancellation(t *testing.T) {
	busyErr := sqliteBusyError(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	attempts := 0
	err := initializeWAL(ctx, func(context.Context) (string, error) {
		attempts++
		cancel()
		return "", busyErr
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("initializeWAL error = %v; want context cancellation", err)
	}
	if attempts != 1 {
		t.Fatalf("journal_mode attempts = %d; want 1", attempts)
	}
}

func sqliteBusyError(t *testing.T) error {
	t.Helper()

	path := filepath.Join(t.TempDir(), "usage.sqlite")
	blocker := inspectDB(t, path)
	contender := inspectDB(t, path)
	blockerConn, err := blocker.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blockerConn.Close()
	if _, err := blockerConn.ExecContext(t.Context(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := blockerConn.ExecContext(t.Context(), `ROLLBACK`); err != nil {
			t.Errorf("release SQLite lock: %v", err)
		}
	}()

	var mode string
	err = contender.QueryRowContext(t.Context(), `PRAGMA journal_mode = WAL`).Scan(&mode)
	if err == nil {
		t.Fatal("PRAGMA journal_mode = WAL returned nil error while EXCLUSIVE lock was held")
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Fatalf("journal_mode error = %T %v; want typed SQLITE_BUSY", err, err)
	}
	return err
}

func assertConnectionPragmas(t *testing.T, db *sql.DB) {
	t.Helper()

	db.SetMaxOpenConns(2)
	first, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, conn := range []*sql.Conn{first, second} {
		for name, want := range map[string]string{
			"busy_timeout": "5000",
			"foreign_keys": "1",
			"journal_mode": "wal",
		} {
			var got string
			if err := conn.QueryRowContext(context.Background(), fmt.Sprintf("PRAGMA %s", name)).
				Scan(&got); err != nil {
				t.Fatalf("read PRAGMA %s: %v", name, err)
			}
			if got != want {
				t.Fatalf("PRAGMA %s = %q; want %q", name, got, want)
			}
		}
	}
}

// assertSharedStickyTemporaryRoot returns the resolved system temporary
// directory after proving it is the root-owned, world-writable, sticky shape the
// earlier-ancestor sticky allowance is written for.
func assertSharedStickyTemporaryRoot(t *testing.T) string {
	t.Helper()

	tempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatalf("resolve /tmp: %v", err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(tempRoot, &stat); err != nil {
		t.Fatalf("stat %q: %v", tempRoot, err)
	}
	if stat.Uid != 0 || stat.Mode&unix.S_ISVTX == 0 || stat.Mode&unix.S_IWOTH == 0 {
		t.Fatalf(
			"%q stat = uid %d mode %#o; want root-owned sticky world-writable directory",
			tempRoot,
			stat.Uid,
			stat.Mode,
		)
	}
	return tempRoot
}

// uniqueTemporaryName derives a collision-free basename for fixtures that must
// live directly in the shared temporary root rather than a private subdirectory.
func uniqueTemporaryName(t *testing.T) string {
	t.Helper()

	unique := t.TempDir()
	return filepath.Base(filepath.Dir(unique)) + "-" + filepath.Base(unique)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %#o; want %#o", path, got, want)
	}
}

func assertPragma(t *testing.T, db *sql.DB, name, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(fmt.Sprintf("PRAGMA %s", name)).Scan(&got); err != nil {
		t.Fatalf("read PRAGMA %s: %v", name, err)
	}
	if got != want {
		t.Fatalf("PRAGMA %s = %q; want %q", name, got, want)
	}
}

// TestReadmeDocumentsRetainedDescriptorResidual pins the one limit of the path
// policy that no amount of validation can close: a descriptor another UID opened
// while the file was briefly exposed keeps writing that inode afterwards, and
// current-state inspection can neither see it nor revoke it.
//
// This is a documentation test because the residual is documentation-shaped. There
// is no portable mechanism to add, so the only failure mode worth defending against
// is the README quietly regrowing a claim that the checks cover descriptor history,
// or losing the fresh-inode remediation that is the actual fix. Both are the kind
// of edit that looks like tightening the prose.
func TestReadmeDocumentsRetainedDescriptorResidual(t *testing.T) {
	t.Parallel()

	readme, readErr := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if readErr != nil {
		t.Fatalf("read README.md: %v", readErr)
	}
	// README prose wraps at a fixed column, so match against a single-spaced
	// rendering; a phrase split across two lines is still the same claim.
	text := strings.Join(strings.Fields(string(readme)), " ")

	// Text that would either overstate what the checks establish or walk the
	// remediation back to reusing the suspect inode. The sidecar sentence used to
	// assert the first; the restore paragraph used to prescribe the second.
	for _, stale := range []struct {
		phrase string
		why    string
	}{
		{
			phrase: "left by a previous owner who still holds a writable descriptor",
			why:    "current-state validation cannot establish or revoke descriptor history",
		},
		{
			phrase: "no descriptor was retained",
			why:    "current-state validation cannot establish or revoke descriptor history",
		},
		{
			phrase: "revokes every retained descriptor",
			why:    "current-state validation cannot establish or revoke descriptor history",
		},
		{
			phrase: "plain-copy",
			why:    "an ordinary copy opens and truncates the existing inode, so a retained descriptor keeps writing the restored database",
		},
		{
			phrase: "copy the snapshot over the resolved",
			why:    "restoring in place reuses the suspect inode the fresh-inode remediation exists to abandon",
		},
		{
			phrase: "copy the snapshot onto the resolved",
			why:    "restoring in place reuses the suspect inode the fresh-inode remediation exists to abandon",
		},
	} {
		if strings.Contains(text, stale.phrase) {
			t.Errorf("README.md contains %q; %s", stale.phrase, stale.why)
		}
	}

	// Unlinking the suspect database before the publish step is its own
	// regression: it drops the resolved path for the duration of the copy-in,
	// and it gives up the rename-over that performs the fresh-inode replacement
	// in one step. Match case-insensitively so a rewrapped or recapitalized
	// sentence cannot reintroduce the instruction under a different first letter.
	lowerText := strings.ToLower(text)
	for _, stale := range []struct {
		phrase string
		why    string
	}{
		{
			phrase: "remove the suspect database",
			why:    "the database must stay in place until the fresh inode is renamed over it",
		},
		{
			phrase: "unlink the suspect database",
			why:    "the database must stay in place until the fresh inode is renamed over it",
		},
		{
			phrase: "delete the suspect database",
			why:    "the database must stay in place until the fresh inode is renamed over it",
		},
		{
			phrase: "suspect database and every suspect",
			why:    "the removal step covers sidecars only; the database is replaced by rename-over",
		},
	} {
		if strings.Contains(lowerText, stale.phrase) {
			t.Errorf("README.md contains %q; %s", stale.phrase, stale.why)
		}
	}

	for _, required := range []struct {
		phrase string
		why    string
	}{
		{
			phrase: "establish nothing about how that file was reachable in the past",
			why:    "the scope limit of the current-state checks",
		},
		{
			phrase: "cannot do is detect or revoke a foreign writable descriptor retained from an earlier exposure",
			why:    "the limit stated in the same terms a reader would search for",
		},
		{
			phrase: "was ever exposed",
			why:    "the trigger condition for the compromised disposition",
		},
		{
			// The leaf policy cites retained descriptors as its reason for refusing
			// rather than repairing, dozens of lines before this section. Without a
			// pointer there, a reader stops at that sentence believing refusal
			// settles the descriptor problem.
			phrase: "That refusal covers the file's current state only",
			why:    "the forward reference from the leaf policy to this residual",
		},
		{
			phrase: "The retained descriptor keeps write access to that same inode",
			why:    "the concrete counterexample the boundary exists for",
		},
		{
			phrase: "neither the later `chmod` nor the later `chown` revokes it",
			why:    "why tightening permissions is not remediation",
		},
		{
			phrase: "The device and inode recheck cannot see it",
			why:    "why the post-open recheck does not cover this",
		},
		{
			phrase: "as compromised",
			why:    "the disposition for a file that was ever exposed",
		},
		{
			phrase: "The remediation is a fresh inode",
			why:    "the only operation a retained descriptor cannot follow",
		},
		{
			phrase: "With Patchbay stopped",
			why:    "remediation is invalid against a running daemon",
		},
		{
			phrase: "Do not reuse the suspect inode",
			why:    "truncate or chmod in place leaves the descriptor attached",
		},
		{
			phrase: "chmodding it, chowning it, or writing over it in place",
			why:    "the metadata repairs a reader would otherwise reach for first",
		},
		{
			phrase: "an ordinary `cp` onto the resolved `$PATCHBAY_USAGE_DB` path does exactly that",
			why:    "the specific command that silently reuses the suspect inode",
		},
		{
			phrase: "Create a fresh temporary file with `O_CREAT|O_EXCL`",
			why:    "only exclusive creation guarantees a new inode; a plain create can reopen an existing name and inherit whatever descriptor is already attached to it",
		},
		{
			phrase: "securely-created `mktemp` file",
			why:    "the portable equivalent of exclusive creation, for a reader not calling open(2) directly",
		},
		{
			phrase: "in the same secure local parent directory",
			why:    "where the replacement inode is built, so it is never reachable by another local UID",
		},
		{
			phrase: "Exclusive creation is what makes it a new inode",
			why:    "why O_EXCL is the requirement rather than an incidental habit",
		},
		{
			phrase: "set it to mode 0600, fsync it, and close it",
			why:    "the restored file is secured and durable before it is published",
		},
		{
			phrase: "Validate that fresh candidate before publishing it",
			why:    "an unvalidated candidate can replace the database with a truncated, corrupt, wrong, or future-schema file",
		},
		{
			phrase: "Point `PATCHBAY_USAGE_DB` at the fresh candidate path",
			why:    "validation must read the exact candidate inode, not the live path or the trusted snapshot it was copied from",
		},
		{
			phrase: "`patchbay usage snapshot <throwaway>`",
			why:    "the concrete validation mechanism, not an unstated instruction to somehow check the file",
		},
		{
			phrase: "does not already exist",
			why:    "the tooling refuses to publish over an existing file, so the throwaway must be a fresh path",
		},
		{
			phrase: "in a trusted directory",
			why:    "the throwaway output is written somewhere the operator already trusts",
		},
		{
			phrase: "A zero exit status is the evidence to publish on",
			why:    "the pass condition, stated as something the operator can observe",
		},
		{
			phrase: "readable SQLite rather than a truncated or corrupt copy",
			why:    "the truncated and corrupt candidates the gate exists to catch",
		},
		{
			phrase: "every page reads back coherently",
			why:    "validation reads the whole candidate, not just a header that any wrong file could fake",
		},
		{
			phrase: "A read-only `PRAGMA integrity_check` runs against that exact opened candidate",
			why:    "the mechanism behind the coherence claim, run against the candidate itself rather than a copy of it",
		},
		{
			phrase: "must report the single result `ok`",
			why:    "the one spelling of success; anything else, including extra rows, is a fault",
		},
		{
			phrase: "every secondary index still agrees with the table it indexes",
			why:    "the corruption a schema read and a rebuilt snapshot both miss",
		},
		{
			phrase: "it proves that forward migration rather than inferring it from the recorded version",
			why:    "an older schema is accepted on evidence, not on the version its schema_migrations rows claim",
		},
		{
			phrase: "Every pending migration is applied to a disposable clone of the command's own output",
			why:    "the mechanism behind the forward-migration claim",
		},
		{
			phrase: "integrity and schema are rechecked on the migrated clone",
			why:    "a migration that runs is not yet a migration that produced a usable database",
		},
		{
			phrase: "the clone is a copy, it is what gets migrated, and it is removed either way",
			why:    "the candidate is never migrated, and the proof leaves nothing behind",
		},
		{
			phrase: "not an unknown future schema",
			why:    "the future-schema candidate the gate exists to catch",
		},
		{
			phrase: "It accepts an older schema that `patchbay serve` would migrate forward",
			why:    "validation must not narrow which snapshots are restorable; serve migrates older schemas forward",
		},
		{
			phrase: "does not modify the candidate",
			why:    "the validating read is read-only, so it cannot itself damage what it approves",
		},
		{
			phrase: "Remove the throwaway output once the command exits zero",
			why:    "the throwaway is evidence of a successful read, not an artifact to retain",
		},
		{
			phrase: "Any validation failure aborts the restore",
			why:    "a candidate that fails validation is never published",
		},
		{
			phrase: "leave the suspect database and all of its `-wal`, `-shm`, and `-journal` sidecars untouched",
			why:    "aborting is only free while the old database and every sidecar are still in place",
		},
		{
			phrase: "remove only the fresh candidate and any partial validation output",
			why:    "cleanup after a failed validation touches the candidate side only",
		},
		{
			phrase: "Only after validation succeeds, publish",
			why:    "the destructive steps are gated on validation having passed",
		},
		{
			phrase: "Remove every suspect `-wal`, `-shm`, and `-journal` sidecar at the resolved path",
			why:    "a surviving sidecar is replayed into the restored database, and the removal step covers the sidecars only",
		},
		{
			phrase: "Leave the suspect database itself in place",
			why:    "unlinking it first would drop the resolved path instead of replacing its inode",
		},
		{
			phrase: "rename the fresh file directly onto the resolved path",
			why:    "the atomic publish step that makes the new inode the database",
		},
		{
			phrase: "replaces the suspect inode with the fresh one in a single atomic step",
			why:    "rename-over is itself the fresh-inode replacement, not merely a publish",
		},
		{
			phrase: "leaves no window in which the resolved path is missing",
			why:    "why rename-over is preferred to unlink-then-publish",
		},
		{
			phrase: "fsync the parent directory",
			why:    "the rename must survive a crash",
		},
		{
			phrase: "Start Patchbay only after the replacement is in place",
			why:    "restarting mid-procedure reopens the suspect inode",
		},
	} {
		if !strings.Contains(text, required.phrase) {
			t.Errorf("README.md is missing %q, which documents %s", required.phrase, required.why)
		}
	}

	// Validation is only a gate if it precedes the two irreversible steps.
	// Sidecar removal and the rename-over are the points of no return: once
	// they land, an unreadable candidate has already replaced the database and
	// the sidecars that would have been needed to recover it are gone too.
	// Prose describing the same check *after* either step documents a
	// different and useless procedure while still containing every phrase
	// required above, so position is the only thing that catches it. Require
	// each anchor exactly once first — a second mention would make the
	// comparison depend on which occurrence Index happened to find.
	const (
		validationAnchor = "Point `PATCHBAY_USAGE_DB` at the fresh candidate path"
		sidecarAnchor    = "Remove every suspect `-wal`, `-shm`, and `-journal` sidecar at the resolved path"
		renameAnchor     = "rename the fresh file directly onto the resolved path"
	)
	for _, anchor := range []struct {
		phrase string
		step   string
	}{
		{phrase: validationAnchor, step: "prepublish validation"},
		{phrase: sidecarAnchor, step: "sidecar removal"},
		{phrase: renameAnchor, step: "the atomic publish"},
	} {
		if got := strings.Count(text, anchor.phrase); got != 1 {
			t.Fatalf(
				"README.md mentions %q %d times; want exactly 1 so the position of %s is unambiguous",
				anchor.phrase,
				got,
				anchor.step,
			)
		}
	}
	validationAt := strings.Index(text, validationAnchor)
	if sidecarAt := strings.Index(text, sidecarAnchor); validationAt > sidecarAt {
		t.Errorf(
			"README.md documents validation at offset %d, after sidecar removal at offset %d; "+
				"validating once the canonical sidecars are gone cannot abort a restore that has already destroyed them",
			validationAt,
			sidecarAt,
		)
	}
	if renameAt := strings.Index(text, renameAnchor); validationAt > renameAt {
		t.Errorf(
			"README.md documents validation at offset %d, after the rename-over at offset %d; "+
				"validating a candidate that is already the live database is not a gate",
			validationAt,
			renameAt,
		)
	}
}
