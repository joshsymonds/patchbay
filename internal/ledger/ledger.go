// Package ledger appends one JSONL transcript record per proxied request.
//
// Files rotate by UTC day and are later shipped off the machine, so every
// append is fsynced before it is reported as written and every read surfaces
// corruption instead of quietly dropping the line it could not parse.
package ledger

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"
)

const (
	ledgerDirMode  = 0o700
	ledgerFileMode = 0o600
	filePrefix     = "patchbay-v2-"
	fileSuffix     = ".jsonl"
	dayLayout      = "2006-01-02"
)

// CorruptionError identifies the byte offset of a ledger line that could not be
// read back as a valid record.
type CorruptionError struct {
	Offset int64
	Err    error
}

func (e *CorruptionError) Error() string {
	return fmt.Sprintf("corrupted ledger record at offset %d: %v", e.Offset, e.Err)
}

// Unwrap exposes the parse or validation failure behind the corruption.
func (e *CorruptionError) Unwrap() error {
	return e.Err
}

// Ledger is an append-only JSONL store rooted at one directory, holding one
// file per UTC day. Rotation is the choice of filename at append time; nothing
// runs in the background.
type Ledger struct {
	mu   sync.Mutex
	root *os.Root
}

// Open prepares dir as the ledger root, creating it 0700 if it does not exist.
// The directory is held as an os.Root handle, so every later file operation is
// resolved inside it and cannot escape.
func Open(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, ledgerDirMode); err != nil {
		return nil, fmt.Errorf("create ledger directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open ledger directory: %w", err)
	}
	return &Ledger{root: root}, nil
}

// Close releases the directory handle held by the ledger.
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.root.Close(); err != nil {
		return fmt.Errorf("close ledger directory: %w", err)
	}
	return nil
}

// Append durably writes rec as one JSONL line in the file for rec's UTC day.
// The record is validated first, so a rejected record leaves the ledger
// untouched.
func (l *Ledger) Append(rec Record) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	rec.ErrorBody = truncateUTF8(rec.ErrorBody, maxErrorBodyBytes)

	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode ledger record: %w", err)
	}
	line = append(line, '\n')

	name := dayFileName(rec.Timestamp)

	l.mu.Lock()
	defer l.mu.Unlock()

	file, err := l.root.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, ledgerFileMode)
	if err != nil {
		return fmt.Errorf("open ledger file %s for append: %w", name, err)
	}
	return appendDurableLine(file, line)
}

// Scan returns the records written on day ("2006-01-02") in append order. A
// line that fails to parse or fails validation, and a final line missing its
// newline, stop the scan: the records read before it are returned alongside a
// *CorruptionError naming the byte offset, so damage is visible rather than
// silently skipped. A day with no file yields no records and no error.
func (l *Ledger) Scan(day string) ([]Record, error) {
	name := filePrefix + day + fileSuffix

	l.mu.Lock()
	defer l.mu.Unlock()

	file, err := l.root.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open ledger file %s for scan: %w", name, err)
	}
	records, scanErr := scanLines(file)
	closeErr := file.Close()
	return records, errors.Join(scanErr, wrapError("close ledger file after scan", closeErr))
}

func dayFileName(when time.Time) string {
	return filePrefix + when.UTC().Format(dayLayout) + fileSuffix
}

func appendDurableLine(file *os.File, line []byte) error {
	written, writeErr := file.Write(line)
	if writeErr == nil && written != len(line) {
		writeErr = io.ErrShortWrite
	}
	var syncErr error
	if writeErr == nil {
		syncErr = file.Sync()
	}
	closeErr := file.Close()
	return errors.Join(
		wrapError("append ledger record", writeErr),
		wrapError("sync ledger record", syncErr),
		wrapError("close ledger record", closeErr),
	)
}

func scanLines(reader io.Reader) ([]Record, error) {
	records := make([]Record, 0)
	buffered := bufio.NewReader(reader)
	var offset int64
	for {
		line, readErr := buffered.ReadBytes('\n')
		if len(line) != 0 {
			if line[len(line)-1] != '\n' {
				return records, &CorruptionError{Offset: offset, Err: io.ErrUnexpectedEOF}
			}
			// decodeRecord owns the whole reading of a line: unknown fields at
			// every depth, trailing data, and the model keys each record shape must
			// and must not carry — none of which survive as questions once a
			// decoded struct is all that is left.
			rec, decodeErr := decodeRecord(line[:len(line)-1])
			if decodeErr != nil {
				return records, &CorruptionError{Offset: offset, Err: decodeErr}
			}
			if validateErr := rec.Validate(); validateErr != nil {
				return records, &CorruptionError{Offset: offset, Err: validateErr}
			}
			records = append(records, rec)
			offset += int64(len(line))
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return records, nil
			}
			return records, fmt.Errorf("read ledger at offset %d: %w", offset, readErr)
		}
	}
}

func wrapError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
