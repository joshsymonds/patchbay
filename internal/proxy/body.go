package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/joshsymonds/patchbay/internal/registry"
)

// Top-level request fields the pipeline reads or rewrites.
const (
	fieldModel        = "model"
	fieldMaxTokens    = "max_tokens"
	fieldCacheControl = "cache_control"
)

// extractModel reads the top-level model name from a request body with a
// partial decode of its own copy. It never touches the bytes that get
// forwarded, and it never fails: a body that is not a JSON object, or one whose
// model is not a string, simply names no model and rides the default route.
func extractModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}

	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}

	return probe.Model
}

// isIdentity reports whether a route forwards the request body unchanged. This
// is the byte-fidelity case: nothing about the document is rewritten, so the
// original bytes are what travel upstream.
func isIdentity(route registry.Route) bool {
	return route.Model == "" &&
		len(route.ExtraBody) == 0 &&
		!route.StripCacheControl &&
		route.MaxOutputTokens == 0
}

// forwardBody returns the bytes to send upstream. On an identity route that is
// the caller's original body, returned as it arrived. Otherwise the document is
// decoded, rewritten and re-encoded.
func forwardBody(body []byte, route registry.Route) ([]byte, *gatewayError) {
	if isIdentity(route) {
		return body, nil
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, &gatewayError{
			status: http.StatusBadRequest,
			err:    fmt.Errorf("route %q rewrites the request, which is not a JSON object: %w", route.Alias, err),
		}
	}

	// JSON null decodes into a nil map without an error, and writing a rewrite
	// into a nil map panics. It is not an object, so it is refused as one.
	//
	// This is defense in depth: no request reaches it through ServeHTTP today,
	// because a null body names no model, an empty alias is rejected at parse,
	// and so a null body always resolves to the identity default route, which
	// returns above without decoding anything. The guard is what keeps that from
	// being a panic the first time some route does rewrite one.
	if document == nil {
		return nil, &gatewayError{
			status: http.StatusBadRequest,
			err:    fmt.Errorf("route %q rewrites the request, which is JSON null rather than an object", route.Alias),
		}
	}

	if route.Model != "" {
		rewritten, err := json.Marshal(route.Model)
		if err != nil {
			return nil, &gatewayError{
				status: http.StatusInternalServerError,
				err:    fmt.Errorf("encode route model: %w", err),
			}
		}

		document[fieldModel] = rewritten
	}

	if err := mergeExtraBody(document, route.ExtraBody); err != nil {
		return nil, &gatewayError{
			status: http.StatusInternalServerError,
			err:    fmt.Errorf("route %q has an unusable extra_body: %w", route.Alias, err),
		}
	}

	if route.StripCacheControl {
		stripCacheControl(document)
	}

	clampMaxTokens(document, route.MaxOutputTokens)

	rewritten, err := json.Marshal(document)
	if err != nil {
		return nil, &gatewayError{
			status: http.StatusInternalServerError,
			err:    fmt.Errorf("encode rewritten request: %w", err),
		}
	}

	return rewritten, nil
}

// mergeExtraBody merges the route's extra_body over the document's top-level
// keys, so a route's own settings win over whatever the caller sent. The
// route's bytes are shared by every request on that alias, so they are decoded
// into fresh values and never written to.
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

// clampMaxTokens lowers the request's max_tokens to the route's ceiling when it
// asks for more. JSON has one number type, so the value is read as one: 1e5 is
// the same request as 100000 and is held to the ceiling the same way. A request
// already under the ceiling, an absent max_tokens and a route with no ceiling
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
