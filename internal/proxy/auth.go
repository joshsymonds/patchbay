package proxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// callerKeyHeader is patchbay's own authentication header: the shared secret a
// caller must present before an inject route will spend the household key.
// Because it is patchbay's and never the upstream's, it is stripped from every
// upstream request (see upstreamRequest's copyHeader call).
const callerKeyHeader = "X-Patchbay-Key"

// callerKeyEnvFile names the environment variable whose value is the path of
// the file holding the caller key — the same double indirection the injected
// upstream key uses, so the secret is never baked into the config.
const callerKeyEnvFile = "PATCHBAY_CALLER_KEY_FILE"

// applyAuth sets the credentials the route pays with.
//
// A forward route leaves the caller's own Authorization and x-api-key headers
// exactly as they arrived — that is the whole point of the default route, where
// the caller's subscription pays. An inject route deletes both first: the
// caller's OAuth token must never reach a foreign upstream, not even alongside
// the injected key.
func applyAuth(req *http.Request, route registry.Route) *gatewayError {
	if route.Auth != registry.AuthInject {
		return nil
	}

	req.Header.Del(anthropic.HeaderAuthorization)
	req.Header.Del(anthropic.HeaderClientKey)

	key, err := readAPIKey(route.APIKeyEnvFile)
	if err != nil {
		return &gatewayError{status: http.StatusInternalServerError, err: err}
	}

	req.Header.Set(anthropic.HeaderAuthorization, "Bearer "+key)

	return nil
}

// checkCallerKey refuses an inject request that does not present the shared
// caller secret, so a local process that lacks it cannot spend the household
// key an inject route injects. It reads the inbound request — before the header
// is stripped from the upstream copy — and fails closed: any problem reading
// the expected key refuses the request rather than injecting the key for a
// caller that proved nothing.
func checkCallerKey(r *http.Request) *gatewayError {
	expected, err := readAPIKey(callerKeyEnvFile)
	if err != nil {
		// A server-side key-file problem, the same class the injected upstream
		// key returns 500 for. Never echo any header value.
		return &gatewayError{
			status: http.StatusInternalServerError,
			err:    fmt.Errorf("caller key unavailable: %w", err),
		}
	}

	presented := strings.TrimSpace(r.Header.Get(callerKeyHeader))

	// Hash both sides first: SHA-256 gives two equal-length digests, so the
	// constant-time compare never leaks the key length through an early return,
	// the way comparing the raw values would.
	presentedSum := sha256.Sum256([]byte(presented))
	expectedSum := sha256.Sum256([]byte(expected))

	if subtle.ConstantTimeCompare(presentedSum[:], expectedSum[:]) != 1 {
		// Never include the presented or expected value: this message is
		// persisted to the ledger as errorBody and shipped to the NAS. 403, not
		// 401, so Claude Code does not treat it as a re-login prompt.
		return &gatewayError{
			status: http.StatusForbidden,
			err:    errors.New("caller key missing or mismatched"),
		}
	}

	return nil
}

// readAPIKey resolves the double indirection an inject route uses: envName is
// the name of an environment variable whose value is the path of a file holding
// the key. The file is read per request, so rotating the key on disk takes
// effect immediately, and nothing keeps the key in memory afterwards.
func readAPIKey(envName string) (string, error) {
	path := os.Getenv(envName)
	if path == "" {
		return "", fmt.Errorf("environment variable %s is not set", envName)
	}

	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("read key file named by %s: %w", envName, err)
	}

	key := strings.TrimSpace(string(contents))
	if key == "" {
		return "", fmt.Errorf("key file named by %s is empty", envName)
	}

	return key, nil
}
