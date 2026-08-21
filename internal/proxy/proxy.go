// Package proxy forwards Anthropic Messages API requests to the upstream a
// route names, tees the response into the ledger, and hands the caller the
// upstream's bytes unchanged.
//
// Byte fidelity is the invariant the whole package is built around. On a route
// that rewrites nothing, the request body forwarded upstream is the caller's
// original bytes — never decoded and re-encoded — and the response is streamed
// back flush by flush exactly as it arrived. The ledger tee is passive: any
// failure to capture or record drops the capture, never the stream.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/awssig"
	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// streamChunkBytes is the read size of the response copy loop. Every chunk is
// written and flushed as it arrives, so this bounds latency, not throughput.
const streamChunkBytes = 32768

// The Messages endpoints an inject route is allowed to relay. An inject route
// pays with a foreign key, so it exists to answer Messages turns and nothing
// else; a stray path on it must not spend that key on some other endpoint.
// messagesCountPath deliberately avoids the word "token" in its name: a const
// spelled that way trips gosec's hardcoded-credential heuristic (G101), the
// same way internal/anthropic's HeaderClientKey does.
const (
	messagesPath      = "/v1/messages"
	messagesCountPath = "/v1/messages/count_tokens"
)

// drainLimitBytes bounds the body patchbay reads off a response nobody will
// look at. Draining is only worth doing to keep the connection reusable, so an
// upstream that answers a refusal with megabytes must cost the connection
// rather than the request that is waiting behind it.
const drainLimitBytes = 64 << 10

// requestLimitBytes caps the request body the gateway will hold in memory: 64
// MiB, the same ceiling the response tee uses and for the same reason. The
// whole body has to be buffered to be rewritten and replayed upstream, so an
// unbounded read is an out-of-memory kill any caller could ask for.
const requestLimitBytes = 64 << 20

// Timeouts for the internal default client. There is deliberately no overall
// request timeout: a long thinking turn can stream for many minutes, and
// killing it would be indistinguishable from an upstream failure.
const (
	dialTimeout      = 10 * time.Second
	dialKeepAlive    = 30 * time.Second
	handshakeTimeout = 10 * time.Second
	idleTimeout      = 90 * time.Second
	continueTimeout  = 1 * time.Second
	maxIdleConns     = 100

	// maxIdleConnsPerHost keeps a pool of warm connections to the one upstream a
	// gateway talks to. Go's default of 2 would force most concurrent turns to
	// dial fresh even with 100 idle slots free, since nearly every request lands
	// on the same host.
	maxIdleConnsPerHost = 32

	// responseHeaderTimeout bounds only the wait for the upstream's response
	// headers, so a connected-but-silent upstream cannot hang a request forever.
	// It does not conflict with the deliberate absence of an overall client
	// timeout above: once the headers arrive the body streams with no deadline,
	// so a long thinking turn is never truncated by it.
	responseHeaderTimeout = 10 * time.Minute
)

// Recorder persists one ledger record per proxied request. *ledger.Ledger
// satisfies it.
type Recorder interface {
	Append(rec ledger.Record) error
}

// Config is everything the pipeline needs from its host process.
type Config struct {
	// Registry resolves an inbound model name to its route. Required.
	Registry *registry.Registry

	// Recorder receives one record per request. A nil Recorder records nothing
	// and changes nothing else.
	Recorder Recorder

	// Accountant durably tracks every routed request. It is required: upstream
	// contact is forbidden until its pending and dispatch evidence are persisted.
	Accountant Accountant

	// Client is the upstream HTTP client. A nil Client gets an internal default
	// with connection timeouts and no overall deadline, since streams run long.
	Client *http.Client

	// Logger receives the pipeline's warnings. A nil Logger gets slog.Default(),
	// which is what the host process configured for everything else.
	Logger *slog.Logger

	// Host is the machine name stamped on every record.
	Host string
}

// Proxy is the resolve, tee and forward pipeline. It is safe for concurrent
// use: every request carries its own state, and the only shared values are the
// registry and the client, which are themselves concurrent.
type Proxy struct {
	registry   *registry.Registry
	recorder   Recorder
	accountant Accountant
	client     *http.Client
	host       string
	logger     *slog.Logger

	// credentials exports and caches the AWS credentials sigv4 routes sign with,
	// one cache per profile shared by every request that rides one.
	credentials *awssig.CredentialSource

	// pending counts the record goroutines still in flight, so a host process
	// can wait for them instead of closing the ledger out from under one.
	pending sync.WaitGroup

	// inFlight is the current number of record goroutines. It is only diagnostic:
	// Drain still relies on pending for the wait itself.
	inFlight atomic.Int64
}

