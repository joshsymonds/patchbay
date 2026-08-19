package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/server"
)

const (
	// introspectPath is the one endpoint that describes the gateway's routing.
	introspectPath = "/_patchbay/registry"

	// callerKey is the shared secret the introspection endpoint is gated on.
	callerKey = "s3cret-caller-key"

	// listenAddr is the address the handler is told it is listening on, which
	// the document has to report back.
	listenAddr = "127.0.0.1:4100"

	// keyEnvName and mapEnvName are the environment variables a route names for
	// its key file and its model map. Only the NAMES and the paths they resolve
	// to may ever appear in the document.
	keyEnvName = "PATCHBAY_TEST_KEY_FILE"
	mapEnvName = "PATCHBAY_TEST_MODEL_MAP"

	// upstreamKey is the injected key the route pays with, and bedrockARN the
	// Bedrock model id its map translates to. Neither may appear in a document.
	upstreamKey = "sk-upstream-never-printed"
	bedrockARN  = "arn:aws:bedrock:us-west-2::foundation-model/never-printed"

	// bedrockRoute is a sigv4 alias: no base_url of its own, an endpoint derived
	// from its region, and a model map named by environment variable.
	bedrockRoute = `"bedrock-opus":{"auth":"sigv4","aws_profile":"prod",` +
		`"aws_region":"us-west-2","model_map_env_file":"` + mapEnvName + `",` +
		`"model":"claude-opus-5","max_input_tokens":200000}`

	// bedrockEndpoint is the upstream that route's region derives.
	bedrockEndpoint = "https://bedrock-runtime.us-west-2.amazonaws.com"
)

// callerKeyFile writes a caller-key file and names it in the environment, the
// same double indirection the proxy's own gate reads.
func callerKeyFile(t *testing.T) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "caller.key")
	if err := os.WriteFile(path, []byte(callerKey), 0o600); err != nil {
		t.Fatalf("write caller key: %v", err)
	}

	t.Setenv(proxy.CallerKeyEnvFile, path)
}

// introspectRequest builds a GET for the introspection endpoint carrying key as
// the caller secret. An empty key presents no header at all.
func introspectRequest(target, key string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if key != "" {
		req.Header.Set(proxy.CallerKeyHeader, key)
	}

	return req
}

// decodeDocument decodes an introspection response body.
func decodeDocument(t *testing.T, body []byte) server.Document {
	t.Helper()

	var doc server.Document
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode introspection document %q: %v", body, err)
	}

	return doc
}

func TestIntrospectionDescribesEveryContext(t *testing.T) {
	callerKeyFile(t)
	t.Setenv(keyEnvName, "/run/agenix/openrouter.key")
	t.Setenv(mapEnvName, "/run/agenix/bedrock-models.json")

	reg := newContextRegistry(t, `"personal":{"routes":{`+lunaRoute+`}},`+
		`"work":{"default_route":{"auth":"sigv4","aws_profile":"prod",`+
		`"aws_region":"us-west-2","model_map_env_file":"`+mapEnvName+`"},`+
		`"routes":{`+bedrockRoute+`}}`)

	proxied := newSentinel()
	rec := serve(t, server.Config{
		Proxy:    proxied,
		Registry: reg,
		Listen:   listenAddr,
	}, introspectRequest(introspectPath, callerKey))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d; want %d (body %q)", introspectPath, rec.Code, http.StatusOK, rec.Body)
	}

	if proxied.hits() != 0 {
		t.Fatal("the introspection endpoint was forwarded upstream")
	}

	doc := decodeDocument(t, rec.Body.Bytes())

	if !doc.RegistryLoaded {
		t.Fatal("registry_loaded = false; want a valid registry reported as loaded")
	}

	if doc.Listen != listenAddr {
		t.Fatalf("listen = %q; want %q", doc.Listen, listenAddr)
	}

	if doc.RegistryPath == "" || !strings.HasSuffix(doc.RegistryPath, "registry.json") {
		t.Fatalf("registry_path = %q; want the file the gateway reads", doc.RegistryPath)
	}

	if len(doc.Contexts) != 2 {
		t.Fatalf("contexts = %v; want personal and work", doc.Contexts)
	}

	assertPersonalContext(t, doc.Contexts["personal"])
	assertWorkContext(t, doc.Contexts["work"])
	assertNoSecrets(t, rec.Body.String())
}

