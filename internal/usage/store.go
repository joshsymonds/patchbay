// Package usage stores durable request-accounting state.
package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	// Register the pure-Go SQLite database/sql driver.
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	parentMode               = 0o700
	databaseMode             = 0o600
	walInitializationTimeout = 5 * time.Second
	initialWALRetryDelay     = 10 * time.Millisecond
	maximumWALRetryDelay     = 100 * time.Millisecond
)

// Store is a local SQLite usage store.
type Store struct {
	db          *sql.DB
	writePermit chan struct{}
}

// Open opens path and applies all pending schema migrations.
func Open(path string) (*Store, error) {
	db, err := openJournaledDatabase(path)
	if err != nil {
		return nil, err
	}
	if migrateErr := migrate(context.Background(), db); migrateErr != nil {
		return nil, closeAfterError(db, fmt.Errorf("open usage database %q: migrate: %w", path, migrateErr))
	}

	return &Store{
		db:          db,
		writePermit: make(chan struct{}, 1),
	}, nil
}

// OpenAtCurrentSchema opens path without applying any migration, refusing a
// database whose recorded schema version is not the version this build
// declares. A database a running daemon serves can be appended to this way
// without its schema advancing under that daemon, which could not reopen a
// version it does not know.
func OpenAtCurrentSchema(path string) (*Store, error) {
	db, err := openJournaledDatabase(path)
	if err != nil {
		return nil, err
	}
	if versionErr := requireCurrentVersion(context.Background(), db); versionErr != nil {
		return nil, closeAfterError(db, fmt.Errorf("open usage database %q: %w", path, versionErr))
	}

	return &Store{
		db:          db,
		writePermit: make(chan struct{}, 1),
	}, nil
}

// openJournaledDatabase opens path in WAL journal mode, leaving its schema
// untouched for the caller to migrate or to measure.
func openJournaledDatabase(path string) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("usage database path is blank")
	}
	path = filepath.Clean(path)

	identity, prepareErr := prepareDatabasePathIdentity(path)
	if prepareErr != nil {
		return nil, prepareErr
	}
	db, openErr := openPreparedDatabase(path, identity)
	if openErr != nil {
		return nil, openErr
	}
	walCtx, cancelWAL := context.WithTimeout(context.Background(), walInitializationTimeout)
	defer cancelWAL()
	if walErr := ensureWAL(walCtx, db); walErr != nil {
		return nil, closeAfterError(
			db,
			fmt.Errorf("open usage database %q: initialize WAL journal mode: %w", path, walErr),
		)
	}
	return db, nil
}

func (store *Store) acquireWritePermit(ctx context.Context) error {
	select {
	case store.writePermit <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for usage write permit: %w", ctx.Err())
	}
}

func (store *Store) releaseWritePermit() {
	<-store.writePermit
}

func ensureWAL(ctx context.Context, db *sql.DB) error {
	return initializeWAL(ctx, func(ctx context.Context) (string, error) {
		var mode string
		if err := db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode); err != nil {
			return "", fmt.Errorf("query WAL journal mode: %w", err)
		}
		return mode, nil
	})
}

func initializeWAL(
	ctx context.Context,
	setJournalMode func(context.Context) (string, error),
) error {
	retryDelay := initialWALRetryDelay
	for {
		mode, err := setJournalMode(ctx)
		if err == nil {
			if mode != "wal" {
				return fmt.Errorf("set journal mode to WAL returned %q; want %q", mode, "wal")
			}
			return nil
		}
		if !isSQLiteBusy(err) {
			return fmt.Errorf("set journal mode to WAL: %w", err)
		}

		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait to retry WAL journal mode: %w", ctx.Err())
		case <-timer.C:
		}
		if retryDelay < maximumWALRetryDelay {
			retryDelay *= 2
			if retryDelay > maximumWALRetryDelay {
				retryDelay = maximumWALRetryDelay
			}
		}
	}
}

func isSQLiteBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqlite3.SQLITE_BUSY
}

