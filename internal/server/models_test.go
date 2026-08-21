package server_test

import (
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/server"
)

func modelRegistry(defaultContext string) string {
	return `{"default_context":"` + defaultContext + `","seats":{` +
		`"anthropic":{"upstream":"https://api.anthropic.com","auth_mode":"forward"},` +
		`"luna":{"upstream":"https://openrouter.ai/api","model":"openai/gpt-5.6-luna","auth_mode":"inject","api_key_env_file":"KEY","max_input_tokens":262144,"max_output_tokens":65536},` +
		`"pin":{"upstream":"https://api.anthropic.com","model":"claude-sonnet-5","auth_mode":"forward"},` +
		`"bedrock":{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2","model_map_env_file":"MODEL_MAP"},` +
		`"sub":{"upstream":"https://openrouter.ai/api","model":"openrouter/worker","auth_mode":"inject","api_key_env_file":"KEY"}},` +
		`"contexts":{` +
		`"personal":{"default_seat":"anthropic","models":{"luna-fast":"luna","claude-opus-5":"pin"},"subagents":{"default_seat":"sub","models":{"requested-worker":"sub"}}},` +
		`"inject":{"default_seat":"luna","models":{"public-luna":"luna"}},` +
		`"aws":{"default_seat":"bedrock","models":{"public-bedrock":"bedrock"}}}}`
}

func TestPublicModelListMergesBindingsAndAnthropicWithShadowingAndPagination(t *testing.T) {
	t.Parallel()
	stub := newUpstream(
		t,
		jsonHandler(
			http.StatusOK,
			`{"data":[{"type":"model","id":"claude-opus-5"},{"type":"model","id":"claude-haiku-5"}],"has_more":true,"last_id":"claude-haiku-5"}`,
		),
	)
	rec := serve(
		t,
		server.Config{
			Proxy:    newSentinel(),
			Registry: newRegistryDocument(t, modelRegistry("personal")),
			Client:   anthropicClient(t, stub),
		},
		modelsRequest("/v1/models"),
	)
	want := []string{"claude-opus-5", "luna-fast", "claude-haiku-5"}
	if got := ids(t, decodeList(t, rec.Body.Bytes())); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids=%v want=%v", got, want)
	}
	list := decodeList(t, rec.Body.Bytes())
	if !list.HasMore || list.LastID == nil || *list.LastID != "claude-haiku-5" {
		t.Fatalf("pagination=%+v", list)
	}
	body := rec.Body.String()
	for _, private := range []string{"anthropic", "luna", "pin", "bedrock", "sub", "requested-worker", "openrouter/worker"} {
		if strings.Contains(body, `"id":"`+private+`"`) {
			t.Errorf("private id exposed: %s", private)
		}
	}

	// A later page repeats no binding entry, but a public selector still shadows
	// an upstream id that collides with it: the binding is what this gateway
	// serves for that name, and letting the upstream's own entry through on page
	// two would advertise a model the context does not actually route there.
	stub = newUpstream(
		t,
		jsonHandler(
			http.StatusOK,
			`{"data":[{"type":"model","id":"next"},{"type":"model","id":"luna-fast"},`+
				`{"type":"model","id":"claude-opus-5"}],"has_more":false,"first_id":"next","last_id":"claude-opus-5"}`,
		),
	)
	rec = serve(
		t,
		server.Config{
			Proxy:    newSentinel(),
			Registry: newRegistryDocument(t, modelRegistry("personal")),
			Client:   anthropicClient(t, stub),
		},
		modelsRequest("/v1/models?after_id=claude-haiku-5"),
	)
	if got := ids(t, decodeList(t, rec.Body.Bytes())); !reflect.DeepEqual(got, []string{"next"}) {
		t.Fatalf("paged ids=%v; want the shadowed upstream ids dropped", got)
	}
}

