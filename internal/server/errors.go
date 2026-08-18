package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/joshsymonds/patchbay/internal/anthropic"
)

// writeError answers with an Anthropic-format error the caller can recognize as
// patchbay's own, and logs what actually went wrong.
func (gw *gateway) writeError(ctx context.Context, w http.ResponseWriter, status int, err error) {
	gw.logger.WarnContext(ctx, "gateway error", slog.Int("status", status), slog.Any("error", err))

	w.Header().Set(anthropic.HeaderContentType, jsonContentType)
	w.WriteHeader(status)
	gw.write(ctx, w, anthropic.ErrorBody(err))
}
