package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type sourceIdentity struct {
	device string
	inode  string
}

type toolingBeforePublishHookKey struct{}

type toolingMigrationCloneHookKey struct{}

const (
	toolingStagingPrefix   = ".patchbay-usage-"
	toolingStagingLockName = ".patchbay.lock"
	// A one-hour minimum avoids treating an active snapshot or export as crash
	// residue during ordinary concurrent and long-running tooling operations.
	toolingStagingOrphanAge = time.Hour
	// The disposable database the forward-migration proof runs against, named
	// beside the artifact inside the same private staging directory.
	toolingMigrationCloneSuffix = ".migration-clone"
	// Connection parameters for the two ways tooling opens a database: sources
	// and generated artifacts are read, and only the disposable clone is written.
	toolingReadOnlyQuery  = "_busy_timeout=5000&_foreign_keys=1&mode=ro"
	toolingReadWriteQuery = "_busy_timeout=5000&_foreign_keys=1"
)

type toolingDestination struct {
	temp    string
	staging string
	lock    *os.File
}

// Snapshot copies source into dest through SQLite so the copy is consistent with
// a live WAL writer.
func Snapshot(ctx context.Context, source, dest string) (result error) {
	if contextErr := validateToolingContext(ctx); contextErr != nil {
		return contextErr
	}
	prepared, prepareErr := prepareToolingDestination(dest)
	if prepareErr != nil {
		return prepareErr
	}
	defer func() {
		temp := prepared.temp
		if removeErr := removeToolingStaging(prepared); removeErr != nil {
			result = errors.Join(
				result,
				fmt.Errorf("remove partial usage snapshot %q: %w", temp, removeErr),
			)
		}
	}()

	db, openErr := openToolingDatabase(ctx, source)
	if openErr != nil {
		return openErr
	}
	closed := false
	defer func() {
		if closed {
			return
		}
		if closeErr := db.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close usage database %q: %w", source, closeErr))
		}
	}()

	if _, vacuumErr := db.ExecContext(ctx, `VACUUM INTO ?`, prepared.temp); vacuumErr != nil {
		return fmt.Errorf("snapshot usage database %q: %w", source, vacuumErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		return fmt.Errorf("close usage database %q: %w", source, closeErr)
	}
	closed = true
	if hook, ok := ctx.Value(toolingBeforePublishHookKey{}).(func(string)); ok {
		hook(prepared.temp)
	}
	artifact, artifactErr := openGeneratedToolingDatabase(ctx, prepared.temp)
	if artifactErr != nil {
		return fmt.Errorf("validate usage snapshot %q: %w", prepared.temp, artifactErr)
	}
	if closeErr := artifact.Close(); closeErr != nil {
		return fmt.Errorf("close usage snapshot %q after validation: %w", prepared.temp, closeErr)
	}
	if migrationErr := validateSnapshotMigrationClone(ctx, prepared.temp); migrationErr != nil {
		return fmt.Errorf("validate usage snapshot %q forward migration: %w", prepared.temp, migrationErr)
	}
	return finalizeToolingDestination(prepared, dest)
}

// validateSnapshotMigrationClone proves the generated artifact still migrates
// forward under this build before it is published. Accepting an older schema is
// deliberate — `patchbay serve` migrates one forward when it opens the database —
// but accepting it on the strength of its recorded version alone certifies a
// restore nobody has attempted. A database whose schema_migrations rows are a
// valid prefix can still contradict the objects actually on disk, and that only
// surfaces when the pending migrations run.
//
// They run against a throwaway byte copy inside the same private staging
// directory, never against the artifact. The artifact is what gets published, so
// an older-schema snapshot has to reach its destination exactly as it was read.
func validateSnapshotMigrationClone(ctx context.Context, artifact string) (result error) {
	clone := artifact + toolingMigrationCloneSuffix
	flags := syscall.O_WRONLY | syscall.O_CREAT | syscall.O_EXCL | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	file, createErr := openToolingFile(clone, flags)
	if createErr != nil {
		return fmt.Errorf("create usage migration clone %q: %w", clone, createErr)
	}
	defer func() {
		result = errors.Join(result, removeToolingMigrationClone(clone))
	}()
	if fillErr := fillSnapshotMigrationClone(file, artifact, clone); fillErr != nil {
		return fillErr
	}
	return migrateToolingMigrationClone(ctx, clone)
}