func openPreparedDatabase(path string, identity sourceIdentity) (*sql.DB, error) {
	absolutePath, absErr := filepath.Abs(path)
	if absErr != nil {
		return nil, fmt.Errorf("resolve usage database path %q: %w", path, absErr)
	}
	dsn := (&url.URL{Scheme: "file", Path: absolutePath}).String() +
		"?_busy_timeout=5000&_foreign_keys=1"
	db, openErr := sql.Open("sqlite", dsn)
	if openErr != nil {
		return nil, fmt.Errorf("open usage database %q: %w", path, openErr)
	}
	if pingErr := db.PingContext(context.Background()); pingErr != nil {
		return nil, closeAfterError(db, fmt.Errorf("open usage database %q: ping: %w", path, pingErr))
	}
	if recheckErr := recheckToolingSource(path, identity); recheckErr != nil {
		return nil, closeAfterError(db, recheckErr)
	}
	return db, nil
}

func prepareDatabasePath(path string) error {
	_, err := prepareDatabasePathIdentity(path)
	return err
}

func prepareDatabasePathIdentity(path string) (identity sourceIdentity, result error) {
	path = filepath.Clean(path)
	absolute, absErr := filepath.Abs(path)
	if absErr != nil {
		return sourceIdentity{}, fmt.Errorf("resolve usage database path %q: %w", path, absErr)
	}
	parentFD, parentErr := openSecureDatabaseParent(filepath.Dir(absolute), createMissingComponents)
	if parentErr != nil {
		return sourceIdentity{}, parentErr
	}
	defer func() {
		if closeErr := syscall.Close(parentFD); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close usage database parent %q: %w", filepath.Dir(path), closeErr))
			identity = sourceIdentity{}
		}
	}()
	name := filepath.Base(absolute)
	if sidecarErr := verifyDatabaseSidecars(parentFD, name, path); sidecarErr != nil {
		return sourceIdentity{}, sidecarErr
	}
	return openSecureDatabaseFile(parentFD, name, path)
}

// databaseSidecarSuffixes returns the companion paths SQLite derives from a
// database pathname and opens by name: the write-ahead log, its shared-memory
// index, and the rollback journal.
func databaseSidecarSuffixes() []string {
	return []string{"-wal", "-shm", "-journal"}
}

// verifyDatabaseSidecars applies the leaf policy to every existing sidecar beside
// name in the already-validated parent, before SQLite receives the database
// pathname. An accepted immediate parent is never group- or world-writable, so a
// missing sidecar is safe: no other local UID can create one. An existing sidecar
// is a different matter — it may predate the parent's current mode. SQLite opens
// these by path during Ping and hot-journal recovery, so a hostile one is read or
// replayed before any leaf check can help. Rejection is read-only: a refused plant
// must be left exactly as it was found.
//
// What this establishes is the sidecar's current owner, mode, and ACL, and that no
// other local UID can open or replace it from here on. It does not establish how
// the sidecar was reachable in the past. A descriptor another UID opened O_RDWR
// during an earlier exposure keeps writing this same inode no matter what the
// current mode says, and no portable stat can see that descriptor or revoke it.
// That residual is out of scope here by construction, not by oversight: the only
// remedy is to replace the file with a fresh inode while Patchbay is stopped. See
// the residual-exposure paragraph in README.md.
func verifyDatabaseSidecars(parentFD int, name, path string) error {
	for _, suffix := range databaseSidecarSuffixes() {
		if err := verifyDatabaseSidecar(parentFD, name+suffix, path+suffix); err != nil {
			return err
		}
	}
	return nil
}