// TestReverseModelPageKeepsBindingsOffAndTheUpstreamCursorIntact pins the
// backward half of pagination. Anthropic pages forward from last_id and back
// from first_id, so a before_id request is as cursor-qualified as an after_id
// one: the bindings page was emitted once in front of the upstream's first page
// and must not reappear here. Shadowing still applies on every page, and when it
// drops the entry the upstream's first_id names, that id is still the only thing
// a client can send back as before_id — the merged envelope has to keep it
// rather than substitute the first entry that happened to survive.
func TestReverseModelPageKeepsBindingsOffAndTheUpstreamCursorIntact(t *testing.T) {
	t.Parallel()
	stub := newUpstream(t, jsonHandler(http.StatusOK,
		`{"data":[{"type":"model","id":"luna-fast"},{"type":"model","id":"claude-sonnet-4"},`+
			`{"type":"model","id":"claude-haiku-4"},{"type":"model","id":"claude-opus-5"}],`+
			`"has_more":true,"first_id":"luna-fast","last_id":"claude-opus-5"}`))
	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistryDocument(t, modelRegistry("personal")),
		Client:   anthropicClient(t, stub),
	}, modelsRequest("/v1/models?before_id=claude-sonnet-5"))

	list := decodeList(t, rec.Body.Bytes())
	want := []string{"claude-sonnet-4", "claude-haiku-4"}
	if got := ids(t, list); !reflect.DeepEqual(got, want) {
		t.Fatalf("reverse page ids = %v; want %v — no bindings, shadowed ids dropped", got, want)
	}
	if strings.Contains(rec.Body.String(), `"created_at":"1970-01-01T00:00:00Z"`) {
		t.Errorf("a synthetic binding entry rode a reverse page: %s", rec.Body.String())
	}
	if !list.HasMore {
		t.Errorf("has_more = false; want the upstream's own true")
	}
	if got := cursorText(list.FirstID); got != "luna-fast" {
		t.Errorf("first_id = %s; want the upstream's reverse cursor luna-fast", got)
	}
	if got := cursorText(list.LastID); got != "claude-opus-5" {
		t.Errorf("last_id = %s; want the upstream's forward cursor claude-opus-5", got)
	}
}

// TestTerminalReverseModelPageKeepsBothUpstreamCursors is the terminal half of
// the same rule. Paging backward, has_more:false says only that nothing lies
// further back — the page still has a forward end, and last_id is the id a
// client sends as after_id to return the way it came. That cursor is the
// upstream's, whichever of its entries a binding shadowed: substituting the last
// survivor hands back an id the upstream never said anything about, and paging
// forward from it lands somewhere else or nowhere.
func TestTerminalReverseModelPageKeepsBothUpstreamCursors(t *testing.T) {
	t.Parallel()
	stub := newUpstream(t, jsonHandler(http.StatusOK,
		`{"data":[{"type":"model","id":"luna-fast"},{"type":"model","id":"claude-sonnet-4"},`+
			`{"type":"model","id":"claude-haiku-4"},{"type":"model","id":"claude-opus-5"}],`+
			`"has_more":false,"first_id":"luna-fast","last_id":"claude-opus-5"}`))
	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistryDocument(t, modelRegistry("personal")),
		Client:   anthropicClient(t, stub),
	}, modelsRequest("/v1/models?before_id=claude-sonnet-5"))

	list := decodeList(t, rec.Body.Bytes())
	want := []string{"claude-sonnet-4", "claude-haiku-4"}
	if got := ids(t, list); !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal reverse page ids = %v; want %v", got, want)
	}
	if list.HasMore {
		t.Errorf("has_more = true; want the upstream's own false")
	}
	if got := cursorText(list.FirstID); got != "luna-fast" {
		t.Errorf("first_id = %s; want the upstream's reverse cursor luna-fast", got)
	}
	if got := cursorText(list.LastID); got != "claude-opus-5" {
		t.Errorf("last_id = %s; want the upstream's forward cursor claude-opus-5", got)
	}
}

// cursorText renders a pagination cursor for a failure message, naming an absent
// one rather than printing the address of a pointer.
func cursorText(cursor *string) string {
	if cursor == nil {
		return "<absent>"
	}

	return *cursor
}

func TestInjectAndSigV4DefaultSeatModelSemantics(t *testing.T) {
	modelMapFile(
		t,
		"MODEL_MAP",
		`{"claude-sonnet-5":"arn:sonnet","claude-haiku-5":"arn:haiku","empty":""}`,
	)

	inject := serve(
		t,
		server.Config{
			Proxy:    newSentinel(),
			Registry: newRegistryDocument(t, modelRegistry("inject")),
		},
		modelsRequest("/v1/models"),
	)
	if got := ids(
		t,
		decodeList(t, inject.Body.Bytes()),
	); !reflect.DeepEqual(
		got,
		[]string{"public-luna", "openai/gpt-5.6-luna"},
	) {
		t.Fatalf("inject ids=%v", got)
	}

	aws := serve(
		t,
		server.Config{Proxy: newSentinel(), Registry: newRegistryDocument(t, modelRegistry("aws"))},
		modelsRequest("/v1/models"),
	)
	if got := ids(
		t,
		decodeList(t, aws.Body.Bytes()),
	); !reflect.DeepEqual(
		got,
		[]string{"public-bedrock", "claude-haiku-5", "claude-sonnet-5"},
	) {
		t.Fatalf("aws ids=%v", got)
	}
	if strings.Contains(aws.Body.String(), "arn:") ||
		strings.Contains(aws.Body.String(), "requested-worker") {
		t.Fatalf("private model data exposed: %s", aws.Body.String())
	}
}

