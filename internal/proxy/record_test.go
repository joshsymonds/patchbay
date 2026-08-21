package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/proxy"
)

// jsonTurn is a complete non-streaming answer with two content blocks.
const jsonTurn = `{"id":"msg_02","type":"message","role":"assistant","model":"vendor/model-1",` +
	`"content":[{"type":"thinking","thinking":"reasoning"},{"type":"text","text":"hello"}],` +
	`"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":22}}`

// TestRecordsStreamingTurn checks the ledger record a streamed turn produces:
// the route it rode, the tokens it burned, the block shape it came back in, and
// the drop signature two adjacent reasoning blocks carry.
func TestRecordsStreamingTurn(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	payload := sseTurn()
	stub := newUpstream(t, sseHandler(t, payload, pathologicalSplits(payload)))
	recorder := newRecorder()

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identityRoute(stub.url())),
		Recorder: recorder,
		Host:     testHost,
	}, messagesRequest(fidelityBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	got := recorder.wait(t)

	if got.Host != testHost {
		t.Errorf("host = %q, want %q", got.Host, testHost)
	}

	if got.Alias != echoAlias {
		t.Errorf("alias = %q, want %q", got.Alias, echoAlias)
	}

	if got.Upstream != stub.url() {
		t.Errorf("upstream = %q, want %q", got.Upstream, stub.url())
	}

	if got.Model != echoAlias || got.RequestModel != echoAlias {
		t.Errorf("model/request_model = %q/%q, want %q for both", got.Model, got.RequestModel, echoAlias)
	}

	if got.AuthMode != "inject" {
		t.Errorf("auth_mode = %q, want %q", got.AuthMode, "inject")
	}

	if got.Status != http.StatusOK {
		t.Errorf("status = %d, want %d", got.Status, http.StatusOK)
	}

	if want := []string{"thinking", "thinking", "text"}; !reflect.DeepEqual(got.BlockShape, want) {
		t.Errorf("block_shape = %v, want %v", got.BlockShape, want)
	}

	if !got.DropSignature {
		t.Error("drop_signature = false, want true for two adjacent reasoning blocks")
	}

	if got.InputTokens != 1234 || got.OutputTokens != 567 {
		t.Errorf("tokens = %d/%d, want 1234/567", got.InputTokens, got.OutputTokens)
	}

	if got.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want %q", got.StopReason, "end_turn")
	}

	if got.Timestamp.IsZero() {
		t.Error("timestamp is zero, want the request start time")
	}

	if got.DurationMS < got.UpstreamMS {
		t.Errorf("durations = %d/%d, want the whole exchange to cover the upstream leg",
			got.DurationMS, got.UpstreamMS)
	}

	if got.RequestBody != "" || got.ResponseBody != "" {
		t.Error("bodies recorded for a route that did not ask for capture")
	}

	if err := got.Validate(); err != nil {
		t.Errorf("record does not validate: %v", err)
	}
}

// TestRecordsNonStreamingTurn reads the same summary out of a plain JSON
// response.
func TestRecordsNonStreamingTurn(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
	recorder := newRecorder()

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, rewriteRoute(stub.url())),
		Recorder: recorder,
		Host:     testHost,
	}, messagesRequest(rewriteBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	got := recorder.wait(t)

	if got.RequestModel != rewriteAlias {
		t.Errorf("request_model = %q, want %q", got.RequestModel, rewriteAlias)
	}

	if got.Model != "vendor/model-1" {
		t.Errorf("model = %q, want the model actually sent upstream", got.Model)
	}

	if want := []string{"thinking", "text"}; !reflect.DeepEqual(got.BlockShape, want) {
		t.Errorf("block_shape = %v, want %v", got.BlockShape, want)
	}

	if got.DropSignature {
		t.Error("drop_signature = true, want false for reasoning followed by text")
	}

	if got.InputTokens != 11 || got.OutputTokens != 22 {
		t.Errorf("tokens = %d/%d, want 11/22", got.InputTokens, got.OutputTokens)
	}

	if got.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want %q", got.StopReason, "end_turn")
	}
}

