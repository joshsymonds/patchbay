package proxy_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// fidelityBody is the awkward request body D2 must forward untouched: already
// escaped JSON, a lone backslash, multi-byte runes and a cache_control key that
// an identity Seat has no business removing.
const fidelityBody = `{"model":"local/echo","max_tokens":1024,"stream":true,` +
	`"system":[{"type":"text","text":"héllo \"quoted\" \\ backslash",` +
	`"cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":"日本語 🎲 é tail"}],` +
	`"top_p":0.30000000000000004}`

// sseTurn is a complete streaming turn: two adjacent reasoning blocks (the drop
// signature) followed by a text block.
func sseTurn() string {
	lines := []string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_01","model":"local/echo",` +
			`"usage":{"input_tokens":1234,"output_tokens":1}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,` +
			`"delta":{"type":"thinking_delta","thinking":"日本語 🎲 first"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,` +
			`"delta":{"type":"thinking_delta","thinking":"second"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"answer"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":2}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},` +
			`"usage":{"output_tokens":567}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}

	return strings.Join(lines, "\n") + "\n"
}

// pathologicalSplits returns chunk boundaries that land mid-line, mid-token and
// inside multi-byte runes.
func pathologicalSplits(payload string) []int {
	splits := []int{1, 4, 7, 13, 29, 61}

	if idx := strings.Index(payload, "🎲"); idx >= 0 {
		splits = append(splits, idx+1, idx+2, idx+3)
	}

	if idx := strings.Index(payload, "日本語"); idx >= 0 {
		splits = append(splits, idx+1, idx+4, idx+8)
	}

	if idx := strings.Index(payload, "content_block_start"); idx >= 0 {
		splits = append(splits, idx+7, idx+8)
	}

	if idx := strings.Index(payload, "message_delta"); idx >= 0 {
		splits = append(splits, idx+3, idx+4, idx+5)
	}

	return splits
}

// TestIdentitySeatPreservesBytesBothWays is the byte-fidelity invariant: on an
// identity Seat the request arrives upstream as the exact bytes the client
// sent, and the response reaches the client as the exact bytes the upstream
// wrote, however awkwardly the upstream chunked them.
func TestIdentitySeatPreservesBytesBothWays(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	payload := sseTurn()
	stub := newUpstream(t, sseHandler(t, payload, pathologicalSplits(payload)))

	gateway := httptest.NewServer(proxy.New(proxy.Config{
		Registry: newRegistry(t, identitySeat(stub.url())),
		Host:     testHost,
	}))
	t.Cleanup(gateway.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		gateway.URL+"/v1/messages", strings.NewReader(fidelityBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	req.Header.Set("Content-Type", jsonType)
	req.Header.Set(callerKeyHeader, testCallerKey)

	resp, err := gateway.Client().Do(req)
	if err != nil {
		t.Fatalf("proxied request: %v", err)
	}

	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()

	if err != nil {
		t.Fatalf("read proxied response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	if hash(got) != hash([]byte(payload)) {
		t.Errorf("response bytes differ:\n got %q\nwant %q", got, payload)
	}

	call := stub.call(t)
	if hash(call.body) != hash([]byte(fidelityBody)) {
		t.Errorf("forwarded request bytes differ:\n got %q\nwant %q", call.body, fidelityBody)
	}
}

// TestForwardPreservesPathQueryAndHeaders covers the wire details Claude Code
// depends on: the raw query survives, the anthropic-* headers arrive
// byte-identical, Accept-Encoding is forced to identity whatever the client
// asked for, and hop-by-hop headers are dropped.
func TestForwardPreservesPathQueryAndHeaders(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true&x=%20a+b",
		strings.NewReader(fidelityBody))
	req.Header.Set(callerKeyHeader, testCallerKey)
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20,interleaved-thinking-2025-05-14")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Accept-Encoding", "gzip, br")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("Connection", "X-Secret-Hop")
	req.Header.Set("X-Secret-Hop", "must-not-travel")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Te", "trailers")

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identitySeat(stub.url())),
		Host:     testHost,
	}, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	call := stub.call(t)

	if call.path != "/v1/messages" {
		t.Errorf("upstream path = %q, want %q", call.path, "/v1/messages")
	}

	if call.rawQuery != "beta=true&x=%20a+b" {
		t.Errorf("upstream raw query = %q, want %q", call.rawQuery, "beta=true&x=%20a+b")
	}

	if got := call.header.Get(
		"Anthropic-Beta",
	); got != "oauth-2025-04-20,interleaved-thinking-2025-05-14" {
		t.Errorf("anthropic-beta = %q, want it byte-identical", got)
	}

	if got := call.header.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want %q", got, "2023-06-01")
	}

	if got := call.header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("accept-encoding = %q, want identity", got)
	}

	if got := call.header.Get("X-Stainless-Lang"); got != "js" {
		t.Errorf("x-stainless-lang = %q, want it forwarded", got)
	}

	for _, name := range []string{"X-Secret-Hop", "Keep-Alive", "Te", "Connection"} {
		if got := call.header.Get(name); got != "" {
			t.Errorf("hop-by-hop header %s = %q, want it dropped", name, got)
		}
	}
}

// TestInjectReplacesCallerCredentials proves an inject Seat never leaks the
// caller's own credentials to a foreign upstream, and pays with the key read
// through the environment-variable-names-a-file indirection.
func TestInjectReplacesCallerCredentials(t *testing.T) {
	writeKeyFile(t, "  "+testAPIKey+"\n")

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))

	req := messagesRequest(fidelityBody)
	req.Header.Set("Authorization", "Bearer caller-oauth-token")
	req.Header.Set("X-Api-Key", "caller-api-key")

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identitySeat(stub.url())),
		Host:     testHost,
	}, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	call := stub.call(t)

	if got := call.header.Get("Authorization"); got != "Bearer "+testAPIKey {
		t.Errorf("authorization = %q, want the injected key", got)
	}

	if got := call.header.Get("X-Api-Key"); got != "" {
		t.Errorf("x-api-key = %q, want the caller's key deleted", got)
	}
}

// TestForwardPassesCallerCredentials proves the default Seat hands the
// caller's own subscription credentials to Anthropic untouched.
func TestForwardPassesCallerCredentials(t *testing.T) {
	stub := newTLSUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))

	req := messagesRequest(`{"model":"claude-sonnet-5","max_tokens":16}`)
	req.Header.Set("Authorization", "Bearer caller-oauth-token")
	req.Header.Set("X-Api-Key", "caller-api-key")

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, ""),
		Client:   anthropicClient(t, stub),
		Host:     testHost,
	}, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	call := stub.call(t)

	if got := call.header.Get("Authorization"); got != "Bearer caller-oauth-token" {
		t.Errorf("authorization = %q, want the caller's token verbatim", got)
	}

	if got := call.header.Get("X-Api-Key"); got != "caller-api-key" {
		t.Errorf("x-api-key = %q, want the caller's key verbatim", got)
	}
}

// TestUpstreamErrorPassesThroughUnmodified keeps error handling honest: Claude
// Code matches upstream error text to decide whether to retry, so the status,
// the headers and the body bytes all survive.
func TestUpstreamErrorPassesThroughUnmodified(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	for _, testCase := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "rate limited",
			status: http.StatusTooManyRequests,
			body:   `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited 🎲"}}`,
		},
		{
			name:   "overloaded",
			status: 529,
			body:   `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stub := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", jsonType)
				w.Header().Set("Retry-After", "42")
				w.WriteHeader(testCase.status)
				io.WriteString(w, testCase.body)
			})

			rec := serve(t, proxy.Config{
				Registry: newRegistry(t, identitySeat(stub.url())),
				Host:     testHost,
			}, messagesRequest(fidelityBody))

			if rec.Code != testCase.status {
				t.Errorf("status = %d, want %d", rec.Code, testCase.status)
			}

			if hash(rec.Body.Bytes()) != hash([]byte(testCase.body)) {
				t.Errorf("body = %q, want %q", rec.Body.String(), testCase.body)
			}

			if got := rec.Header().Get("Retry-After"); got != "42" {
				t.Errorf("retry-after = %q, want it passed through", got)
			}
		})
	}
}

// TestMissingKeyFails proves an inject Seat fails closed: every way of failing
// to read the key returns a gateway error and the upstream is never contacted.
func TestMissingKeyFails(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{
			name: "environment variable unset",
			setup: func(t *testing.T) {
				t.Helper()
				writeCallerKeyFile(t)
				t.Setenv(keyEnvName, "")
			},
		},
		{
			name: "key file missing",
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv(keyEnvName, writeKeyFile(t, testAPIKey)+".gone")
			},
		},
		{
			name:  "key file empty",
			setup: func(t *testing.T) { t.Helper(); writeKeyFile(t, "   \n") },
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.setup(t)

			stub := newUpstream(t, jsonHandler(http.StatusOK, `{}`))

			rec := serve(t, proxy.Config{
				Registry: newRegistry(t, identitySeat(stub.url())),
				Host:     testHost,
			}, messagesRequest(fidelityBody))

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
			}

			assertGatewayError(t, rec.Body.Bytes())

			if stub.hits() != 0 {
				t.Errorf("upstream received %d requests, want 0", stub.hits())
			}
		})
	}
}

// TestUnreachableUpstreamReturns502 reports a connection failure as a gateway
// error, never as a silent fallback to another Seat.
func TestUnreachableUpstreamReturns502(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{}`))
	dead := stub.url()

	stub.server.Close()

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identitySeat(dead)),
		Host:     testHost,
	}, messagesRequest(fidelityBody))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf(
			"status = %d, want %d (body %q)",
			rec.Code,
			http.StatusBadGateway,
			rec.Body.String(),
		)
	}

	assertGatewayError(t, rec.Body.Bytes())
}