func TestModelDetailUsesPublicBindingThenDefaultSemantics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ctx, id string
		want    int
	}{
		{"inject", "public-luna", http.StatusOK}, {"inject", "openai%2Fgpt-5.6-luna", http.StatusOK}, {"inject", "requested-worker", http.StatusNotFound}, {"inject", "luna", http.StatusNotFound},
	} {
		rec := serve(
			t,
			server.Config{
				Proxy:    newSentinel(),
				Registry: newRegistryDocument(t, modelRegistry(tc.ctx)),
			},
			modelsRequest("/v1/models/"+tc.id),
		)
		if rec.Code != tc.want {
			t.Errorf("%s/%s status=%d body=%s", tc.ctx, tc.id, rec.Code, rec.Body.String())
		}
	}
}

// TestModelFetchCarriesOnlyTheCallersAllowedHeaders pins what crosses when the
// gateway asks Anthropic on somebody's behalf: the credentials that pay for the
// answer and the version flags that shape it, addressed to Anthropic and nowhere
// else. Anything else a client invents stays home.
func TestModelFetchCarriesOnlyTheCallersAllowedHeaders(t *testing.T) {
	t.Parallel()
	stub := newUpstream(t, jsonHandler(http.StatusOK, upstreamList("claude-opus-5")))

	req := modelsRequest("/v1/models?limit=3&after_id=model_x")
	req.Header.Set("Authorization", "Bearer sk-ant-oat-caller")
	req.Header.Set("X-Api-Key", "sk-ant-caller")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Add("Anthropic-Beta", "one-2026-01-01")
	req.Header.Add("Anthropic-Beta", "two-2026-01-01")
	req.Header.Set("X-Something-Else", "not forwarded")
	req.Header.Set("Cookie", "session=nope")

	serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistryDocument(t, modelRegistry("personal")),
		Client:   anthropicClient(t, stub),
	}, req)

	call := stub.call(t)
	if call.method != http.MethodGet || call.host != anthropicHost || call.path != modelsPathV1 {
		t.Fatalf("upstream call = %+v", call)
	}
	if call.rawQuery != "limit=3&after_id=model_x" {
		t.Fatalf("upstream query = %q; want the caller's own", call.rawQuery)
	}
	for _, header := range []struct {
		name string
		want []string
	}{
		{name: "Authorization", want: []string{"Bearer sk-ant-oat-caller"}},
		{name: "X-Api-Key", want: []string{"sk-ant-caller"}},
		{name: "Anthropic-Version", want: []string{"2023-06-01"}},
		{name: "Anthropic-Beta", want: []string{"one-2026-01-01", "two-2026-01-01"}},
	} {
		if got := call.header.Values(header.name); !reflect.DeepEqual(got, header.want) {
			t.Errorf("upstream %s = %v; want the caller's %v", header.name, got, header.want)
		}
	}
	for _, dropped := range []string{"X-Something-Else", "Cookie"} {
		if got := call.header.Values(dropped); len(got) != 0 {
			t.Errorf("upstream %s = %v; want it dropped", dropped, got)
		}
	}
}

const modelsPathV1 = "/v1/models"

// TestModelListEntryShape pins the exact fields a binding entry carries. A
// strict SDK decoder reads this, and a ceiling the Seat does not set is omitted
// rather than published as zero.
func TestModelListEntryShape(t *testing.T) {
	t.Parallel()
	stub := newUpstream(t, jsonHandler(http.StatusOK, upstreamList("upstream-model")))
	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistryDocument(t, modelRegistry("personal")),
		Client:   anthropicClient(t, stub),
	}, modelsRequest("/v1/models"))

	list := decodeList(t, rec.Body.Bytes())
	if len(list.Data) != 3 {
		t.Fatalf("entries = %v", ids(t, list))
	}
	for _, tc := range []struct {
		name  string
		index int
		want  map[string]any
	}{
		{
			name:  "a binding with no ceilings",
			index: 0,
			want: map[string]any{
				"type": "model", "id": "claude-opus-5", "display_name": "claude-opus-5",
				"created_at": "1970-01-01T00:00:00Z",
			},
		},
		{
			name:  "a binding with both ceilings",
			index: 1,
			want: map[string]any{
				"type": "model", "id": "luna-fast", "display_name": "luna-fast",
				"created_at":       "1970-01-01T00:00:00Z",
				"max_input_tokens": float64(262144), "max_tokens": float64(65536),
			},
		},
		{
			name:  "an upstream entry keeps every field it arrived with",
			index: 2,
			want: map[string]any{
				"type": "model", "id": "upstream-model", "display_name": "upstream-model",
				"created_at": "2026-01-01T00:00:00Z",
			},
		},
	} {
		if got := entryFields(t, list.Data[tc.index]); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: entry %d = %v; want exactly %v", tc.name, tc.index, got, tc.want)
		}
	}
}

