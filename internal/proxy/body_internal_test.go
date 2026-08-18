package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/registry"
)

// TestForwardBodyRefusesJSONNull covers forwardBody's nil-map guard, which no
// request can reach through ServeHTTP: an empty alias is rejected at parse, so
// a body of JSON null always resolves to the identity default route and is
// forwarded as the bytes it arrived as. The guard exists for the day some route
// does rewrite a null body, and only an in-package call can hold it to that.
func TestForwardBodyRefusesJSONNull(t *testing.T) {
	t.Parallel()

	body, gwErr := forwardBody([]byte("null"), registry.Route{Alias: "x", Model: "y"})
	if gwErr == nil {
		t.Fatal("forwardBody(null) error = nil; want the body refused rather than written into")
	}

	if body != nil {
		t.Fatalf("forwardBody(null) body = %q; want nothing to forward", body)
	}

	if gwErr.status != http.StatusBadRequest {
		t.Fatalf("status = %d; want %d — a null body is the caller's mistake", gwErr.status, http.StatusBadRequest)
	}

	if !strings.Contains(gwErr.Error(), "JSON null rather than an object") {
		t.Fatalf("message = %q; want it to say the body is JSON null rather than an object", gwErr.Error())
	}
}
