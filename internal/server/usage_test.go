package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/server"
	"github.com/joshsymonds/patchbay/internal/usage"
)

const usageSummaryPath = "/_patchbay/usage/summary"

type summaryService struct {
	summary usage.Summary
	err     error
	filters []usage.SummaryFilter
}

func (service *summaryService) SummarizeUsage(
	_ context.Context,
	filter usage.SummaryFilter,
) (usage.Summary, error) {
	service.filters = append(service.filters, filter)
	return service.summary, service.err
}

type usageSummaryResponse struct {
	Since   string        `json:"since"`
	Until   string        `json:"until"`
	Context string        `json:"context"`
	Summary usage.Summary `json:"summary"`
}

func usageRequest(method, target, key string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if key != "" {
		req.Header.Set(proxy.CallerKeyHeader, key)
	}
	return req
}

func TestUsageSummaryAnswersAuthenticatedRequests(t *testing.T) {
	callerKeyFile(t)
	start := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	service := &summaryService{summary: usage.Summary{
		Requests: usage.RequestCounts{Messages: 2},
		Messages: usage.MessageSummary{
			Finalized:        1,
			Pending:          1,
			KnownCostNanoUSD: 7,
			ByBillingClass: map[string]usage.CostSummary{
				"metered": {Requests: 2, KnownCostNanoUSD: 7},
			},
			ByCostBasis: map[string]usage.CostSummary{
				"provider_reported": {Requests: 1, KnownCostNanoUSD: 7},
			},
		},
	}}
	proxy := newSentinel()
	rec := serve(t, server.Config{
		Proxy:    proxy,
		Registry: newRegistry(t, lunaRoute),
		Usage:    service,
	}, usageRequest(http.MethodGet, usageSummaryPath+"?since="+start.Format(time.RFC3339)+"&until="+end.Format(time.RFC3339)+"&context=work", callerKey))

	if rec.Code != http.StatusOK {
		t.Fatalf(
			"GET usage summary status = %d; want %d (body %q)",
			rec.Code,
			http.StatusOK,
			rec.Body,
		)
	}
	if proxy.hits() != 0 {
		t.Fatal("usage summary reached the proxy")
	}
	if len(service.filters) != 1 ||
		service.filters[0] != (usage.SummaryFilter{Since: start, Until: end, Context: "work"}) {
		t.Fatalf("summary filters = %#v; want the parsed window and context", service.filters)
	}
	if got := rec.Header().Get("Content-Type"); got != jsonType {
		t.Fatalf("content type = %q; want %q", got, jsonType)
	}

	var got usageSummaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode usage response %q: %v", rec.Body, err)
	}
	if got.Since != start.Format(time.RFC3339) || got.Until != end.Format(time.RFC3339) ||
		got.Context != "work" ||
		!reflect.DeepEqual(got.Summary, service.summary) {
		t.Fatalf("usage response = %#v; want echoed filter and summary %#v", got, service.summary)
	}
}

func TestUsageSummaryRefusesMissingOrBadCallerKey(t *testing.T) {
	callerKeyFile(t)
	for _, key := range []string{"", "wrong"} {
		t.Run("key="+key, func(t *testing.T) {
			service := &summaryService{}
			rec := serve(
				t,
				server.Config{
					Proxy:    newSentinel(),
					Registry: newRegistry(t, lunaRoute),
					Usage:    service,
				},
				usageRequest(
					http.MethodGet,
					usageSummaryPath+"?since=2026-08-20T12:00:00Z&until=2026-08-20T13:00:00Z",
					key,
				),
			)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d; want %d", rec.Code, http.StatusForbidden)
			}
			if len(service.filters) != 0 ||
				!strings.Contains(rec.Body.String(), "caller key missing or mismatched") {
				t.Fatalf(
					"refusal = filters %#v body %q; want the introspection caller-key refusal",
					service.filters,
					rec.Body,
				)
			}
		})
	}
}

func TestUsageSummaryFailsWhenCallerKeyIsUnavailable(t *testing.T) {
	t.Setenv(proxy.CallerKeyEnvFile, "")
	service := &summaryService{}
	rec := serve(
		t,
		server.Config{
			Proxy:    newSentinel(),
			Registry: newRegistry(t, lunaRoute),
			Usage:    service,
		},
		usageRequest(
			http.MethodGet,
			usageSummaryPath+"?since=2026-08-20T12:00:00Z&until=2026-08-20T13:00:00Z",
			callerKey,
		),
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusInternalServerError)
	}
	if len(service.filters) != 0 || !strings.Contains(rec.Body.String(), "caller key unavailable") {
		t.Fatalf(
			"response = filters %#v body %q; want the introspection caller-key failure",
			service.filters,
			rec.Body,
		)
	}
}

