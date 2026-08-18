package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/server"
)

func TestModelListPutsAliasesBeforeUpstreamModels(t *testing.T) {
	t.Parallel()

	stub := newUpstream(t, jsonHandler(http.StatusOK, upstreamList("claude-opus-5", "claude-haiku-4-5")))

	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, lunaRoute+","+barePinRoute),
		Client:   anthropicClient(t, stub),
	}, modelsRequest("/v1/models"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusOK)
	}

	if got := rec.Header().Get("Content-Type"); got != jsonType {
		t.Fatalf("Content-Type = %q; want %q", got, jsonType)
	}

	list := decodeList(t, rec.Body.Bytes())

	want := []string{"anthropic/pinned", "luna-fast", "claude-opus-5", "claude-haiku-4-5"}
	if got := ids(t, list); !reflect.DeepEqual(got, want) {
		t.Fatalf("data ids = %v; want the aliases first, then upstream: %v", got, want)
	}

	if list.HasMore {
		t.Fatal("has_more = true; want false — the merged listing is the whole listing")
	}

	if list.FirstID == nil || *list.FirstID != "anthropic/pinned" {
		t.Fatalf("first_id = %v; want anthropic/pinned", list.FirstID)
	}

	if list.LastID == nil || *list.LastID != "claude-haiku-4-5" {
		t.Fatalf("last_id = %v; want claude-haiku-4-5", list.LastID)
	}
}

func TestModelListEntryShape(t *testing.T) {
	t.Parallel()

	stub := newUpstream(t, jsonHandler(http.StatusOK, upstreamList("claude-opus-5")))

	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, lunaRoute+","+barePinRoute),
		Client:   anthropicClient(t, stub),
	}, modelsRequest("/v1/models"))

	list := decodeList(t, rec.Body.Bytes())

	tests := []struct {
		name  string
		index int
		want  map[string]any
	}{
		{
			name:  "no ceilings set",
			index: 0,
			want: map[string]any{
				"type":         "model",
				"id":           "anthropic/pinned",
				"display_name": "anthropic/pinned",
				"created_at":   epochCreatedAt,
			},
		},
		{
			name:  "both ceilings set",
			index: 1,
			want: map[string]any{
				"type":             "model",
				"id":               "luna-fast",
				"display_name":     "luna-fast",
				"max_input_tokens": float64(262144),
				"max_tokens":       float64(65536),
				"created_at":       epochCreatedAt,
			},
		},
		{
			name:  "an upstream entry keeps every field it arrived with",
			index: 2,
			want: map[string]any{
				"type":         "model",
				"id":           "claude-opus-5",
				"display_name": "claude-opus-5",
				"created_at":   "2026-01-01T00:00:00Z",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := entryFields(t, list.Data[tc.index])
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("entry %d = %v; want exactly %v", tc.index, got, tc.want)
			}
		})
	}
}

func TestModelListCarriesTheCallersOwnCredentials(t *testing.T) {
	t.Parallel()

	stub := newUpstream(t, jsonHandler(http.StatusOK, upstreamList("claude-opus-5")))

	req := modelsRequest("/v1/models?limit=3&after_id=model_x")
	req.Header.Set("Authorization", "Bearer sk-ant-oat-caller")
	req.Header.Set("X-Api-Key", "sk-ant-caller")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Add("Anthropic-Beta", "one-2026-01-01")
	req.Header.Add("Anthropic-Beta", "two-2026-01-01")
	req.Header.Set("X-Something-Else", "not forwarded")

	serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, lunaRoute),
		Client:   anthropicClient(t, stub),
	}, req)

	call := stub.call(t)

	if call.method != http.MethodGet {
		t.Fatalf("upstream method = %q; want GET", call.method)
	}

	if call.host != anthropicHost {
		t.Fatalf("upstream host = %q; want %q — the caller's credentials go nowhere else", call.host, anthropicHost)
	}

	if call.path != "/v1/models" {
		t.Fatalf("upstream path = %q; want /v1/models", call.path)
	}

	if call.rawQuery != "limit=3&after_id=model_x" {
		t.Fatalf("upstream query = %q; want the caller's own", call.rawQuery)
	}

	headers := []struct {
		name string
		want []string
	}{
		{name: "Authorization", want: []string{"Bearer sk-ant-oat-caller"}},
		{name: "X-Api-Key", want: []string{"sk-ant-caller"}},
		{name: "Anthropic-Version", want: []string{"2023-06-01"}},
		{name: "Anthropic-Beta", want: []string{"one-2026-01-01", "two-2026-01-01"}},
	}

	for _, header := range headers {
		if got := call.header.Values(header.name); !reflect.DeepEqual(got, header.want) {
			t.Fatalf("upstream %s = %v; want the caller's %v", header.name, got, header.want)
		}
	}

	// Only the allowlist crosses. Nothing else about the inbound request — a
	// tracing header, a cookie, whatever a client invents — belongs on a request
	// patchbay makes on somebody's behalf.
	if got := call.header.Values("X-Something-Else"); len(got) != 0 {
		t.Fatalf("upstream X-Something-Else = %v; want it dropped", got)
	}
}