// partialSSETurn is the prefix of a streamed turn that never finishes: a
// message_start and two adjacent thinking blocks, but no message_delta and no
// message_stop. Summarized as if it were whole it would report a block shape, a
// drop signature and input tokens — none of which a cut-short turn actually
// completed.
func partialSSETurn() string {
	lines := []string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_01","model":"local/echo",` +
			`"usage":{"input_tokens":1234,"output_tokens":1}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
		"",
	}

	return strings.Join(lines, "\n") + "\n"
}

// TestRecordsTruncatedStreamAsIncomplete covers a stream cut short mid-flight:
// the upstream writes a partial turn and then drops the connection, so the
// proxy's read fails short of the turn's own message_stop. The record must
// reflect an incomplete turn — no block shape, no drop signature, no summarized
// tokens or stop reason — rather than reporting the partial prefix as a
// completed turn.
func TestRecordsTruncatedStreamAsIncomplete(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	partial := partialSSETurn()
	stub := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", sseType)
		// Promise more than is delivered, so returning here leaves the response
		// body short of its Content-Length and the proxy's read ends with an
		// unexpected EOF rather than a clean one.
		w.Header().Set("Content-Length", fmt.Sprint(len(partial)+1024))
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, partial)
	})
	recorder := newRecorder()
	sink := newLogSink()

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identityRoute(stub.url())),
		Recorder: recorder,
		Logger:   slog.New(sink),
		Host:     testHost,
	}, messagesRequest(fidelityBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	sink.waitFor(t, "upstream stream ended early")

	got := recorder.wait(t)

	if len(got.BlockShape) != 0 {
		t.Errorf("block_shape = %v, want empty for a truncated turn", got.BlockShape)
	}

	if got.DropSignature {
		t.Error("drop_signature = true, want false — a truncated prefix is not a summarized turn")
	}

	if got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Errorf("tokens = %d/%d, want 0/0 — a truncated turn is not summarized",
			got.InputTokens, got.OutputTokens)
	}

	if got.StopReason != "" {
		t.Errorf("stop_reason = %q, want empty — the turn never reached its stop", got.StopReason)
	}

	if err := got.Validate(); err != nil {
		t.Errorf("record does not validate: %v", err)
	}
}

// TestRecordsUpstreamError keeps the upstream's own error text in the record,
// with no block shape to report.
func TestRecordsUpstreamError(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	const upstreamError = `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`

	stub := newUpstream(t, jsonHandler(http.StatusTooManyRequests, upstreamError))
	recorder := newRecorder()

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identityRoute(stub.url())),
		Recorder: recorder,
		Host:     testHost,
	}, messagesRequest(fidelityBody))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}

	got := recorder.wait(t)

	if got.Status != http.StatusTooManyRequests {
		t.Errorf("record status = %d, want %d", got.Status, http.StatusTooManyRequests)
	}

	if got.ErrorBody != upstreamError {
		t.Errorf("error_body = %q, want %q", got.ErrorBody, upstreamError)
	}

	if len(got.BlockShape) != 0 {
		t.Errorf("block_shape = %v, want empty", got.BlockShape)
	}

	if err := got.Validate(); err != nil {
		t.Errorf("record does not validate: %v", err)
	}
}

// TestRecordsGatewayError records patchbay's own failures too, with the error
// JSON patchbay returned. The caller key is provisioned and only the inject
// key is broken, so the failure recorded is the upstream key's — not a caller
// rejection, and not whatever PATCHBAY_CALLER_KEY_FILE names outside the test.
func TestRecordsGatewayError(t *testing.T) {
	writeCallerKeyFile(t)
	t.Setenv(keyEnvName, "")

	stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
	recorder := newRecorder()

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identityRoute(stub.url())),
		Recorder: recorder,
		Host:     testHost,
	}, messagesRequest(fidelityBody))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	got := recorder.wait(t)

	if got.Status != http.StatusInternalServerError {
		t.Errorf("record status = %d, want %d", got.Status, http.StatusInternalServerError)
	}

	if got.ErrorBody != rec.Body.String() {
		t.Errorf("error_body = %q, want the returned error JSON %q", got.ErrorBody, rec.Body.String())
	}

	if err := got.Validate(); err != nil {
		t.Errorf("record does not validate: %v", err)
	}
}

// TestCaptureBodiesRecordsBothBodies proves the capture flag stores the bytes
// actually forwarded and the bytes actually returned — and that even the route
// which records the most still records no credential of any kind: not the
// caller's, and not the one patchbay injected.
func TestCaptureBodiesRecordsBothBodies(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
	route := fmt.Sprintf(`%q:{"base_url":%q,"auth":"inject","api_key_env_file":%q,`+
		`"capture_bodies":true}`, echoAlias, stub.url(), keyEnvName)
	recorder := newRecorder()

	req := messagesRequest(fidelityBody)
	req.Header.Set("Authorization", "Bearer caller-oauth-token")
	req.Header.Set("X-Api-Key", "caller-api-key")

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, route),
		Recorder: recorder,
		Host:     testHost,
	}, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	got := recorder.wait(t)

	if got.RequestBody != fidelityBody {
		t.Errorf("request_body = %q, want the forwarded bytes", got.RequestBody)
	}

	if got.ResponseBody != jsonTurn {
		t.Errorf("response_body = %q, want the captured bytes", got.ResponseBody)
	}

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}

	for _, credential := range []string{"caller-oauth-token", "caller-api-key", testAPIKey} {
		if bytes.Contains(encoded, []byte(credential)) {
			t.Errorf("record carries the credential %q: %s", credential, encoded)
		}
	}
}

// TestRecorderFailuresNeverReachTheClient covers the passive-tee rule from both
// sides: a recorder that errors and a recorder that blocks forever both leave
// the client's bytes and status alone.
func TestRecorderFailuresNeverReachTheClient(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		recorder *stubRecorder
	}{
		{
			name:     "recorder returns an error",
			recorder: &stubRecorder{records: make(chan ledger.Record, 1), err: io.ErrClosedPipe},
		},
		{
			name: "recorder blocks",
			recorder: &stubRecorder{
				records: make(chan ledger.Record, 1),
				release: make(chan struct{}),
			},
		},
		{
			name:     "no recorder at all",
			recorder: nil,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			writeKeyFile(t, testAPIKey)

			payload := sseTurn()
			stub := newUpstream(t, sseHandler(t, payload, pathologicalSplits(payload)))

			cfg := proxy.Config{
				Registry: newRegistry(t, identityRoute(stub.url())),
				Host:     testHost,
			}

			if testCase.recorder != nil {
				cfg.Recorder = testCase.recorder

				if testCase.recorder.release != nil {
					t.Cleanup(func() { close(testCase.recorder.release) })
				}
			}

			rec := serve(t, cfg, messagesRequest(fidelityBody))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			if hash(rec.Body.Bytes()) != hash([]byte(payload)) {
				t.Errorf("response bytes differ from what the upstream wrote")
			}
		})
	}
}

// TestDroppedRecordIsLoggedToTheConfiguredLogger proves the passive tee is not
// a silent one, and that the warning goes to the logger the host process
// supplied rather than the process-wide default.
func TestDroppedRecordIsLoggedToTheConfiguredLogger(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
	sink := newLogSink()

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identityRoute(stub.url())),
		Recorder: &stubRecorder{records: make(chan ledger.Record, 1), err: io.ErrClosedPipe},
		Logger:   slog.New(sink),
		Host:     testHost,
	}, messagesRequest(fidelityBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	sink.waitFor(t, "ledger record dropped")
}

// TestOversizedResponseDropsCaptureNotStream proves the tee is passive at the
// one place it could hurt: past the capture ceiling the record loses its
// summary and the client still gets every byte.
func TestOversizedResponseDropsCaptureNotStream(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	const (
		chunkBytes = 1 << 20
		chunks     = 65
	)

	stub := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", sseType)
		w.WriteHeader(http.StatusOK)

		chunk := strings.Repeat("x", chunkBytes)
		for range chunks {
			io.WriteString(w, chunk)
		}
	})
	recorder := newRecorder()

	gateway := httptest.NewServer(newProxy(t, proxy.Config{
		Registry:   newRegistry(t, identityRoute(stub.url())),
		Recorder:   recorder,
		Host:       testHost,
		Accountant: permissiveAccountant{},
	}))
	t.Cleanup(gateway.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		gateway.URL+"/v1/messages", strings.NewReader(fidelityBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	req.Header.Set(callerKeyHeader, testCallerKey)

	resp, err := gateway.Client().Do(req)
	if err != nil {
		t.Fatalf("proxied request: %v", err)
	}

	delivered, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if err != nil {
		t.Fatalf("read proxied response: %v", err)
	}

	if want := int64(chunkBytes) * chunks; delivered != want {
		t.Errorf("delivered %d bytes, want %d", delivered, want)
	}

	got := recorder.wait(t)

	if len(got.BlockShape) != 0 {
		t.Errorf("block_shape = %v, want empty after the capture was dropped", got.BlockShape)
	}

	if got.ResponseBody != "" {
		t.Error("response_body recorded despite the dropped capture")
	}

	if got.Status != http.StatusOK {
		t.Errorf("record status = %d, want %d", got.Status, http.StatusOK)
	}
}

// workContext is a second context whose default route rides a foreign upstream,
// so a request in it resolves nothing the personal context holds.
func workContext(baseURL string) string {
	return fmt.Sprintf(`"work":{"default_route":{"base_url":%q,"model":"vendor/work-1",`+
		`"auth":"inject","api_key_env_file":%q},"routes":{}}`, baseURL, keyEnvName)
}

// TestRecordsTheResolvedContext pins the attribution half of contexts: every
// record names the context that served it, and a named context resolves its own
// routes rather than another context's aliases.
func TestRecordsTheResolvedContext(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	tests := []struct {
		name        string
		named       string
		wantContext string
		wantAlias   string
		wantModel   string
	}{
		{
			name:        "a bare path is the personal context",
			named:       "",
			wantContext: "personal",
			wantAlias:   echoAlias,
			wantModel:   echoAlias,
		},
		{
			name:        "a named context resolves its own default route",
			named:       "work",
			wantContext: "work",
			wantAlias:   "",
			wantModel:   "vendor/work-1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
			recorder := newRecorder()

			req := messagesRequest(fidelityBody)
			if tc.named != "" {
				req = inContext(req, tc.named)
			}

			rec := serve(t, proxy.Config{
				Registry: newContextRegistry(t,
					`"personal":{"routes":{`+identityRoute(stub.url())+`}},`+workContext(stub.url())),
				Recorder: recorder,
				Host:     testHost,
			}, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			got := recorder.wait(t)

			if got.Context != tc.wantContext {
				t.Errorf("context = %q, want %q", got.Context, tc.wantContext)
			}

			if got.Alias != tc.wantAlias {
				t.Errorf("alias = %q, want %q", got.Alias, tc.wantAlias)
			}

			if got.Model != tc.wantModel {
				t.Errorf("model = %q, want %q", got.Model, tc.wantModel)
			}

			if err := got.Validate(); err != nil {
				t.Errorf("record does not validate: %v", err)
			}
		})
	}
}

// TestPipelineFailsClosedOnAnUnknownContext is the pipeline's own half of the
// fail-closed rule. The HTTP surface refuses an unknown context before the
// pipeline is reached, so this is the registry-reloaded-underneath race: the
// request is refused, never served by another context or the default route.
func TestPipelineFailsClosedOnAnUnknownContext(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
	recorder := newRecorder()

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identityRoute(stub.url())),
		Recorder: recorder,
		Host:     testHost,
	}, inContext(messagesRequest(fidelityBody), "vanished"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	if stub.hits() != 0 {
		t.Fatalf("upstream received %d requests; want none for an unknown context", stub.hits())
	}

	if !strings.Contains(rec.Body.String(), `unknown context \"vanished\"`) {
		t.Fatalf("body = %s; want it to name the context it refused", rec.Body.String())
	}

	// A refusal is still an event worth auditing, so it leaves a record naming
	// the context that was refused.
	got := recorder.wait(t)

	if got.Context != "vanished" || got.Status != http.StatusNotFound {
		t.Fatalf("record = %+v; want the refusal recorded against the named context", got)
	}

	if err := got.Validate(); err != nil {
		t.Fatalf("refusal record does not validate: %v", err)
	}
}

