package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/awssig"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// The Bedrock request shape. anthropic_version is the field Bedrock requires in
// place of the model and stream fields it refuses, which is the whole of the
// request translation: everything else the caller sent rides along untouched.
const (
	fieldAnthropicVersion = "anthropic_version"
	fieldStream           = "stream"
	bedrockAnthropicTag   = "bedrock-2023-05-31"
)

// The Bedrock runtime paths: one model invocation, streamed or not.
const (
	invokePath       = "/model/"
	invokeSuffix     = "/invoke"
	invokeStreamPath = "/invoke-with-response-stream"
)

// bedrockService is the SigV4 service name bedrock-runtime signs under.
const bedrockService = "bedrock"

// bedrockStreamType is the content type a streamed Bedrock answer arrives as,
// before patchbay translates it into the SSE the caller expects.
const bedrockStreamType = "application/vnd.amazon.eventstream"

// jsonContentType is what a translated Bedrock request is sent as and what a
// non-streamed answer comes back as.
const jsonContentType = "application/json"

// errorTypeInvalidRequest is Anthropic's own type for a request the gateway
// refuses on its face — here, a model no map entry claims.
const errorTypeInvalidRequest = "invalid_request_error"

// bedrockEventEnvelope is how Bedrock carries one Anthropic stream event: the
// event JSON, base64 encoded, inside a one-field object.
type bedrockEventEnvelope struct {
	Bytes []byte `json:"bytes"`
}

// eventTypeProbe reads the type an Anthropic stream event names itself, which
// is the SSE event name the caller's client dispatches on.
type eventTypeProbe struct {
	Type string `json:"type"`
}

// bedrockRequest builds the signed Bedrock request for one Messages turn: the
// endpoint gates first, then the caller-key gate, then the model map, the body
// translation and the signature. Nothing is exported or signed until the caller
// has proved it may spend the account's credentials.
func (p *Proxy) bedrockRequest(r *http.Request, route registry.Route, body []byte) (*http.Request, *gatewayError) {
	if gwErr := bedrockPathGate(r.URL.Path); gwErr != nil {
		return nil, gwErr
	}

	if keyErr := checkCallerKey(r); keyErr != nil {
		return nil, keyErr
	}

	model := route.Model
	if model == "" {
		model = extractModel(body)
	}

	target, gwErr := bedrockTarget(route, model, body)
	if gwErr != nil {
		return nil, gwErr
	}

	translated, translateErr := translateBedrockBody(body)
	if translateErr != nil {
		return nil, translateErr
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(translated))
	if err != nil {
		return nil, &gatewayError{
			status:  http.StatusInternalServerError,
			errType: "",
			err:     fmt.Errorf("build bedrock request: %w", err),
		}
	}

	copyHeader(req.Header, r.Header,
		droppedHeaders(r.Header, headerHost, headerContentLength, headerAcceptEncoding, CallerKeyHeader))
	req.Header.Del(anthropic.HeaderAuthorization)
	req.Header.Del(anthropic.HeaderClientKey)
	req.Header.Set(headerAcceptEncoding, encodingIdentity)
	req.Header.Set(anthropic.HeaderContentType, jsonContentType)
	req.Header.Set("Accept", jsonContentType)

	return req, p.signBedrock(r.Context(), req, route, translated)
}

// signBedrock exports the route's AWS credentials and signs req with them.
func (p *Proxy) signBedrock(
	ctx context.Context,
	req *http.Request,
	route registry.Route,
	payload []byte,
) *gatewayError {
	creds, err := p.credentials.Credentials(ctx, route.AWSProfile)
	if err != nil {
		// The source's own error is already sanitized; the advice is what the
		// operator can act on, and neither says anything about a credential.
		return &gatewayError{
			status:  http.StatusBadGateway,
			errType: "",
			err: fmt.Errorf("aws credentials for profile %q are unavailable; run: aws sso login --profile %s",
				route.AWSProfile, route.AWSProfile),
		}
	}

	if signErr := awssig.Sign(req, creds, bedrockService, route.AWSRegion, payload, time.Now()); signErr != nil {
		return &gatewayError{
			status:  http.StatusBadGateway,
			errType: "",
			err:     fmt.Errorf("sign the bedrock request: %w", signErr),
		}
	}

	return nil
}