// assertPersonalContext checks the inject alias and the built-in default route
// the personal context still rides.
func assertPersonalContext(t *testing.T, summary server.ContextSummary) {
	t.Helper()

	if summary.DefaultRoute.Auth != string(registry.AuthForward) ||
		summary.DefaultRoute.Upstream != "https://api.anthropic.com" {
		t.Fatalf("personal default_route = %+v; want the built-in Anthropic forward", summary.DefaultRoute)
	}

	route, ok := summary.Routes["luna-fast"]
	if !ok {
		t.Fatalf("personal routes = %v; want the luna-fast alias", summary.Routes)
	}

	if route.Auth != string(registry.AuthInject) || route.Upstream != "https://openrouter.ai/api" {
		t.Fatalf("luna-fast summary = %+v; want an inject route to OpenRouter", route)
	}

	if route.Model != "openai/gpt-5.6-luna" || route.MaxInputTokens != 262144 {
		t.Fatalf("luna-fast summary = %+v; want its model and input ceiling", route)
	}

	if route.KeyEnvFile != "OPENROUTER_API_KEY_FILE" {
		t.Fatalf("luna-fast api_key_env_file = %q; want the environment variable NAME", route.KeyEnvFile)
	}
}

// assertWorkContext checks the sigv4 half: an endpoint derived from the region,
// and the model map named by environment variable and resolved to a path.
func assertWorkContext(t *testing.T, summary server.ContextSummary) {
	t.Helper()

	if summary.DefaultRoute.Auth != string(registry.AuthSigV4) ||
		summary.DefaultRoute.Upstream != bedrockEndpoint {
		t.Fatalf("work default_route = %+v; want the derived Bedrock endpoint", summary.DefaultRoute)
	}

	route, ok := summary.Routes["bedrock-opus"]
	if !ok {
		t.Fatalf("work routes = %v; want the bedrock-opus alias", summary.Routes)
	}

	if route.Upstream != bedrockEndpoint || route.Model != "claude-opus-5" {
		t.Fatalf("bedrock-opus summary = %+v; want the derived endpoint and its model", route)
	}

	if route.ModelMapEnvFile != mapEnvName {
		t.Fatalf("bedrock-opus model_map_env_file = %q; want %q", route.ModelMapEnvFile, mapEnvName)
	}

	if route.ModelMapFile != "/run/agenix/bedrock-models.json" {
		t.Fatalf("bedrock-opus model_map_file = %q; want the resolved path", route.ModelMapFile)
	}
}

// assertNoSecrets fails if a document carries any value that only a secret file
// holds — a key, or a Bedrock model id from a map.
func assertNoSecrets(t *testing.T, body string) {
	t.Helper()

	for _, secret := range []string{callerKey, upstreamKey, bedrockARN} {
		if strings.Contains(body, secret) {
			t.Fatalf("introspection document leaked a secret value: %q", body)
		}
	}
}

func TestIntrospectionRefusesACallerWithoutTheKey(t *testing.T) {
	callerKeyFile(t)

	tests := []struct {
		name string
		key  string
	}{
		{name: "no key at all", key: ""},
		{name: "the wrong key", key: "not-the-key"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proxied := newSentinel()
			rec := serve(t, server.Config{
				Proxy:    proxied,
				Registry: newRegistry(t, lunaRoute),
				Listen:   listenAddr,
			}, introspectRequest(introspectPath, tc.key))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d; want %d", rec.Code, http.StatusForbidden)
			}

			if proxied.hits() != 0 {
				t.Fatal("a refused introspection request was forwarded upstream")
			}

			body := rec.Body.String()
			if !strings.Contains(body, "patchbay: ") || !strings.Contains(body, "caller key") {
				t.Fatalf("body = %q; want patchbay's own caller-key refusal", body)
			}

			assertNoSecrets(t, body)
		})
	}
}