func verifyDatabaseSidecar(parentFD int, name, path string) error {
	// O_NONBLOCK keeps a planted FIFO from stalling the open; it is ignored for
	// the regular files this check accepts.
	flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC
	fd, openErr := unix.Openat(parentFD, name, flags, 0)
	if openErr != nil {
		if errors.Is(openErr, syscall.ENOENT) {
			return nil
		}
		return databaseSidecarOpenError(path, openErr)
	}
	if inspectErr := inspectDatabaseSidecarDescriptor(fd, path); inspectErr != nil {
		return closeDescriptorAfterError(fd, "usage database sidecar", inspectErr)
	}
	if closeErr := syscall.Close(fd); closeErr != nil {
		return fmt.Errorf("close usage database sidecar %q handle: %w", path, closeErr)
	}
	return nil
}

func inspectDatabaseSidecarDescriptor(fd int, path string) error {
	var stat syscall.Stat_t
	if statErr := syscall.Fstat(fd, &stat); statErr != nil {
		return fmt.Errorf("inspect usage database sidecar %q: %w", path, statErr)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return fmt.Errorf("usage database sidecar %q is not a regular file", path)
	}
	effectiveUID := os.Geteuid()
	if !databaseFileOwnerSafe(stat.Uid, effectiveUID, false) {
		return fmt.Errorf(
			"usage database sidecar %q has unsafe owner uid %d; want root or effective uid %d",
			path,
			stat.Uid,
			effectiveUID,
		)
	}
	if !databaseFileModeSafe(os.FileMode(stat.Mode)) {
		return fmt.Errorf(
			"usage database sidecar %q is writable by another local user: mode %#o",
			path,
			os.FileMode(stat.Mode).Perm(),
		)
	}
	return verifyDescriptorACL("usage database sidecar", path, fd, descriptorHasExtendedACL)
}

func databaseSidecarOpenError(path string, cause error) error {
	if errors.Is(cause, syscall.ELOOP) {
		return fmt.Errorf("usage database sidecar %q is a symbolic link", path)
	}
	return fmt.Errorf("open usage database sidecar %q without following links: %w", path, cause)
}

// openSecureToolingSource validates an existing usage database with the same
// descriptor-relative ancestor, sidecar, and leaf policy the live serve path
// applies, but strictly read-only: it never creates a component, never chmods, and
// never otherwise modifies the source. Snapshot and export use it so a foreign
// local UID cannot substitute a path component, or preplant a sidecar, before
// SQLite opens the pathname.
func openSecureToolingSource(path string) (identity sourceIdentity, result error) {
	path = filepath.Clean(path)
	absolute, absErr := filepath.Abs(path)
	if absErr != nil {
		return sourceIdentity{}, fmt.Errorf("resolve usage database path %q: %w", path, absErr)
	}
	parentFD, parentErr := openSecureDatabaseParent(filepath.Dir(absolute), rejectMissingComponents)
	if parentErr != nil {
		return sourceIdentity{}, parentErr
	}
	defer func() {
		if closeErr := syscall.Close(parentFD); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close usage database parent %q: %w", filepath.Dir(path), closeErr))
			identity = sourceIdentity{}
		}
	}()
	name := filepath.Base(absolute)
	if sidecarErr := verifyDatabaseSidecars(parentFD, name, path); sidecarErr != nil {
		return sourceIdentity{}, sidecarErr
	}
	return inspectToolingSourceFile(parentFD, name, path)
}

// inspectToolingSourceFile opens the validated parent's named entry read-only
// and applies the leaf owner, mode, and extended-ACL policy without mutating it.
func inspectToolingSourceFile(parentFD int, name, path string) (sourceIdentity, error) {
	// O_NONBLOCK so a planted FIFO or a slow character device cannot park this
	// read-only open before fstat can refuse the shape; a bare O_RDONLY open of a
	// pipe with no writer never returns. It is ignored for the regular files this
	// check accepts.
	flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC
	fd, openErr := unix.Openat(parentFD, name, flags, 0)
	if openErr != nil {
		if errors.Is(openErr, syscall.ENOENT) {
			return sourceIdentity{}, fmt.Errorf("usage database %q does not exist", path)
		}
		return sourceIdentity{}, databaseFileOpenError(path, openErr)
	}
	identity, inspectErr := inspectToolingSourceDescriptor(fd, path)
	if inspectErr != nil {
		return sourceIdentity{}, closeDescriptorAfterError(fd, "usage database source handle", inspectErr)
	}
	if closeErr := syscall.Close(fd); closeErr != nil {
		return sourceIdentity{}, fmt.Errorf("close usage database %q source handle: %w", path, closeErr)
	}
	return identity, nil
}