// bedrockPathGate refuses every endpoint a Bedrock route cannot answer.
// count_tokens is called out by name because Claude Code asks for it routinely
// and Bedrock has no equivalent: an honest 501 is worth more than a 404 that
// reads like a broken gateway.
func bedrockPathGate(path string) *gatewayError {
	switch path {
	case messagesPath:
		return nil
	case messagesCountPath:
		return &gatewayError{
			status:  http.StatusNotImplemented,
			errType: "",
			err:     errors.New("count_tokens is not supported for Bedrock routes"),
		}
	default:
		return &gatewayError{
			status:  http.StatusNotFound,
			errType: anthropic.ErrorTypeNotFound,
			err:     fmt.Errorf("a Bedrock route only relays %s, not %s", messagesPath, path),
		}
	}
}

// bedrockTarget resolves the model through the route's map file and returns the
// invocation URL for it, streamed or not.
func bedrockTarget(route registry.Route, model string, body []byte) (string, *gatewayError) {
	if model == "" {
		return "", &gatewayError{
			status:  http.StatusBadRequest,
			errType: errorTypeInvalidRequest,
			err:     errors.New("the request names no model to map to a Bedrock model"),
		}
	}

	mapping, err := readModelMap(route.ModelMapEnvFile)
	if err != nil {
		return "", &gatewayError{
			status:  http.StatusInternalServerError,
			errType: "",
			err:     fmt.Errorf("route %q has an unusable model map: %w", route.Alias, err),
		}
	}

	bedrockModel, mapped := mapping[model]
	if !mapped || bedrockModel == "" {
		return "", &gatewayError{
			status:  http.StatusBadRequest,
			errType: errorTypeInvalidRequest,
			err:     fmt.Errorf("model %q is not available on this Bedrock route", model),
		}
	}

	suffix := invokeSuffix
	if wantsStream(body) {
		suffix = invokeStreamPath
	}

	return registry.BedrockEndpoint(route.AWSRegion) + invokePath + awssig.EscapePathSegment(bedrockModel) + suffix,
		nil
}

// readModelMap resolves the double indirection a sigv4 route uses for its model
// map: envName is the name of an environment variable whose value is the path of
// a JSON file mapping Anthropic model ids to Bedrock ones. The file is read per
// request, so a rebuild that changes it takes effect immediately, and no Bedrock
// ARN is ever held anywhere else.
func readModelMap(envName string) (map[string]string, error) {
	path := os.Getenv(envName)
	if path == "" {
		return nil, fmt.Errorf("environment variable %s is not set", envName)
	}

	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read the model map named by %s: %w", envName, err)
	}

	var mapping map[string]string
	if decodeErr := json.Unmarshal(contents, &mapping); decodeErr != nil {
		return nil, fmt.Errorf("decode the model map named by %s: %w", envName, decodeErr)
	}

	return mapping, nil
}

// wantsStream reports whether the caller asked for a streamed turn.
func wantsStream(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}

	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}

	return probe.Stream
}

// translateBedrockBody is the whole request translation: drop the two fields
// Bedrock carries in the URL rather than the body, add the version tag it
// requires, and leave every other field exactly as the caller wrote it. The
// document is walked as raw values rather than a struct on purpose — a field
// this build has never heard of must still reach the model.
func translateBedrockBody(body []byte) ([]byte, *gatewayError) {
	var document map[string]json.RawMessage

	if err := json.Unmarshal(body, &document); err != nil || document == nil {
		return nil, &gatewayError{
			status:  http.StatusBadRequest,
			errType: errorTypeInvalidRequest,
			err:     errors.New("a Bedrock route needs a JSON object body"),
		}
	}

	delete(document, fieldModel)
	delete(document, fieldStream)

	version, err := json.Marshal(bedrockAnthropicTag)
	if err != nil {
		return nil, &gatewayError{
			status:  http.StatusInternalServerError,
			errType: "",
			err:     fmt.Errorf("encode the bedrock version tag: %w", err),
		}
	}

	document[fieldAnthropicVersion] = version

	translated, err := json.Marshal(document)
	if err != nil {
		return nil, &gatewayError{
			status:  http.StatusInternalServerError,
			errType: "",
			err:     fmt.Errorf("encode the translated request: %w", err),
		}
	}

	return translated, nil
}