func TestModelListAliasShadowsAnUpstreamModel(t *testing.T) {
	t.Parallel()

	// An alias named after a real Claude model is how that model gets pinned
	// somewhere else. It must appear once, as the gateway's version.
	const pinned = `"claude-opus-5":{"base_url":"https://api.anthropic.com",` +
		`"model":"claude-sonnet-5","auth":"forward","max_output_tokens":8192}`

	stub := newUpstream(t, jsonHandler(http.StatusOK, upstreamList("claude-opus-5", "claude-haiku-4-5")))

	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, pinned),
		Client:   anthropicClient(t, stub),
	}, modelsRequest("/v1/models"))

	list := decodeList(t, rec.Body.Bytes())

	want := []string{"claude-opus-5", "claude-haiku-4-5"}
	if got := ids(t, list); !reflect.DeepEqual(got, want) {
		t.Fatalf("data ids = %v; want the shadowed model listed once: %v", got, want)
	}

	got := entryFields(t, list.Data[0])
	wantEntry := map[string]any{
		"type":         "model",
		"id":           "claude-opus-5",
		"display_name": "claude-opus-5",
		"max_tokens":   float64(8192),
		"created_at":   epochCreatedAt,
	}

	if !reflect.DeepEqual(got, wantEntry) {
		t.Fatalf("shadowed entry = %v; want the alias version %v", got, wantEntry)
	}
}

func TestModelListDegradesToTheRegistryAlone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "upstream fails", status: http.StatusInternalServerError, body: `{"type":"error"}`},
		{name: "upstream refuses the credentials", status: http.StatusUnauthorized, body: `{"type":"error"}`},
		{name: "upstream body is not json", status: http.StatusOK, body: `<html>a proxy ate it</html>`},
		{name: "upstream body is json but not a listing", status: http.StatusOK, body: `["surprise"]`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sink := newLogSink()
			stub := newUpstream(t, jsonHandler(tc.status, tc.body))

			rec := serve(t, server.Config{
				Proxy:    newSentinel(),
				Registry: newRegistry(t, lunaRoute+","+barePinRoute),
				Client:   anthropicClient(t, stub),
				Logger:   slog.New(sink),
			}, modelsRequest("/v1/models"))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; want %d — a broken upstream is never the caller's error",
					rec.Code, http.StatusOK)
			}

			list := decodeList(t, rec.Body.Bytes())

			want := []string{"anthropic/pinned", "luna-fast"}
			if got := ids(t, list); !reflect.DeepEqual(got, want) {
				t.Fatalf("data ids = %v; want the registry alone: %v", got, want)
			}

			// With no upstream half there is no page after this one to point at.
			if list.HasMore {
				t.Fatal("has_more = true; want false — the aliases are the whole listing")
			}

			sink.waitFor(t, "upstream model list unavailable")
		})
	}
}

