package registry

import (
	"encoding/json"

	"github.com/joshsymonds/patchbay/internal/billing"
)

// AuthMode selects how patchbay authenticates a proxied request upstream.
type AuthMode string

// Supported upstream authentication modes.
const (
	AuthForward AuthMode = "forward"
	AuthInject  AuthMode = "inject"
	AuthSigV4   AuthMode = "sigv4"
)

const (
	bedrockEndpointPrefix = "https://bedrock-runtime."
	bedrockEndpointSuffix = ".amazonaws.com"
)

// SpeedFast is the one value the speed field carries: the upstream's fast
// tier. It is Anthropic's own fast-mode request value, and the Codex
// translator maps the same field onto its priority service tier.
const SpeedFast = "fast"

// CacheTTLOneHour is the one lifetime the cache_ttl field carries: Anthropic's
// extended prompt-cache TTL, written as the cache_control marker spells it.
const CacheTTLOneHour = "1h"

// Seat is a reusable upstream destination and all policy applied when it is used.
type Seat struct {
	Upstream        string
	Model           string
	AuthMode        AuthMode
	APIKeyEnvFile   string
	AWSProfile      string
	AWSRegion       string
	ModelMapEnvFile string

	// BillingClass is the explicit billing class an inject seat declares, and
	// BillingClassSet says whether the seat declared one at all. The pair is
	// what separates an omitted declaration from a present but empty value:
	// admission requires the declaration on an inject seat and refuses it on
	// every other, so only the presence bit can tell those two refusals apart.
	BillingClass    billing.Class
	BillingClassSet bool

	ExtraBody         json.RawMessage
	MaxInputTokens    int
	MaxOutputTokens   int
	Insecure          bool
	StripCacheControl bool
	CaptureBodies     bool

	// Speed is the upstream tier this seat asks for on every request it
	// carries: empty for the upstream's default, SpeedFast for its fast tier.
	// It is seat policy, so it replaces whatever the caller sent.
	Speed string

	// CacheTTL is the prompt-cache lifetime this seat writes into every
	// cache_control marker a request carries: empty leaves the caller's markers
	// alone, CacheTTLOneHour holds each entry for an hour instead of the
	// default five minutes. A caller that idles between requests longer than
	// five minutes — an agent waiting on the children it dispatched — otherwise
	// rewrites its whole context into the cache on every wake. It is seat
	// policy, so it replaces whatever lifetime the caller asked for.
	CacheTTL string

	// StripBetas names the beta flags a sigv4 seat withholds from Bedrock. The
	// Bedrock translation moves every flag the caller sent, by header or in
	// the body, into the body's anthropic_beta list, and Bedrock refuses the
	// whole request over any flag it does not know; this is where the operator
	// names those.
	StripBetas []string
}

// EffectiveUpstream is the API root this seat reaches.
func (s Seat) EffectiveUpstream() string {
	if s.AuthMode == AuthSigV4 {
		return BedrockEndpoint(s.AWSRegion)
	}
	return s.Upstream
}

// CredentialSource is the configured non-secret credential source descriptor.
func (s Seat) CredentialSource() string {
	switch s.AuthMode {
	case AuthForward:
		return "caller"
	case AuthInject:
		return s.APIKeyEnvFile
	case AuthSigV4:
		return "aws_profile:" + s.AWSProfile
	default:
		return ""
	}
}

// BillingPolicy reports how this seat determines the billing class of a
// request. It depends on the auth mode alone, because the auth mode is what
// decides whose money is spent: forwarded caller credentials classify
// themselves, sigv4 is always metered against the signed AWS account, and an
// inject seat spends an operator key that nothing outside the registry can
// classify — so it names the class itself.
func (s Seat) BillingPolicy() billing.Policy {
	switch s.AuthMode {
	case AuthForward:
		return billing.PolicyCallerDerived
	case AuthInject:
		return billing.PolicyExplicit
	case AuthSigV4:
		return billing.PolicySigV4Metered
	default:
		return ""
	}
}

// BedrockEndpoint returns the bedrock-runtime API root for region.
func BedrockEndpoint(region string) string {
	return bedrockEndpointPrefix + region + bedrockEndpointSuffix
}

// Subagents is the policy applied only to positively marked subagent requests.
//
// AgentTypes binds the agent type Claude Code reports for marked traffic (the
// built-in Explore, say) to a Seat, ahead of Models: a built-in agent that
// inherits its session's model names that model on the wire, so the model
// alone cannot tell it apart from the session it serves.
type Subagents struct {
	DefaultSeat string
	AgentTypes  map[string]string
	Models      map[string]string
}

// Context binds public and optional marked-subagent selectors to global seats.
type Context struct {
	DefaultSeat    string
	Models         map[string]string
	Subagents      *Subagents
	foreignTargets map[string]string
}