// fillSnapshotMigrationClone secures the exclusively created clone and copies the
// artifact into it byte for byte. The mode is set through the opened descriptor
// rather than relied upon from the creation mode, which the process umask can
// narrow.
func fillSnapshotMigrationClone(clone *os.File, artifact, clonePath string) (result error) {
	defer func() {
		if closeErr := clone.Close(); closeErr != nil {
			result = errors.Join(
				result,
				fmt.Errorf("close usage migration clone %q: %w", clonePath, closeErr),
			)
		}
	}()
	if chmodErr := clone.Chmod(databaseMode); chmodErr != nil {
		return fmt.Errorf("secure usage migration clone %q: %w", clonePath, chmodErr)
	}
	source, openErr := openToolingFile(artifact, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC)
	if openErr != nil {
		return fmt.Errorf("open usage snapshot %q for its migration clone: %w", artifact, openErr)
	}
	defer func() {
		if closeErr := source.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close usage snapshot %q after cloning: %w", artifact, closeErr))
		}
	}()
	if _, copyErr := io.Copy(clone, source); copyErr != nil {
		return fmt.Errorf("copy usage snapshot %q into migration clone %q: %w", artifact, clonePath, copyErr)
	}
	return nil
}

// migrateToolingMigrationClone opens the clone under destination trust — the
// same shape check the generated artifact gets, and deliberately not the strict
// source policy, because an all-squashed destination reports an owner Patchbay
// never runs as — then applies every pending migration and revalidates the
// migrated result.
func migrateToolingMigrationClone(ctx context.Context, clone string) (result error) {
	identity, inspectErr := inspectGeneratedToolingDatabase(clone)
	if inspectErr != nil {
		return inspectErr
	}
	absolute, absoluteErr := filepath.Abs(clone)
	if absoluteErr != nil {
		return fmt.Errorf("resolve usage migration clone path %q: %w", clone, absoluteErr)
	}
	db, openErr := sql.Open("sqlite", toolingDatabaseURI(absolute, toolingReadWriteQuery))
	if openErr != nil {
		return fmt.Errorf("open usage migration clone %q: %w", clone, openErr)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close usage migration clone %q: %w", clone, closeErr))
		}
	}()
	if pingErr := db.PingContext(ctx); pingErr != nil {
		return fmt.Errorf("open usage migration clone %q: ping: %w", clone, pingErr)
	}
	if recheckErr := recheckToolingSource(clone, identity); recheckErr != nil {
		return recheckErr
	}
	if hook, ok := ctx.Value(toolingMigrationCloneHookKey{}).(func(string)); ok {
		hook(clone)
	}
	if migrateErr := migrate(ctx, db); migrateErr != nil {
		return fmt.Errorf("migrate usage migration clone %q: %w", clone, migrateErr)
	}
	if integrityErr := verifyToolingIntegrity(ctx, db, clone); integrityErr != nil {
		return integrityErr
	}
	if _, schemaErr := validateToolingSchema(ctx, db); schemaErr != nil {
		return fmt.Errorf("validate migrated usage migration clone %q: %w", clone, schemaErr)
	}
	return nil
}

// toolingDatabaseURI builds the file URI the SQLite driver expects for an
// absolute local path, with the connection parameters appended as its query.
func toolingDatabaseURI(absolute, query string) string {
	return (&url.URL{Scheme: "file", Path: absolute, RawQuery: query}).String()
}

