package proxy_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/joshsymonds/patchbay/internal/proxy"
)

// TestDefaultClientRelaysARedirectRatherThanFollowingIt pins the pipeline's own
// client, the one production actually runs on: Config.Client is nil here, so
// nothing about the test substitutes the redirect policy under test.
//
// Go's default policy follows up to ten hops, and crossing to another host it
// strips only Authorization, Www-Authenticate, Cookie and the Proxy-* headers —
// never x-api-key, anthropic-version or anthropic-beta. Following a redirect
// would hand the caller's key to whatever host the upstream named. It would
// also break byte fidelity: a 3xx is an answer to relay, not an instruction to
// obey.
func TestDefaultClientRelaysARedirectRatherThanFollowingIt(t *testing.T) {
	writeKeyFile(t, testAPIKey)

	const redirectBody = `{"type":"error","error":{"type":"moved","message":"elsewhere"}}`

	attacker := newUpstream(t, jsonHandler(http.StatusOK, `{"credentials":"stolen"}`))
	stub := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", attacker.url()+"/v1/messages")
		w.Header().Set("Content-Type", jsonType)
		w.WriteHeader(http.StatusFound)
		io.WriteString(w, redirectBody)
	})

	rec := serve(t, proxy.Config{
		Registry: newRegistry(t, identitySeat(stub.url())),
		Client:   nil,
		Host:     testHost,
	}, messagesRequest(`{"model":"`+echoSelector+`"}`))

	if attacker.hits() != 0 {
		t.Fatalf("the redirect target saw %d requests; want 0 — following one leaks the caller's credentials",
			attacker.hits())
	}

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d; want the upstream's own %d", rec.Code, http.StatusFound)
	}

	if got := rec.Body.String(); got != redirectBody {
		t.Fatalf("body = %q; want the upstream's bytes %q", got, redirectBody)
	}

	if got := rec.Header().Get("Location"); got != attacker.url()+"/v1/messages" {
		t.Fatalf("Location = %q; want the upstream's own header relayed", got)
	}
}
