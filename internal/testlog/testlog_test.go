package testlog_test

import (
	"log/slog"
	"maps"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/testlog"
)

func TestCaptureHonorsHandlerDerivation(t *testing.T) {
	t.Parallel()

	capture := testlog.New(16)
	logger := capture.Logger().
		With(slog.String("host", "testhost")).
		WithGroup("request").
		With(
			slog.String("method", "GET"),
			slog.Group(
				"route",
				slog.String("context", "personal"),
				slog.Group("selection", slog.String("seat", "luna")),
			),
		).
		WithGroup("upstream")

	logger.Warn("gateway error", slog.Int("status", 502))
	capture.Logger().With(slog.String("seat", "anthropic")).Info("sibling")
	capture.Logger().Warn("bare")

	const wantAttrs = "host=testhost request.method=GET request.route.context=personal " +
		"request.route.selection.seat=luna request.upstream.status=502 "

	entries := capture.Matching("gateway error")
	if len(entries) != 1 {
		t.Fatalf("capture holds %d gateway errors; want 1\n%s", len(entries), capture)
	}
	if got := entries[0].Attrs(); got != wantAttrs {
		t.Errorf("attrs = %q; want %q", got, wantAttrs)
	}
	if !entries[0].OperatorVisible() {
		t.Errorf("entry %q is quieter than a warning", entries[0].Text())
	}

	if got := capture.Matching("sibling"); len(got) != 1 || got[0].Attrs() != "seat=anthropic " {
		t.Errorf("sibling entries = %v; want one immutable sibling", got)
	}
	if got := capture.Matching("bare"); len(got) != 1 || got[0].Attrs() != "" {
		t.Errorf("bare entries = %v; want one entry without derived attrs", got)
	}

	wantLine := map[string]string{
		"message":                      "gateway error",
		"host":                         "testhost",
		"request.method":               "GET",
		"request.route.context":        "personal",
		"request.route.selection.seat": "luna",
		"request.upstream.status":      "502",
	}
	if got := capture.WaitFor(t, "gateway error", time.Second); !maps.Equal(got, wantLine) {
		t.Errorf("line = %v; want %v", got, wantLine)
	}
}

func TestCaptureIsRaceSafe(t *testing.T) {
	t.Parallel()

	const writers = 32
	capture := testlog.New(writers)
	logger := capture.Logger()

	var wait sync.WaitGroup
	wait.Add(writers)
	for index := range writers {
		go func() {
			defer wait.Done()
			logger.Info("concurrent", slog.Int("index", index))
		}()
	}

	for range writers {
		_ = capture.Matching("concurrent")
		_ = capture.String()
	}
	wait.Wait()

	entries := capture.Matching("concurrent")
	if len(entries) != writers {
		t.Fatalf("captured %d concurrent entries; want %d", len(entries), writers)
	}

	seen := make(map[string]bool, writers)
	for _, entry := range entries {
		seen[entry.Attrs()] = true
	}
	for index := range writers {
		want := "index=" + strconv.Itoa(index) + " "
		if !seen[want] {
			t.Errorf("captured no %q", want)
		}
	}
}