// removeToolingMigrationClone deletes the disposable clone and every sidecar the
// migration connection may have left beside it. Migrations write, so a rollback
// journal exists for the duration of each one, and none of that may survive into
// the staging directory the publish step is about to finish with.
func removeToolingMigrationClone(clone string) error {
	paths := make([]string, 0, len(databaseSidecarSuffixes())+1)
	paths = append(paths, clone)
	for _, suffix := range databaseSidecarSuffixes() {
		paths = append(paths, clone+suffix)
	}
	removals := make([]error, 0, len(paths))
	for _, path := range paths {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			removals = append(removals, fmt.Errorf("remove usage migration clone %q: %w", path, removeErr))
		}
	}
	return errors.Join(removals...)
}

func validateToolingContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("usage tooling context is nil")
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("usage tooling context: %w", contextErr)
	}
	return nil
}

// toolingPathValidator proves a path is safe to hand to SQLite and records the
// identity the post-open recheck compares against. Which validator applies is the
// trust boundary: external sources get the strict local traversal, while an
// artifact Patchbay generated on the destination filesystem does not.
type toolingPathValidator func(string) (sourceIdentity, error)

// openToolingDatabase opens an existing, supported usage database that Patchbay
// did not create — the live serve database a snapshot or export reads — without
// ever creating, migrating, chmodding, or otherwise modifying the source file.
// External sources get the full strict traversal, sidecars included.
func openToolingDatabase(ctx context.Context, path string) (*sql.DB, error) {
	db, _, openErr := openVersionedToolingDatabase(ctx, path)
	return db, openErr
}

// openVersionedToolingDatabase is openToolingDatabase for the caller that has to
// act on which schema the source records — export reads the current schema's
// columns, so an older source has to be brought forward before it can be read
// that way, and only the validated recorded version says whether it is one.
func openVersionedToolingDatabase(ctx context.Context, path string) (*sql.DB, int, error) {
	return openValidatedToolingDatabase(ctx, path, openSecureToolingSource)
}

// openGeneratedToolingDatabase opens a database Patchbay itself just wrote into
// its own private staging directory — a snapshot artifact, or the migrated clone
// an export of an older schema reads — to confirm it is a readable usage database
// before anything depends on it. It applies destination trust, not source trust:
// see inspectGeneratedToolingDatabase for why the local ownership policy cannot
// run against an all-squashed destination.
func openGeneratedToolingDatabase(ctx context.Context, path string) (*sql.DB, error) {
	db, _, openErr := openValidatedToolingDatabase(ctx, path, inspectGeneratedToolingDatabase)
	return db, openErr
}

func openValidatedToolingDatabase(
	ctx context.Context,
	path string,
	validate toolingPathValidator,
) (*sql.DB, int, error) {
	if path == "" {
		return nil, 0, errors.New("usage database path is blank")
	}
	path = filepath.Clean(path)
	identity, identityErr := validate(path)
	if identityErr != nil {
		return nil, 0, identityErr
	}
	absolute, absoluteErr := filepath.Abs(path)
	if absoluteErr != nil {
		return nil, 0, fmt.Errorf("resolve usage database path %q: %w", path, absoluteErr)
	}
	db, openErr := sql.Open("sqlite", toolingDatabaseURI(absolute, toolingReadOnlyQuery))
	if openErr != nil {
		return nil, 0, fmt.Errorf("open usage database %q: %w", path, openErr)
	}
	if pingErr := db.PingContext(ctx); pingErr != nil {
		return nil, 0, closeAfterError(db, fmt.Errorf("open usage database %q: ping: %w", path, pingErr))
	}
	// SQLite opens database files by path and may reopen them internally; its Go
	// driver offers no fd-passing API. Rechecking detects a source-path swap that
	// occurred while establishing this connection rather than claiming to prevent it.
	if recheckErr := recheckToolingSource(path, identity); recheckErr != nil {
		return nil, 0, closeAfterError(db, recheckErr)
	}
	if integrityErr := verifyToolingIntegrity(ctx, db, path); integrityErr != nil {
		return nil, 0, closeAfterError(db, integrityErr)
	}
	version, schemaErr := validateToolingSchema(ctx, db)
	if schemaErr != nil {
		return nil, 0, closeAfterError(db, schemaErr)
	}
	return db, version, nil
}