// TestUnreadableRequestBodyFails covers a client that cuts its own request
// short: the gateway answers in Anthropic's error shape, contacts nobody, and
// still records the failure as a record the ledger will accept, even though the
// body it never read could name no model.
func TestUnreadableRequestBodyFails(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{}`))
	recorder := newRecorder()

	req := messagesRequest(fidelityBody)
	req.Body = io.NopCloser(&failingReader{})

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identitySeat(stub.url())),
		Recorder: recorder,
		Host:     testHost,
	}, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	assertGatewayError(t, rec.Body.Bytes())

	if stub.hits() != 0 {
		t.Errorf("upstream received %d requests, want 0", stub.hits())
	}

	got := recorder.wait(t)

	if got.Selection != nil || got.Seat != "" || got.Model != nil || got.RequestModel != nil {
		t.Errorf("pre-selection read failure recorded selected fields: %+v", got)
	}

	if err := got.Validate(); err != nil {
		t.Errorf("record does not validate: %v", err)
	}
}

// TestUnroutableBodyRidesDefaultSeat proves model extraction never fails the
// request: a body that is not JSON at all still forwards, on the default Seat,
// and still produces a record the ledger will accept — a request that named no
// model is recorded as naming none, not as an empty field Append would reject.
func TestUnroutableBodyRidesDefaultSeat(t *testing.T) {
	stub := newTLSUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))
	recorder := newRecorder()

	req := messagesRequest("not json at all")

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identitySeat("http://127.0.0.1:1")),
		Recorder: recorder,
		Client:   anthropicClient(t, stub),
		Host:     testHost,
	}, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := string(stub.call(t).body); got != "not json at all" {
		t.Errorf("forwarded body = %q, want it untouched", got)
	}

	got := recorder.wait(t)

	if got.Model != nil || got.RequestModel != nil {
		t.Errorf(
			"model/request_model = %s/%s, want an absent model for both",
			shownModel(got.Model),
			shownModel(got.RequestModel),
		)
	}

	if err := got.Validate(); err != nil {
		t.Errorf("record does not validate: %v", err)
	}
}

// TestRewriteTargetIsRefusedNotForwarded is the fallthrough guard: a request
// naming a foreign Seat's rewrite target — the shape of a client echoing a
// response's model id instead of the selector it asked with — is answered 404 in
// Anthropic's not_found_error shape, naming the selector, and nothing is sent to
// the default Seat's upstream.
func TestRewriteTargetIsRefusedNotForwarded(t *testing.T) {
	stub := newTLSUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))
	recorder := newRecorder()

	reg := newRegistry(t, `"or/flash":{"upstream":"https://openrouter.ai/api",`+
		`"auth_mode":"inject","api_key_env_file":"`+keyEnvName+`","model":"upstream/flash-1"}`)

	rec := serve(t, proxy.Config{
		Registry: reg,
		Recorder: recorder,
		Client:   anthropicClient(t, stub),
		Host:     testHost,
	}, messagesRequest(`{"model":"upstream/flash-1"}`))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusNotFound, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"not_found_error"`) {
		t.Errorf("error body %q is not in the not_found_error shape", body)
	}

	if !strings.Contains(body, "or/flash") {
		t.Errorf("error body %q does not name the selector to ask for instead", body)
	}

	if stub.hits() != 0 {
		t.Errorf("the default Seat's upstream received %d requests, want none", stub.hits())
	}

	got := recorder.wait(t)

	if got.Status != http.StatusNotFound || !modelIs(got.RequestModel, "upstream/flash-1") {
		t.Errorf("record status/request_model = %d/%s, want %d/%q",
			got.Status, shownModel(got.RequestModel), http.StatusNotFound, "upstream/flash-1")
	}
	if got.Selection != nil || got.Seat != "" || got.Upstream != "" || got.Model != nil ||
		got.AuthMode != "" || got.ConfigSource != "" {
		t.Errorf("rewrite-target refusal populated selection-dependent fields: %+v", got)
	}

	if err := got.Validate(); err != nil {
		t.Errorf("record does not validate: %v", err)
	}
}