func TestUsageSummaryAuthFailuresMatchRegistry(t *testing.T) {
	query := "?since=2026-08-20T12:00:00Z&until=2026-08-20T13:00:00Z"
	tests := []struct {
		name   string
		method string
		setup  func(*testing.T)
		key    string
	}{
		{
			name:   "missing key",
			method: http.MethodGet,
			setup: func(t *testing.T) {
				t.Helper()
				callerKeyFile(t)
			},
		},
		{
			name:   "wrong key",
			method: http.MethodGet,
			setup: func(t *testing.T) {
				t.Helper()
				callerKeyFile(t)
			},
			key: "wrong",
		},
		{
			name:   "missing key file",
			method: http.MethodGet,
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv(proxy.CallerKeyEnvFile, "")
			},
			key: callerKey,
		},
		{
			name:   "unauthenticated unsupported method",
			method: http.MethodPost,
			setup: func(t *testing.T) {
				t.Helper()
				callerKeyFile(t)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			cfg := server.Config{
				Proxy:    newSentinel(),
				Registry: newRegistry(t, lunaRoute),
				Usage:    &summaryService{},
			}
			registryResponse := serve(t, cfg, usageRequest(tc.method, introspectPath+query, tc.key))
			usageResponse := serve(t, cfg, usageRequest(tc.method, usageSummaryPath+query, tc.key))

			if registryResponse.Code != usageResponse.Code {
				t.Fatalf(
					"statuses = registry %d, usage %d; want equal",
					registryResponse.Code,
					usageResponse.Code,
				)
			}
			if !reflect.DeepEqual(registryResponse.Header(), usageResponse.Header()) {
				t.Fatalf(
					"headers = registry %#v, usage %#v; want equal",
					registryResponse.Header(),
					usageResponse.Header(),
				)
			}
			if !reflect.DeepEqual(registryResponse.Body.Bytes(), usageResponse.Body.Bytes()) {
				t.Fatalf(
					"bodies = registry %q, usage %q; want equal",
					registryResponse.Body,
					usageResponse.Body,
				)
			}
		})
	}
}

func TestUsageSummaryRejectsInvalidQuery(t *testing.T) {
	callerKeyFile(t)
	for _, target := range []string{
		usageSummaryPath,
		usageSummaryPath + "?since=2026-08-20T12:00:00Z",
		usageSummaryPath + "?until=2026-08-20T13:00:00Z",
		usageSummaryPath + "?since=wrong&until=2026-08-20T13:00:00Z",
		usageSummaryPath + "?since=2026-08-20T12:00:00Z&until=wrong",
		usageSummaryPath + "?since=2026-08-20T13:00:00Z&until=2026-08-20T12:00:00Z",
		usageSummaryPath + "?since=2026-08-20T12:00:00Z&until=2026-08-20T13:00:00Z&context=Work",
	} {
		t.Run(target, func(t *testing.T) {
			service := &summaryService{}
			rec := serve(
				t,
				server.Config{
					Proxy:    newSentinel(),
					Registry: newRegistry(t, lunaRoute),
					Usage:    service,
				},
				usageRequest(http.MethodGet, target, callerKey),
			)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf(
					"status = %d; want %d (body %q)",
					rec.Code,
					http.StatusBadRequest,
					rec.Body,
				)
			}
			if len(service.filters) != 0 {
				t.Fatalf("invalid query reached summary service with %#v", service.filters)
			}
			if strings.Contains(rec.Body.String(), "wrong") {
				t.Fatalf("bad-query response echoed raw input: %q", rec.Body)
			}
		})
	}
}

func TestUsageSummaryMethodsAndUnavailableStore(t *testing.T) {
	callerKeyFile(t)
	query := "?since=2026-08-20T12:00:00Z&until=2026-08-20T13:00:00Z"
	for _, tc := range []struct {
		name   string
		method string
		usage  server.UsageSummaryStore
		want   int
	}{
		{name: "head", method: http.MethodHead, usage: &summaryService{}, want: http.StatusOK},
		{name: "post", method: http.MethodPost, usage: &summaryService{}, want: http.StatusMethodNotAllowed},
		{name: "nil usage", method: http.MethodGet, usage: nil, want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(
				t,
				server.Config{
					Proxy:    newSentinel(),
					Registry: newRegistry(t, lunaRoute),
					Usage:    tc.usage,
				},
				usageRequest(tc.method, usageSummaryPath+query, callerKey),
			)
			if rec.Code != tc.want {
				t.Fatalf(
					"%s status = %d; want %d (body %q)",
					tc.method,
					rec.Code,
					tc.want,
					rec.Body,
				)
			}
			if tc.want == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != "GET, HEAD" {
				t.Fatalf("Allow = %q; want GET, HEAD", rec.Header().Get("Allow"))
			}
			if tc.want == http.StatusServiceUnavailable &&
				!strings.Contains(rec.Body.String(), "usage accounting unavailable") {
				t.Fatalf("unavailable body = %q; want a local availability error", rec.Body)
			}
		})
	}
}

func TestUsageSummaryReportsStoreFailureWithoutLeakingIt(t *testing.T) {
	callerKeyFile(t)
	storeErr := errors.New("SQLite diagnostic that callers must not see")
	logs := newLogSink()
	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, lunaRoute),
		Usage:    &summaryService{err: storeErr},
		Logger:   slog.New(logs),
	}, usageRequest(http.MethodGet, usageSummaryPath+"?since=2026-08-20T12:00:00Z&until=2026-08-20T13:00:00Z", callerKey))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), storeErr.Error()) {
		t.Fatalf("store error leaked to caller: %q", rec.Body)
	}
	logs.waitFor(t, "gateway error")
}
