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
// place of the model and stream fields it refuses, and anthropic_beta is where
// it reads the beta flags an Anthropic client sends as a header — Bedrock never
// reads the header, so a flag left there gates nothing and every field it
// would have unlocked is refused as an extra input. That is the whole of the
// request translation: everything else the caller sent rides along untouched.
const (
	fieldAnthropicVersion = "anthropic_version"
	fieldAnthropicBeta    = "anthropic_beta"
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

// firstPrintableByte is the lowest byte an SSE event name may carry: every byte
// below it is a control character, and CR and LF among them would end the line
// the name is written on.
const firstPrintableByte = 0x20

// errorEventName is the SSE event name patchbay's own failures are reported
// under. It is a constant of this package, never anything an upstream named.
const errorEventName = "error"

// jsonContentType is what a translated Bedrock request is sent as and what a
// non-streamed answer comes back as.
const jsonContentType = "application/json"

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
//
// It also reports the credential generation the request was signed with, so a
// refusal invalidates the credential that actually signed it rather than
// whichever one the cache holds by the time the refusal lands, and the resolved
// Bedrock target the request is addressed to, which the accounting row keeps as
// dispatch evidence. A non-empty upstreamTarget is that already-recorded
// destination, reused verbatim: the retry re-signs for the target the evidence
// names rather than re-reading a model map that may have changed underneath it.
func (p *Proxy) bedrockRequest(
	r *http.Request,
	seat registry.Seat,
	body []byte,
	upstreamTarget string,
) (*http.Request, uint64, string, *gatewayError) {
	if gwErr := bedrockPathGate(r.URL.Path); gwErr != nil {
		return nil, 0, "", gwErr
	}

	if keyErr := checkCallerKey(r); keyErr != nil {
		return nil, 0, "", keyErr
	}

	// The body is decoded exactly once per attempt, and the model, the stream
	// flag and the translated document all come out of that one document: a
	// second parse of the same bytes is a second chance for the two readings to
	// disagree about what the caller asked for.
	document, docErr := bedrockDocument(body)
	if docErr != nil {
		return nil, 0, "", docErr
	}

	model := seat.Model
	if model == "" {
		model = documentModel(document)
	}

	if upstreamTarget == "" {
		var gwErr *gatewayError
		upstreamTarget, gwErr = bedrockTarget(seat, model)
		if gwErr != nil {
			return nil, 0, "", gwErr
		}
	}
	target := bedrockInvocationURL(seat, upstreamTarget, documentWantsStream(document))

	if betaErr := mergeBetas(document, r.Header.Values(anthropic.HeaderBeta), seat.StripBetas); betaErr != nil {
		return nil, 0, "", betaErr
	}

	translated, translateErr := translateBedrockDocument(document)
	if translateErr != nil {
		return nil, 0, "", translateErr
	}

	req, err := http.NewRequestWithContext(
		r.Context(),
		http.MethodPost,
		target,
		bytes.NewReader(translated),
	)
	if err != nil {
		return nil, 0, "", &gatewayError{
			status:  http.StatusInternalServerError,
			errType: "",
			err:     fmt.Errorf("build bedrock request: %w", err),
		}
	}

	copyHeader(
		req.Header,
		r.Header,
		droppedHeaders(
			r.Header,
			headerHost,
			headerContentLength,
			headerAcceptEncoding,
			CallerKeyHeader,
			// Already carried in the body, where Bedrock reads it.
			anthropic.HeaderBeta,
		),
	)
	req.Header.Del(anthropic.HeaderAuthorization)
	req.Header.Del(anthropic.HeaderClientKey)
	req.Header.Set(headerAcceptEncoding, encodingIdentity)
	req.Header.Set(anthropic.HeaderContentType, jsonContentType)
	req.Header.Set("Accept", jsonContentType)

	generation, signErr := p.signBedrock(r.Context(), req, seat, translated)

	return req, generation, upstreamTarget, signErr
}

// signBedrock exports the seat's AWS credentials and signs req with them,
// reporting which generation of the profile's credentials it signed with.
func (p *Proxy) signBedrock(
	ctx context.Context,
	req *http.Request,
	seat registry.Seat,
	payload []byte,
) (uint64, *gatewayError) {
	creds, generation, err := p.credentials.Credentials(ctx, seat.AWSProfile)
	if err != nil {
		// The source's own error is already sanitized; the advice is what the
		// operator can act on, and neither says anything about a credential.
		return 0, &gatewayError{
			status:  http.StatusBadGateway,
			errType: "",
			err: fmt.Errorf(
				"aws credentials for profile %q are unavailable; run: aws sso login --profile %s",
				seat.AWSProfile,
				seat.AWSProfile,
			),
		}
	}

	if signErr := awssig.Sign(
		req,
		creds,
		bedrockService,
		seat.AWSRegion,
		payload,
		time.Now(),
	); signErr != nil {
		return 0, &gatewayError{
			status:  http.StatusBadGateway,
			errType: "",
			err:     fmt.Errorf("sign the bedrock request: %w", signErr),
		}
	}

	return generation, nil
}

// bedrockPathGate refuses every endpoint a Bedrock seat cannot answer.
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
			err:     errors.New("count_tokens is not supported for Bedrock Seats"),
		}
	default:
		return &gatewayError{
			status:  http.StatusNotFound,
			errType: anthropic.ErrorTypeNotFound,
			err:     fmt.Errorf("a Bedrock seat only relays %s, not %s", messagesPath, path),
		}
	}
}

