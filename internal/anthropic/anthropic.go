// Package anthropic holds the details of the Anthropic wire protocol that more
// than one of patchbay's own packages has to speak.
//
// There is exactly one right answer to what a gateway failure looks like on the
// wire and what the credential headers are called, and every package that
// answers a caller has to give the same one. Keeping them here is what makes
// that true by construction rather than by two copies happening to agree.
package anthropic

import "encoding/json"

// ErrorMessagePrefix marks a failure as patchbay's own rather than the
// upstream's. Claude Code matches upstream error text to decide whether to
// retry, so an error patchbay invented must never read like one Anthropic sent.
const ErrorMessagePrefix = "patchbay: "

// fallbackErrorBody is returned if the error envelope itself fails to encode,
// which keeps the caller's contract — always Anthropic-shaped JSON — even then.
const fallbackErrorBody = `{"type":"error","error":{"type":"api_error","message":"patchbay: internal error"}}`

// Header names patchbay handles itself rather than treating as ordinary
// request metadata: the credentials that pay for an answer, the version and
// beta flags that shape it, and the type of the answer itself.
// HeaderClientKey deliberately avoids spelling out "API key": a const named
// that way trips gosec's hardcoded-credential heuristic (G101), the same way
// the registry's key-file field name does.
const (
	HeaderAuthorization = "Authorization"
	HeaderClientKey     = "X-Api-Key"
	HeaderVersion       = "Anthropic-Version"
	HeaderBeta          = "Anthropic-Beta"
	HeaderContentType   = "Content-Type"
)

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

// ErrorBody renders a failure patchbay originated as Anthropic-format error
// JSON the caller can recognize as patchbay's own. An upstream's own error is
// never one of these: it is passed through untouched.
func ErrorBody(err error) []byte {
	body, marshalErr := json.Marshal(errorEnvelope{
		Type: "error",
		Error: errorPayload{
			Type:    "api_error",
			Message: ErrorMessagePrefix + err.Error(),
		},
	})
	if marshalErr != nil {
		return []byte(fallbackErrorBody)
	}

	return body
}