func inspectToolingSourceDescriptor(fd int, path string) (sourceIdentity, error) {
	var stat syscall.Stat_t
	if statErr := syscall.Fstat(fd, &stat); statErr != nil {
		return sourceIdentity{}, fmt.Errorf("inspect usage database %q opened handle: %w", path, statErr)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return sourceIdentity{}, fmt.Errorf("usage database path %q is not a regular file", path)
	}
	effectiveUID := os.Geteuid()
	if !databaseFileOwnerSafe(stat.Uid, effectiveUID, false) {
		return sourceIdentity{}, fmt.Errorf(
			"usage database %q has unsafe owner uid %d; want root or effective uid %d",
			path,
			stat.Uid,
			effectiveUID,
		)
	}
	if !databaseFileModeSafe(os.FileMode(stat.Mode)) {
		return sourceIdentity{}, fmt.Errorf(
			"usage database %q is writable by another local user: mode %#o",
			path,
			os.FileMode(stat.Mode).Perm(),
		)
	}
	if aclErr := verifyDescriptorACL("usage database", path, fd, descriptorHasExtendedACL); aclErr != nil {
		return sourceIdentity{}, aclErr
	}
	return sourceIdentity{
		device: fmt.Sprintf("%d", stat.Dev),
		inode:  fmt.Sprintf("%d", stat.Ino),
	}, nil
}

// missingComponentPolicy decides what a traversal does with an absent ancestor:
// the live serve path creates it, while read-only tooling refuses.
type missingComponentPolicy bool

const (
	createMissingComponents missingComponentPolicy = true
	rejectMissingComponents missingComponentPolicy = false
)

func openSecureDatabaseParent(parent string, missing missingComponentPolicy) (int, error) {
	flags := syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	current, openErr := syscall.Open(string(filepath.Separator), flags, 0)
	if openErr != nil {
		return -1, fmt.Errorf("open usage database path root: %w", openErr)
	}
	if verifyErr := verifyDatabaseParentDescriptor(parent, string(filepath.Separator), current); verifyErr != nil {
		return -1, closeDescriptorAfterError(current, "usage database path root", verifyErr)
	}
	trimmed := strings.TrimPrefix(filepath.Clean(parent), string(filepath.Separator))
	components := strings.Split(trimmed, string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." {
			continue
		}
		next, componentErr := openSecureDatabaseParentComponent(current, component, parent, flags, missing)
		if componentErr != nil {
			return -1, closeDescriptorAfterError(current, "usage database parent traversal handle", componentErr)
		}
		if closeErr := syscall.Close(current); closeErr != nil {
			cause := fmt.Errorf("close usage database parent traversal handle: %w", closeErr)
			return -1, closeDescriptorAfterError(next, "next usage database parent handle", cause)
		}
		current = next
	}
	if verifyErr := verifyDatabaseImmediateParentDescriptor(parent, current); verifyErr != nil {
		return -1, closeDescriptorAfterError(current, "usage database immediate parent", verifyErr)
	}
	return current, nil
}

func openSecureDatabaseParentComponent(
	parentFD int,
	component, parent string,
	flags int,
	missing missingComponentPolicy,
) (int, error) {
	next, openErr := unix.Openat(parentFD, component, flags, 0)
	if openErr == nil {
		if verifyErr := verifyDatabaseParentDescriptor(parent, component, next); verifyErr != nil {
			return -1, closeDescriptorAfterError(next, "usage database parent", verifyErr)
		}
		return next, nil
	}
	if !errors.Is(openErr, syscall.ENOENT) {
		return -1, databaseParentComponentError(parent, component, openErr)
	}
	if missing == rejectMissingComponents {
		return -1, fmt.Errorf("usage database parent %q component %q does not exist", parent, component)
	}
	return createDatabaseParentComponent(parentFD, component, parent, flags)
}

