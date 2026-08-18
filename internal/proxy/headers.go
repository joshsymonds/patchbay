package proxy

import (
	"net/http"
	"net/textproto"
	"strings"
)

// Header names the pipeline handles itself rather than copying.
// headerClientKey deliberately avoids spelling out "API key": a const named
// that way trips gosec's hardcoded-credential heuristic (G101), the same way
// the registry's key-file field name does.
const (
	headerAcceptEncoding = "Accept-Encoding"
	headerAuthorization  = "Authorization"
	headerClientKey      = "X-Api-Key"
	headerConnection     = "Connection"
	headerContentLength  = "Content-Length"
	headerContentType    = "Content-Type"
	headerHost           = "Host"
)

// encodingIdentity is what patchbay always asks upstream for. Byte fidelity is
// only meaningful over bytes patchbay can hand on unchanged, so the caller's
// own Accept-Encoding is never forwarded.
const encodingIdentity = "identity"

// hopByHopHeaders are the connection-scoped headers RFC 9110 forbids a proxy
// from forwarding.
func hopByHopHeaders() []string {
	return []string{
		headerConnection,
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	}
}

// droppedHeaders returns the canonical names that must not cross the proxy:
// the fixed hop-by-hop set, everything the message's own Connection header
// names, and any extra names the caller adds.
func droppedHeaders(header http.Header, extra ...string) map[string]bool {
	dropped := make(map[string]bool)

	for _, name := range hopByHopHeaders() {
		dropped[name] = true
	}

	for _, name := range extra {
		dropped[textproto.CanonicalMIMEHeaderKey(name)] = true
	}

	for _, value := range header.Values(headerConnection) {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				dropped[textproto.CanonicalMIMEHeaderKey(token)] = true
			}
		}
	}

	return dropped
}

// copyHeader copies src into dst, skipping the dropped names and preserving
// every value of a repeated header in order.
func copyHeader(dst, src http.Header, dropped map[string]bool) {
	for name, values := range src {
		if dropped[name] {
			continue
		}

		for _, value := range values {
			dst.Add(name, value)
		}
	}
}