// verifyToolingIntegrity proves the exact database this connection opened reads
// back coherently, before anything downstream treats it as good data. Schema
// validation reads only schema_migrations and sqlite_master, and `VACUUM INTO`
// rebuilds secondary indexes from table rows rather than copying them, so a
// source whose index disagrees with its table satisfies both while remaining a
// corrupt original — and the snapshot taken from it certifies that corruption as
// a good backup. The check is read-only: it never repairs what it inspects.
func verifyToolingIntegrity(ctx context.Context, queryer schemaQueryer, path string) (result error) {
	rows, queryErr := queryer.QueryContext(ctx, `PRAGMA integrity_check`)
	if queryErr != nil {
		return fmt.Errorf("check usage database %q integrity: %w", path, queryErr)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			result = errors.Join(
				result,
				fmt.Errorf("close usage database %q integrity results: %w", path, closeErr),
			)
		}
	}()
	reported := make([]string, 0, 1)
	for rows.Next() {
		var message string
		if scanErr := rows.Scan(&message); scanErr != nil {
			return fmt.Errorf("scan usage database %q integrity result: %w", path, scanErr)
		}
		reported = append(reported, message)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("iterate usage database %q integrity results: %w", path, rowsErr)
	}
	return toolingIntegrityResultError(path, reported)
}

// toolingIntegrityResultError judges what PRAGMA integrity_check reported.
// SQLite spells success exactly one way — a single row holding the literal
// "ok" — and reports every fault as one or more other rows. Anything else is a
// failure rather than a variant of success: no rows at all means the check did
// not run, and extra rows alongside an "ok" mean it found faults.
func toolingIntegrityResultError(path string, reported []string) error {
	if len(reported) == 1 && reported[0] == "ok" {
		return nil
	}
	if len(reported) == 0 {
		return fmt.Errorf("usage database %q failed integrity check: no result rows", path)
	}
	return fmt.Errorf("usage database %q failed integrity check: %s", path, strings.Join(reported, "; "))
}

// inspectGeneratedToolingDatabase records the device and inode of the snapshot
// Patchbay just wrote inside its own private staging directory, after proving the
// path is a regular file reached without following a symbolic link.
//
// It deliberately performs no ancestor traversal and no local-UID ownership check.
// The artifact is not an external source: it lives on the destination filesystem,
// which in deployment is an all-squashed network export where every file and
// directory reports a mapped owner such as uid 1024 that Patchbay never runs as.
// Applying openSecureToolingSource there would refuse Patchbay's own output.
// Destination integrity rests instead on the private staging directory, its
// exclusive lock, and no-replace publication.
func inspectGeneratedToolingDatabase(path string) (sourceIdentity, error) {
	flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC
	fd, openErr := syscall.Open(path, flags, 0)
	if openErr != nil {
		if errors.Is(openErr, syscall.ENOENT) {
			return sourceIdentity{}, fmt.Errorf("generated usage database %q does not exist", path)
		}
		if errors.Is(openErr, syscall.ELOOP) {
			return sourceIdentity{}, fmt.Errorf("generated usage database %q is a symbolic link", path)
		}
		return sourceIdentity{}, fmt.Errorf(
			"open generated usage database %q without following links: %w",
			path,
			openErr,
		)
	}
	var stat syscall.Stat_t
	if statErr := syscall.Fstat(fd, &stat); statErr != nil {
		cause := fmt.Errorf("inspect generated usage database %q: %w", path, statErr)
		return sourceIdentity{}, closeDescriptorAfterError(fd, "generated usage database handle", cause)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		cause := fmt.Errorf("generated usage database %q is not a regular file", path)
		return sourceIdentity{}, closeDescriptorAfterError(fd, "non-regular generated usage database", cause)
	}
	if closeErr := syscall.Close(fd); closeErr != nil {
		return sourceIdentity{}, fmt.Errorf("close generated usage database %q handle: %w", path, closeErr)
	}
	return sourceIdentity{
		device: fmt.Sprintf("%d", stat.Dev),
		inode:  fmt.Sprintf("%d", stat.Ino),
	}, nil
}

