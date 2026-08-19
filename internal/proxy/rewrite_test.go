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

const rewriteAlias = "local/rewrite"

// rewriteBody is a request carrying everything a rewrite route has to touch: a
// model to replace, a max_tokens above the route's ceiling, a value extra_body
// overrides, cache_control markers nested two levels deep, and a float whose
// exact decimal expansion must survive re-encoding.
const rewriteBody = `{"model":"local/rewrite","max_tokens":9000,"temperature":0.5,` +
	`"top_p":0.30000000000000004,` +
	`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"hi",` +
	`"cache_control":{"type":"ephemeral"}}]}]}`

// rewriteRoute returns a route that rewrites the model, merges an extra body,
// strips cache_control and caps output tokens.
func rewriteRoute(baseURL string) string {
	return fmt.Sprintf(`%q:{"base_url":%q,"model":"vendor/model-1","auth":"inject",`+
		`"api_key_env_file":%q,"extra_body":{"reasoning_effort":"low","temperature":1},`+
		`"strip_cache_control":true,"max_output_tokens":4096}`,
		rewriteAlias, baseURL, keyEnvName)
}

// TestRewriteRouteTransformsRequest checks the whole rewrite: model replaced,
// extra_body merged over the caller's values, cache_control removed everywhere,
// max_tokens clamped, and every untouched value preserved exactly.
func TestRewriteRouteTransformsRequest(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, rewriteRoute(stub.url())),
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
// route's output cap alone.
func TestRewriteClampsOnlyAboveTheCeiling(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, rewriteRoute(stub.url())),
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
	route := fmt.Sprintf(`%q:{"base_url":%q,"model":"vendor/model-1","auth":"inject",`+
		`"api_key_env_file":%q,"max_output_tokens":100}`, rewriteAlias, stub.url(), keyEnvName)

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, route),
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

// TestRewriteLeavesSharedExtraBodyIntact protects the registry's shared bytes:
// every Resolve of an alias hands out the same backing array, so a merge that
// wrote into it would corrupt every later request.
func TestRewriteLeavesSharedExtraBodyIntact(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))
	reg := newRegistry(t, rewriteRoute(stub.url()))

	before := resolveRoute(t, reg, rewriteAlias)
	snapshot := bytes.Clone(before.ExtraBody)

	rec := serve(t, proxy.Config{Registry: reg, Host: testHost}, messagesRequest(rewriteBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	after := resolveRoute(t, reg, rewriteAlias)

	if len(after.ExtraBody) == 0 || len(before.ExtraBody) == 0 {
		t.Fatal("route has no extra_body to protect")
	}

	if &after.ExtraBody[0] != &before.ExtraBody[0] {
		t.Fatal("registry handed out fresh bytes; the test cannot observe the shared array")
	}

	if !bytes.Equal(after.ExtraBody, snapshot) {
		t.Errorf("extra_body backing array changed:\n got %s\nwant %s", after.ExtraBody, snapshot)
	}
}

// TestStripOnlyRouteKeepsEverythingElse covers the narrowest rewrite there is:
// a route that only strips cache_control still forwards the caller's model and
// every other value untouched.
func TestStripOnlyRouteKeepsEverythingElse(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	stub := newUpstream(t, jsonHandler(http.StatusOK, `{"type":"message","content":[]}`))
	route := fmt.Sprintf(`%q:{"base_url":%q,"auth":"inject","api_key_env_file":%q,`+
		`"strip_cache_control":true}`, echoAlias, stub.url(), keyEnvName)

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, route),
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
