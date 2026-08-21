package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/usage"
)

// UsageSummaryEndpoint is the caller-key-gated local usage-accounting endpoint.
const UsageSummaryEndpoint = introspectPrefix + "usage/summary"

type usageSummaryResponse struct {
	Since   string        `json:"since"`
	Until   string        `json:"until"`
	Context string        `json:"context"`
	Summary usage.Summary `json:"summary"`
}

// usageSummary answers a durable aggregate over the caller's requested window.
// It lives beneath the introspection prefix because the data names how this
// machine spent money, and the caller key protects it for the same reason it
// protects the routing document.
func (gw *gateway) usageSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
		gw.writeError(
			r.Context(),
			w,
			http.StatusMethodNotAllowed,
			errors.New(introspectionMethodRefusal),
		)

		return
	}
	if status, err := gw.checkCallerKey(r); err != nil {
		gw.writeError(r.Context(), w, status, err)

		return
	}
	filter, err := parseUsageSummaryFilter(r)
	if err != nil {
		gw.writeError(r.Context(), w, http.StatusBadRequest, err)

		return
	}
	if gw.usage == nil {
		gw.writeError(
			r.Context(),
			w,
			http.StatusServiceUnavailable,
			errors.New("usage accounting unavailable"),
		)

		return
	}

	summary, err := gw.usage.SummarizeUsage(r.Context(), filter)
	if err != nil {
		gw.logger.ErrorContext(r.Context(), "usage summary query failed", slog.Any("error", err))
		gw.writeError(
			r.Context(),
			w,
			http.StatusInternalServerError,
			errors.New("usage summary query failed"),
		)

		return
	}

	gw.writeJSON(r.Context(), w, usageSummaryResponse{
		Since:   filter.Since.UTC().Format(time.RFC3339Nano),
		Until:   filter.Until.UTC().Format(time.RFC3339Nano),
		Context: filter.Context,
		Summary: summary,
	})
}

func parseUsageSummaryFilter(r *http.Request) (usage.SummaryFilter, error) {
	query := r.URL.Query()
	since, err := parseUsageSummaryTime(query.Get("since"), "since")
	if err != nil {
		return usage.SummaryFilter{}, err
	}
	until, err := parseUsageSummaryTime(query.Get("until"), "until")
	if err != nil {
		return usage.SummaryFilter{}, err
	}
	if !until.After(since) {
		return usage.SummaryFilter{}, errors.New("usage summary until must be after since")
	}
	contextName := query.Get("context")
	if contextName != "" {
		if contextErr := registry.ValidContextName(contextName); contextErr != nil {
			return usage.SummaryFilter{}, errors.New("usage summary context is invalid")
		}
	}
	return usage.SummaryFilter{Since: since, Until: until, Context: contextName}, nil
}

func parseUsageSummaryTime(value, name string) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("usage summary %s is required", name)
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("usage summary %s must be RFC3339", name)
	}
	return parsed, nil
}
