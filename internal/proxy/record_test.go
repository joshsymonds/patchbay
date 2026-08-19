package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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
// JSON patchbay returned.
func TestRecordsGatewayError(t *testing.T) {
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

	gateway := httptest.NewServer(proxy.New(proxy.Config{
		Registry: newRegistry(t, identityRoute(stub.url())),
		Recorder: recorder,
		Host:     testHost,
	}))
	t.Cleanup(gateway.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		gateway.URL+"/v1/messages", strings.NewReader(fidelityBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

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
