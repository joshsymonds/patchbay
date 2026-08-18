package registry

import "encoding/json"

// AuthMode selects how patchbay authenticates a proxied request upstream.
type AuthMode string

const (
	// AuthForward passes the caller's own credentials upstream untouched. Since
	// that hands over the caller's subscription bearer token, it is only ever
	// valid against Anthropic itself.
	AuthForward AuthMode = "forward"

	// AuthInject drops the caller's credentials and substitutes a key read from
	// the file named by the route's APIKeyEnvFile.
	AuthInject AuthMode = "inject"
)

// anthropicBaseURL is the only upstream a forward route may target, and the
// base URL of the built-in default route.
const anthropicBaseURL = "https://api.anthropic.com"

// Route is the resolved destination for one request: where it goes, what model
// it asks for, and how it pays.
type Route struct {
	// Alias is the registry key this route was found under. It is empty for the
	// built-in default route.
	Alias string

	// BaseURL is the upstream API root.
	BaseURL string

	// Model is the rewrite target. Empty means identity: forward the model the
	// caller asked for.
	Model string

	// Auth selects forwarded or injected credentials.
	Auth AuthMode

	// APIKeyEnvFile is the NAME of an environment variable whose VALUE is the
	// path of a file holding the API key — never the key itself. Inject routes
	// only.
	APIKeyEnvFile string

	// ExtraBody is merged into the request JSON at forward time. It is nil when
	// the route sets no extra body. Treat it as read-only: every Resolve of the
	// same alias shares these bytes.
	ExtraBody json.RawMessage

	// MaxInputTokens caps the request's input tokens. Zero means no cap.
	MaxInputTokens int

	// MaxOutputTokens caps the request's output tokens. Zero means no cap.
	MaxOutputTokens int

	// StripCacheControl removes cache_control markers before forwarding.
	StripCacheControl bool

	// CaptureBodies records request and response bodies for this route.
	CaptureBodies bool
}

// DefaultRoute returns the built-in route every unrecognized model rides: a
// plain forward to Anthropic with no rewrite and no capture. It is a fixed
// value that is never read from the registry file, so a broken registry can
// never break it.
func DefaultRoute() Route {
	return Route{
		BaseURL: anthropicBaseURL,
		Auth:    AuthForward,
	}
}