func recheckToolingSource(path string, expected sourceIdentity) error {
	info, statErr := os.Lstat(path)
	if statErr != nil {
		return fmt.Errorf("reinspect usage database %q after open: %w", path, statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("usage database path %q changed while opening", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect usage database %q identity after open: unsupported stat type", path)
	}
	if fmt.Sprintf("%d", stat.Dev) != expected.device || fmt.Sprintf("%d", stat.Ino) != expected.inode {
		return fmt.Errorf("usage database path %q changed while opening", path)
	}
	return nil
}

type schemaQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validateToolingSchema(ctx context.Context, queryer schemaQueryer) (version int, result error) {
	rows, queryErr := queryer.QueryContext(ctx, `SELECT version, name FROM schema_migrations ORDER BY version`)
	if queryErr != nil {
		return 0, fmt.Errorf("not a patchbay usage database: read schema_migrations: %w", queryErr)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close schema_migrations rows: %w", closeErr))
		}
	}()
	migrations := declaredMigrations()
	index := 0
	for rows.Next() {
		var recordedVersion int
		var recordedName string
		if scanErr := rows.Scan(&recordedVersion, &recordedName); scanErr != nil {
			return 0, fmt.Errorf("not a patchbay usage database: scan schema_migrations: %w", scanErr)
		}
		if index >= len(migrations) || recordedVersion > migrations[len(migrations)-1].version {
			return 0, fmt.Errorf(
				"unknown future schema version %d (latest supported is %d)",
				recordedVersion,
				latestSchemaVersion(),
			)
		}
		expected := migrations[index]
		if recordedVersion != expected.version || recordedName != expected.name {
			return 0, fmt.Errorf(
				"not a patchbay usage database: migration %d is %q; want %d %q",
				recordedVersion, recordedName, expected.version, expected.name,
			)
		}
		version = recordedVersion
		index++
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return 0, fmt.Errorf("not a patchbay usage database: iterate schema_migrations: %w", rowsErr)
	}
	if index == 0 {
		return 0, errors.New("not a patchbay usage database: schema_migrations is empty")
	}
	// The rows parsed above are a claim about which migrations ran, not evidence
	// that what they built is still there. Hold the database to the objects that
	// version promised.
	if objectErr := validateRecordedSchemaObjects(ctx, queryer, version); objectErr != nil {
		return 0, objectErr
	}
	return version, nil
}

func latestSchemaVersion() int {
	migrations := declaredMigrations()
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}

func prepareToolingDestination(dest string) (*toolingDestination, error) {
	if dest == "" {
		return nil, errors.New("usage tooling destination is blank")
	}
	dest = filepath.Clean(dest)
	dir := filepath.Dir(dest)
	info, statErr := os.Stat(dir)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil, fmt.Errorf("destination directory %q does not exist", dir)
		}
		return nil, fmt.Errorf("inspect destination directory %q: %w", dir, statErr)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("destination directory %q is not a directory", dir)
	}
	if _, destinationErr := os.Lstat(dest); destinationErr == nil {
		return nil, fmt.Errorf("usage tooling destination %q already exists", dest)
	} else if !errors.Is(destinationErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect usage tooling destination %q: %w", dest, destinationErr)
	}
	prefix := toolingStagingNamePrefix(dest)
	if scavengeErr := scavengeToolingStaging(dir, prefix, time.Now()); scavengeErr != nil {
		return nil, scavengeErr
	}
	staging, createErr := os.MkdirTemp(dir, prefix+"*")
	if createErr != nil {
		return nil, fmt.Errorf("create temporary usage output directory in %q: %w", dir, createErr)
	}
	if chmodErr := os.Chmod(staging, parentMode); chmodErr != nil {
		if removeErr := os.Remove(staging); removeErr != nil {
			return nil, errors.Join(
				fmt.Errorf("secure temporary usage output directory %q: %w", staging, chmodErr),
				fmt.Errorf("remove temporary usage output directory %q: %w", staging, removeErr),
			)
		}
		return nil, fmt.Errorf("secure temporary usage output directory %q: %w", staging, chmodErr)
	}
	lock, lockErr := createToolingStagingLock(staging)
	if lockErr != nil {
		if removeErr := os.RemoveAll(staging); removeErr != nil {
			return nil, errors.Join(lockErr, fmt.Errorf("remove unlocked usage staging %q: %w", staging, removeErr))
		}
		return nil, lockErr
	}
	return &toolingDestination{
		temp:    filepath.Join(staging, filepath.Base(dest)),
		staging: staging,
		lock:    lock,
	}, nil
}

