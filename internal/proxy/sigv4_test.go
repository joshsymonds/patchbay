package proxy_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"log/slog"

	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/registry"
)

const (
	// bedrockAlias is the sigv4 route every test in this file resolves through,
	// and bedrockModel the map key it names.
	bedrockAlias = "bedrock/sonnet"
	bedrockModel = "claude-sonnet-5"

	// bedrockARN is an application-inference-profile ARN: colons and a slash,
	// which is exactly the escaping the request path has to get right.
	bedrockARN = "arn:aws:bedrock:us-east-2:123456789012:application-inference-profile/abcdef123456"

	// bedrockEndpoint is what a sigv4 route's region derives, and what the ledger
	// records as the upstream.
	bedrockEndpoint = "https://bedrock-runtime.us-east-2.amazonaws.com"

	mapEnvName  = "PATCHBAY_TEST_BEDROCK_MAP"
	awsLogEnv   = "PATCHBAY_TEST_AWS_LOG"
	awsProfile  = "work"
	fakeKeyID   = "AKIAFAKE"
	fakeSecret  = "fake-secret"
	fakeSession = "fake-token"

	eventStreamType   = "application/vnd.amazon.eventstream"
	stringHeaderType  = 7
	frameOverheadSize = 16
)

// sigv4Route is the registry entry for a Bedrock route: no base_url, since the
// endpoint comes from the region.
func sigv4Route(model string) string {
	return fmt.Sprintf(`%q:{"auth":"sigv4","aws_profile":%q,"aws_region":"us-east-2",`+
		`"model_map_env_file":%q,"model":%q}`, bedrockAlias, awsProfile, mapEnvName, model)
}

// sigv4DefaultContext is a context whose default route signs for Bedrock, so
// every model it does not claim is mapped through the map file.
func sigv4DefaultContext(name string) string {
	return fmt.Sprintf(`%q:{"default_route":{"auth":"sigv4","aws_profile":%q,"aws_region":"us-east-2",`+
		`"model_map_env_file":%q},"routes":{}}`, name, awsProfile, mapEnvName)
}

// writeModelMap points the route's model-map environment variable at a file
// holding mapping.
func writeModelMap(t *testing.T, mapping string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(mapping), 0o600); err != nil {
		t.Fatalf("write model map: %v", err)
	}

	t.Setenv(mapEnvName, path)
}

// defaultModelMap maps the route's model key to the test ARN.
func defaultModelMap(t *testing.T) {
	t.Helper()

	writeModelMap(t, fmt.Sprintf(`{%q:%q}`, bedrockModel, bedrockARN))
}

// fakeAWS installs a script named aws ahead of everything else on PATH and
// returns the path of the log every invocation appends to, so a test can count
// credential exports without a clock or a hook.
func fakeAWS(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> \"$" + awsLogEnv + "\"\n" + body

	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake aws: %v", err)
	}

	t.Setenv(awsLogEnv, logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return logPath
}

// workingAWS installs a fake aws that exports usable credentials.
func workingAWS(t *testing.T) string {
	t.Helper()

	return fakeAWS(t, "cat <<'JSON'\n"+
		fmt.Sprintf(`{"AccessKeyId":%q,"SecretAccessKey":%q,"SessionToken":%q,`+
			`"Expiration":"2099-01-01T00:00:00Z"}`, fakeKeyID, fakeSecret, fakeSession)+
		"\nJSON\n")
}

// execCount reports how many times the fake aws ran.
func execCount(t *testing.T, logPath string) int {
	t.Helper()

	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}

		t.Fatalf("read aws call log: %v", err)
	}

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return 0
	}

	return len(strings.Split(trimmed, "\n"))
}