func createDatabaseParentComponent(parentFD int, component, parent string, flags int) (int, error) {
	mkdirErr := unix.Mkdirat(parentFD, component, parentMode)
	if mkdirErr != nil && !errors.Is(mkdirErr, syscall.EEXIST) {
		return -1, fmt.Errorf("create usage database parent %q: %w", parent, mkdirErr)
	}
	next, openErr := unix.Openat(parentFD, component, flags, 0)
	if openErr != nil {
		if mkdirErr == nil && errors.Is(openErr, syscall.EACCES) {
			return -1, fmt.Errorf(
				"open created usage database parent %q: process umask removed required %#o permissions: %w",
				parent,
				parentMode,
				openErr,
			)
		}
		return -1, databaseParentComponentError(parent, component, openErr)
	}
	if mkdirErr == nil {
		if verifyErr := verifyCreatedDatabaseParentDescriptor(parent, next, os.Geteuid()); verifyErr != nil {
			return -1, closeDescriptorAfterError(next, "created usage database parent", verifyErr)
		}
	} else if verifyErr := verifyDatabaseParentDescriptor(parent, component, next); verifyErr != nil {
		return -1, closeDescriptorAfterError(next, "usage database parent", verifyErr)
	}
	return next, nil
}

func verifyDatabaseParentDescriptor(parent, component string, fd int) error {
	var stat unix.Stat_t
	if statErr := unix.Fstat(fd, &stat); statErr != nil {
		return fmt.Errorf("inspect usage database parent %q component %q: %w", parent, component, statErr)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("usage database parent %q component %q is not a directory", parent, component)
	}
	if !databaseParentDescriptorSafe(stat, os.Geteuid()) {
		return fmt.Errorf(
			"usage database parent %q component %q is replaceable by another local user: uid %d, mode %#o",
			parent,
			component,
			stat.Uid,
			stat.Mode&(unix.S_ISVTX|unix.S_IRWXU|unix.S_IRWXG|unix.S_IRWXO),
		)
	}
	return verifyDescriptorACL(
		fmt.Sprintf("usage database parent %q component", parent),
		component,
		fd,
		descriptorHasExtendedACL,
	)
}

func verifyDatabaseImmediateParentDescriptor(parent string, fd int) error {
	var stat unix.Stat_t
	if statErr := unix.Fstat(fd, &stat); statErr != nil {
		return fmt.Errorf("inspect usage database immediate parent %q: %w", parent, statErr)
	}
	if stat.Mode&(unix.S_IWGRP|unix.S_IWOTH) != 0 {
		return fmt.Errorf(
			"usage database parent %q is a shared writable immediate parent; SQLite sidecar paths would be replaceable by another local user",
			parent,
		)
	}
	return nil
}

func databaseParentDescriptorSafe(stat unix.Stat_t, effectiveUID int) bool {
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return false
	}
	if stat.Uid != 0 && int64(stat.Uid) != int64(effectiveUID) {
		return false
	}
	if stat.Mode&(unix.S_IWGRP|unix.S_IWOTH) == 0 {
		return true
	}
	return stat.Mode&unix.S_ISVTX != 0
}

type descriptorACLInspector func(int) (bool, error)

func verifyDescriptorACL(
	label string,
	path string,
	fd int,
	inspect descriptorACLInspector,
) error {
	hasACL, inspectErr := inspect(fd)
	if inspectErr != nil {
		return fmt.Errorf("inspect %s %q extended ACL: %w", label, path, inspectErr)
	}
	if hasACL {
		return fmt.Errorf("%s %q has a nontrivial extended ACL", label, path)
	}
	return nil
}

func databaseFileModeSafe(mode os.FileMode) bool {
	return mode&(os.FileMode(unix.S_IWGRP)|os.FileMode(unix.S_IWOTH)) == 0
}