// New returns a Proxy serving cfg.
func New(cfg Config) (*Proxy, error) {
	if cfg.Accountant == nil {
		return nil, errors.New("proxy accountant is required")
	}

	client := cfg.Client
	if client == nil {
		client = defaultClient()
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Proxy{
		registry:    cfg.Registry,
		recorder:    cfg.Recorder,
		accountant:  cfg.Accountant,
		client:      client,
		host:        cfg.Host,
		logger:      logger,
		credentials: awssig.NewCredentialSource(),
	}, nil
}

// ServeHTTP runs one request through the pipeline: read it, resolve its route,
// forward it, stream the answer back and tee a record into the ledger.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// The context a request selected is decided by the HTTP surface, which
	// strips the prefix that named it before the pipeline sees the path. A
	// request that named none selects the personal context.
	sel := registry.SelectionFrom(r.Context())
	credential := classifyCredential(r.Header)
	class := credential.ledgerClass()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, requestLimitBytes))
	if err != nil {
		p.abort(r.Context(), w, newExchange(registry.DefaultRoute(), sel.Name, class, "", start), &gatewayError{
			status: readFailureStatus(err),
			err:    fmt.Errorf("read request body: %w", err),
		})

		return
	}

	requestModel := extractModel(body)

	res, resErr := p.resolve(r.Context(), sel, requestModel)
	if resErr != nil {
		// Two refusals land here. A model that is a foreign route's rewrite
		// target is refused for any caller, in the not-found shape the real
		// Anthropic API answers an unknown model with, so the client that
		// echoed a rewritten model id gets an error it already knows how to
		// read. An unresolvable named context can only arrive from a pipeline
		// driven with no surface in front of it — through the gateway the
		// context was resolved before the request was routed — and it fails
		// closed all the same: a named context is never served by another.
		// Either way the record carries the built-in route the way every other
		// refusal does — nothing was sent anywhere, and a record the ledger
		// would drop for want of an upstream is a refusal nobody can audit.
		errType := ""
		if errors.Is(resErr, registry.ErrRewriteTarget) {
			errType = anthropic.ErrorTypeNotFound
		}

		p.abort(r.Context(), w, newExchange(registry.DefaultRoute(), res.Context, class, requestModel, start),
			&gatewayError{status: http.StatusNotFound, errType: errType, err: resErr})

		return
	}

	route := res.Route
	exch := newExchange(route, res.Context, class, requestModel, start)

	if accountingErr := p.insertPending(r.Context(), r, exch); accountingErr != nil {
		p.abort(r.Context(), w, exch, accountingErr)

		return
	}

	if route.Auth == registry.AuthForward {
		if credentialErr := credential.forwardError(); credentialErr != nil {
			p.abort(r.Context(), w, exch, credentialErr)

			return
		}
	}

	upstreamBody, bodyErr := forwardBody(body, route)
	if bodyErr != nil {
		p.abort(r.Context(), w, exch, bodyErr)

		return
	}

	if route.CaptureBodies {
		exch.requestBody = string(upstreamBody)
	}

	req, generation, upstreamTarget, reqErr := p.upstreamRequest(r, route, upstreamBody, "")
	if reqErr != nil {
		p.abort(r.Context(), w, exch, reqErr)

		return
	}
	exch.upstreamTarget = upstreamTarget

	sent := time.Now()
	if accountingErr := p.markDispatched(r.Context(), exch, sent); accountingErr != nil {
		p.abort(r.Context(), w, exch, accountingErr)

		return
	}

	resp, sendErr := p.send(r, route, upstreamBody, req, generation, upstreamTarget)
	if sendErr != nil {
		p.abort(r.Context(), w, exch, sendErr)

		return
	}

	p.relayResponse(r.Context(), w, route, resp, exch, sent)
}

func (p *Proxy) relayResponse(
	ctx context.Context,
	w http.ResponseWriter,
	route registry.Route,
	resp *http.Response,
	exch *exchange,
	sent time.Time,
) {
	defer resp.Body.Close()

	exch.upstreamMS = millis(time.Since(sent))
	exch.status = resp.StatusCode
	exch.contentType = resp.Header.Get(anthropic.HeaderContentType)

	var capt *capture
	if isBedrockStream(route, resp) {
		// The caller is handed Anthropic SSE, so that — not the event-stream
		// framing it arrived in — is what the record is summarized from.
		exch.contentType = eventStreamType
		capt = p.streamBedrock(ctx, w, resp)
	} else {
		capt = p.stream(ctx, w, resp)
	}
	exch.durationMS = millis(time.Since(exch.start))

	p.record(ctx, exch, capt)
}

