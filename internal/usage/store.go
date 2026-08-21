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

	if prepareErr := prepareDatabasePath(path); prepareErr != nil {
		return nil, prepareErr
	}

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

func prepareDatabasePath(path string) error {
	path = filepath.Clean(path)
	if err := secureDatabaseParent(filepath.Dir(path)); err != nil {
		return err
	}

	for {
		info, err := os.Lstat(path)
		if err == nil {
			return secureExistingDatabase(path, info)
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect usage database %q: %w", path, err)
		}
		created, err := createSecureDatabase(path)
		if err != nil {
			return err
		}
		if created {
			return nil
		}
	}
}

func secureDatabaseParent(parent string) error {
	parent = filepath.Clean(parent)
	if err := os.MkdirAll(parent, parentMode); err != nil {
		return fmt.Errorf("create usage database parent %q: %w", parent, err)
	}
	if err := os.Chmod(parent, parentMode); err != nil {
		return fmt.Errorf("secure usage database parent %q: %w", parent, err)
	}
	return nil
}

func secureExistingDatabase(path string, info os.FileInfo) error {
	path = filepath.Clean(path)
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("usage database path %q is a symbolic link", path)
	}
	if info.IsDir() {
		return fmt.Errorf("usage database path %q is a directory", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("usage database path %q is not a regular file", path)
	}
	if chmodErr := os.Chmod(path, databaseMode); chmodErr != nil {
		return fmt.Errorf("secure usage database %q: %w", path, chmodErr)
	}
	return nil
}

func createSecureDatabase(path string) (bool, error) {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, databaseMode)
	if err == nil {
		if secureErr := secureCreatedDatabase(path, fd, syscall.Fchmod, syscall.Close); secureErr != nil {
			return false, secureErr
		}
		return true, nil
	}
	if errors.Is(err, syscall.EEXIST) {
		return false, nil
	}
	return false, fmt.Errorf("create usage database %q: %w", path, err)
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
