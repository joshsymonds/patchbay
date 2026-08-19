package proxy

import "github.com/joshsymonds/patchbay/internal/anthropic"

// gatewayError is a failure patchbay originated, carrying the HTTP status that
// honestly describes it. An upstream's own error is never one of these: it is
// passed through untouched.
type gatewayError struct {
	status int

	// errType names the failure in Anthropic's own error taxonomy. An empty
	// errType is the generic gateway failure, which is what nearly every refusal
	// is; a request refused on its face names something more specific.
	errType string

	err error
}

// anthropicType is the error type this failure is described by on the wire.
func (e *gatewayError) anthropicType() string {
	if e.errType == "" {
		return anthropic.ErrorTypeAPI
	}

	return e.errType
}

// Error reports the underlying failure.
func (e *gatewayError) Error() string {
	return e.err.Error()
}

// Unwrap exposes the failure the gateway error describes.
func (e *gatewayError) Unwrap() error {
	return e.err
}
