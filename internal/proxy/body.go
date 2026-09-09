package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// Top-level request fields the pipeline reads or rewrites.
const (
	fieldModel        = "model"
	fieldMaxTokens    = "max_tokens"
	fieldCacheControl = "cache_control"
	fieldSpeed        = "speed"
)

// extractModel reads the top-level model name from a request body with a
// partial decode of its own copy. It never touches the bytes that get
// forwarded, and it never fails: a body that is not a JSON object, or one whose
// model is not a string, simply names no model and rides the default seat.
func extractModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}

	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}

	return probe.Model
}

// isIdentity reports whether a seat forwards the request body unchanged. This
// is the byte-fidelity case: nothing about the document is rewritten, so the
// original bytes are what travel upstream.
func isIdentity(seat registry.Seat) bool {
	return seat.Model == "" &&
		len(seat.ExtraBody) == 0 &&
		!seat.StripCacheControl &&
		seat.MaxOutputTokens == 0 &&
		seat.Speed == ""
}

// forwardBody returns the bytes to send upstream. On an identity seat that is
// the caller's original body, returned as it arrived. Otherwise the document is
// decoded, rewritten and re-encoded.
func forwardBody(body []byte, seat registry.Seat) ([]byte, *gatewayError) {
	if isIdentity(seat) {
		return body, nil
	}

	// Both refusals below are the caller's body being refused on its own face
	// rather than anything the gateway failed at, so they say so in Anthropic's
	// taxonomy: a client reads that type to tell a request worth fixing from a
	// gateway worth retrying.
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, &gatewayError{
			status:  http.StatusBadRequest,
			errType: anthropic.ErrorTypeInvalidRequest,
			err: fmt.Errorf(
				"the selected Seat rewrites the request, which is not a JSON object: %w",
				err,
			),
		}
	}

	// JSON null decodes into a nil map without an error, and writing a rewrite
	// into a nil map panics. It is not an object, so it is refused as one.
	//
	// A caller can reach this. A null body names no model, so it rides its
	// Context's default_seat — and nothing stops that Seat from rewriting: the
	// registry admits an inject or sigv4 default_seat with a model of its own,
	// and then the identity branch above does not return. The guard is what
	// stands between such a Context and a panic on the request path.
	if document == nil {
		return nil, &gatewayError{
			status:  http.StatusBadRequest,
			errType: anthropic.ErrorTypeInvalidRequest,
			err: errors.New(
				"the selected Seat rewrites the request, which is JSON null rather than an object",
			),
		}
	}

	if seat.Model != "" {
		rewritten, err := json.Marshal(seat.Model)
		if err != nil {
			return nil, &gatewayError{
				status: http.StatusInternalServerError,
				err:    fmt.Errorf("encode seat model: %w", err),
			}
		}

		document[fieldModel] = rewritten
	}

	if err := mergeExtraBody(document, seat.ExtraBody); err != nil {
		return nil, &gatewayError{
			status: http.StatusInternalServerError,
			err:    fmt.Errorf("the selected Seat has an unusable extra_body: %w", err),
		}
	}

	// The Seat's speed is policy, applied after extra_body so that it wins over
	// both the caller and any speed an extra_body happened to carry.
	if seat.Speed != "" {
		rewritten, err := json.Marshal(seat.Speed)
		if err != nil {
			return nil, &gatewayError{
				status: http.StatusInternalServerError,
				err:    fmt.Errorf("encode seat speed: %w", err),
			}
		}

		document[fieldSpeed] = rewritten
	}

	if seat.StripCacheControl {
		stripCacheControl(document)
	}

	clampMaxTokens(document, seat.MaxOutputTokens)

	rewritten, err := json.Marshal(document)
	if err != nil {
		return nil, &gatewayError{
			status: http.StatusInternalServerError,
			err:    fmt.Errorf("encode rewritten request: %w", err),
		}
	}

	return rewritten, nil
}

// mergeExtraBody merges the seat's extra_body over the document's top-level
// keys, so a seat's own settings win over whatever the caller sent. The Seat's
// bytes belong to the Resolution that selected it and describe the registry as
// configured, so they are decoded into fresh values and never written to: what
// this request merges is its own to keep.
func mergeExtraBody(document map[string]json.RawMessage, extraBody json.RawMessage) error {
	if len(extraBody) == 0 {
		return nil
	}

	var extra map[string]json.RawMessage
	if err := json.Unmarshal(extraBody, &extra); err != nil {
		return fmt.Errorf("decode extra_body: %w", err)
	}

	for name, value := range extra {
		document[name] = value
	}

	return nil
}

// stripCacheControl removes every cache_control key from the whole document,
// at any depth. Prompt caching is Anthropic's; a foreign upstream that does not
// understand the marker may well reject the request over it.
func stripCacheControl(document map[string]json.RawMessage) {
	delete(document, fieldCacheControl)

	for name, value := range document {
		document[name] = strippedValue(value)
	}
}

// strippedValue returns value with every nested cache_control key removed.
// Anything that is not an object or an array is returned as the exact bytes it
// arrived as, so numbers keep the precision they were written with.
func strippedValue(value json.RawMessage) json.RawMessage {
	// Nothing to strip if the field name appears nowhere in this subtree, so the
	// whole unmarshal/remarshal of a container that cannot hold it is skipped. A
	// false-positive substring match — the name inside some string value — simply
	// falls through to the existing, correct path below and is handled there.
	if !bytes.Contains(value, []byte(fieldCacheControl)) {
		return value
	}

	switch firstToken(value) {
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(value, &object); err != nil {
			return value
		}

		stripCacheControl(object)

		return remarshal(object, value)
	case '[':
		var array []json.RawMessage
		if err := json.Unmarshal(value, &array); err != nil {
			return value
		}

		for i, element := range array {
			array[i] = strippedValue(element)
		}

		return remarshal(array, value)
	default:
		return value
	}
}

// remarshal encodes a rewritten container, falling back to the original bytes
// if it somehow cannot be encoded — bytes that decoded a moment ago.
func remarshal(container any, original json.RawMessage) json.RawMessage {
	encoded, err := json.Marshal(container)
	if err != nil {
		return original
	}

	return encoded
}

// firstToken returns the first non-whitespace byte of a JSON value, or zero if
// there is none.
func firstToken(value json.RawMessage) byte {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 {
		return 0
	}

	return trimmed[0]
}

// clampMaxTokens lowers the request's max_tokens to the seat's ceiling when it
// asks for more. JSON has one number type, so the value is read as one: 1e5 is
// the same request as 100000 and is held to the ceiling the same way. A request
// already under the ceiling, an absent max_tokens and a seat with no ceiling
// are all left exactly as they are.
func clampMaxTokens(document map[string]json.RawMessage, ceiling int) {
	if ceiling <= 0 {
		return
	}

	raw, present := document[fieldMaxTokens]
	if !present {
		return
	}

	var requested float64
	if err := json.Unmarshal(raw, &requested); err != nil {
		return
	}

	if requested <= float64(ceiling) {
		return
	}

	document[fieldMaxTokens] = json.RawMessage(strconv.Itoa(ceiling))
}