// TestModelListDegradesWithoutFailing pins the half that can fail. Every way the
// upstream can let the gateway down costs the upstream entries and nothing else:
// the caller still gets this context's own bindings, with 200.
func TestModelListDegradesWithoutFailing(t *testing.T) {
	t.Parallel()
	oversized := `{"data":[{"type":"model","id":"` + strings.Repeat("a", 4<<20) + `"}]}`
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		dead    bool
	}{
		{name: "unreachable", dead: true},
		{name: "non 2xx", handler: jsonHandler(http.StatusUnauthorized, `{"error":"nope"}`)},
		{name: "server error", handler: jsonHandler(http.StatusInternalServerError, `{}`)},
		{name: "not a listing", handler: jsonHandler(http.StatusOK, `["not","a","listing"]`)},
		{name: "not json at all", handler: jsonHandler(http.StatusOK, `<html>nope</html>`)},
		{name: "oversized", handler: jsonHandler(http.StatusOK, oversized)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := deadClient()
			if !tc.dead {
				client = anthropicClient(t, newUpstream(t, tc.handler))
			}
			proxied := newSentinel()
			rec := serve(t, server.Config{
				Proxy:    proxied,
				Registry: newRegistryDocument(t, modelRegistry("personal")),
				Client:   client,
				Logger:   quietLogger(),
			}, modelsRequest("/v1/models"))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200 with the local bindings", rec.Code)
			}
			list := decodeList(t, rec.Body.Bytes())
			want := []string{"claude-opus-5", "luna-fast"}
			if got := ids(t, list); !reflect.DeepEqual(got, want) {
				t.Fatalf("ids = %v; want %v", got, want)
			}
			// An uncursored page with no next page ends where the merged listing
			// ends, and there is no upstream cursor to prefer over it.
			if list.HasMore {
				t.Error("has_more = true; want false with no upstream listing to page")
			}
			if got := cursorText(list.FirstID); got != want[0] {
				t.Errorf("first_id = %s; want the merged first id %s", got, want[0])
			}
			if got := cursorText(list.LastID); got != want[len(want)-1] {
				t.Errorf("last_id = %s; want the merged final id %s", got, want[len(want)-1])
			}
			if proxied.hits() != 0 {
				t.Error("a degraded listing reached the proxy")
			}
		})
	}
}

// TestModelListRelaysEntriesItCannotRead keeps an upstream entry the merge
// cannot parse an id out of. It has no id a binding could shadow, so it is
// listed as it arrived rather than dropped.
func TestModelListRelaysEntriesItCannotRead(t *testing.T) {
	t.Parallel()
	stub := newUpstream(t, jsonHandler(http.StatusOK,
		`{"data":["a bare string",{"no_id":true},{"type":"model","id":"real"}],"has_more":false}`))
	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistryDocument(t, modelRegistry("personal")),
		Client:   anthropicClient(t, stub),
	}, modelsRequest("/v1/models"))

	list := decodeList(t, rec.Body.Bytes())
	if len(list.Data) != 5 {
		t.Fatalf("entries = %d; want the 2 bindings and all 3 upstream entries", len(list.Data))
	}
	if string(list.Data[2]) != `"a bare string"` || string(list.Data[3]) != `{"no_id":true}` {
		t.Fatalf("unreadable entries were not relayed verbatim: %s", rec.Body.String())
	}
}