// bedrockTarget resolves the model through the seat's map file and returns the
// mapped Bedrock target. The resolved target is immutable for one relay attempt,
// including its retry, and is recorded as dispatch evidence before it is sent.
func bedrockTarget(seat registry.Seat, model string) (string, *gatewayError) {
	if model == "" {
		return "", &gatewayError{
			status:  http.StatusBadRequest,
			errType: anthropic.ErrorTypeInvalidRequest,
			err:     errors.New("the request names no model to map to a Bedrock model"),
		}
	}

	mapping, err := readModelMap(seat.ModelMapEnvFile)
	if err != nil {
		return "", &gatewayError{
			status:  http.StatusInternalServerError,
			errType: "",
			err:     fmt.Errorf("the selected Seat has an unusable model map: %w", err),
		}
	}

	bedrockModel, mapped := mapping[model]
	if !mapped || bedrockModel == "" {
		return "", &gatewayError{
			status:  http.StatusBadRequest,
			errType: anthropic.ErrorTypeInvalidRequest,
			err:     fmt.Errorf("model %q is not available on this Bedrock seat", model),
		}
	}

	return bedrockModel, nil
}

// bedrockInvocationURL is the endpoint one already-resolved target is invoked
// at, streamed or not. Only the suffix depends on the request; the destination
// itself is the target the accounting row already names.
func bedrockInvocationURL(seat registry.Seat, target string, streamed bool) string {
	suffix := invokeSuffix
	if streamed {
		suffix = invokeStreamPath
	}

	return registry.BedrockEndpoint(
		seat.AWSRegion,
	) + invokePath + awssig.EscapePathSegment(
		target,
	) + suffix
}

// readModelMap resolves the double indirection a sigv4 seat uses for its model
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

// bedrockDocument decodes the caller's body into the one document every other
// step of the translation reads. It is the only place these bytes are parsed.
func bedrockDocument(body []byte) (map[string]json.RawMessage, *gatewayError) {
	var document map[string]json.RawMessage

	if err := json.Unmarshal(body, &document); err != nil || document == nil {
		return nil, &gatewayError{
			status:  http.StatusBadRequest,
			errType: anthropic.ErrorTypeInvalidRequest,
			err:     errors.New("a Bedrock seat needs a JSON object body"),
		}
	}

	return document, nil
}

// documentModel reads the model the decoded request names. A model that is not
// a string names nothing, and is refused a moment later for having no mapping.
func documentModel(document map[string]json.RawMessage) string {
	var model string
	if err := json.Unmarshal(document[fieldModel], &model); err != nil {
		return ""
	}

	return model
}

// documentWantsStream reports whether the decoded request asked for a streamed
// turn. Anything that is not a JSON true is not a request to stream.
func documentWantsStream(document map[string]json.RawMessage) bool {
	var streamed bool
	if err := json.Unmarshal(document[fieldStream], &streamed); err != nil {
		return false
	}

	return streamed
}

// mergeBetas moves the caller's beta flags to where Bedrock reads them: the
// header values, split on commas, are joined with whatever anthropic_beta list
// the body already carries, in the order first seen and without repeats, minus
// every flag the seat strips. Bedrock refuses the whole request over a flag it
// does not know, so the operator names those on the seat. A body list that is
// not an array of strings is the caller's own error and is refused as one.
func mergeBetas(document map[string]json.RawMessage, header []string, strip []string) *gatewayError {
	var body []string
	if raw, present := document[fieldAnthropicBeta]; present {
		if err := json.Unmarshal(raw, &body); err != nil {
			return &gatewayError{
				status:  http.StatusBadRequest,
				errType: anthropic.ErrorTypeInvalidRequest,
				err:     fmt.Errorf("%s must be an array of strings: %w", fieldAnthropicBeta, err),
			}
		}
	}

	stripped := make(map[string]bool, len(strip))
	for _, name := range strip {
		stripped[name] = true
	}

	seen := make(map[string]bool)
	merged := make([]string, 0, len(body)+len(header))
	for _, name := range betaNames(body, header) {
		if name == "" || seen[name] || stripped[name] {
			continue
		}
		seen[name] = true
		merged = append(merged, name)
	}

	if len(merged) == 0 {
		delete(document, fieldAnthropicBeta)

		return nil
	}

	encoded, err := json.Marshal(merged)
	if err != nil {
		return &gatewayError{
			status:  http.StatusInternalServerError,
			errType: "",
			err:     fmt.Errorf("encode the merged %s list: %w", fieldAnthropicBeta, err),
		}
	}
	document[fieldAnthropicBeta] = encoded

	return nil
}

