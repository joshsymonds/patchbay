package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/registry"
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

// workRoutes is a second context's aliases, distinct from the personal ones so
// a listing or a resolution that crossed contexts is visible.
const workRoutes = `"work-atlas":{"base_url":"https://openrouter.ai/api",` +
	`"model":"vendor/atlas-2","auth":"inject",` +
	`"api_key_env_file":"OPENROUTER_API_KEY_FILE","max_input_tokens":131072}`

// twoContexts is a registry with the personal context and a work context whose
// default route is the built-in Anthropic forward.
func twoContexts() string {
	return `"personal":{"routes":{` + lunaRoute + `}},"work":{"routes":{` + workRoutes + `}}`
}

func TestContextPrefixIsStrippedBeforeDispatch(t *testing.T) {
	t.Parallel()

	// The prefix names the context and then gets out of the way: everything
	// downstream — the path gates, the upstream URL — sees the request as if it
	// had arrived without one.
	tests := []struct {
		name        string
		target      string
		wantPath    string
		wantEscaped string
		wantQuery   string
	}{
		{
			name:     "messages under the personal context",
			target:   "/ctx/personal/v1/messages",
			wantPath: "/v1/messages",
		},
		{
			name:     "count tokens under a named context",
			target:   "/ctx/work/v1/messages/count_tokens",
			wantPath: "/v1/messages/count_tokens",
		},
		{
			name:      "the query string survives the strip",
			target:    "/ctx/work/v1/organizations/usage_report?limit=7&after_id=x",
			wantPath:  "/v1/organizations/usage_report",
			wantQuery: "limit=7&after_id=x",
		},
		{
			name:        "an escaped path keeps its own bytes",
			target:      "/ctx/work/v1/spaces/a%2Fb",
			wantPath:    "/v1/spaces/a/b",
			wantEscaped: "/v1/spaces/a%2Fb",
		},
		{
			name:     "the context prefix alone is the root",
			target:   "/ctx/work",
			wantPath: "/",
		},
		{
			name:     "the context prefix with a trailing slash is the root",
			target:   "/ctx/work/",
			wantPath: "/",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proxy := newSentinel()

			rec := serve(t, server.Config{
				Proxy:    proxy,
				Registry: newContextRegistry(t, twoContexts()),
				Client:   anthropicClient(t, newUpstream(t, jsonHandler(http.StatusOK, upstreamList()))),
			}, httptest.NewRequest(http.MethodPost, tc.target, strings.NewReader("")))

			if rec.Code != http.StatusTeapot {
				t.Fatalf("status = %d; want the proxy's %d — the request did not reach the pipeline",
					rec.Code, http.StatusTeapot)
			}

			got := proxy.call(t)

			if got.path != tc.wantPath {
				t.Fatalf("proxy saw path %q; want the stripped %q", got.path, tc.wantPath)
			}

			wantEscaped := tc.wantEscaped
			if wantEscaped == "" {
				wantEscaped = tc.wantPath
			}

			if got.escaped != wantEscaped {
				t.Fatalf("proxy saw escaped path %q; want %q", got.escaped, wantEscaped)
			}

			if got.rawQuery != tc.wantQuery {
				t.Fatalf("proxy saw query %q; want %q", got.rawQuery, tc.wantQuery)
			}
		})
	}
}

func TestUnknownContextFailsClosed(t *testing.T) {
	t.Parallel()

	// A request that named a context patchbay cannot resolve is refused. It is
	// never quietly served by another context, by the personal one, or by the
	// built-in default route — which is what makes a context a boundary rather
	// than a hint.
	tests := []struct {
		name     string
		body     string
		target   string
		wantName string
	}{
		{
			name:     "a name no context claims",
			body:     `{"contexts":{` + twoContexts() + `}}`,
			target:   "/ctx/bogus/v1/messages",
			wantName: "bogus",
		},
		{
			name:     "a name no context claims on the model list",
			body:     `{"contexts":{` + twoContexts() + `}}`,
			target:   "/ctx/bogus/v1/models",
			wantName: "bogus",
		},
		{
			name:     "a name no context claims on one model",
			body:     `{"contexts":{` + twoContexts() + `}}`,
			target:   "/ctx/bogus/v1/models/luna-fast",
			wantName: "bogus",
		},
		{
			name:     "an empty name",
			body:     `{"contexts":{` + twoContexts() + `}}`,
			target:   "/ctx//v1/messages",
			wantName: "",
		},
		{
			name:     "the prefix and nothing else",
			body:     `{"contexts":{` + twoContexts() + `}}`,
			target:   "/ctx/",
			wantName: "",
		},
		{
			name:     "the bare prefix word",
			body:     `{"contexts":{` + twoContexts() + `}}`,
			target:   "/ctx",
			wantName: "",
		},
		{
			name:     "a known name while the registry is broken",
			body:     `{"contexts":{`,
			target:   "/ctx/personal/v1/messages",
			wantName: "personal",
		},
		{
			name:     "a known name while the registry fails validation",
			body:     `{"contexts":{"personal":{"routes":{"bad":{"auth":"nonsense"}}}}}`,
			target:   "/ctx/personal/v1/messages",
			wantName: "personal",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proxy := newSentinel()
			stub := newUpstream(t, jsonHandler(http.StatusOK, upstreamList("claude-opus-5")))

			rec := serve(t, server.Config{
				Proxy:    proxy,
				Registry: newRawRegistry(t, tc.body),
				Client:   anthropicClient(t, stub),
			}, httptest.NewRequest(http.MethodPost, tc.target, strings.NewReader("")))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d; want %d for an unresolvable context", rec.Code, http.StatusNotFound)
			}

			if proxy.hits() != 0 {
				t.Fatalf("proxy saw %d requests; want none — the refusal must not fall through", proxy.hits())
			}

			if stub.hits() != 0 {
				t.Fatalf("upstream saw %d requests; want none", stub.hits())
			}

			assertUnknownContextBody(t, rec.Body.Bytes(), tc.wantName)
		})
	}
}