func TestModelListDegradesWhenTheUpstreamNeverAnswers(t *testing.T) {
	t.Parallel()

	// The fetch is bounded, and a caller that gives up is bounded sooner. Both
	// end the same way: the aliases, with 200. The stub blocks until the test
	// releases it, so nothing here waits on a clock.
	block := make(chan struct{})
	stub := newUpstream(t, func(http.ResponseWriter, *http.Request) { <-block })

	// Released before the stub is shut down: cleanups run last-registered
	// first, and closing a server with a handler still in it would wait forever.
	t.Cleanup(func() { close(block) })

	sink := newLogSink()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	handler := server.Handler(server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, lunaRoute),
		Client:   anthropicClient(t, stub),
		Logger:   slog.New(sink),
	})

	rec := httptest.NewRecorder()
	done := make(chan struct{})

	go func() {
		defer close(done)
		handler.ServeHTTP(rec, modelsRequest("/v1/models").WithContext(ctx))
	}()

	<-stub.calls
	cancel()
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusOK)
	}

	if got := ids(t, decodeList(t, rec.Body.Bytes())); !reflect.DeepEqual(got, []string{"luna-fast"}) {
		t.Fatalf("data ids = %v; want [luna-fast]", got)
	}

	sink.waitFor(t, "upstream model list unavailable")
}

func TestModelListWithNothingToList(t *testing.T) {
	t.Parallel()

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"data":[],"has_more":false}`))

	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, ""),
		Client:   anthropicClient(t, stub),
	}, modelsRequest("/v1/models"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusOK)
	}

	var list struct {
		Data    []json.RawMessage `json:"data"`
		HasMore bool              `json:"has_more"`
		FirstID *string           `json:"first_id"`
		LastID  *string           `json:"last_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode empty listing %q: %v", rec.Body.Bytes(), err)
	}

	if len(list.Data) != 0 {
		t.Fatalf("data = %v; want empty", list.Data)
	}

	if list.FirstID != nil || list.LastID != nil {
		t.Fatalf("first_id = %v, last_id = %v; want both null", list.FirstID, list.LastID)
	}

	if !bytes.Contains(rec.Body.Bytes(), []byte(`"data":[]`)) {
		t.Fatalf("body = %s; want an empty data array rather than null", rec.Body.Bytes())
	}
}

func TestModelListSurvivesABrokenRegistry(t *testing.T) {
	t.Parallel()

	// A registry file that fails to parse costs the aliases and nothing else:
	// the upstream half of the listing still arrives. The registry is re-read on
	// every request, so a rebuild can break it long after startup — a listing
	// that silently lost every alias has to say so somewhere.
	sink := newLogSink()
	stub := newUpstream(t, jsonHandler(http.StatusOK, upstreamList("claude-opus-5")))

	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, `"broken":{"auth":"nonsense"}`),
		Client:   anthropicClient(t, stub),
		Logger:   slog.New(sink),
	}, modelsRequest("/v1/models"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d — a broken registry is never the caller's error", rec.Code, http.StatusOK)
	}

	if got := ids(t, decodeList(t, rec.Body.Bytes())); !reflect.DeepEqual(got, []string{"claude-opus-5"}) {
		t.Fatalf("data ids = %v; want the upstream alone", got)
	}

	sink.waitFor(t, "registry file is broken; foreign aliases are unavailable")
}

func TestModelListRelaysUpstreamPagination(t *testing.T) {
	t.Parallel()

	// An account with more models than one page is not a truncated account.
	// Reporting has_more:false would hide every model past the first page, and
	// the cursor that fetches the next one has to be the upstream's own — an
	// alias id is not something Anthropic can page from.
	const listing = `{"data":[{"type":"model","id":"m1"},{"type":"model","id":"m2"}],` +
		`"has_more":true,"first_id":"m1","last_id":"m2"}`

	stub := newUpstream(t, jsonHandler(http.StatusOK, listing))

	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, lunaRoute),
		Client:   anthropicClient(t, stub),
	}, modelsRequest("/v1/models"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusOK)
	}

	list := decodeList(t, rec.Body.Bytes())

	want := []string{"luna-fast", "m1", "m2"}
	if got := ids(t, list); !reflect.DeepEqual(got, want) {
		t.Fatalf("data ids = %v; want the aliases first, then the page: %v", got, want)
	}

	if !list.HasMore {
		t.Fatal("has_more = false; want the upstream's own true")
	}

	if list.LastID == nil || *list.LastID != "m2" {
		t.Fatalf("last_id = %v; want the upstream's own cursor m2", list.LastID)
	}

	if list.FirstID == nil || *list.FirstID != "luna-fast" {
		t.Fatalf("first_id = %v; want the first merged entry luna-fast", list.FirstID)
	}
}

