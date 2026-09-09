package proxy_test

import (
	"bytes"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/proxy"
)

const rewriteSelector = "local/rewrite"

// rewriteBody is a request carrying everything a rewrite Seat has to touch: a
// model to replace, a max_tokens above the Seat's ceiling, a value extra_body
// overrides, cache_control markers nested two levels deep, and a float whose
// exact decimal expansion must survive re-encoding.
const rewriteBody = `{"model":"local/rewrite","max_tokens":9000,"temperature":0.5,` +
	`"top_p":0.30000000000000004,` +
	`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"hi",` +
	`"cache_control":{"type":"ephemeral"}}]}]}`

// rewriteSeat returns a Seat that rewrites the model, merges an extra body,
// strips cache_control and caps output tokens.
func rewriteSeat(baseURL string) string {
	return fmt.Sprintf(`%q:{"upstream":%q,"model":"vendor/model-1","auth_mode":"inject",`+
		`"api_key_env_file":%q,"billing":"metered",`+
		`"extra_body":{"reasoning_effort":"low","temperature":1},`+
		`"strip_cache_control":true,"max_output_tokens":4096}`,
		rewriteSelector, baseURL, keyEnvName)
}

// TestRewriteSeatTransformsRequest checks the whole rewrite: model replaced,
// extra_body merged over the caller's values, cache_control removed everywhere,
// max_tokens clamped, and every untouched value preserved exactly.
func TestRewriteSeatTransformsRequest(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, rewriteSeat(stub.url())),
		Host:     testHost,
	}, messagesRequest(rewriteBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	call := stub.call(t)

	want := decodeJSON(t, []byte(`{"model":"vendor/model-1","max_tokens":4096,"temperature":1,`+
		`"top_p":0.30000000000000004,"reasoning_effort":"low",`+
		`"system":[{"type":"text","text":"sys"}],`+
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))

	if got := decodeJSON(t, call.body); !reflect.DeepEqual(got, want) {
		t.Errorf("forwarded body = %s\nwant %v", call.body, want)
	}

	if bytes.Contains(call.body, []byte("cache_control")) {
		t.Errorf("forwarded body still carries cache_control: %s", call.body)
	}

	if !bytes.Contains(call.body, []byte("0.30000000000000004")) {
		t.Errorf("forwarded body lost the exact float expansion: %s", call.body)
	}
}

// TestRewriteClampsOnlyAboveTheCeiling leaves a request already under the
// Seat's output cap alone.
func TestRewriteClampsOnlyAboveTheCeiling(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, rewriteSeat(stub.url())),
		Host:     testHost,
	}, messagesRequest(`{"model":"local/rewrite","max_tokens":100}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	call := stub.call(t)

	want := decodeJSON(t, []byte(`{"model":"vendor/model-1","max_tokens":100,`+
		`"temperature":1,"reasoning_effort":"low"}`))

	if got := decodeJSON(t, call.body); !reflect.DeepEqual(got, want) {
		t.Errorf("forwarded body = %s\nwant %v", call.body, want)
	}
}

// TestRewriteClampsNonIntegerMaxTokens holds the ceiling against a max_tokens
// that is not written as a plain integer: JSON has one number type, so 1e5 is
// the same request as 100000 and gets clamped the same way.
func TestRewriteClampsNonIntegerMaxTokens(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))
	seat := fmt.Sprintf(`%q:{"upstream":%q,"model":"vendor/model-1","auth_mode":"inject",`+
		`"api_key_env_file":%q,"billing":"metered","max_output_tokens":100}`,
		rewriteSelector, stub.url(), keyEnvName)

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, seat),
		Host:     testHost,
	}, messagesRequest(`{"model":"local/rewrite","max_tokens":1e5}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	call := stub.call(t)

	if !bytes.Contains(call.body, []byte(`"max_tokens":100,`)) {
		t.Errorf("forwarded body = %s, want max_tokens clamped to exactly 100", call.body)
	}

	want := decodeJSON(t, []byte(`{"model":"vendor/model-1","max_tokens":100}`))

	if got := decodeJSON(t, call.body); !reflect.DeepEqual(got, want) {
		t.Errorf("forwarded body = %s\nwant %v", call.body, want)
	}
}

// TestRewriteLeavesTheConfiguredExtraBodyIntact holds a merge to the request it
// belongs to. Every resolution is an isolated snapshot, so the extra_body the
// pipeline merges the caller's body over is that request's own copy — and the
// registry still has to answer the next request with the extra_body its file
// configured, whatever the last one did to the copy it was handed.
func TestRewriteLeavesTheConfiguredExtraBodyIntact(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))
	reg := newRegistry(t, rewriteSeat(stub.url()))

	configured := resolveSeat(t, reg, rewriteSelector)
	if len(configured.ExtraBody) == 0 {
		t.Fatal("Seat has no extra_body to protect")
	}

	rec := serve(t, proxy.Config{Registry: reg, Host: testHost}, messagesRequest(rewriteBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	after := resolveSeat(t, reg, rewriteSelector)

	if !bytes.Equal(after.ExtraBody, configured.ExtraBody) {
		t.Errorf("extra_body changed across a merging request:\n got %s\nwant %s",
			after.ExtraBody, configured.ExtraBody)
	}
}

// TestStripOnlySeatKeepsEverythingElse covers the narrowest rewrite there is:
// a Seat that only strips cache_control still forwards the caller's model and
// every other value untouched.
func TestStripOnlySeatKeepsEverythingElse(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))
	seat := fmt.Sprintf(`%q:{"upstream":%q,"auth_mode":"inject","api_key_env_file":%q,`+
		`"billing":"metered","strip_cache_control":true}`, echoSelector, stub.url(), keyEnvName)

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, seat),
		Host:     testHost,
	}, messagesRequest(fidelityBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	call := stub.call(t)

	want := decodeJSON(t, []byte(strings.ReplaceAll(fidelityBody,
		`,"cache_control":{"type":"ephemeral"}`, "")))

	if got := decodeJSON(t, call.body); !reflect.DeepEqual(got, want) {
		t.Errorf("forwarded body = %s\nwant %v", call.body, want)
	}
}

// TestSpeedSeatSetsFastOverTheCaller checks that a speed Seat writes its tier
// into the request, replacing the caller's own value, and touches nothing else.
func TestSpeedSeatSetsFastOverTheCaller(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))
	seat := fmt.Sprintf(`%q:{"upstream":%q,"auth_mode":"inject","api_key_env_file":%q,`+
		`"billing":"subscription","speed":"fast"}`, echoSelector, stub.url(), keyEnvName)
	body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"speed":"standard",`+
		`"messages":[{"role":"user","content":"hi"}]}`, echoSelector)

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, seat),
		Host:     testHost,
	}, messagesRequest(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	call := stub.call(t)

	want := decodeJSON(t, []byte(fmt.Sprintf(`{"model":%q,"max_tokens":16,"speed":"fast",`+
		`"messages":[{"role":"user","content":"hi"}]}`, echoSelector)))

	if got := decodeJSON(t, call.body); !reflect.DeepEqual(got, want) {
		t.Errorf("forwarded body = %s\nwant %v", call.body, want)
	}
}