func TestIntrospectionFailsWhenNoKeyFileIsConfigured(t *testing.T) {
	t.Setenv(proxy.CallerKeyEnvFile, "")

	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, lunaRoute),
		Listen:   listenAddr,
	}, introspectRequest(introspectPath, callerKey))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want %d for an unconfigured caller key", rec.Code, http.StatusInternalServerError)
	}
}

func TestIntrospectionTellsTheDegradedTruth(t *testing.T) {
	callerKeyFile(t)

	tests := []struct {
		name string
		reg  func(t *testing.T) *registry.Registry
	}{
		{
			name: "a registry that will not validate",
			reg: func(t *testing.T) *registry.Registry {
				t.Helper()

				return newRawRegistry(t, `{"contexts":{"personal":{"routes":{"broken":{"auth":"nonsense"}}}}}`)
			},
		},
		{
			name: "no registry file at all",
			reg: func(t *testing.T) *registry.Registry {
				t.Helper()

				return registry.New(filepath.Join(t.TempDir(), "absent.json"))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(t, server.Config{
				Proxy:    newSentinel(),
				Registry: tc.reg(t),
				Listen:   listenAddr,
			}, introspectRequest(introspectPath, callerKey))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; want %d", rec.Code, http.StatusOK)
			}

			doc := decodeDocument(t, rec.Body.Bytes())

			if doc.RegistryLoaded {
				t.Fatal("registry_loaded = true; want a broken or absent registry reported as not loaded")
			}

			if len(doc.Contexts) != 1 {
				t.Fatalf("contexts = %v; want the built-in personal context alone", doc.Contexts)
			}

			personal, ok := doc.Contexts[registry.PersonalContext]
			if !ok {
				t.Fatalf("contexts = %v; want the personal context", doc.Contexts)
			}

			if personal.DefaultRoute.Auth != string(registry.AuthForward) || len(personal.Routes) != 0 {
				t.Fatalf("personal = %+v; want the built-in default route and no aliases", personal)
			}
		})
	}
}

func TestIntrospectionNeverForwardsItsOwnPaths(t *testing.T) {
	callerKeyFile(t)

	targets := []string{"/_patchbay", "/_patchbay/", "/_patchbay/other", "/_patchbay/registry/extra"}

	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			proxied := newSentinel()
			rec := serve(t, server.Config{
				Proxy:    proxied,
				Registry: newRegistry(t, lunaRoute),
				Listen:   listenAddr,
			}, introspectRequest(target, callerKey))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s status = %d; want %d", target, rec.Code, http.StatusNotFound)
			}

			if proxied.hits() != 0 {
				t.Fatalf("GET %s reached the proxy; a /_patchbay path is never forwarded", target)
			}

			if !strings.Contains(rec.Body.String(), "not_found_error") {
				t.Fatalf("GET %s body = %q; want an Anthropic not_found_error", target, rec.Body)
			}
		})
	}
}

func TestIntrospectionMethods(t *testing.T) {
	callerKeyFile(t)

	tests := []struct {
		name   string
		method string
		want   int
	}{
		{name: "head answers like get", method: http.MethodHead, want: http.StatusOK},
		{name: "post is refused", method: http.MethodPost, want: http.StatusMethodNotAllowed},
		{name: "delete is refused", method: http.MethodDelete, want: http.StatusMethodNotAllowed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proxied := newSentinel()
			req := httptest.NewRequest(tc.method, introspectPath, nil)
			req.Header.Set(proxy.CallerKeyHeader, callerKey)

			rec := serve(t, server.Config{
				Proxy:    proxied,
				Registry: newRegistry(t, lunaRoute),
				Listen:   listenAddr,
			}, req)

			if rec.Code != tc.want {
				t.Fatalf("%s %s status = %d; want %d", tc.method, introspectPath, rec.Code, tc.want)
			}

			if proxied.hits() != 0 {
				t.Fatalf("%s %s reached the proxy", tc.method, introspectPath)
			}
		})
	}
}