func databaseFileOwnerSafe(ownerUID uint32, effectiveUID int, created bool) bool {
	if created {
		return int64(ownerUID) == int64(effectiveUID)
	}
	return ownerUID == 0 || int64(ownerUID) == int64(effectiveUID)
}

func verifyCreatedDatabaseParentDescriptor(parent string, fd int, effectiveUID int) error {
	var stat syscall.Stat_t
	if statErr := syscall.Fstat(fd, &stat); statErr != nil {
		return fmt.Errorf("inspect created usage database parent %q: %w", parent, statErr)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return fmt.Errorf("created usage database parent %q changed to a non-directory", parent)
	}
	if int64(stat.Uid) != int64(effectiveUID) {
		return fmt.Errorf(
			"created usage database parent %q changed owner while opening: uid %d, want %d",
			parent,
			stat.Uid,
			effectiveUID,
		)
	}
	permissions := os.FileMode(stat.Mode).Perm()
	if permissions != parentMode {
		return fmt.Errorf(
			"created usage database parent %q has mode %#o, want %#o; process umask removed required permissions or the component changed while opening",
			parent,
			permissions,
			parentMode,
		)
	}
	return verifyDescriptorACL(
		"created usage database parent",
		parent,
		fd,
		descriptorHasExtendedACL,
	)
}

func databaseParentComponentError(parent, component string, cause error) error {
	if errors.Is(cause, syscall.ELOOP) || errors.Is(cause, syscall.ENOTDIR) {
		return fmt.Errorf(
			"usage database parent %q contains a symbolic link or non-directory component %q",
			parent,
			component,
		)
	}
	return fmt.Errorf("open usage database parent %q component %q: %w", parent, component, cause)
}

func openSecureDatabaseFile(parentFD int, name, path string) (sourceIdentity, error) {
	// O_NONBLOCK for the same reason as the read-only tooling open: refuse a
	// planted special file promptly rather than waiting on a peer that an
	// attacker will never provide. It has no effect on the regular files this
	// path opens or creates.
	flags := syscall.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC
	for {
		fd, openErr := unix.Openat(parentFD, name, flags, 0)
		if openErr == nil {
			return inspectSecureDatabaseFile(fd, false, path)
		}
		if !errors.Is(openErr, syscall.ENOENT) {
			return sourceIdentity{}, databaseFileOpenError(path, openErr)
		}
		fd, createErr := unix.Openat(
			parentFD,
			name,
			flags|syscall.O_CREAT|syscall.O_EXCL,
			databaseMode,
		)
		if errors.Is(createErr, syscall.EEXIST) {
			continue
		}
		if createErr != nil {
			return sourceIdentity{}, databaseFileOpenError(path, createErr)
		}
		return inspectSecureDatabaseFile(fd, true, path)
	}
}