func TestOneModelAnswersAnAliasFromTheRegistry(t *testing.T) {
	t.Parallel()

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"model","id":"upstream-should-not-be-asked"}`))

	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, lunaRoute),
		Client:   anthropicClient(t, stub),
	}, modelsRequest("/v1/models/luna-fast"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusOK)
	}

	if stub.hits() != 0 {
		t.Fatalf("upstream saw %d requests; want 0 — the alias is patchbay's own answer", stub.hits())
	}

	got := entryFields(t, rec.Body.Bytes())
	want := map[string]any{
		"type":             "model",
		"id":               "luna-fast",
		"display_name":     "luna-fast",
		"max_input_tokens": float64(262144),
		"max_tokens":       float64(65536),
		"created_at":       epochCreatedAt,
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entry = %v; want %v", got, want)
	}
}

func TestOneModelRelaysTheUpstreamAnswerUnmodified(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target string
		path   string
		status int
		body   string
	}{
		{
			name:   "a model the caller can see",
			target: "/v1/models/claude-opus-5",
			path:   "/v1/models/claude-opus-5",
			status: http.StatusOK,
			body:   `{"type":"model","id":"claude-opus-5","display_name":"Claude Opus 5","created_at":"2026-01-01T00:00:00Z"}`,
		},
		{
			name:   "a model that does not exist",
			target: "/v1/models/not-a-model",
			path:   "/v1/models/not-a-model",
			status: http.StatusNotFound,
			body:   `{"type":"error","error":{"type":"not_found_error","message":"model: not-a-model"}}`,
		},
		{
			name:   "credentials the upstream rejects",
			target: "/v1/models/claude-opus-5",
			path:   "/v1/models/claude-opus-5",
			status: http.StatusUnauthorized,
			body:   `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stub := newUpstream(t, jsonHandler(tc.status, tc.body))

			req := modelsRequest(tc.target)
			req.Header.Set("Authorization", "Bearer sk-ant-oat-caller")
			req.Header.Set("X-Api-Key", "sk-ant-caller")
			req.Header.Set("Anthropic-Version", "2023-06-01")
			req.Header.Set("Anthropic-Beta", "one-2026-01-01")

			rec := serve(t, server.Config{
				Proxy:    newSentinel(),
				Registry: newRegistry(t, lunaRoute),
				Client:   anthropicClient(t, stub),
			}, req)

			if rec.Code != tc.status {
				t.Fatalf("status = %d; want the upstream's own %d", rec.Code, tc.status)
			}

			if got := rec.Body.String(); got != tc.body {
				t.Fatalf("body = %q; want the upstream's bytes %q", got, tc.body)
			}

			call := stub.call(t)
			if call.path != tc.path {
				t.Fatalf("upstream path = %q; want %q", call.path, tc.path)
			}

			if call.host != anthropicHost {
				t.Fatalf("upstream host = %q; want %q — the relay asks Anthropic and nobody else",
					call.host, anthropicHost)
			}

			for _, name := range []string{"Authorization", "X-Api-Key", "Anthropic-Version", "Anthropic-Beta"} {
				if call.header.Get(name) == "" {
					t.Fatalf("upstream %s was not forwarded", name)
				}
			}
		})
	}
}

func TestOneModelReportsAnUnreachableUpstream(t *testing.T) {
	t.Parallel()

	sink := newLogSink()

	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, lunaRoute),
		Client:   deadClient(t),
		Logger:   slog.New(sink),
	}, modelsRequest("/v1/models/claude-opus-5"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d; want %d", rec.Code, http.StatusBadGateway)
	}

	if got := rec.Header().Get("Content-Type"); got != jsonType {
		t.Fatalf("Content-Type = %q; want %q", got, jsonType)
	}

	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.Bytes(), err)
	}

	if envelope.Type != "error" || envelope.Error.Type != "api_error" {
		t.Fatalf("error envelope = %+v; want an Anthropic-format api_error", envelope)
	}

	if !strings.Contains(envelope.Error.Message, "patchbay: ") {
		t.Fatalf("message = %q; want it marked as patchbay's own", envelope.Error.Message)
	}
}