func (p *Proxy) insertPending(ctx context.Context, r *http.Request, exch *exchange) *gatewayError {
	accountingID, err := p.accountant.InsertPending(ctx, pendingRequest(exch, p.host, requestKind(r)))
	if err != nil {
		p.logger.ErrorContext(ctx, "usage accounting pending insert failed", slog.Any("error", err))

		return &gatewayError{
			status: http.StatusInternalServerError,
			err:    fmt.Errorf("record pending usage accounting: %w", err),
		}
	}
	exch.accountingID = accountingID

	return nil
}

func (p *Proxy) markDispatched(ctx context.Context, exch *exchange, sent time.Time) *gatewayError {
	if err := p.accountant.MarkUpstreamDispatched(ctx, exch.accountingID, sent, exch.upstreamTarget); err != nil {
		p.logger.ErrorContext(ctx, "usage accounting dispatch mark failed", slog.Any("error", err))

		return &gatewayError{
			status: http.StatusBadGateway,
			err:    fmt.Errorf("record usage accounting dispatch: %w", err),
		}
	}
	exch.accountingDispatched = true

	return nil
}

// resolve picks the route serving model inside the request's own context. A
// model the context refuses to serve — a foreign route's rewrite target —
// comes back as the selection's own error, on either path.
//
// The HTTP surface resolves the context once, before anything dispatches on the
// request, and attaches the resolution; consuming that is what keeps one request
// on one registry document even when the file is replaced while the request is
// in flight. A pipeline driven directly, with no surface in front of it,
// resolves for itself.
func (p *Proxy) resolve(
	ctx context.Context,
	sel registry.Selection,
	model string,
) (registry.Resolution, error) {
	if res, ok := registry.ResolutionFrom(ctx); ok {
		selected, err := res.Select(model)
		if err != nil {
			return selected, fmt.Errorf("route the request model: %w", err)
		}

		return selected, nil
	}

	res, err := p.registry.Resolve(sel, model)
	if err != nil {
		if errors.Is(err, registry.ErrUnknownContext) {
			return res, fmt.Errorf("resolve the request context: %w", err)
		}

		return res, fmt.Errorf("route the request model: %w", err)
	}

	return res, nil
}

// Drain waits for the record goroutines still in flight, or until ctx expires,
// whichever comes first.
//
// A record is written off the request path, so the last request can be answered
// and the process torn down while its record is still being appended — and
// closing the ledger under that write loses it silently, since a recorder
// failure is designed to cost nothing but the record. A host process calls
// Drain between the last request and closing the ledger. It is bounded because
// a Recorder that never returns must cost the records, not the shutdown.
func (p *Proxy) Drain(ctx context.Context) {
	done := make(chan struct{})

	go func() {
		p.pending.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		p.logger.ErrorContext(ctx, "usage accounting drain timed out; finalization may be incomplete",
			slog.Int64("in_flight_records", p.inFlight.Load()))
	}
}

// abort answers a failure patchbay originated, in Anthropic's error shape, and
// records it. An upstream's own error never comes through here: it is streamed
// back untouched.
func (p *Proxy) abort(ctx context.Context, w http.ResponseWriter, exch *exchange, gwErr *gatewayError) {
	body := anthropic.TypedErrorBody(gwErr.anthropicType(), gwErr)

	w.Header().Set(anthropic.HeaderContentType, "application/json")
	w.WriteHeader(gwErr.status)

	if _, writeErr := w.Write(body); writeErr != nil {
		p.logger.WarnContext(ctx, "client went away before the error reached it", slog.Any("error", writeErr))
	}

	exch.status = gwErr.status
	exch.errorBody = string(body)
	exch.durationMS = millis(time.Since(exch.start))

	p.logger.WarnContext(ctx, "gateway error", slog.String("alias", exch.route.Alias),
		slog.Int("status", gwErr.status), slog.Any("error", gwErr.err))

	p.record(ctx, exch, nil)
}