// bedrockSetup provisions everything a sigv4 request needs: a caller key, a
// working aws, a model map, and a TLS stub standing in for bedrock-runtime.
func bedrockSetup(t *testing.T, handle http.HandlerFunc) (*upstream, *http.Client, string) {
	t.Helper()

	writeCallerKeyFile(t)
	defaultModelMap(t)

	logPath := workingAWS(t)
	stub := newTLSUpstream(t, handle)

	return stub, anthropicClient(t, stub), logPath
}

// bedrockConfig is a pipeline serving one sigv4 alias against stub.
func bedrockConfig(t *testing.T, client *http.Client, rec proxy.Recorder) proxy.Config {
	t.Helper()

	return proxy.Config{
		Registry: newRegistry(t, sigv4Route(bedrockModel)),
		Recorder: rec,
		Client:   client,
		Logger:   nil,
		Host:     testHost,
	}
}

// bedrockFrame encodes one event-stream message the way Bedrock does.
func bedrockFrame(headers map[string]string, payload []byte) []byte {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}

	sort.Strings(names)

	var block bytes.Buffer

	for _, name := range names {
		block.WriteByte(byte(len(name)))
		block.WriteString(name)
		block.WriteByte(stringHeaderType)
		binary.Write(&block, binary.BigEndian, uint16(len(headers[name])))
		block.WriteString(headers[name])
	}

	var prelude bytes.Buffer

	binary.Write(&prelude, binary.BigEndian, uint32(block.Len()+len(payload)+frameOverheadSize))
	binary.Write(&prelude, binary.BigEndian, uint32(block.Len()))

	var message bytes.Buffer

	message.Write(prelude.Bytes())
	binary.Write(&message, binary.BigEndian, crc32.ChecksumIEEE(prelude.Bytes()))
	message.Write(block.Bytes())
	message.Write(payload)
	binary.Write(&message, binary.BigEndian, crc32.ChecksumIEEE(message.Bytes()))

	return message.Bytes()
}

// bedrockEvent wraps one Anthropic stream event as Bedrock delivers it:
// base64 inside a {"bytes":...} envelope inside an event frame.
func bedrockEvent(eventType, event string) []byte {
	payload, err := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(event))})
	if err != nil {
		panic(err)
	}

	return bedrockFrame(map[string]string{
		":event-type":   eventType,
		":message-type": "event",
		":content-type": "application/json",
	}, payload)
}

// bedrockStreamHandler answers with an event stream built from frames.
func bedrockStreamHandler(t *testing.T, frames ...[]byte) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", eventStreamType)
		w.WriteHeader(http.StatusOK)

		flusher, isFlusher := w.(http.Flusher)
		if !isFlusher {
			t.Error("stub response writer is not an http.Flusher")

			return
		}

		for _, frame := range frames {
			w.Write(frame)
			flusher.Flush()
		}
	}
}

// errorType reads the error type out of an Anthropic error body.
func errorType(t *testing.T, body []byte) string {
	t.Helper()

	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}

	return envelope.Error.Type
}

// errorMessage reads the message out of an Anthropic error body.
func errorMessage(t *testing.T, body []byte) string {
	t.Helper()

	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}

	return envelope.Error.Message
}

// assertNoCredentialMaterial fails if any AWS secret reached the record.
func assertNoCredentialMaterial(t *testing.T, rec ledger.Record) {
	t.Helper()

	encoded, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("encode record: %v", err)
	}

	for _, secret := range []string{fakeKeyID, fakeSecret, fakeSession, "AWS4-HMAC-SHA256", "Signature="} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Errorf("ledger record carries %q: %s", secret, encoded)
		}
	}
}