// isBedrockStream reports whether resp is an event-stream Bedrock answer, which
// is the only response patchbay translates rather than relays.
func isBedrockStream(route registry.Route, resp *http.Response) bool {
	if route.Auth != registry.AuthSigV4 {
		return false
	}

	return strings.Contains(strings.ToLower(resp.Header.Get(anthropic.HeaderContentType)), bedrockStreamType)
}

// streamBedrock translates a Bedrock event stream into the Anthropic SSE the
// caller's client speaks, flushing every event as it arrives, and tees the
// translated bytes into the capture the record is summarized from — the SSE, not
// the framing, because that is what the caller actually received.
func (p *Proxy) streamBedrock(ctx context.Context, w http.ResponseWriter, resp *http.Response) *capture {
	copyHeader(w.Header(), resp.Header,
		droppedHeaders(resp.Header, headerContentLength, anthropic.HeaderContentType))
	w.Header().Set(anthropic.HeaderContentType, eventStreamType)
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	capt := &capture{buf: nil, dropped: false, truncated: false}
	decoder := awssig.NewDecoder(resp.Body)

	for {
		msg, err := decoder.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// A frame that does not decode ends the turn: the events already
				// written are real, and everything after them is unreadable.
				capt.truncated = true
				p.logger.WarnContext(ctx, "bedrock stream ended early", slog.Any("error", err))
			}

			return capt
		}

		event, done := bedrockSSE(msg)
		if event != nil && !p.writeEvent(ctx, w, capt, flusher, canFlush, event) {
			return capt
		}

		if done {
			return capt
		}
	}
}

// writeEvent hands one translated event to the caller and the capture,
// reporting whether the stream may continue.
func (p *Proxy) writeEvent(
	ctx context.Context,
	w http.ResponseWriter,
	capt *capture,
	flusher http.Flusher,
	canFlush bool,
	event []byte,
) bool {
	capt.write(event)

	if _, err := w.Write(event); err != nil {
		capt.truncated = true
		p.logger.WarnContext(ctx, "client went away mid-stream", slog.Any("error", err))

		return false
	}

	if canFlush {
		flusher.Flush()
	}

	return true
}

// bedrockSSE renders one decoded event-stream message as an SSE event, and
// reports whether it ends the stream. An exception message becomes an Anthropic
// error event, which is what a client knows how to surface; a message that
// carries neither an event nor an exception is not the caller's business and is
// skipped.
func bedrockSSE(msg awssig.Message) ([]byte, bool) {
	if exception := msg.Headers[":exception-type"]; exception != "" {
		body := anthropic.ErrorBody(fmt.Errorf("bedrock stream failed: %s", exception))

		return sseEvent("error", body), true
	}

	eventType := msg.Headers[":event-type"]
	if eventType == "" {
		return nil, false
	}

	var envelope bedrockEventEnvelope
	if err := json.Unmarshal(msg.Payload, &envelope); err != nil || len(envelope.Bytes) == 0 {
		body := anthropic.ErrorBody(errors.New("bedrock sent an event patchbay could not decode"))

		return sseEvent("error", body), true
	}

	var probe eventTypeProbe
	if err := json.Unmarshal(envelope.Bytes, &probe); err == nil && probe.Type != "" {
		eventType = probe.Type
	}

	return sseEvent(eventType, envelope.Bytes), false
}

// sseEvent renders one server-sent event in the shape Anthropic's own stream
// uses: a named event and its JSON payload, terminated by a blank line.
func sseEvent(name string, data []byte) []byte {
	var out bytes.Buffer

	out.WriteString("event: ")
	out.WriteString(name)
	out.WriteString("\ndata: ")
	out.Write(data)
	out.WriteString("\n\n")

	return out.Bytes()
}