// betaNames lists every flag named in the body list and then in the header
// values, each header value split on commas as the header grammar allows and
// each name trimmed of the whitespace that grammar also allows.
func betaNames(body []string, header []string) []string {
	names := make([]string, 0, len(body)+len(header))
	for _, name := range body {
		names = append(names, strings.TrimSpace(name))
	}
	for _, value := range header {
		for _, name := range strings.Split(value, ",") {
			names = append(names, strings.TrimSpace(name))
		}
	}

	return names
}

// translateBedrockDocument is the rest of the request translation after
// mergeBetas: drop the two fields Bedrock carries in the URL rather than the
// body, add the version tag it requires, and leave every other field exactly
// as the caller wrote it. The document is walked as raw values rather than a
// struct on purpose — a field this build has never heard of must still reach
// the model.
//
// It consumes the document rather than the bytes, so nothing here re-reads what
// bedrockDocument already read.
func translateBedrockDocument(document map[string]json.RawMessage) ([]byte, *gatewayError) {
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
func isBedrockStream(seat registry.Seat, resp *http.Response) bool {
	if seat.AuthMode != registry.AuthSigV4 {
		return false
	}

	return strings.Contains(
		strings.ToLower(resp.Header.Get(anthropic.HeaderContentType)),
		bedrockStreamType,
	)
}

// streamBedrock translates a Bedrock event stream into the Anthropic SSE the
// caller's client speaks, flushing every event as it arrives, and tees the
// translated bytes into the capture the record is summarized from — the SSE, not
// the framing, because that is what the caller actually received.
func (p *Proxy) streamBedrock(
	ctx context.Context,
	w http.ResponseWriter,
	resp *http.Response,
) *capture {
	copyHeader(w.Header(), resp.Header,
		droppedHeaders(resp.Header, headerContentLength, anthropic.HeaderContentType))
	w.Header().Set(anthropic.HeaderContentType, eventStreamType)
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	capt := &capture{buf: nil, dropped: false, truncated: false, observeSSE: true}
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
		if event != nil && !p.writeChunk(ctx, w, capt, flusher, canFlush, event) {
			return capt
		}

		if done {
			return capt
		}
	}
}

// bedrockSSE renders one decoded event-stream message as an SSE event, and
// reports whether it ends the stream. An exception message becomes an Anthropic
// error event, which is what a client knows how to surface; a message that
// carries neither an event nor an exception is not the caller's business and is
// skipped.
func bedrockSSE(msg awssig.Message) ([]byte, bool) {
	if exception := msg.Headers[":exception-type"]; exception != "" {
		body := anthropic.ErrorBody(fmt.Errorf("bedrock stream failed: %s", exception))

		return sseEvent(errorEventName, body), true
	}

	eventType := msg.Headers[":event-type"]
	if eventType == "" {
		return nil, false
	}

	var envelope bedrockEventEnvelope
	if err := json.Unmarshal(msg.Payload, &envelope); err != nil || len(envelope.Bytes) == 0 {
		body := anthropic.ErrorBody(errors.New("bedrock sent an event patchbay could not decode"))

		return sseEvent(errorEventName, body), true
	}

	var probe eventTypeProbe
	if err := json.Unmarshal(envelope.Bytes, &probe); err == nil && probe.Type != "" {
		eventType = probe.Type
	}

	// The name is written into the SSE framing itself, so one carrying CR or LF
	// could forge whole events the model never sent — and every other control
	// byte is equally not a name. Such a message is refused rather than rendered:
	// the caller gets patchbay's own error event, under the one fixed safe name,
	// and the stream ends there.
	if !safeEventName(eventType) {
		body := anthropic.ErrorBody(errors.New("bedrock named an event patchbay will not render"))

		return sseEvent(errorEventName, body), true
	}

	return sseEvent(eventType, envelope.Bytes), false
}

// safeEventName reports whether name can be written as an SSE event name at
// all: anything below the printable range would end the line, or the event,
// somewhere the framing did not intend.
func safeEventName(name string) bool {
	for _, char := range []byte(name) {
		if char < firstPrintableByte {
			return false
		}
	}

	return true
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