func TestSigV4RequestIsTranslatedAndSigned(t *testing.T) {
	stub, client, logPath := bedrockSetup(t, jsonHandler(http.StatusOK,
		`{"id":"msg_1","content":[{"type":"text"}],"usage":{"input_tokens":3,"output_tokens":4},`+
			`"stop_reason":"end_turn"}`))

	recorder := newRecorder()
	body := `{"model":"bedrock/sonnet","stream":false,"max_tokens":5,` +
		`"messages":[{"role":"user","content":"hi"}],"future_field":{"nested":[1,2]}}`

	answer := serve(t, bedrockConfig(t, client, recorder), messagesRequest(body))

	if answer.Code != http.StatusOK {
		t.Fatalf("status = %d (%s); want 200", answer.Code, answer.Body.String())
	}

	call := stub.call(t)

	if call.method != http.MethodPost {
		t.Errorf("method = %q; want POST", call.method)
	}

	wantPath := "/model/arn%3Aaws%3Abedrock%3Aus-east-2%3A123456789012%3A" +
		"application-inference-profile%2Fabcdef123456/invoke"
	if call.escapedPath != wantPath {
		t.Errorf("path = %q; want %q", call.escapedPath, wantPath)
	}

	authorization := call.header.Get("Authorization")
	if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 Credential="+fakeKeyID+"/") {
		t.Errorf("Authorization = %q; want a SigV4 signature from the exported key", authorization)
	}

	if got := call.header.Get("X-Amz-Security-Token"); got != fakeSession {
		t.Errorf("X-Amz-Security-Token = %q; want the exported session token", got)
	}

	if got := call.header.Get("X-Amz-Date"); got == "" {
		t.Error("X-Amz-Date is missing from the signed request")
	}

	for _, name := range []string{"X-Api-Key", "X-Patchbay-Key"} {
		if got := call.header.Get(name); got != "" {
			t.Errorf("%s = %q; want it stripped before signing", name, got)
		}
	}

	var sent map[string]any
	if err := json.Unmarshal(call.body, &sent); err != nil {
		t.Fatalf("decode translated body %q: %v", call.body, err)
	}

	if _, present := sent["model"]; present {
		t.Errorf("translated body still carries model: %s", call.body)
	}

	if _, present := sent["stream"]; present {
		t.Errorf("translated body still carries stream: %s", call.body)
	}

	if got := sent["anthropic_version"]; got != "bedrock-2023-05-31" {
		t.Errorf("anthropic_version = %v; want %q", got, "bedrock-2023-05-31")
	}

	if got := sent["max_tokens"]; got != float64(5) {
		t.Errorf("max_tokens = %v; want it passed through", got)
	}

	if _, present := sent["future_field"]; !present {
		t.Errorf("translated body dropped an unknown field: %s", call.body)
	}

	if got := execCount(t, logPath); got != 1 {
		t.Errorf("aws ran %d times; want 1", got)
	}

	rec := recorder.wait(t)

	if rec.AuthMode != "sigv4" {
		t.Errorf("record auth_mode = %q; want %q", rec.AuthMode, "sigv4")
	}

	if rec.Upstream != bedrockEndpoint {
		t.Errorf("record upstream = %q; want %q", rec.Upstream, bedrockEndpoint)
	}

	if rec.Model != bedrockModel {
		t.Errorf("record model = %q; want %q", rec.Model, bedrockModel)
	}

	if rec.RequestModel != bedrockAlias {
		t.Errorf("record request_model = %q; want %q", rec.RequestModel, bedrockAlias)
	}

	if rec.Context != registry.PersonalContext {
		t.Errorf("record context = %q; want %q", rec.Context, registry.PersonalContext)
	}

	if rec.InputTokens != 3 || rec.OutputTokens != 4 {
		t.Errorf("record tokens = %d/%d; want 3/4", rec.InputTokens, rec.OutputTokens)
	}

	if err := rec.Validate(); err != nil {
		t.Errorf("record does not validate: %v", err)
	}

	assertNoCredentialMaterial(t, rec)
}

