package server

import (
	"net/http"

	"github.com/joshsymonds/patchbay/internal/anthropic"
)

// jsonContentType is what every answer this package writes itself carries.
const jsonContentType = "application/json"

// callerHeaders are the inbound headers patchbay repeats when it asks Anthropic
// on the caller's behalf: the credentials that pay for the answer and the API
// version and beta flags that shape it. Nothing else about the inbound request
// belongs on a request patchbay makes.
func callerHeaders() []string {
	return []string{
		anthropic.HeaderAuthorization,
		anthropic.HeaderClientKey,
		anthropic.HeaderVersion,
		anthropic.HeaderBeta,
	}
}

// copyCallerAuth copies the caller's credentials and version headers onto an
// outbound request, preserving every value of a repeated header in order.
func copyCallerAuth(dst, src http.Header) {
	for _, name := range callerHeaders() {
		for _, value := range src.Values(name) {
			dst.Add(name, value)
		}
	}
}
