// Command patchbay is a single-binary local HTTP gateway: an Anthropic Messages
// API-compatible proxy. This entrypoint is a placeholder that reports its build
// version; the gateway itself lands in later work.
package main

import (
	"log/slog"
	"os"

	"github.com/joshsymonds/patchbay/internal/buildinfo"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	logger.Info("patchbay", "version", buildinfo.Version())
}