// A sigv4 default route maps the model the caller asked for, since no alias
// named one for it.
func TestSigV4DefaultRouteMapsTheInboundModel(t *testing.T) {
	writeCallerKeyFile(t)
	writeModelMap(t, fmt.Sprintf(`{"claude-opus-5":%q}`, bedrockARN))
	workingAWS(t)

	stub := newTLSUpstream(t, jsonHandler(http.StatusOK, `{"id":"msg_1","content":[]}`))
	recorder := newRecorder()

	cfg := proxy.Config{
		Registry: newContextRegistry(t, sigv4DefaultContext("work")),
		Recorder: recorder,
		Client:   anthropicClient(t, stub),
		Logger:   nil,
		Host:     testHost,
	}

	req := inContext(messagesRequest(`{"model":"claude-opus-5","max_tokens":4}`), "work")

	if answer := serve(t, cfg, req); answer.Code != http.StatusOK {
		t.Fatalf("status = %d (%s); want 200", answer.Code, answer.Body.String())
	}

	call := stub.call(t)
	if !strings.Contains(call.escapedPath, "application-inference-profile%2Fabcdef123456") {
		t.Errorf("path = %q; want the mapped ARN", call.escapedPath)
	}

	rec := recorder.wait(t)
	if rec.Model != "claude-opus-5" || rec.Context != "work" {
		t.Errorf("record = %q in %q; want the inbound model in the work context", rec.Model, rec.Context)
	}
}

func TestSigV4RefusesAnUnmappedModel(t *testing.T) {
	writeCallerKeyFile(t)
	writeModelMap(t, `{"some-other-model":"arn:aws:bedrock:us-east-2:1:foundation-model/x"}`)

	logPath := workingAWS(t)
	stub := newTLSUpstream(t, jsonHandler(http.StatusOK, `{}`))
	recorder := newRecorder()

	answer := serve(t, bedrockConfig(t, anthropicClient(t, stub), recorder),
		messagesRequest(`{"model":"bedrock/sonnet","max_tokens":4}`))

	if answer.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (%s); want 400", answer.Code, answer.Body.String())
	}

	if got := errorType(t, answer.Body.Bytes()); got != "invalid_request_error" {
		t.Errorf("error type = %q; want %q", got, "invalid_request_error")
	}

	if got := errorMessage(t, answer.Body.Bytes()); !strings.Contains(got, bedrockModel) {
		t.Errorf("error message = %q; want it to name %q", got, bedrockModel)
	}

	if stub.hits() != 0 {
		t.Errorf("upstream received %d requests; want none", stub.hits())
	}

	if got := execCount(t, logPath); got != 0 {
		t.Errorf("aws ran %d times; want none for a request that never leaves", got)
	}

	assertNoCredentialMaterial(t, recorder.wait(t))
}

func TestSigV4RequiresTheCallerKey(t *testing.T) {
	stub, client, logPath := bedrockSetup(t, jsonHandler(http.StatusOK, `{}`))
	recorder := newRecorder()

	req := messagesRequest(`{"model":"bedrock/sonnet","max_tokens":4}`)
	req.Header.Del("X-Patchbay-Key")

	answer := serve(t, bedrockConfig(t, client, recorder), req)

	if answer.Code != http.StatusForbidden {
		t.Fatalf("status = %d (%s); want 403", answer.Code, answer.Body.String())
	}

	if stub.hits() != 0 {
		t.Errorf("upstream received %d requests; want none", stub.hits())
	}

	if got := execCount(t, logPath); got != 0 {
		t.Errorf("aws ran %d times; want none — an unauthenticated caller must not spend AWS credentials", got)
	}
}

