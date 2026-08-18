package proxy_test

import (
	"net/http"
	"testing"

	"github.com/joshsymonds/patchbay/internal/proxy"
)

func TestNullBodyRidesTheDefaultRouteUntouched(t *testing.T) {
	t.Parallel()

	// A body of JSON null names no model, so it rides the built-in default
	// route, which rewrites nothing: the bytes reach Anthropic as they arrived.
	// That is what keeps a null body away from the rewrite path, where a nil
	// document would otherwise be written into.
	stub := newTLSUpstream(t, jsonHandler(http.StatusBadRequest, `{"type":"error"}`))

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identityRoute(stub.url())),
		Client:   anthropicClient(t, stub),
		Host:     testHost,
	}, messagesRequest("null"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want the upstream's own %d", rec.Code, http.StatusBadRequest)
	}

	call := stub.call(t)
	if string(call.body) != "null" {
		t.Fatalf("upstream body = %q; want the caller's own %q", call.body, "null")
	}
}
