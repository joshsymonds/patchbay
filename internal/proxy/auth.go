package proxy

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/registry"
)

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
