package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

type sourceIdentity struct {
	device string
	inode  string
}

// Snapshot copies source into dest through SQLite so the copy is consistent with
// a live WAL writer.
func Snapshot(ctx context.Context, source, dest string) (result error) {
	if contextErr := validateToolingContext(ctx); contextErr != nil {
		return contextErr
	}
	temp, prepareErr := prepareToolingDestination(dest)
	if prepareErr != nil {
		return prepareErr
	}
	defer func() {
		if temp == "" {
			return
		}
		if removeErr := os.Remove(temp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove partial usage snapshot %q: %w", temp, removeErr))
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

	if _, vacuumErr := db.ExecContext(ctx, `VACUUM INTO ?`, temp); vacuumErr != nil {
		return fmt.Errorf("snapshot usage database %q: %w", source, vacuumErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		return fmt.Errorf("close usage database %q: %w", source, closeErr)
	}
	closed = true
	artifact, artifactErr := openToolingDatabase(ctx, temp)
	if artifactErr != nil {
		return fmt.Errorf("validate usage snapshot %q: %w", temp, artifactErr)
	}
	if closeErr := artifact.Close(); closeErr != nil {
		return fmt.Errorf("close usage snapshot %q after validation: %w", temp, closeErr)
	}
	if publishErr := finalizeToolingDestination(temp, dest); publishErr != nil {
		return publishErr
	}
	temp = ""
	return nil
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

// openToolingDatabase opens an existing, supported usage database without ever
// creating, migrating, chmodding, or otherwise modifying the source file.
func openToolingDatabase(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("usage database path is blank")
	}
	path = filepath.Clean(path)
	identity, identityErr := probeToolingSource(path)
	if identityErr != nil {
		return nil, identityErr
	}
	absolute, absoluteErr := filepath.Abs(path)
	if absoluteErr != nil {
		return nil, fmt.Errorf("resolve usage database path %q: %w", path, absoluteErr)
	}
	uri := &url.URL{Scheme: "file", Path: absolute, RawQuery: "_busy_timeout=5000&_foreign_keys=1&mode=ro"}
	db, openErr := sql.Open("sqlite", uri.String())
	if openErr != nil {
		return nil, fmt.Errorf("open usage database %q: %w", path, openErr)
	}
	if pingErr := db.PingContext(ctx); pingErr != nil {
		return nil, closeAfterError(db, fmt.Errorf("open usage database %q: ping: %w", path, pingErr))
	}
	// SQLite opens database files by path and may reopen them internally; its Go
	// driver offers no fd-passing API. Rechecking detects a source-path swap that
	// occurred while establishing this connection rather than claiming to prevent it.
	if recheckErr := recheckToolingSource(path, identity); recheckErr != nil {
		return nil, closeAfterError(db, recheckErr)
	}
	if _, schemaErr := validateToolingSchema(ctx, db); schemaErr != nil {
		return nil, closeAfterError(db, schemaErr)
	}
	return db, nil
}

func probeToolingSource(path string) (sourceIdentity, error) {
	info, statErr := os.Lstat(path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return sourceIdentity{}, fmt.Errorf("usage database %q does not exist", path)
		}
		return sourceIdentity{}, fmt.Errorf("inspect usage database %q: %w", path, statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return sourceIdentity{}, fmt.Errorf("usage database path %q is a symbolic link", path)
	}
	if !info.Mode().IsRegular() {
		return sourceIdentity{}, fmt.Errorf("usage database path %q is not a regular file", path)
	}
	fd, openErr := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if openErr != nil {
		return sourceIdentity{}, fmt.Errorf("probe usage database %q without following links: %w", path, openErr)
	}
	var stat syscall.Stat_t
	fstatErr := syscall.Fstat(fd, &stat)
	closeErr := syscall.Close(fd)
	if fstatErr != nil {
		return sourceIdentity{}, fmt.Errorf("stat usage database %q probe: %w", path, fstatErr)
	}
	if closeErr != nil {
		return sourceIdentity{}, fmt.Errorf("close usage database %q probe: %w", path, closeErr)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return sourceIdentity{}, fmt.Errorf("usage database path %q is not a regular file", path)
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
	const accountingRequestsTableQuery = `SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'accounting_requests'`
	var accountingRequests int
	table := queryer.QueryRowContext(ctx, accountingRequestsTableQuery)
	if tableErr := table.Scan(&accountingRequests); tableErr != nil {
		return 0, fmt.Errorf("not a patchbay usage database: inspect accounting_requests: %w", tableErr)
	}
	if accountingRequests != 1 {
		return 0, errors.New("not a patchbay usage database: accounting_requests is absent")
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

func prepareToolingDestination(dest string) (string, error) {
	if dest == "" {
		return "", errors.New("usage tooling destination is blank")
	}
	dest = filepath.Clean(dest)
	dir := filepath.Dir(dest)
	info, statErr := os.Stat(dir)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("destination directory %q does not exist", dir)
		}
		return "", fmt.Errorf("inspect destination directory %q: %w", dir, statErr)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("destination directory %q is not a directory", dir)
	}
	if _, destinationErr := os.Lstat(dest); destinationErr == nil {
		return "", fmt.Errorf("usage tooling destination %q already exists", dest)
	} else if !errors.Is(destinationErr, os.ErrNotExist) {
		return "", fmt.Errorf("inspect usage tooling destination %q: %w", dest, destinationErr)
	}
	file, createErr := os.CreateTemp(dir, filepath.Base(dest)+".partial-*")
	if createErr != nil {
		return "", fmt.Errorf("create temporary usage output in %q: %w", dir, createErr)
	}
	temp := file.Name()
	if closeErr := file.Close(); closeErr != nil {
		if removeErr := os.Remove(temp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return "", errors.Join(
				fmt.Errorf("close temporary usage output %q: %w", temp, closeErr),
				fmt.Errorf("remove temporary usage output %q: %w", temp, removeErr),
			)
		}
		return "", fmt.Errorf("close temporary usage output %q: %w", temp, closeErr)
	}
	if removeErr := os.Remove(temp); removeErr != nil {
		return "", fmt.Errorf("prepare temporary usage output %q: %w", temp, removeErr)
	}
	return temp, nil
}

func finalizeToolingDestination(temp, dest string) error {
	if chmodErr := os.Chmod(temp, databaseMode); chmodErr != nil {
		return fmt.Errorf("secure temporary usage output %q: %w", temp, chmodErr)
	}
	if syncErr := syncToolingFile(temp); syncErr != nil {
		return syncErr
	}
	if linkErr := os.Link(temp, dest); linkErr != nil {
		return fmt.Errorf("publish usage output %q without replacement: %w", dest, linkErr)
	}
	if removeErr := os.Remove(temp); removeErr != nil {
		return fmt.Errorf("remove published temporary usage output %q: %w", temp, removeErr)
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