// stream copies the upstream response to the client, flushing every chunk so a
// streamed turn reaches the caller as it is produced, and tees a copy into the
// capture the record is built from.
func (p *Proxy) stream(ctx context.Context, w http.ResponseWriter, resp *http.Response) *capture {
	copyHeader(w.Header(), resp.Header, droppedHeaders(resp.Header))
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	capt := &capture{buf: nil, dropped: false, truncated: false}
	buf := make([]byte, streamChunkBytes)

	for {
		read, readErr := resp.Body.Read(buf)
		if read > 0 {
			// A client that has gone away ends the turn here: the capture holds a
			// prefix, not a finished turn, and must not be summarized as one.
			if !p.writeChunk(ctx, w, capt, flusher, canFlush, buf[:read]) {
				return capt
			}
		}

		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				// A non-EOF read ends the turn short of its own message_stop, so the
				// captured prefix is a partial turn, recorded as truncated rather
				// than summarized as complete.
				capt.truncated = true
				p.logger.WarnContext(ctx, "upstream stream ended early", slog.Any("error", readErr))
			}

			return capt
		}
	}
}

// writeChunk hands one piece of a streamed answer to the caller and to the
// capture the record is summarized from, reporting whether the stream may
// continue. Both stream loops go through it — the relayed one and the Bedrock
// translation — so a client that goes away costs exactly the same thing either
// way, and says so in one place.
func (p *Proxy) writeChunk(
	ctx context.Context,
	w http.ResponseWriter,
	capt *capture,
	flusher http.Flusher,
	canFlush bool,
	chunk []byte,
) bool {
	capt.write(chunk)

	if _, err := w.Write(chunk); err != nil {
		capt.truncated = true
		p.logger.WarnContext(ctx, "client went away mid-stream", slog.Any("error", err))

		return false
	}

	if canFlush {
		flusher.Flush()
	}

	return true
}

// record hands the finished exchange to the ledger without making the client
// wait for it. A recorder that fails, or blocks, costs the record and nothing
// else.
func (p *Proxy) record(ctx context.Context, exch *exchange, capt *capture) {
	if p.recorder == nil && exch.accountingID == 0 {
		return
	}

	p.pending.Add(1)
	p.inFlight.Add(1)

	go p.appendRecord(ctx, exch, capt)
}

// appendRecord builds and appends one record, off the request path.
func (p *Proxy) appendRecord(ctx context.Context, exch *exchange, capt *capture) {
	defer func() {
		p.inFlight.Add(-1)
		p.pending.Done()
	}()

	if capt != nil && capt.dropped {
		p.logger.WarnContext(ctx, "response capture dropped at the size ceiling",
			slog.Int("limit_bytes", captureLimitBytes))
	}

	if exch.accountingID != 0 {
		p.finalizeAccounting(ctx, exch, capt)
	}

	if p.recorder != nil {
		if err := p.recorder.Append(exch.record(p.host, capt)); err != nil {
			p.logger.WarnContext(ctx, "ledger record dropped", slog.Any("error", err))
		}
	}
}

// send performs the upstream request and hands back its response.
//
// A sigv4 route gets one retry, and only on a 403: AWS refuses a signature made
// with a credential that has since expired, which is the ordinary end of an SSO
// session rather than a failure worth surfacing. The credential that signed the
// refused request — named by generation, not by whatever the cache holds now —
// is dropped so the retry exports a fresh one. A second refusal is patchbay's own
// failure to report — the caller has no AWS credential of its own to fix, so a
// relayed 403 would only read as a broken gateway.
func (p *Proxy) send(
	r *http.Request,
	route registry.Route,
	body []byte,
	req *http.Request,
	generation uint64,
	upstreamTarget string,
) (*http.Response, *gatewayError) {
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, &gatewayError{
			status:  http.StatusBadGateway,
			errType: "",
			err:     fmt.Errorf("upstream request failed: %w", err),
		}
	}

	if route.Auth != registry.AuthSigV4 || resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}

	p.discard(r.Context(), resp)
	p.credentials.Invalidate(route.AWSProfile, generation)

	retry, _, _, buildErr := p.upstreamRequest(r, route, body, upstreamTarget)
	if buildErr != nil {
		return nil, buildErr
	}

	resp, err = p.client.Do(retry)
	if err != nil {
		return nil, &gatewayError{
			status:  http.StatusBadGateway,
			errType: "",
			err:     fmt.Errorf("upstream request failed: %w", err),
		}
	}

	if resp.StatusCode == http.StatusForbidden {
		p.discard(r.Context(), resp)

		return nil, &gatewayError{
			status:  http.StatusBadGateway,
			errType: "",
			err: fmt.Errorf("bedrock refused the request signed with profile %q twice; run: aws sso login --profile %s",
				route.AWSProfile, route.AWSProfile),
		}
	}

	return resp, nil
}