func inspectSecureDatabaseFile(fd int, created bool, path string) (sourceIdentity, error) {
	var stat syscall.Stat_t
	if statErr := syscall.Fstat(fd, &stat); statErr != nil {
		cause := fmt.Errorf("inspect usage database %q opened handle: %w", path, statErr)
		return sourceIdentity{}, closeDescriptorAfterError(fd, "usage database opened handle", cause)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		cause := fmt.Errorf("usage database path %q is not a regular file", path)
		if stat.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			cause = fmt.Errorf("usage database path %q is a directory", path)
		}
		return sourceIdentity{}, closeDescriptorAfterError(fd, "non-regular usage database", cause)
	}
	identity := sourceIdentity{device: fmt.Sprintf("%d", stat.Dev), inode: fmt.Sprintf("%d", stat.Ino)}
	effectiveUID := os.Geteuid()
	if !databaseFileOwnerSafe(stat.Uid, effectiveUID, created) {
		ownerErr := fmt.Errorf(
			"usage database %q has unsafe owner uid %d; want root or effective uid %d",
			path,
			stat.Uid,
			effectiveUID,
		)
		if created {
			ownerErr = fmt.Errorf(
				"created usage database %q changed owner while opening: uid %d, want %d",
				path,
				stat.Uid,
				effectiveUID,
			)
		}
		return sourceIdentity{}, closeDescriptorAfterError(fd, "usage database after owner rejection", ownerErr)
	}
	if !created && !databaseFileModeSafe(os.FileMode(stat.Mode)) {
		modeErr := fmt.Errorf(
			"usage database %q is writable by another local user: mode %#o",
			path,
			os.FileMode(stat.Mode).Perm(),
		)
		return sourceIdentity{}, closeDescriptorAfterError(fd, "usage database after mode rejection", modeErr)
	}
	if aclErr := verifyDescriptorACL(
		"usage database",
		path,
		fd,
		descriptorHasExtendedACL,
	); aclErr != nil {
		return sourceIdentity{}, closeDescriptorAfterError(fd, "usage database after ACL rejection", aclErr)
	}
	if created {
		if secureErr := secureCreatedDatabase(path, fd, syscall.Fchmod, syscall.Close); secureErr != nil {
			return sourceIdentity{}, secureErr
		}
		return identity, nil
	}
	if secureErr := secureExistingDatabaseDescriptor(
		path,
		fd,
		stat.Uid,
		effectiveUID,
		syscall.Fchmod,
		syscall.Close,
	); secureErr != nil {
		return sourceIdentity{}, secureErr
	}
	return identity, nil
}

func secureExistingDatabaseDescriptor(
	path string,
	fd int,
	ownerUID uint32,
	effectiveUID int,
	chmod func(int, uint32) error,
	closeFile func(int) error,
) error {
	if !databaseFileOwnerSafe(ownerUID, effectiveUID, false) {
		ownerErr := fmt.Errorf(
			"usage database %q has unsafe owner uid %d; want root or effective uid %d",
			path,
			ownerUID,
			effectiveUID,
		)
		closeErr := closeFile(fd)
		if closeErr != nil {
			closeErr = fmt.Errorf("close usage database %q after owner rejection: %w", path, closeErr)
		}
		return errors.Join(ownerErr, closeErr)
	}
	chmodErr := chmod(fd, databaseMode)
	closeErr := closeFile(fd)
	if chmodErr != nil {
		chmodErr = fmt.Errorf("secure usage database %q: %w", path, chmodErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("close usage database %q after securing: %w", path, closeErr)
	}
	return errors.Join(chmodErr, closeErr)
}

func databaseFileOpenError(path string, cause error) error {
	if errors.Is(cause, syscall.ELOOP) {
		return fmt.Errorf("usage database path %q is a symbolic link", path)
	}
	return fmt.Errorf("open usage database %q without following links: %w", path, cause)
}

func closeDescriptorAfterError(fd int, label string, cause error) error {
	if closeErr := syscall.Close(fd); closeErr != nil {
		return errors.Join(cause, fmt.Errorf("close %s: %w", label, closeErr))
	}
	return cause
}

func secureCreatedDatabase(
	path string,
	fd int,
	chmod func(int, uint32) error,
	closeFile func(int) error,
) error {
	chmodErr := chmod(fd, databaseMode)
	closeErr := closeFile(fd)
	if chmodErr != nil {
		chmodErr = fmt.Errorf("secure usage database %q after creation: %w", path, chmodErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("close usage database %q after secure creation: %w", path, closeErr)
	}
	if chmodErr != nil && closeErr != nil {
		return errors.Join(chmodErr, closeErr)
	}
	if chmodErr != nil {
		return chmodErr
	}
	return closeErr
}

// Close closes the usage store.
func (store *Store) Close() error {
	if err := store.db.Close(); err != nil {
		return fmt.Errorf("close usage database: %w", err)
	}
	return nil
}

func closeAfterError(db *sql.DB, cause error) error {
	if closeErr := db.Close(); closeErr != nil {
		return errors.Join(cause, fmt.Errorf("close usage database after error: %w", closeErr))
	}
	return cause
}
