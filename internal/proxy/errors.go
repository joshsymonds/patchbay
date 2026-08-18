package proxy

import "encoding/json"

// errorMessagePrefix marks a failure as patchbay's own rather than the
// upstream's. Claude Code matches upstream error text to decide whether to
// retry, so an error patchbay invented must never read like one Anthropic sent.
const errorMessagePrefix = "patchbay: "

// fallbackErrorBody is returned if the error envelope itself fails to encode,
// which keeps the caller's contract — always Anthropic-shaped JSON — even then.
const fallbackErrorBody = `{"type":"error","error":{"type":"api_error","message":"patchbay: internal error"}}`

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

// errorEnvelope is the Anthropic error wire shape.
type errorEnvelope struct {
	Type  string       `json:"type"`
	Error errorPayload `json:"error"`
}

// errorPayload is the detail object inside an Anthropic error.
type errorPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// errorBody renders a gateway failure as Anthropic-format error JSON.
func errorBody(gwErr *gatewayError) []byte {
	body, err := json.Marshal(errorEnvelope{
		Type: "error",
		Error: errorPayload{
			Type:    "api_error",
			Message: errorMessagePrefix + gwErr.Error(),
		},
	})
	if err != nil {
		return []byte(fallbackErrorBody)
	}

	return body
}
