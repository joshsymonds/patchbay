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
	gw.writeTyped(ctx, w, status, anthropic.ErrorTypeAPI, err)
}

// writeTyped is writeError with the failure named in Anthropic's own taxonomy,
// for a refusal whose honest description is something other than a generic
// gateway error.
func (gw *gateway) writeTyped(ctx context.Context, w http.ResponseWriter, status int, errType string, err error) {
	gw.logger.WarnContext(ctx, "gateway error", slog.Int("status", status), slog.Any("error", err))

	w.Header().Set(anthropic.HeaderContentType, jsonContentType)
	w.WriteHeader(status)
	gw.write(ctx, w, anthropic.TypedErrorBody(errType, err))
}
