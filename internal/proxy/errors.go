package proxy

import (
	"errors"
	"net/http"

	"github.com/joshsymonds/patchbay/internal/anthropic"
)

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

// forwardError maps a resolved forward Seat's credential shape to the local
// refusal it requires. Inject and SigV4 Seats never call it.
func (c credentialShape) forwardError() *gatewayError {
	switch c {
	case credentialOAuth, credentialAPIKey:
		return nil
	case credentialOAuthAndAPIKey:
		return &gatewayError{
			status:  http.StatusBadRequest,
			errType: anthropic.ErrorTypeInvalidRequest,
			err:     errors.New("forward Seat received both OAuth and API key credentials"),
		}
	case credentialUnrecognizedAuthorization:
		return &gatewayError{
			status:  http.StatusUnauthorized,
			errType: anthropic.ErrorTypeAuthentication,
			err:     errors.New("forward Seat received an unrecognized Authorization scheme"),
		}
	case credentialRepeatedOAuthAuthorization, credentialRepeatedAuthorization:
		return &gatewayError{
			status:  http.StatusUnauthorized,
			errType: anthropic.ErrorTypeAuthentication,
			err:     errors.New("forward Seat received repeated Authorization values"),
		}
	case credentialRepeatedAPIKey:
		return &gatewayError{
			status:  http.StatusUnauthorized,
			errType: anthropic.ErrorTypeAuthentication,
			err:     errors.New("forward Seat received repeated API key values"),
		}
	default:
		return &gatewayError{
			status:  http.StatusUnauthorized,
			errType: anthropic.ErrorTypeAuthentication,
			err:     errors.New("forward Seat requires an OAuth bearer or API key credential for usage accounting"),
		}
	}
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