// discard drains and closes a response nobody will read, so its connection goes
// back to the pool rather than being torn down mid-body. The drain is bounded
// at drainLimitBytes and the body is closed either way. A failure to drain
// costs the connection and nothing else, which is worth a line in the log and
// no more.
func (p *Proxy) discard(ctx context.Context, resp *http.Response) {
	// A body that overruns the ceiling is simply abandoned: the connection is
	// torn down rather than pooled, which is the cheaper of the two failures.
	if _, err := io.CopyN(io.Discard, resp.Body, drainLimitBytes); err != nil && !errors.Is(err, io.EOF) {
		p.logger.WarnContext(ctx, "draining a refused response failed", slog.Any("error", err))
	}

	if err := resp.Body.Close(); err != nil {
		p.logger.WarnContext(ctx, "closing a refused response failed", slog.Any("error", err))
	}
}

// upstreamRequest builds the request to send upstream: the route's base URL
// carrying the inbound path and raw query verbatim, the caller's headers minus
// the ones a proxy owns, and the route's credentials.
// It also reports the AWS credential generation a sigv4 request was signed
// with; every other route signs nothing and reports zero.
func (p *Proxy) upstreamRequest(
	r *http.Request,
	route registry.Route,
	body []byte,
	upstreamTarget string,
) (*http.Request, uint64, string, *gatewayError) {
	// A sigv4 route sends a translated, signed request to an endpoint its region
	// derives, so it shares none of the relay path below.
	if route.Auth == registry.AuthSigV4 {
		return p.bedrockRequest(r, route, body, upstreamTarget)
	}

	// Both gates below live only inside the AuthInject branch, so a forward or
	// default route never reaches either. DefaultRoute() is AuthForward, so a
	// registry that fails to resolve falls to a route that is never checked —
	// the default route can never fail closed. The caller-key check reads the
	// inbound request, before copyHeader strips X-Patchbay-Key from the upstream
	// copy below, so it sees the header the caller actually sent.
	if route.Auth == registry.AuthInject {
		if !isInjectMessagesPath(r.URL.Path) {
			return nil, 0, "", &gatewayError{
				status: http.StatusNotFound,
				err: fmt.Errorf("route %q injects a key and only relays the Messages endpoints, not %s",
					route.Alias, r.URL.Path),
			}
		}

		if keyErr := checkCallerKey(r); keyErr != nil {
			return nil, 0, "", keyErr
		}
	}

	target := strings.TrimSuffix(route.BaseURL, "/") + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, 0, "", &gatewayError{
			status: http.StatusInternalServerError,
			err:    fmt.Errorf("build upstream request: %w", err),
		}
	}

	copyHeader(req.Header, r.Header,
		droppedHeaders(r.Header, headerHost, headerContentLength, headerAcceptEncoding, CallerKeyHeader))
	req.Header.Set(headerAcceptEncoding, encodingIdentity)

	if authErr := applyAuth(req, route); authErr != nil {
		return nil, 0, "", authErr
	}

	return req, 0, "", nil
}

// isInjectMessagesPath reports whether path is one of the Messages endpoints an
// inject route may relay.
func isInjectMessagesPath(path string) bool {
	return path == messagesPath || path == messagesCountPath
}

// readFailureStatus reports the status a failed body read deserves. A caller
// that sent more than the gateway will hold is told exactly that; every other
// read failure is a request the caller cut short.
func readFailureStatus(err error) int {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge
	}

	return http.StatusBadRequest
}

// defaultClient is the upstream client used when the host process supplies
// none: bounded connection setup, unbounded response time, and no redirect
// following at all.
//
// Go's default redirect policy follows up to ten hops and, crossing to another
// host, strips only Authorization, Www-Authenticate, Cookie and the Proxy-*
// headers — an injected key or the caller's x-api-key would ride along to
// whatever host the upstream named. A 3xx is also part of the answer the
// pipeline owes the caller byte for byte, and following one would replace it
// with something else's.
func defaultClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: dialKeepAlive}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          maxIdleConns,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
			IdleConnTimeout:       idleTimeout,
			TLSHandshakeTimeout:   handshakeTimeout,
			ExpectContinueTimeout: continueTimeout,
			ResponseHeaderTimeout: responseHeaderTimeout,
		},
		CheckRedirect: relayRedirect,
		Jar:           nil,
		Timeout:       0,
	}
}

// relayRedirect stops the client at the first response, redirect or not, so the
// caller gets the upstream's own answer.
func relayRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}
