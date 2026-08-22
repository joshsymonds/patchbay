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
	if migrateErr := migrate(db); migrateErr != nil {
		return nil, closeAfterError(db, fmt.Errorf("open usage database %q: migrate: %w", path, migrateErr))
	}

	return &Store{
		db:          db,
		writePermit: make(chan struct{}, 1),
	}, nil
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
	parentFD, parentErr := openSecureDatabaseParent(filepath.Dir(absolute))
	if parentErr != nil {
		return sourceIdentity{}, parentErr
	}
	defer func() {
		if closeErr := syscall.Close(parentFD); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close usage database parent %q: %w", filepath.Dir(path), closeErr))
			identity = sourceIdentity{}
		}
	}()
	return openSecureDatabaseFile(parentFD, filepath.Base(absolute), path)
}

func openSecureDatabaseParent(parent string) (int, error) {
	flags := syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	current, openErr := syscall.Open(string(filepath.Separator), flags, 0)
	if openErr != nil {
		return -1, fmt.Errorf("open usage database path root: %w", openErr)
	}
	trimmed := strings.TrimPrefix(filepath.Clean(parent), string(filepath.Separator))
	components := strings.Split(trimmed, string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." {
			continue
		}
		next, componentErr := openSecureDatabaseParentComponent(current, component, parent, flags)
		if componentErr != nil {
			return -1, closeDescriptorAfterError(current, "usage database parent traversal handle", componentErr)
		}
		if closeErr := syscall.Close(current); closeErr != nil {
			cause := fmt.Errorf("close usage database parent traversal handle: %w", closeErr)
			return -1, closeDescriptorAfterError(next, "next usage database parent handle", cause)
		}
		current = next
	}
	return current, nil
}

func openSecureDatabaseParentComponent(parentFD int, component, parent string, flags int) (int, error) {
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
	var stat syscall.Stat_t
	if statErr := syscall.Fstat(fd, &stat); statErr != nil {
		return fmt.Errorf("inspect usage database parent %q component %q: %w", parent, component, statErr)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return fmt.Errorf("usage database parent %q component %q is not a directory", parent, component)
	}
	return nil
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
	return nil
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
	flags := syscall.O_RDWR | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
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
	if created {
		if secureErr := secureCreatedDatabase(path, fd, syscall.Fchmod, syscall.Close); secureErr != nil {
			return sourceIdentity{}, secureErr
		}
		return identity, nil
	}
	if secureErr := secureExistingDatabaseDescriptor(path, fd); secureErr != nil {
		return sourceIdentity{}, secureErr
	}
	return identity, nil
}

func secureExistingDatabaseDescriptor(path string, fd int) error {
	chmodErr := syscall.Fchmod(fd, databaseMode)
	closeErr := syscall.Close(fd)
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