// TestRecordsTheInboundCredentialClass classifies the credential the CLIENT
// presented, which is not the one the route pays with: every case below rides
// an inject route, whose own credential never appears in the record.
func TestRecordsTheInboundCredentialClass(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    billing.CredentialClass
	}{
		{
			name:    "oauth bearer",
			headers: map[string]string{"Authorization": "Bearer sk-ant-oat01-subscription"},
			want:    billing.CredentialOAuth,
		},
		{
			name:    "lowercase bearer scheme",
			headers: map[string]string{"Authorization": "bearer sk-ant-oat01-subscription"},
			want:    billing.CredentialOAuth,
		},
		{
			name:    "api key",
			headers: map[string]string{"X-Api-Key": "sk-ant-api03-console"},
			want:    billing.CredentialAPIKey,
		},
		{
			name: "both, an oauth session that also sent a key",
			headers: map[string]string{
				"Authorization": "Bearer sk-ant-oat01-subscription",
				"X-Api-Key":     "sk-ant-api03-console",
			},
			want: billing.CredentialOAuth,
		},
		{
			name:    "an authorization header that is not a bearer",
			headers: map[string]string{"Authorization": "Basic dXNlcjpwYXNz"},
			want:    billing.CredentialNone,
		},
		{
			name:    "no credential at all",
			headers: map[string]string{},
			want:    billing.CredentialNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			writeKeyFile(t, testAPIKey)

			stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
			recorder := newRecorder()

			req := messagesRequest(fidelityBody)
			for name, value := range tc.headers {
				req.Header.Set(name, value)
			}

			rec := serve(t, proxy.Config{
				Registry: newRegistry(t, identityRoute(stub.url())),
				Recorder: recorder,
				Host:     testHost,
			}, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			got := recorder.wait(t)

			if got.CredentialClass != tc.want {
				t.Errorf("credential_class = %q, want %q", got.CredentialClass, tc.want)
			}
		})
	}
}

