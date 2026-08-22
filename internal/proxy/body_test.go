package proxy_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/proxy"
)

func TestExtraBodyPreservesSeatModelRewrite(t *testing.T) {
	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message"}`))
	writeKeyFile(t, testAPIKey)

	rec := serve(t, proxy.Config{
		Registry: newRegistryDocument(t, []byte(fmt.Sprintf(
			`{"default_context":"personal","seats":{"rewriter":{"upstream":%q,`+
				`"model":"openai/gpt-5.6-luna","auth_mode":"inject","api_key_env_file":%q,`+
				`"billing":"metered","extra_body":{"reasoning_effort":"low"}}},`+
				`"contexts":{"personal":{"default_seat":"rewriter","models":{}}}}`,
			stub.url(), keyEnvName,
		))),
		Client: stub.server.Client(),
		Host:   testHost,
	}, messagesRequest(`{"model":"claude-opus-5","messages":[]}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d\nbody=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(stub.call(t).body, &body); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	if got := string(body["model"]); got != `"openai/gpt-5.6-luna"` {
		t.Fatalf("upstream model = %s; want Seat model rewrite", got)
	}
	if got := string(body["reasoning_effort"]); got != `"low"` {
		t.Fatalf("upstream reasoning_effort = %s; want extra_body value", got)
	}
}

func TestNullBodyRidesTheDefaultSeatUntouched(t *testing.T) {
	t.Parallel()

	// A body of JSON null names no model, so it rides the Context's own default
	// Seat. On a Seat that rewrites nothing there is nothing to decode, and the
	// bytes reach Anthropic as they arrived.
	stub := newTLSUpstream(t, jsonHandler(http.StatusBadRequest, `{"type":"error"}`))

	req := messagesRequest("null")
	req.Header.Set("Authorization", "Bearer caller-oauth-token")

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identitySeat(stub.url())),
		Client:   anthropicClient(t, stub),
		Host:     testHost,
	}, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want the upstream's own %d", rec.Code, http.StatusBadRequest)
	}

	call := stub.call(t)
	if string(call.body) != "null" {
		t.Fatalf("upstream body = %q; want the caller's own %q", call.body, "null")
	}
}

// TestNullBodyOnARewritingDefaultSeatIsRefused is the other half: the same null
// body against a Context whose default_seat DOES rewrite.
//
// Nothing in the registry forbids that Context. default_seat names any global
// Seat, and an inject Seat carries a model of its own — so a null body reaches
// the rewrite path through ServeHTTP, where a nil document would be written
// into. The refusal is the caller's own 400, the upstream is never dialed, and
// the request is answered rather than panicking the process.
func TestNullBodyOnARewritingDefaultSeatIsRefused(t *testing.T) {
	message := refuseRewrittenBody(t, "null")

	if !strings.Contains(
		message,
		"the selected Seat rewrites the request, which is JSON null rather than an object",
	) {
		t.Fatalf(
			"refusal message = %q; want it to say the body is JSON null rather than an object",
			message,
		)
	}
}

// TestMalformedBodyOnARewritingDefaultSeatIsRefused is the same refusal for
// bytes that are not JSON at all.
//
// A body the model probe cannot read names no model, so it too rides its
// Context's default_seat and reaches the rewrite path — where there is no
// document to rewrite. Like the null body, it is a request refused on its own
// face rather than a gateway that failed, and the wire says so.
func TestMalformedBodyOnARewritingDefaultSeatIsRefused(t *testing.T) {
	message := refuseRewrittenBody(t, "not json at all")

	if !strings.Contains(
		message,
		"the selected Seat rewrites the request, which is not a JSON object",
	) {
		t.Fatalf(
			"refusal message = %q; want it to say the body is not a JSON object",
			message,
		)
	}
}

// refuseRewrittenBody sends body to a Context whose default_seat rewrites, and
// returns the message the caller was refused with.
//
// It pins everything the two refusals share. The status is the caller's own 400.
// The envelope is Anthropic-shaped, marked as patchbay's own, and typed
// invalid_request_error rather than api_error: a body the rewrite path cannot
// use is a request bad on its face, not a gateway that failed, and a client
// deciding whether to retry reads that type. No Seat ID is invented to say it
// with, and the upstream is never dialed.
func refuseRewrittenBody(t *testing.T, body string) string {
	t.Helper()

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message"}`))
	writeKeyFile(t, testAPIKey)

	rec := serve(t, proxy.Config{
		Registry: newRegistryDocument(t, []byte(fmt.Sprintf(
			`{"default_context":"personal","seats":{"rewriter":{"upstream":%q,`+
				`"model":"openai/gpt-5.6-luna","auth_mode":"inject","api_key_env_file":%q,`+
				`"billing":"metered"}},`+
				`"contexts":{"personal":{"default_seat":"rewriter","models":{}}}}`,
			stub.url(), keyEnvName,
		))),
		Client: stub.server.Client(),
		Logger: slog.New(newLogSink()),
		Host:   testHost,
	}, messagesRequest(body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want %d\nbody=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode refusal %q: %v", rec.Body.String(), err)
	}
	if envelope.Type != "error" || envelope.Error.Type != "invalid_request_error" {
		t.Fatalf("refusal envelope = %+v; want a request refused on its own face", envelope)
	}
	if !strings.HasPrefix(envelope.Error.Message, "patchbay: ") {
		t.Fatalf("refusal message = %q; want it marked as patchbay's own", envelope.Error.Message)
	}
	if strings.Contains(envelope.Error.Message, `"selected"`) {
		t.Fatalf("refusal message = %q; want no fabricated Seat ID quoted into it",
			envelope.Error.Message)
	}

	if hits := stub.hits(); hits != 0 {
		t.Fatalf("a refused body reached the upstream %d times", hits)
	}

	return envelope.Error.Message
}
