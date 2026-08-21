package registry

import "encoding/json"

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

// Seat is a reusable upstream destination and all policy applied when it is used.
type Seat struct {
	Upstream          string
	Model             string
	AuthMode          AuthMode
	APIKeyEnvFile     string
	AWSProfile        string
	AWSRegion         string
	ModelMapEnvFile   string
	ExtraBody         json.RawMessage
	MaxInputTokens    int
	MaxOutputTokens   int
	Insecure          bool
	StripCacheControl bool
	CaptureBodies     bool
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

// BedrockEndpoint returns the bedrock-runtime API root for region.
func BedrockEndpoint(region string) string {
	return bedrockEndpointPrefix + region + bedrockEndpointSuffix
}

// Subagents is the policy applied only to positively marked subagent requests.
type Subagents struct {
	DefaultSeat string
	Models      map[string]string
}

// Context binds public and optional marked-subagent selectors to global seats.
type Context struct {
	DefaultSeat    string
	Models         map[string]string
	Subagents      *Subagents
	foreignTargets map[string]string
}