// TestOversizedRequestBodyFails caps what a caller can make the gateway hold in
// memory: past the request ceiling the request is refused as too large and the
// upstream is never contacted. A body this size names no model, so the Seat it
// would have ridden is the default one — pinned to a local stub here.
func TestOversizedRequestBodyFails(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	// One byte past the 64 MiB the gateway is willing to read.
	const oversizeBytes = 64<<20 + 1

	stub := newTLSUpstream(t, jsonHandler(http.StatusOK, `{}`))

	req := messagesRequest(fidelityBody)
	req.Body = io.NopCloser(&oversizedReader{remaining: oversizeBytes})

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identitySeat("http://127.0.0.1:1")),
		Client:   anthropicClient(t, stub),
		Host:     testHost,
	}, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}

	assertGatewayError(t, rec.Body.Bytes())

	if stub.hits() != 0 {
		t.Errorf("upstream received %d requests, want 0", stub.hits())
	}
}

// TestResponseHopByHopHeadersAreDropped covers the answer direction of the
// hop-by-hop rule: a connection-scoped header the upstream set, and anything
// its own Connection header names, stop at the proxy.
func TestResponseHopByHopHeadersAreDropped(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", jsonType)
		w.Header().Set("Connection", "X-Upstream-Secret")
		w.Header().Set("X-Upstream-Secret", "v")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-Request-Id", "req-1")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"type":"message","content":[]}`)
	})

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identitySeat(stub.url())),
		Host:     testHost,
	}, messagesRequest(fidelityBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	for _, name := range []string{"Connection", "X-Upstream-Secret", "Keep-Alive"} {
		if got := rec.Header().Get(name); got != "" {
			t.Errorf("response header %s = %q, want it dropped", name, got)
		}
	}

	if got := rec.Header().Get("X-Request-Id"); got != "req-1" {
		t.Errorf("x-request-id = %q, want it passed through", got)
	}

	if got := rec.Header().Get("Content-Type"); got != jsonType {
		t.Errorf("content-type = %q, want %q", got, jsonType)
	}
}

