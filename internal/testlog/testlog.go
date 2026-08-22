// Package testlog provides slog capture infrastructure for tests.
package testlog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// Entry is one captured log record.
type Entry struct {
	level   slog.Level
	message string
	attrs   string
}

// Attrs returns the record's attributes in slog text-handler key=value form.
func (e Entry) Attrs() string { return e.attrs }

// Text renders the record's level, message, and attributes.
func (e Entry) Text() string {
	return e.level.String() + " " + e.message + " " + e.attrs
}

// OperatorVisible reports whether the record is at warning level or higher.
func (e Entry) OperatorVisible() bool { return e.level >= slog.LevelWarn }

type store struct {
	mu      sync.Mutex
	entries []Entry
	lines   chan map[string]string
}

// Capture is an immutable slog.Handler view over shared captured storage.
type Capture struct {
	store  *store
	attrs  []boundAttr
	groups []string
}

type boundAttr struct {
	groups []string
	attr   slog.Attr
}

// New returns a capture whose wait queue can hold buffer records.
func New(buffer int) *Capture {
	return &Capture{store: &store{lines: make(chan map[string]string, buffer)}}
}

// Logger returns a logger that writes to the capture.
func (c *Capture) Logger() *slog.Logger { return slog.New(c) }

// Enabled captures records at every level.
func (*Capture) Enabled(context.Context, slog.Level) bool { return true }

// Handle stores one record and makes it available to WaitFor without blocking.
func (c *Capture) Handle(_ context.Context, record slog.Record) error {
	line := map[string]string{"message": record.Message}
	var attrs strings.Builder

	for _, bound := range c.attrs {
		captureAttr(line, &attrs, bound.groups, bound.attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		captureAttr(line, &attrs, c.groups, attr)

		return true
	})

	c.store.mu.Lock()
	c.store.entries = append(c.store.entries, Entry{
		level:   record.Level,
		message: record.Message,
		attrs:   attrs.String(),
	})
	c.store.mu.Unlock()

	select {
	case c.store.lines <- line:
	default:
	}

	return nil
}

// WithAttrs returns a derived handler with attrs bound under the current groups.
func (c *Capture) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return c
	}

	bound := make([]boundAttr, 0, len(c.attrs)+len(attrs))
	bound = append(bound, c.attrs...)
	for _, attr := range attrs {
		bound = append(bound, boundAttr{groups: cloneStrings(c.groups), attr: attr})
	}

	return &Capture{store: c.store, attrs: bound, groups: cloneStrings(c.groups)}
}

// WithGroup returns a derived handler with name appended to its group path.
func (c *Capture) WithGroup(name string) slog.Handler {
	if name == "" {
		return c
	}

	groups := make([]string, len(c.groups)+1)
	copy(groups, c.groups)
	groups[len(c.groups)] = name

	return &Capture{store: c.store, attrs: c.attrs, groups: groups}
}

// Matching returns captured records with exactly message, in logging order.
func (c *Capture) Matching(message string) []Entry {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	found := make([]Entry, 0, len(c.store.entries))
	for _, entry := range c.store.entries {
		if entry.message == message {
			found = append(found, entry)
		}
	}

	return found
}

// WaitFor returns the first queued record with message or fails after timeout.
func (c *Capture) WaitFor(tb testing.TB, message string, timeout time.Duration) map[string]string {
	tb.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case line := <-c.store.lines:
			if line["message"] == message {
				return line
			}
		case <-timer.C:
			tb.Fatalf("log message %q never arrived within %s", message, timeout)

			return nil
		}
	}
}

// String renders every captured record, one per line.
func (c *Capture) String() string {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	var out strings.Builder
	for _, entry := range c.store.entries {
		out.WriteString(entry.Text())
		out.WriteByte('\n')
	}

	return out.String()
}

func captureAttr(line map[string]string, out *strings.Builder, groups []string, attr slog.Attr) {
	value := attr.Value.Resolve()
	if value.Kind() != slog.KindGroup {
		key := qualify(groups, attr.Key)
		line[key] = value.String()
		fmt.Fprintf(out, "%s=%v ", key, value)

		return
	}

	nested := groups
	if attr.Key != "" {
		nested = append(cloneStrings(groups), attr.Key)
	}
	for _, member := range value.Group() {
		captureAttr(line, out, nested, member)
	}
}

func qualify(groups []string, key string) string {
	if len(groups) == 0 {
		return key
	}

	return strings.Join(groups, ".") + "." + key
}

func cloneStrings(values []string) []string {
	return append([]string(nil), values...)
}