// TestRecordsRejectedForwardCredentials proves every credential shape refused
// locally still leaves an auditable record carrying the refusal sent to the
// caller, without extending the ledger's credential vocabulary or retaining a
// bare credential payload anywhere in the serialized record or response.
func TestRecordsRejectedForwardCredentials(t *testing.T) {
	tests := []struct {
		name                string
		headers             http.Header
		secrets             []string
		wantStatus          int
		wantCredentialClass billing.CredentialClass
	}{
		{
			name:                "no credential",
			headers:             http.Header{},
			wantStatus:          http.StatusUnauthorized,
			wantCredentialClass: billing.CredentialNone,
		},
		{
			name: "empty bearer credential",
			headers: http.Header{
				"Authorization": []string{"Bearer "},
			},
			wantStatus:          http.StatusUnauthorized,
			wantCredentialClass: billing.CredentialNone,
		},
		{
			name: "whitespace-only bearer credential",
			headers: http.Header{
				"Authorization": []string{"Bearer \t "},
			},
			wantStatus:          http.StatusUnauthorized,
			wantCredentialClass: billing.CredentialNone,
		},
		{
			name: "empty api key",
			headers: http.Header{
				"X-Api-Key": []string{""},
			},
			wantStatus:          http.StatusUnauthorized,
			wantCredentialClass: billing.CredentialNone,
		},
		{
			name: "whitespace-only api key",
			headers: http.Header{
				"X-Api-Key": []string{" \t "},
			},
			wantStatus:          http.StatusUnauthorized,
			wantCredentialClass: billing.CredentialNone,
		},
		{
			name: "unrecognized authorization scheme",
			headers: http.Header{
				"Authorization": []string{"Basic basic-payload-sentinel"},
			},
			secrets:             []string{"basic-payload-sentinel"},
			wantStatus:          http.StatusUnauthorized,
			wantCredentialClass: billing.CredentialNone,
		},
		{
			name: "bearer followed by basic authorization",
			headers: http.Header{
				"Authorization": []string{
					"Bearer bearer-before-basic-sentinel",
					"Basic basic-after-bearer-sentinel",
				},
			},
			secrets: []string{
				"bearer-before-basic-sentinel",
				"basic-after-bearer-sentinel",
			},
			wantStatus:          http.StatusUnauthorized,
			wantCredentialClass: billing.CredentialOAuth,
		},
		{
			name: "repeated bearer authorization",
			headers: http.Header{
				"Authorization": []string{
					"Bearer repeated-bearer-first-sentinel",
					"Bearer repeated-bearer-second-sentinel",
				},
			},
			secrets: []string{
				"repeated-bearer-first-sentinel",
				"repeated-bearer-second-sentinel",
			},
			wantStatus:          http.StatusUnauthorized,
			wantCredentialClass: billing.CredentialOAuth,
		},
		{
			name: "repeated api key",
			headers: http.Header{
				"X-Api-Key": []string{
					"repeated-api-key-first-sentinel",
					"repeated-api-key-second-sentinel",
				},
			},
			secrets: []string{
				"repeated-api-key-first-sentinel",
				"repeated-api-key-second-sentinel",
			},
			wantStatus:          http.StatusUnauthorized,
			wantCredentialClass: billing.CredentialAPIKey,
		},
		{
			name: "oauth bearer and api key",
			headers: http.Header{
				"Authorization": []string{"Bearer mixed-oauth-sentinel"},
				"X-Api-Key":     []string{"mixed-api-key-sentinel"},
			},
			secrets:             []string{"mixed-oauth-sentinel", "mixed-api-key-sentinel"},
			wantStatus:          http.StatusBadRequest,
			wantCredentialClass: billing.CredentialOAuth,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := newTLSUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
			recorder := newRecorder()

			req := messagesRequest(`{"model":"claude-sonnet-5","max_tokens":16}`)
			for name, values := range tc.headers {
				for _, value := range values {
					req.Header.Add(name, value)
				}
			}

			answer := serve(t, proxy.Config{
				Registry: newRegistry(t, ""),
				Recorder: recorder,
				Client:   anthropicClient(t, stub),
				Host:     testHost,
			}, req)

			if answer.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", answer.Code, tc.wantStatus, answer.Body.String())
			}

			if stub.hits() != 0 {
				t.Errorf("upstream received %d requests; want none", stub.hits())
			}

			got := recorder.wait(t)
			if got.Status != tc.wantStatus {
				t.Errorf("record status = %d, want %d", got.Status, tc.wantStatus)
			}

			if got.AuthMode != "forward" {
				t.Errorf("auth_mode = %q, want %q", got.AuthMode, "forward")
			}

			if got.CredentialClass != tc.wantCredentialClass {
				t.Errorf("credential_class = %q, want %q", got.CredentialClass, tc.wantCredentialClass)
			}

			if got.ErrorBody != answer.Body.String() {
				t.Errorf("error_body = %q, want response body %q", got.ErrorBody, answer.Body.String())
			}

			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal refusal record: %v", err)
			}

			for _, secret := range tc.secrets {
				if bytes.Contains(encoded, []byte(secret)) {
					t.Errorf("serialized record contains bare credential payload %q: %s", secret, encoded)
				}

				if bytes.Contains(answer.Body.Bytes(), []byte(secret)) {
					t.Errorf("response body contains bare credential payload %q: %s", secret, answer.Body.Bytes())
				}
			}

			if err := got.Validate(); err != nil {
				t.Errorf("refusal record does not validate: %v", err)
			}
		})
	}
}