// TestConcurrentRequestsAreIndependent runs the pipeline from many goroutines
// at once; under -race this is the check that the proxy holds no shared mutable
// state across requests.
func TestConcurrentRequestsAreIndependent(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	payload := sseTurn()
	stub := newUpstream(t, sseHandler(t, payload, pathologicalSplits(payload)))

	gateway := proxy.New(proxy.Config{
		Registry: newRegistry(t, identitySeat(stub.url())),
		Recorder: newRecorder(),
		Host:     testHost,
	})

	const callers = 16

	var group sync.WaitGroup

	group.Add(callers)

	for range callers {
		go func() {
			defer group.Done()

			rec := httptest.NewRecorder()
			gateway.ServeHTTP(rec, messagesRequest(fidelityBody))

			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			if hash(rec.Body.Bytes()) != hash([]byte(payload)) {
				t.Errorf("concurrent response bytes differ from upstream")
			}
		}()
	}

	group.Wait()
}

// TestInjectSeatRelaysOnlyMessagesEndpoints proves an inject Seat, which pays
// with a foreign key, refuses any path but the Messages endpoints: a stray
// GET /v1/key on it is a patchbay 404 that never spends the key, while
// /v1/messages on the same Seat still forwards.
func TestInjectSeatRelaysOnlyMessagesEndpoints(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))

	// A non-Messages path on the inject Seat is refused before the key is spent.
	blocked := httptest.NewRequest(
		http.MethodGet,
		"/v1/key",
		strings.NewReader(`{"model":"local/echo"}`),
	)
	blocked.Header.Set("Content-Type", jsonType)

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identitySeat(stub.url())),
		Host:     testHost,
	}, blocked)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d for a non-Messages path on an inject Seat (body %q)",
			rec.Code, http.StatusNotFound, rec.Body.String())
	}

	assertGatewayError(t, rec.Body.Bytes())

	// The refusal names the path it would not relay and no Seat at all: a Seat ID
	// is the operator's private name for a registry row, and a fabricated one
	// reads to everybody as a Seat that exists.
	body := rec.Body.String()
	if !strings.Contains(
		body,
		"the selected Seat injects a key and only relays the Messages endpoints, not /v1/key",
	) {
		t.Errorf("refusal %q does not name the selected Seat and the path it refused", body)
	}
	if strings.Contains(body, `selected\"`) || strings.Contains(body, `"selected"`) {
		t.Errorf("refusal %q quotes a fabricated Seat ID", body)
	}

	if stub.hits() != 0 {
		t.Errorf(
			"upstream received %d requests, want 0 — the inject key must not be spent",
			stub.hits(),
		)
	}

	// The Messages endpoint on the same Seat still forwards.
	rec = serve(t, proxy.Config{
		Registry: newRegistry(t, identitySeat(stub.url())),
		Host:     testHost,
	}, messagesRequest(`{"model":"local/echo"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d — /v1/messages still forwards on an inject Seat (body %q)",
			rec.Code, http.StatusOK, rec.Body.String())
	}

	if stub.hits() != 1 {
		t.Errorf(
			"upstream received %d requests, want 1 for the forwarded Messages call",
			stub.hits(),
		)
	}
}

// TestInjectSeatAcceptsCallerKeyAndStripsIt proves the caller gate's happy
// path: a request presenting the shared secret in X-Patchbay-Key is injected
// and forwarded, and the header — patchbay's own, never the upstream's — never
// reaches the upstream.
func TestInjectSeatAcceptsCallerKeyAndStripsIt(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identitySeat(stub.url())),
		Host:     testHost,
	}, messagesRequest(fidelityBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := stub.call(t).header.Get(callerKeyHeader); got != "" {
		t.Errorf("upstream saw %s = %q, want it stripped", callerKeyHeader, got)
	}
}

// TestInjectSeatRefusesMissingOrWrongCallerKey proves the gate fails closed on
// a caller that cannot prove the secret: a wrong key and an absent header both
// get a 403 (never 401, which Claude Code would treat as a re-login prompt),
// the foreign key is never spent, and neither the presented nor the expected
// secret leaks into the error body the ledger persists.
func TestInjectSeatRefusesMissingOrWrongCallerKey(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		presented string
		setHeader bool
	}{
		{name: "wrong key", presented: "sk-not-the-secret", setHeader: true},
		{name: "empty header", presented: "", setHeader: true},
		{name: "missing header", presented: "", setHeader: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			writeKeyFile(t, testAPIKey)

			stub := newUpstream(t, jsonHandler(http.StatusOK, `{}`))
			recorder := newRecorder()

			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/messages",
				strings.NewReader(fidelityBody),
			)
			req.Header.Set("Content-Type", jsonType)

			if testCase.setHeader {
				req.Header.Set(callerKeyHeader, testCase.presented)
			}

			rec := serve(t, proxy.Config{
				Registry: newRegistry(t, identitySeat(stub.url())),
				Recorder: recorder,
				Host:     testHost,
			}, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf(
					"status = %d, want %d (body %q)",
					rec.Code,
					http.StatusForbidden,
					rec.Body.String(),
				)
			}

			assertGatewayError(t, rec.Body.Bytes())

			if stub.hits() != 0 {
				t.Errorf(
					"upstream received %d requests, want 0 — the inject key must not be spent",
					stub.hits(),
				)
			}

			got := recorder.wait(t)

			for _, secret := range []string{testCallerKey, testAPIKey, testCase.presented} {
				if secret == "" {
					continue
				}

				if strings.Contains(rec.Body.String(), secret) {
					t.Errorf("error body leaks a key value %q", secret)
				}

				if strings.Contains(got.ErrorBody, secret) {
					t.Errorf("ledger error_body leaks a key value %q", secret)
				}
			}
		})
	}
}

// TestInjectSeatFailsClosedWhenCallerKeyUnavailable proves a server-side
// problem reading the expected caller key refuses the inject request with a 500
// rather than injecting the household key for a caller that proved nothing.
func TestInjectSeatFailsClosedWhenCallerKeyUnavailable(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{
			name: "environment variable unset",
			setup: func(t *testing.T) {
				t.Helper()
				writeKeyFile(t, testAPIKey)
				t.Setenv(callerKeyEnvName, "")
			},
		},
		{
			name: "caller key file missing",
			setup: func(t *testing.T) {
				t.Helper()
				writeKeyFile(t, testAPIKey)
				t.Setenv(callerKeyEnvName, writeCallerKeyFile(t)+".gone")
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.setup(t)

			stub := newUpstream(t, jsonHandler(http.StatusOK, `{}`))

			rec := serve(t, proxy.Config{
				Registry: newRegistry(t, identitySeat(stub.url())),
				Host:     testHost,
			}, messagesRequest(fidelityBody))

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf(
					"status = %d, want %d (body %q)",
					rec.Code,
					http.StatusInternalServerError,
					rec.Body.String(),
				)
			}

			assertGatewayError(t, rec.Body.Bytes())

			if stub.hits() != 0 {
				t.Errorf(
					"upstream received %d requests, want 0 — the inject key must not be spent",
					stub.hits(),
				)
			}
		})
	}
}

// TestForwardSeatSkipsCallerKeyAndStripsIt proves the gate is unreachable on a
// forward Seat: the caller-key header is never required — a request without
// any caller key file provisioned still forwards — yet the header, being
// patchbay's own, is still stripped from the upstream request.
func TestForwardSeatSkipsCallerKeyAndStripsIt(t *testing.T) {
	stub := newTLSUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))

	req := messagesRequest(`{"model":"claude-sonnet-5","max_tokens":16}`)
	req.Header.Set(callerKeyHeader, "whatever-the-caller-sent")

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, ""),
		Client:   anthropicClient(t, stub),
		Host:     testHost,
	}, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := stub.call(t).header.Get(callerKeyHeader); got != "" {
		t.Errorf(
			"upstream saw %s = %q, want it stripped on the forward Seat too",
			callerKeyHeader,
			got,
		)
	}
}

// failingReader is a request body that fails on first read.
type failingReader struct{}

// Read always fails.
func (*failingReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

// oversizedReader is a request body one byte past the gateway's request
// ceiling. It generates its bytes rather than holding them, and it does end, so
// a run where the ceiling is not enforced still finishes.
type oversizedReader struct {
	remaining int
}

// Read fills the buffer until the body is spent.
func (o *oversizedReader) Read(buf []byte) (int, error) {
	if o.remaining <= 0 {
		return 0, io.EOF
	}

	if len(buf) > o.remaining {
		buf = buf[:o.remaining]
	}

	for i := range buf {
		buf[i] = 'x'
	}

	o.remaining -= len(buf)

	return len(buf), nil
}

// assertGatewayError checks a body is a well-formed Anthropic error naming
// patchbay as the source.
func assertGatewayError(t *testing.T, body []byte) {
	t.Helper()

	decoded, isObject := decodeJSON(t, body).(map[string]any)
	if !isObject {
		t.Fatalf("gateway error %q is not a JSON object", body)
	}

	if decoded["type"] != "error" {
		t.Errorf("gateway error type = %v, want %q", decoded["type"], "error")
	}

	detail, isObject := decoded["error"].(map[string]any)
	if !isObject {
		t.Fatalf("gateway error %q has no error object", body)
	}

	if detail["type"] != "api_error" {
		t.Errorf("error.type = %v, want %q", detail["type"], "api_error")
	}

	message, isString := detail["message"].(string)
	if !isString || !strings.HasPrefix(message, "patchbay: ") {
		t.Errorf("error.message = %v, want a patchbay-prefixed message", detail["message"])
	}
}

// The HTTP surface resolves a request's context once, before anything
// dispatches on it, and the pipeline forwards by that resolution. A registry
// file replaced while the request is in flight must not move it: half a request
// served from one document and half from the next is exactly the race the
// single resolution closes.
func TestPipelineForwardsByTheAttachedResolution(t *testing.T) {
	first := newUpstream(t, jsonHandler(http.StatusOK, `{"id":"msg_first","content":[]}`))
	second := newUpstream(t, jsonHandler(http.StatusOK, `{"id":"msg_second","content":[]}`))

	writeKeyFile(t, "sk-upstream")

	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistryFile(t, path, identitySeat(first.server.URL))

	reg, err := registry.New(path)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	res, err := reg.Resolve(registry.Selection{}, "", nil)
	if err != nil {
		t.Fatalf("Resolve() = %v; want the personal context", err)
	}

	// The file moves to a different upstream between the resolution and the
	// forward, exactly as a home-manager rebuild would.
	writeRegistryFile(t, path, identitySeat(second.server.URL))

	req := messagesRequest(`{"model":"` + echoSelector + `","max_tokens":4}`)
	req = req.WithContext(registry.WithResolution(req.Context(), res))

	answer := serve(t, proxy.Config{Registry: reg, Host: testHost}, req)

	if answer.Code != http.StatusOK {
		t.Fatalf("status = %d (%s); want 200", answer.Code, answer.Body.String())
	}

	if first.hits() != 1 {
		t.Fatalf(
			"the resolved upstream saw %d requests; want the one this request resolved to",
			first.hits(),
		)
	}

	if second.hits() != 0 {
		t.Fatalf(
			"the reloaded upstream saw %d requests; a request never changes documents mid-flight",
			second.hits(),
		)
	}
}

func writeRegistryFile(t *testing.T, path, entry string) {
	t.Helper()
	var seats map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{`+entry+`}`), &seats); err != nil {
		t.Fatal(err)
	}
	seat := seats[echoSelector]
	doc := map[string]any{
		"default_context": "personal",
		"seats": map[string]json.RawMessage{
			"anthropic": json.RawMessage(
				`{"upstream":"https://api.anthropic.com","auth_mode":"forward"}`,
			),
			"echo": seat,
		},
		"contexts": map[string]any{
			"personal": map[string]any{
				"default_seat": "anthropic",
				"models":       map[string]string{echoSelector: "echo"},
			},
		},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
}