func TestSigV4RefusesNonMessagesPaths(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		status int
		want   string
	}{
		{
			name:   "count_tokens is not implemented",
			path:   "/v1/messages/count_tokens",
			status: http.StatusNotImplemented,
			want:   "patchbay: count_tokens is not supported for Bedrock routes",
		},
		{
			name:   "another endpoint is not found",
			path:   "/v1/complete",
			status: http.StatusNotFound,
			want:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub, client, _ := bedrockSetup(t, jsonHandler(http.StatusOK, `{}`))
			recorder := newRecorder()

			req := httptest.NewRequest(http.MethodPost, tc.path,
				strings.NewReader(`{"model":"bedrock/sonnet","max_tokens":4}`))
			req.Header.Set("Content-Type", jsonType)
			req.Header.Set(callerKeyHeader, testCallerKey)

			answer := serve(t, bedrockConfig(t, client, recorder), req)

			if answer.Code != tc.status {
				t.Fatalf("status = %d (%s); want %d", answer.Code, answer.Body.String(), tc.status)
			}

			if tc.want != "" {
				if got := errorMessage(t, answer.Body.Bytes()); got != tc.want {
					t.Errorf("error message = %q; want %q", got, tc.want)
				}
			}

			if stub.hits() != 0 {
				t.Errorf("upstream received %d requests; want none", stub.hits())
			}

			assertNoCredentialMaterial(t, recorder.wait(t))
		})
	}
}