func TestOneModelMatchesAnAliasExactly(t *testing.T) {
	t.Parallel()

	// Only an exact alias is patchbay's to answer. A prefix, a suffix, or a
	// different case is somebody else's model id, and the upstream says so.
	tests := []struct {
		name   string
		target string
	}{
		{name: "prefix of an alias", target: "/v1/models/luna"},
		{name: "suffix added", target: "/v1/models/luna-fast-2"},
		{name: "different case", target: "/v1/models/Luna-Fast"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stub := newUpstream(t, jsonHandler(http.StatusNotFound, `{"type":"error"}`))

			rec := serve(t, server.Config{
				Proxy:    newSentinel(),
				Registry: newRegistry(t, lunaRoute),
				Client:   anthropicClient(t, stub),
			}, modelsRequest(tc.target))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d; want the upstream's %d", rec.Code, http.StatusNotFound)
			}

			if stub.hits() != 1 {
				t.Fatalf("upstream saw %d requests; want the lookup relayed", stub.hits())
			}
		})
	}
}

func TestModelListRelaysUpstreamEntriesItCannotRead(t *testing.T) {
	t.Parallel()

	// An entry with no id, or one that is not an object at all, is nothing an
	// alias can shadow — and it is not patchbay's to drop either.
	const listing = `{"data":["surprise",{"display_name":"nameless"},` +
		`{"type":"model","id":"claude-opus-5"}],"has_more":false}`

	stub := newUpstream(t, jsonHandler(http.StatusOK, listing))

	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, lunaRoute),
		Client:   anthropicClient(t, stub),
	}, modelsRequest("/v1/models"))

	list := decodeList(t, rec.Body.Bytes())

	want := []json.RawMessage{
		json.RawMessage(`"surprise"`),
		json.RawMessage(`{"display_name":"nameless"}`),
		json.RawMessage(`{"type":"model","id":"claude-opus-5"}`),
	}

	if got := list.Data[1:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("upstream entries = %s; want them relayed as they arrived: %s", got, want)
	}
}

func TestOneModelRelayDefaultsToJSON(t *testing.T) {
	t.Parallel()

	// An upstream that declares no content type still answered with a JSON
	// document, and the caller has to be able to read it.
	stub := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		// A nil entry suppresses net/http's own content sniffing, so this
		// answer really does arrive with no content type at all.
		w.Header()["Content-Type"] = nil
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"type":"model","id":"claude-opus-5"}`)
	})

	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, lunaRoute),
		Client:   anthropicClient(t, stub),
	}, modelsRequest("/v1/models/claude-opus-5"))

	if got := rec.Header().Get("Content-Type"); got != jsonType {
		t.Fatalf("Content-Type = %q; want %q", got, jsonType)
	}
}

func TestOneModelFindsANamespacedAlias(t *testing.T) {
	t.Parallel()

	// A directory-style alias holds a slash, which is a path separator to
	// anything routing on it. Percent-encoded, it is one path segment and the
	// lookup finds it.
	const namespaced = `"openrouter/sol":{"base_url":"https://openrouter.ai/api",` +
		`"model":"openrouter/sol-1","auth":"inject","api_key_env_file":"OPENROUTER_API_KEY_FILE"}`

	stub := newUpstream(t, jsonHandler(http.StatusNotFound, `{"type":"error"}`))

	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistry(t, namespaced),
		Client:   anthropicClient(t, stub),
	}, modelsRequest("/v1/models/openrouter%2Fsol"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d — the alias is patchbay's own to answer", rec.Code, http.StatusOK)
	}

	got := entryFields(t, rec.Body.Bytes())
	want := map[string]any{
		"type":         "model",
		"id":           "openrouter/sol",
		"display_name": "openrouter/sol",
		"created_at":   epochCreatedAt,
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entry = %v; want %v", got, want)
	}
}
