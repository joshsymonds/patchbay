package registry

import (
	"encoding/json"

	"github.com/joshsymonds/patchbay/internal/anthropic"
)

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

	// AuthSigV4 drops the caller's credentials and signs the request with AWS
	// SigV4 for Bedrock, using credentials exported from the route's named AWS
	// profile.
	AuthSigV4 AuthMode = "sigv4"
)

// bedrockEndpointPrefix and bedrockEndpointSuffix bracket the region in a
// bedrock-runtime endpoint. A sigv4 route names no base_url: its endpoint is
// derived from the region it signs for, so the two can never disagree.
const (
	bedrockEndpointPrefix = "https://bedrock-runtime."
	bedrockEndpointSuffix = ".amazonaws.com"
)

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

	// AWSProfile is the AWS profile whose credentials sign the request. Sigv4
	// routes only.
	AWSProfile string

	// AWSRegion is the AWS region the request is signed for, and the region of
	// the Bedrock endpoint it is sent to. Sigv4 routes only.
	AWSRegion string

	// ModelMapEnvFile is the NAME of an environment variable whose VALUE is the
	// path of a JSON file mapping Anthropic model ids to Bedrock model ids —
	// never a Bedrock id itself, which is why none of them appears in the
	// registry. Sigv4 routes only.
	ModelMapEnvFile string

	// ExtraBody is merged into the request JSON at forward time. It is nil when
	// the route sets no extra body. Treat it as read-only: every Resolve of the
	// same alias shares these bytes.
	ExtraBody json.RawMessage

	// MaxInputTokens is advertised as max_input_tokens on /v1/models. The proxy
	// does not enforce it: it is a ceiling published for clients to read, not one
	// the pipeline applies to a request. Zero omits it from the listing.
	MaxInputTokens int

	// MaxOutputTokens caps the request's output tokens. Zero means no cap.
	MaxOutputTokens int

	// Insecure permits a plain-http base_url on a non-loopback host. The route
	// then travels an eavesdroppable link, so the operator opts in per route,
	// where a registry review will see it.
	Insecure bool

	// StripCacheControl removes cache_control markers before forwarding.
	StripCacheControl bool

	// CaptureBodies records request and response bodies for this route.
	CaptureBodies bool
}

// Context is one named set of routes: the aliases it serves, and the default
// route every model no alias in it claims rides. Contexts are how one gateway
// keeps separate worlds separate — a request selects one by URL path prefix,
// and nothing it holds is visible from any other.
type Context struct {
	// Name is the key the context was found under, and the name a request
	// selects it by in its path.
	Name string

	// DefaultRoute governs every model this context's routes do not claim. A
	// context that declares none gets the built-in default route, which is a
	// plain forward to Anthropic on the caller's own credentials.
	DefaultRoute Route

	// Routes are the context's aliases, keyed by the model name that selects
	// them.
	Routes map[string]Route

	// foreignTargets are the rewrite targets a fallthrough must never ride,
	// each keyed to the alias that asks for it. Built once at parse time; see
	// foreignTargets below for what qualifies.
	foreignTargets map[string]string
}

// foreignTargets returns the rewrite targets the context must refuse to serve
// from its default route: every route's model rewrite, keyed by the first
// alias (in sorted order) asking for it — minus any target some route serves
// from the same upstream the default route reaches, since for those a
// fallthrough arrives at an upstream that genuinely serves the name. That
// exemption is what keeps a pinning route (one Claude id rewritten to another,
// forwarded to Anthropic) from confiscating the target id's ordinary
// fallthrough.
func foreignTargets(ctx Context) map[string]string {
	targets := map[string]string{}

	for _, alias := range sortedKeys(ctx.Routes) {
		route := ctx.Routes[alias]
		if route.Model == "" || route.Upstream() == ctx.DefaultRoute.Upstream() {
			continue
		}

		if _, claimed := targets[route.Model]; !claimed {
			targets[route.Model] = alias
		}
	}

	for _, route := range ctx.Routes {
		if route.Model != "" && route.Upstream() == ctx.DefaultRoute.Upstream() {
			delete(targets, route.Model)
		}
	}

	return targets
}

// Upstream is the API root this route actually reaches: the base_url it names,
// or — for a sigv4 route, which names none — the Bedrock endpoint its region
// derives. It is what the ledger records as the upstream, so a record always
// says where the request went.
func (r Route) Upstream() string {
	if r.Auth == AuthSigV4 {
		return BedrockEndpoint(r.AWSRegion)
	}

	return r.BaseURL
}

// BedrockEndpoint returns the bedrock-runtime API root for region.
func BedrockEndpoint(region string) string {
	return bedrockEndpointPrefix + region + bedrockEndpointSuffix
}

// DefaultRoute returns the built-in route every unrecognized model rides: a
// plain forward to Anthropic with no rewrite and no capture. It is a fixed
// value that is never read from the registry file, so a broken registry can
// never break it.
func DefaultRoute() Route {
	return Route{
		BaseURL: anthropic.BaseURL,
		Auth:    AuthForward,
	}
}