func TestSigV4TranslatesAStreamToSSE(t *testing.T) {
	frames := [][]byte{
		bedrockEvent("message_start",
			`{"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":0}}}`),
		bedrockEvent("content_block_start",
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		bedrockEvent("content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
		bedrockEvent("message_delta",
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`),
		bedrockEvent("message_stop", `{"type":"message_stop"}`),
	}

	stub, client, _ := bedrockSetup(t, bedrockStreamHandler(t, frames...))
	recorder := newRecorder()

	answer := serve(t, bedrockConfig(t, client, recorder),
		messagesRequest(`{"model":"bedrock/sonnet","stream":true,"max_tokens":4}`))

	if answer.Code != http.StatusOK {
		t.Fatalf("status = %d (%s); want 200", answer.Code, answer.Body.String())
	}

	if got := answer.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Errorf("Content-Type = %q; want an SSE stream", got)
	}

	if got := stub.call(t).escapedPath; !strings.HasSuffix(got, "/invoke-with-response-stream") {
		t.Errorf("path = %q; want the streaming endpoint", got)
	}

	stream := answer.Body.String()

	for _, want := range []string{
		"event: message_start\ndata: {\"type\":\"message_start\"",
		"event: content_block_delta\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream is missing %q:\n%s", want, stream)
		}
	}

	rec := recorder.wait(t)

	if rec.InputTokens != 11 || rec.OutputTokens != 7 {
		t.Errorf("record tokens = %d/%d; want 11/7", rec.InputTokens, rec.OutputTokens)
	}

	if rec.StopReason != "end_turn" {
		t.Errorf("record stop_reason = %q; want %q", rec.StopReason, "end_turn")
	}

	if len(rec.BlockShape) != 1 || rec.BlockShape[0] != "text" {
		t.Errorf("record block_shape = %v; want [text]", rec.BlockShape)
	}

	assertNoCredentialMaterial(t, rec)
}

// A stream that fails midway — Bedrock's exception frame — reaches the caller
// as an SSE error event, not as a stream that simply stops.
func TestSigV4TranslatesAStreamException(t *testing.T) {
	frames := [][]byte{
		bedrockEvent("message_start", `{"type":"message_start","message":{}}`),
		bedrockFrame(map[string]string{
			":exception-type": "modelStreamErrorException",
			":message-type":   "exception",
			":content-type":   "application/json",
		}, []byte(`{"message":"the model gave up"}`)),
	}

	stub, client, _ := bedrockSetup(t, bedrockStreamHandler(t, frames...))
	recorder := newRecorder()

	answer := serve(t, bedrockConfig(t, client, recorder),
		messagesRequest(`{"model":"bedrock/sonnet","stream":true,"max_tokens":4}`))

	stream := answer.Body.String()

	if !strings.Contains(stream, "event: error\n") {
		t.Errorf("stream carries no error event:\n%s", stream)
	}

	if !strings.Contains(stream, "modelStreamErrorException") {
		t.Errorf("error event does not name the exception:\n%s", stream)
	}

	if stub.hits() != 1 {
		t.Errorf("upstream received %d requests; want 1", stub.hits())
	}

	assertNoCredentialMaterial(t, recorder.wait(t))
}

// A corrupted frame ends the stream rather than being decoded on faith. The
// events that arrived intact still reached the caller.
func TestSigV4EndsACorruptedStream(t *testing.T) {
	sound := bedrockEvent("message_start", `{"type":"message_start","message":{}}`)
	broken := bytes.Clone(bedrockEvent("content_block_delta", `{"type":"content_block_delta"}`))
	broken[len(broken)-1] ^= 0xFF

	stub, client, _ := bedrockSetup(t, bedrockStreamHandler(t, sound, broken))

	sink := newLogSink()
	cfg := bedrockConfig(t, client, newRecorder())
	cfg.Logger = slog.New(sink)

	answer := serve(t, cfg, messagesRequest(`{"model":"bedrock/sonnet","stream":true,"max_tokens":4}`))

	stream := answer.Body.String()

	if !strings.Contains(stream, "event: message_start\n") {
		t.Errorf("the intact event never reached the caller:\n%s", stream)
	}

	if strings.Contains(stream, "content_block_delta") {
		t.Errorf("a corrupted frame was decoded anyway:\n%s", stream)
	}

	if stub.hits() != 1 {
		t.Errorf("upstream received %d requests; want 1", stub.hits())
	}
}

// A rejected signature is retried exactly once, with freshly exported
// credentials — an expired session is the ordinary case, and re-exporting is
// what fixes it.
func TestSigV4RetriesOnceOnRejection(t *testing.T) {
	// The stub serves each request on its own goroutine, so the attempt counter
	// is atomic rather than merely sequential.
	var attempts atomic.Int64

	stub, client, logPath := bedrockSetup(t, func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Type", jsonType)
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message":"The security token included in the request is expired"}`)

			return
		}

		w.Header().Set("Content-Type", jsonType)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"msg_2","content":[]}`)
	})

	recorder := newRecorder()

	answer := serve(t, bedrockConfig(t, client, recorder),
		messagesRequest(`{"model":"bedrock/sonnet","max_tokens":4}`))

	if answer.Code != http.StatusOK {
		t.Fatalf("status = %d (%s); want the retry to succeed with 200", answer.Code, answer.Body.String())
	}

	if stub.hits() != 2 {
		t.Errorf("upstream received %d requests; want 2", stub.hits())
	}

	if got := execCount(t, logPath); got != 2 {
		t.Errorf("aws ran %d times; want 2 — the retry must sign with fresh credentials", got)
	}

	assertNoCredentialMaterial(t, recorder.wait(t))
}

// A signature AWS refuses twice is patchbay's failure to report, not a 403 to
// hand a caller that has no AWS credentials of its own to fix.
func TestSigV4ReportsAPersistentRejection(t *testing.T) {
	stub, client, logPath := bedrockSetup(t, jsonHandler(http.StatusForbidden,
		`{"message":"The security token included in the request is expired"}`))

	recorder := newRecorder()

	answer := serve(t, bedrockConfig(t, client, recorder),
		messagesRequest(`{"model":"bedrock/sonnet","max_tokens":4}`))

	if answer.Code != http.StatusBadGateway {
		t.Fatalf("status = %d (%s); want 502", answer.Code, answer.Body.String())
	}

	if stub.hits() != 2 {
		t.Errorf("upstream received %d requests; want 2", stub.hits())
	}

	if got := execCount(t, logPath); got != 2 {
		t.Errorf("aws ran %d times; want 2", got)
	}

	assertNoCredentialMaterial(t, recorder.wait(t))
}

// A profile whose credentials cannot be exported is a local problem with a
// local fix, and the error says so without quoting the CLI.
func TestSigV4ReportsACredentialFailure(t *testing.T) {
	const stderrSecret = "ASIA-LEAKED-KEY-MATERIAL"

	writeCallerKeyFile(t)
	defaultModelMap(t)
	fakeAWS(t, "echo "+stderrSecret+" >&2\nexit 1\n")

	stub := newTLSUpstream(t, jsonHandler(http.StatusOK, `{}`))
	recorder := newRecorder()

	answer := serve(t, bedrockConfig(t, anthropicClient(t, stub), recorder),
		messagesRequest(`{"model":"bedrock/sonnet","max_tokens":4}`))

	if answer.Code != http.StatusBadGateway {
		t.Fatalf("status = %d (%s); want 502", answer.Code, answer.Body.String())
	}

	message := errorMessage(t, answer.Body.Bytes())

	if !strings.Contains(message, "aws sso login --profile "+awsProfile) {
		t.Errorf("error message = %q; want it to name the login command", message)
	}

	if strings.Contains(message, stderrSecret) {
		t.Errorf("error message = %q; want no output from the aws process", message)
	}

	if stub.hits() != 0 {
		t.Errorf("upstream received %d requests; want none", stub.hits())
	}

	assertNoCredentialMaterial(t, recorder.wait(t))
}

// A model map that cannot be read is a server-side misconfiguration, refused
// before anything is signed or sent.
func TestSigV4ReportsAnUnreadableModelMap(t *testing.T) {
	writeCallerKeyFile(t)
	t.Setenv(mapEnvName, filepath.Join(t.TempDir(), "missing.json"))
	workingAWS(t)

	stub := newTLSUpstream(t, jsonHandler(http.StatusOK, `{}`))
	recorder := newRecorder()

	answer := serve(t, bedrockConfig(t, anthropicClient(t, stub), recorder),
		messagesRequest(`{"model":"bedrock/sonnet","max_tokens":4}`))

	if answer.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d (%s); want 500", answer.Code, answer.Body.String())
	}

	if stub.hits() != 0 {
		t.Errorf("upstream received %d requests; want none", stub.hits())
	}
}

// An event name is written into the SSE framing itself, so a name carrying CR
// or LF could forge events the model never sent. A hostile name is refused
// rather than rendered: the caller gets patchbay's own error event and the
// stream ends there.
func TestSigV4RefusesAHostileEventName(t *testing.T) {
	tests := []struct {
		name    string
		frame   []byte
		forbids string
	}{
		{
			name: "event type carrying a blank line",
			frame: bedrockEvent("message_start\n\nevent: message_stop\ndata: {}",
				`{"message":"harmless"}`),
			forbids: "\nevent: message_stop",
		},
		{
			name: "event type carrying a carriage return",
			frame: bedrockEvent("message_start\rdata: forged",
				`{"message":"harmless"}`),
			forbids: "data: forged",
		},
		{
			name: "exception type carrying a carriage return",
			frame: bedrockFrame(map[string]string{
				":exception-type": "modelStreamError\r\n\r\nevent: message_stop",
				":message-type":   "exception",
				":content-type":   "application/json",
			}, []byte(`{"message":"the model gave up"}`)),
			forbids: "\nevent: message_stop",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub, client, _ := bedrockSetup(t, bedrockStreamHandler(t, tc.frame))
			recorder := newRecorder()

			answer := serve(t, bedrockConfig(t, client, recorder),
				messagesRequest(`{"model":"bedrock/sonnet","stream":true,"max_tokens":4}`))

			stream := answer.Body.String()

			for _, line := range strings.Split(stream, "\n") {
				if strings.HasPrefix(line, "event: ") && strings.ContainsAny(line, "\r") {
					t.Fatalf("an event name carries a control byte:\n%q", stream)
				}
			}

			if strings.Contains(stream, tc.forbids) {
				t.Fatalf("a forged event reached the caller:\n%q", stream)
			}

			if stub.hits() != 1 {
				t.Errorf("upstream received %d requests; want 1", stub.hits())
			}

			assertNoCredentialMaterial(t, recorder.wait(t))
		})
	}
}