func createToolingStagingLock(staging string) (*os.File, error) {
	path := filepath.Join(staging, toolingStagingLockName)
	flags := syscall.O_RDWR | syscall.O_CREAT | syscall.O_EXCL | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
	lock, openErr := openToolingFile(path, flags)
	if openErr != nil {
		return nil, fmt.Errorf("create usage staging lock %q: %w", path, openErr)
	}
	if lockErr := unix.Flock(int(lock.Fd()), unix.LOCK_EX); lockErr != nil {
		if closeErr := lock.Close(); closeErr != nil {
			return nil, errors.Join(
				fmt.Errorf("lock usage staging %q: %w", staging, lockErr),
				fmt.Errorf("close usage staging lock %q: %w", path, closeErr),
			)
		}
		return nil, fmt.Errorf("lock usage staging %q: %w", staging, lockErr)
	}
	return lock, nil
}

func toolingStagingNamePrefix(dest string) string {
	return toolingStagingPrefix + filepath.Base(dest) + ".partial-"
}

func scavengeToolingStaging(dir, prefix string, now time.Time) error {
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		return fmt.Errorf("scan destination directory %q for partial usage outputs: %w", dir, readErr)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, infoErr := entry.Info()
		if infoErr != nil {
			if errors.Is(infoErr, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("inspect partial usage output %q: %w", path, infoErr)
		}
		if !info.IsDir() || now.Sub(info.ModTime()) < toolingStagingOrphanAge {
			continue
		}
		if removeErr := removeUnlockedToolingStaging(path); removeErr != nil {
			return removeErr
		}
	}
	return nil
}