// TestRecordNeverCarriesACredentialValue is the safety property behind the
// classification: a request whose every credential header holds a distinctive
// secret leaves a record with none of them anywhere in it.
func TestRecordNeverCarriesACredentialValue(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	const (
		oauthSecret  = "sk-ant-oat01-do-not-record-me"
		clientSecret = "sk-ant-api03-do-not-record-me"
	)

	stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
	recorder := newRecorder()

	req := messagesRequest(fidelityBody)
	req.Header.Set("Authorization", "Bearer "+oauthSecret)
	req.Header.Set("X-Api-Key", clientSecret)

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identityRoute(stub.url())),
		Recorder: recorder,
		Host:     testHost,
	}, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	got := recorder.wait(t)

	line, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode record: %v", err)
	}

	for _, secret := range []string{oauthSecret, clientSecret, testAPIKey, testCallerKey} {
		if strings.Contains(string(line), secret) {
			t.Fatalf("record line contains the secret %q: %s", secret, line)
		}
	}

	if got.CredentialClass != billing.CredentialOAuth {
		t.Fatalf("credential_class = %q, want %q", got.CredentialClass, billing.CredentialOAuth)
	}
}

// TestEveryRecordNamesItsContextAndCredentialClass is the write side of the
// ledger's read-side leniency: Validate accepts a record with neither
// attribution field so ledger files written before contexts existed can still
// be read, and this is what keeps a current binary from ever writing one that
// shape — down every path that produces a record, including the ones where
// nothing was forwarded at all.
func TestEveryRecordNamesItsContextAndCredentialClass(t *testing.T) {
	tests := []struct {
		name        string
		request     func(t *testing.T, stub *upstream) *http.Request
		wantContext string
	}{
		{
			name: "a forwarded turn",
			request: func(*testing.T, *upstream) *http.Request {
				return messagesRequest(fidelityBody)
			},
			wantContext: "personal",
		},
		{
			name: "a turn in a named context",
			request: func(*testing.T, *upstream) *http.Request {
				return inContext(messagesRequest(fidelityBody), "personal")
			},
			wantContext: "personal",
		},
		{
			name: "a request whose body could not be read",
			request: func(*testing.T, *upstream) *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/v1/messages",
					iotest.ErrReader(errors.New("the caller went away mid-body")))
				req.Header.Set(callerKeyHeader, testCallerKey)

				return req
			},
			wantContext: "personal",
		},
		{
			name: "a request refused for naming an unknown context",
			request: func(*testing.T, *upstream) *http.Request {
				return inContext(messagesRequest(fidelityBody), "vanished")
			},
			wantContext: "vanished",
		},
		{
			name: "a request refused at the inject path gate",
			request: func(*testing.T, *upstream) *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/v1/organizations/usage_report",
					strings.NewReader(fidelityBody))
				req.Header.Set(callerKeyHeader, testCallerKey)

				return req
			},
			wantContext: "personal",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			writeKeyFile(t, testAPIKey)

			stub := newUpstream(t, jsonHandler(http.StatusOK, jsonTurn))
			recorder := newRecorder()

			serve(t, proxy.Config{
				Registry: newRegistry(t, identityRoute(stub.url())),
				Recorder: recorder,
				Host:     testHost,
			}, tc.request(t, stub))

			got := recorder.wait(t)

			if got.Context != tc.wantContext {
				t.Errorf("context = %q, want %q", got.Context, tc.wantContext)
			}

			if got.CredentialClass == "" {
				t.Error("credential_class is empty; a current binary must never write a legacy-shaped record")
			}

			if err := got.Validate(); err != nil {
				t.Errorf("record does not validate: %v", err)
			}
		})
	}
}
