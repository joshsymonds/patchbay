package proxy

// gatewayError is a failure patchbay originated, carrying the HTTP status that
// honestly describes it. An upstream's own error is never one of these: it is
// passed through untouched.
type gatewayError struct {
	status int
	err    error
}

// Error reports the underlying failure.
func (e *gatewayError) Error() string {
	return e.err.Error()
}

// Unwrap exposes the failure the gateway error describes.
func (e *gatewayError) Unwrap() error {
	return e.err
}