func removeUnlockedToolingStaging(staging string) error {
	lockPath := filepath.Join(staging, toolingStagingLockName)
	flags := syscall.O_RDWR | syscall.O_CREAT | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
	lock, openErr := openToolingFile(lockPath, flags)
	if openErr != nil {
		if errors.Is(openErr, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open stale usage staging lock %q: %w", lockPath, openErr)
	}
	lockErr := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if lockErr != nil {
		closeErr := lock.Close()
		if errors.Is(lockErr, unix.EWOULDBLOCK) || errors.Is(lockErr, unix.EAGAIN) {
			if closeErr != nil {
				return fmt.Errorf("close live usage staging lock %q: %w", lockPath, closeErr)
			}
			return nil
		}
		return errors.Join(
			fmt.Errorf("lock stale usage staging %q: %w", staging, lockErr),
			wrapToolingLockCloseError(lockPath, closeErr),
		)
	}
	removeErr := os.RemoveAll(staging)
	releaseErr := releaseToolingStagingLock(staging, lock)
	return errors.Join(
		wrapToolingStagingRemoveError(staging, removeErr),
		releaseErr,
	)
}

func removeToolingStaging(prepared *toolingDestination) error {
	if prepared == nil {
		return nil
	}
	staging := prepared.staging
	lock := prepared.lock
	prepared.temp = ""
	prepared.staging = ""
	prepared.lock = nil

	var removeErr error
	if staging != "" {
		removeErr = os.RemoveAll(staging)
	}
	return errors.Join(
		wrapToolingStagingRemoveError(staging, removeErr),
		releaseToolingStagingLock(staging, lock),
	)
}

func wrapToolingStagingRemoveError(staging string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("remove temporary usage output directory %q: %w", staging, err)
}

func releaseToolingStagingLock(staging string, lock *os.File) error {
	if lock == nil {
		return nil
	}
	lockPath := lock.Name()
	unlockErr := unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	closeErr := lock.Close()
	return errors.Join(
		wrapToolingLockUnlockError(staging, unlockErr),
		wrapToolingLockCloseError(lockPath, closeErr),
	)
}

func wrapToolingLockUnlockError(staging string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("unlock usage staging %q: %w", staging, err)
}

func wrapToolingLockCloseError(path string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close usage staging lock %q: %w", path, err)
}

func finalizeToolingDestination(prepared *toolingDestination, dest string) error {
	if prepared == nil || prepared.temp == "" {
		return errors.New("usage tooling destination is not prepared")
	}
	temp := prepared.temp
	if chmodErr := os.Chmod(temp, databaseMode); chmodErr != nil {
		return fmt.Errorf("secure temporary usage output %q: %w", temp, chmodErr)
	}
	if syncErr := syncToolingFile(temp); syncErr != nil {
		return syncErr
	}
	if linkErr := os.Link(temp, dest); linkErr != nil {
		return fmt.Errorf("publish usage output %q without replacement: %w", dest, linkErr)
	}
	if removeErr := removeToolingStaging(prepared); removeErr != nil {
		return removeErr
	}
	return syncToolingDirectory(filepath.Dir(dest))
}

func syncToolingFile(path string) error {
	file, openErr := openToolingFile(path, syscall.O_RDONLY)
	if openErr != nil {
		return fmt.Errorf("open temporary usage output %q for sync: %w", path, openErr)
	}
	if syncErr := file.Sync(); syncErr != nil {
		if closeErr := file.Close(); closeErr != nil {
			return errors.Join(
				fmt.Errorf("sync temporary usage output %q: %w", path, syncErr),
				fmt.Errorf("close temporary usage output %q after failed sync: %w", path, closeErr),
			)
		}
		return fmt.Errorf("sync temporary usage output %q: %w", path, syncErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("close temporary usage output %q after sync: %w", path, closeErr)
	}
	return nil
}

func syncToolingDirectory(path string) error {
	dir, openErr := openToolingFile(path, syscall.O_RDONLY)
	if openErr != nil {
		return fmt.Errorf("open destination directory %q for sync: %w", path, openErr)
	}
	if syncErr := dir.Sync(); syncErr != nil {
		if closeErr := dir.Close(); closeErr != nil {
			return errors.Join(
				fmt.Errorf("sync destination directory %q: %w", path, syncErr),
				fmt.Errorf("close destination directory %q after failed sync: %w", path, closeErr),
			)
		}
		return fmt.Errorf("sync destination directory %q: %w", path, syncErr)
	}
	if closeErr := dir.Close(); closeErr != nil {
		return fmt.Errorf("close destination directory %q after sync: %w", path, closeErr)
	}
	return nil
}

func openToolingFile(path string, flags int) (*os.File, error) {
	fd, openErr := syscall.Open(path, flags, uint32(databaseMode))
	if openErr != nil {
		return nil, fmt.Errorf("open %q: %w", path, openErr)
	}
	return os.NewFile(uintptr(fd), path), nil
}
