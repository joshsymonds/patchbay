package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/server"
)

func TestEverythingElseRidesTheProxy(t *testing.T) {
	t.Parallel()

	// The pipeline is the catch-all, not a fallback for paths worth
	// enumerating: a request that names no model resolves to the built-in
	// default route, so every path the Messages API has — and every one it does
	// not — is served by forwarding it.
	tests := []struct {
		name   string
		method string
		target string
	}{
		{name: "messages", method: http.MethodPost, target: "/v1/messages"},
		{name: "count tokens", method: http.MethodPost, target: "/v1/messages/count_tokens"},
		{name: "an endpoint nobody enumerated", method: http.MethodGet, target: "/v1/organizations/usage_report"},
		{name: "the root", method: http.MethodGet, target: "/"},
		{name: "a path that does not exist upstream either", method: http.MethodGet, target: "/nonsense"},
		{name: "a non-GET method on the model list", method: http.MethodPost, target: "/v1/models"},
		{name: "a non-GET method on one model", method: http.MethodDelete, target: "/v1/models/claude-opus-5"},
		{name: "a deeper path under models", method: http.MethodGet, target: "/v1/models/claude-opus-5/extra"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proxy := newSentinel()
			stub := newUpstream(t, jsonHandler(http.StatusOK, upstreamList("claude-opus-5")))

			rec := serve(t, server.Config{
				Proxy:    proxy,
				Registry: newRegistry(t, lunaRoute),
				Client:   anthropicClient(t, stub),
			}, httptest.NewRequest(tc.method, tc.target, strings.NewReader("")))

			if rec.Code != http.StatusTeapot {
				t.Fatalf("status = %d; want the proxy's %d — the request did not reach the pipeline",
					rec.Code, http.StatusTeapot)
			}

			if got := proxy.call(t); got.path != tc.target {
				t.Fatalf("proxy saw path %q; want %q", got.path, tc.target)
			}

			if stub.hits() != 0 {
				t.Fatalf("upstream saw %d model fetches; want 0 for a proxied request", stub.hits())
			}
		})
	}
}

func TestModelEndpointsNeverReachTheProxy(t *testing.T) {
	t.Parallel()

	// The two model endpoints are gateway-local: they are answered from the
	// registry and a fetch of patchbay's own, never through the pipeline, so no
	// ledger record is written for them.
	tests := []struct {
		name   string
		target string
	}{
		{name: "the list", target: "/v1/models"},
		{name: "one model", target: "/v1/models/luna-fast"},
		{name: "one model that is not an alias", target: "/v1/models/claude-opus-5"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proxy := newSentinel()
			stub := newUpstream(t, jsonHandler(http.StatusOK, upstreamList("claude-opus-5")))

			rec := serve(t, server.Config{
				Proxy:    proxy,
				Registry: newRegistry(t, lunaRoute),
				Client:   anthropicClient(t, stub),
			}, modelsRequest(tc.target))

			if rec.Code == http.StatusTeapot {
				t.Fatal("the request reached the proxy; want it answered by the gateway")
			}

			if proxy.hits() != 0 {
				t.Fatalf("proxy saw %d requests; want 0", proxy.hits())
			}
		})
	}
}

func TestANilLoggerFallsBackToTheDefault(t *testing.T) {
	t.Parallel()

	// A nil Logger must not panic the first warning that fires: the upstream
	// here answers 500, which is exactly the degrade path that logs.
	stub := newUpstream(t, jsonHandler(http.StatusInternalServerError, `{"type":"error"}`))

	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, lunaRoute),
		Client:   anthropicClient(t, stub),
		Logger:   nil,
	}, modelsRequest("/v1/models"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusOK)
	}

	if got := ids(t, decodeList(t, rec.Body.Bytes())); len(got) != 1 || got[0] != "luna-fast" {
		t.Fatalf("data ids = %v; want the registry-only [luna-fast]", got)
	}
}