// TestSigV4ListingSurvivesAnUnreadableModelMap pins the sigv4 half of the
// synthesized listing: a map that cannot be read costs its entries and nothing
// else, and never leaks a mapped Bedrock identifier.
func TestSigV4ListingSurvivesAnUnreadableModelMap(t *testing.T) {
	t.Setenv("MODEL_MAP", filepath.Join(t.TempDir(), "absent.json"))
	rec := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistryDocument(t, modelRegistry("aws")),
		Logger:   quietLogger(),
	}, modelsRequest("/v1/models"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if got := ids(t, decodeList(t, rec.Body.Bytes())); !reflect.DeepEqual(got, []string{"public-bedrock"}) {
		t.Fatalf("ids = %v; want the public bindings alone", got)
	}
}

// TestSigV4OneModelNamesTheMapsKeysAndNeverItsTargets pins the single-model half
// of a sigv4 context. A caller may name a public binding of the context or a key
// of its model map, and nothing else exists: the map decides what this context
// serves, and Anthropic — which such a context never reaches — is never asked.
// A key mapping to nothing is a model the pipeline would refuse, so it is absent
// here too rather than described as reachable.
//
// The entries are pinned whole because the interesting failure is a describe
// that answers with what the key maps TO. That value is a Bedrock model id, the
// one thing the map file exists to keep out of sight, and an entry echoing it
// would publish it to every caller who can guess a key.
//
// The client is a dead one on purpose: this context has no upstream to ask, so a
// relay that reached for Anthropic anyway would fail here rather than quietly
// describing a model from the wrong place.
func TestSigV4OneModelNamesTheMapsKeysAndNeverItsTargets(t *testing.T) {
	modelMapFile(
		t,
		"MODEL_MAP",
		`{"claude-sonnet-5":"arn:sonnet","claude-haiku-5":"arn:haiku","empty":""}`,
	)

	for _, tc := range []struct {
		name, id string
		want     int
	}{
		{name: "a public binding of the context", id: "public-bedrock", want: http.StatusOK},
		{name: "a key of the model map", id: "claude-sonnet-5", want: http.StatusOK},
		{name: "another key of the model map", id: "claude-haiku-5", want: http.StatusOK},
		{name: "a model the map does not claim", id: "claude-opus-5", want: http.StatusNotFound},
		{name: "a key the map maps to nothing", id: "empty", want: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(t, server.Config{
				Proxy:    newSentinel(),
				Registry: newRegistryDocument(t, modelRegistry("aws")),
				Client:   deadClient(),
				Logger:   quietLogger(),
			}, modelsRequest("/v1/models/"+tc.id))

			if rec.Code != tc.want {
				t.Fatalf("status = %d; want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
			// Every value this map holds, and every Bedrock id shaped like one.
			if strings.Contains(rec.Body.String(), "arn:") {
				t.Errorf("answer carries a mapped Bedrock id: %s", rec.Body)
			}
			if tc.want != http.StatusOK {
				return
			}

			want := map[string]any{
				"type": "model", "id": tc.id, "display_name": tc.id,
				"created_at": "1970-01-01T00:00:00Z",
			}
			if got := entryFields(t, rec.Body.Bytes()); !reflect.DeepEqual(got, want) {
				t.Errorf("entry = %v; want exactly %v", got, want)
			}
		})
	}
}

// TestOneModelRelaysTheUpstreamAnswer pins the single-model relay: the
// upstream's own status and bytes reach the caller untouched, because the
// upstream's answer about its own models is the honest one.
func TestOneModelRelaysTheUpstreamAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		contentType string
		wantType    string
	}{
		{name: "found", status: http.StatusOK, body: `{"type":"model","id":"claude-haiku-5"}`, contentType: jsonType, wantType: jsonType},
		{name: "not found", status: http.StatusNotFound, body: `{"type":"error"}`, contentType: jsonType, wantType: jsonType},
		{name: "refused key", status: http.StatusUnauthorized, body: `{"type":"error","error":{"type":"authentication_error"}}`, contentType: jsonType, wantType: jsonType},
		{name: "no content type defaults to json", status: http.StatusOK, body: `{"type":"model","id":"x"}`, wantType: jsonType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				} else {
					// Go sniffs a content type for a handler that sets none, so the
					// header has to be suppressed explicitly to produce an upstream
					// that genuinely declares nothing.
					w.Header()["Content-Type"] = nil
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			rec := serve(t, server.Config{
				Proxy:    newSentinel(),
				Registry: newRegistryDocument(t, modelRegistry("personal")),
				Client:   anthropicClient(t, stub),
			}, modelsRequest("/v1/models/claude-haiku-5"))

			if rec.Code != tc.status {
				t.Fatalf("status = %d; want the upstream's %d", rec.Code, tc.status)
			}
			if rec.Body.String() != tc.body {
				t.Fatalf("body = %q; want the upstream's %q", rec.Body.String(), tc.body)
			}
			if got := rec.Header().Get("Content-Type"); got != tc.wantType {
				t.Fatalf("content type = %q; want %q", got, tc.wantType)
			}
		})
	}
}