// assertUnknownContextBody checks the fail-closed answer is an Anthropic-shaped
// not-found error, marked as patchbay's own and naming the context refused.
func assertUnknownContextBody(t *testing.T, body []byte, name string) {
	t.Helper()

	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}

	if envelope.Type != "error" || envelope.Error.Type != "not_found_error" {
		t.Fatalf("error body = %s; want an Anthropic not_found_error", body)
	}

	want := `patchbay: unknown context "` + name + `"`
	if envelope.Error.Message != want {
		t.Fatalf("error message = %q; want %q", envelope.Error.Message, want)
	}
}

func TestBarePathsDegradeWhenTheRegistryIsBroken(t *testing.T) {
	t.Parallel()

	// The other half of the bargain: a request that named no context keeps
	// working with no registry at all, riding the built-in default route through
	// the pipeline exactly as it did before contexts existed.
	tests := []struct {
		name string
		body string
	}{
		{name: "empty registry file", body: ``},
		{name: "unparsable registry", body: `{"contexts":{`},
		{name: "registry failing validation", body: `{"contexts":{"personal":{"routes":{"bad":{}}}}}`},
		{name: "registry with no personal context", body: `{"contexts":{"work":{"routes":{}}}}`},
		{name: "a v1 document", body: `{"routes":{` + lunaRoute + `}}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proxy := newSentinel()

			rec := serve(t, server.Config{
				Proxy:    proxy,
				Registry: newRawRegistry(t, tc.body),
				Client:   anthropicClient(t, newUpstream(t, jsonHandler(http.StatusOK, upstreamList()))),
			}, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("")))

			if rec.Code != http.StatusTeapot {
				t.Fatalf("status = %d; want the proxy's %d — a bare path must never fail closed",
					rec.Code, http.StatusTeapot)
			}

			if got := proxy.call(t); got.path != "/v1/messages" {
				t.Fatalf("proxy saw path %q; want %q", got.path, "/v1/messages")
			}
		})
	}
}

// The surface resolves a request's context once and hands the pipeline that
// resolution, so a registry file replaced while the request is in flight cannot
// serve half of it from one document and half from the next.
func TestTheSurfaceResolvesOncePerRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, `{"contexts":{"personal":{"routes":{`+lunaRoute+`}}}}`)

	resolved := make(chan registry.Route, 1)
	swapped := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The file moves underneath the request, exactly as a rebuild would.
		writeRegistry(t, path, `{"contexts":{"personal":{"routes":{}}}}`)

		res, ok := registry.ResolutionFrom(r.Context())
		if !ok {
			t.Error("the pipeline was handed no resolution; every request must carry the one it made")

			resolved <- registry.Route{}

			return
		}

		selected, selectErr := res.Select("luna-fast")
		if selectErr != nil {
			t.Errorf("Select(%q) = %v; want no error for an alias", "luna-fast", selectErr)
		}

		resolved <- selected.Route
	})

	serve(t, server.Config{
		Proxy:    swapped,
		Registry: registry.New(path),
	}, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"luna-fast"}`)))

	if got := <-resolved; got.Alias != "luna-fast" {
		t.Fatalf("route alias = %q; want the alias the request resolved to before the file changed", got.Alias)
	}
}

// writeRegistry writes a registry document to path.
func writeRegistry(t *testing.T, path, body string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
}