// TestOneModelReportsAnUpstreamItCannotUse pins the two failures the relay must
// answer for itself, in Anthropic's error shape rather than a silent 200.
//
// The whole answer is pinned rather than sampled, because every part of it is
// something a client acts on. The status is what tells a caller the gateway
// could not use its upstream; the content type is what makes the body readable
// at all; the taxonomy is what a client dispatches on, having never heard of
// patchbay; and the "patchbay: " prefix is what keeps Claude Code from matching
// this text against Anthropic's own and retrying. Comparing the bytes is also
// the only assertion that catches the failure that matters most here — a field
// this envelope should not carry, holding a detail about the caller's
// credentials or this machine's insides that no error body owes anyone.
func TestOneModelReportsAnUpstreamItCannotUse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		build    func(*testing.T) *http.Client
		wantBody string
	}{
		{
			name:  "unreachable",
			build: func(*testing.T) *http.Client { return deadClient() },
			wantBody: `{"type":"error","error":{"type":"api_error","message":` +
				`"patchbay: upstream request failed: Get ` +
				`\"https://api.anthropic.com/v1/models/claude-haiku-5\": upstream unreachable"}}`,
		},
		{
			name: "oversized",
			build: func(t *testing.T) *http.Client {
				t.Helper()
				body := `{"type":"model","id":"` + strings.Repeat("a", 4<<20) + `"}`
				return anthropicClient(t, newUpstream(t, jsonHandler(http.StatusOK, body)))
			},
			wantBody: `{"type":"error","error":{"type":"api_error","message":` +
				`"patchbay: upstream response exceeds the 4194304-byte ceiling"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := serve(t, server.Config{
				Proxy:    newSentinel(),
				Registry: newRegistryDocument(t, modelRegistry("personal")),
				Client:   tc.build(t),
				Logger:   quietLogger(),
			}, modelsRequest("/v1/models/claude-haiku-5"))

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d; want 502 (body %s)", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Content-Type"); got != jsonType {
				t.Errorf("content type = %q; want %q", got, jsonType)
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Errorf("body = %s\nwant exactly = %s", got, tc.wantBody)
			}
		})
	}
}

// TestNamedContextModelEndpointsAreCallerKeyGated pins the gate on the one form
// that asks what some other world of this machine's serves. The bare path stays
// open: it is what Claude Code sends with no idea patchbay is there.
func TestNamedContextModelEndpointsAreCallerKeyGated(t *testing.T) {
	callerKeyFile(t)
	for _, target := range []string{"/ctx/inject/v1/models", "/ctx/inject/v1/models/public-luna"} {
		for _, key := range []string{"", "wrong"} {
			rec := serve(t, server.Config{
				Proxy:    newSentinel(),
				Registry: newRegistryDocument(t, modelRegistry("personal")),
			}, keyedRequest(target, key))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s with key %q status = %d; want 403", target, key, rec.Code)
			}
		}
		rec := serve(t, server.Config{
			Proxy:    newSentinel(),
			Registry: newRegistryDocument(t, modelRegistry("personal")),
		}, keyedRequest(target, callerKey))
		if rec.Code != http.StatusOK {
			t.Errorf("%s with the right key status = %d; want 200", target, rec.Code)
		}
	}

	bare := serve(t, server.Config{
		Proxy:    newSentinel(),
		Registry: newRegistryDocument(t, modelRegistry("inject")),
	}, modelsRequest("/v1/models"))
	if bare.Code != http.StatusOK {
		t.Errorf("the bare listing is gated: status = %d", bare.Code)
	}
}

// TestNamedContextModelEndpointsReportAnUnreadableCallerKey separates the
// server's own misconfiguration from the caller's refusal: a key file that
// cannot be read is a 500, not a 403.
func TestNamedContextModelEndpointsReportAnUnreadableCallerKey(t *testing.T) {
	unreadableCallerKey(t)
	for _, target := range []string{"/ctx/inject/v1/models", "/ctx/inject/v1/models/public-luna"} {
		rec := serve(t, server.Config{
			Proxy:    newSentinel(),
			Registry: newRegistryDocument(t, modelRegistry("personal")),
		}, keyedRequest(target, callerKey))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s status = %d; want 500", target, rec.Code)
		}
	}
}
