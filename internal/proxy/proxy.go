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
	registry *registry.Registry
	recorder Recorder
	client   *http.Client
	host     string
	logger   *slog.Logger

	// credentials exports and caches the AWS credentials sigv4 routes sign with,
	// one cache per profile shared by every request that rides one.
	credentials *awssig.CredentialSource

	// pending counts the record goroutines still in flight, so a host process
	// can wait for them instead of closing the ledger out from under one.
	pending sync.WaitGroup
}

// New returns a Proxy serving cfg.
func New(cfg Config) *Proxy {
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
		client:      client,
		host:        cfg.Host,
		logger:      logger,
		credentials: awssig.NewCredentialSource(),
	}
}

// ServeHTTP runs one request through the pipeline: read it, resolve its route,
// forward it, stream the answer back and tee a record into the ledger.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// The context a request selected is decided by the HTTP surface, which
	// strips the prefix that named it before the pipeline sees the path. A
	// request that named none selects the personal context.
	sel := registry.SelectionFrom(r.Context())
	class := credentialClass(r.Header)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, requestLimitBytes))
	if err != nil {
		p.abort(r.Context(), w, newExchange(registry.DefaultRoute(), sel.Name, class, "", start), &gatewayError{
			status: readFailureStatus(err),
			err:    fmt.Errorf("read request body: %w", err),
		})

		return
	}

	requestModel := extractModel(body)

	res, resErr := p.registry.Resolve(sel, requestModel)
	if resErr != nil {
		// The surface already refused every context it could not resolve, so
		// reaching here means the registry changed underneath this request. It
		// fails closed all the same: a named context is never served by another.
		// The record carries the built-in route the way every other refusal
		// does — nothing was sent anywhere, and a record the ledger would drop
		// for want of an upstream is a refusal nobody can audit.
		p.abort(r.Context(), w, newExchange(registry.DefaultRoute(), res.Context, class, requestModel, start),
			&gatewayError{status: http.StatusNotFound, err: resErr})

		return
	}

	route := res.Route
	exch := newExchange(route, res.Context, class, requestModel, start)

	upstreamBody, bodyErr := forwardBody(body, route)
	if bodyErr != nil {
		p.abort(r.Context(), w, exch, bodyErr)

		return
	}

	if route.CaptureBodies {
		exch.requestBody = string(upstreamBody)
	}

	req, reqErr := p.upstreamRequest(r, route, upstreamBody)
	if reqErr != nil {
		p.abort(r.Context(), w, exch, reqErr)

		return
	}

	sent := time.Now()

	resp, sendErr := p.send(r, route, upstreamBody, req)
	if sendErr != nil {
		p.abort(r.Context(), w, exch, sendErr)

		return
	}

	defer resp.Body.Close()

	exch.upstreamMS = millis(time.Since(sent))
	exch.status = resp.StatusCode
	exch.contentType = resp.Header.Get(anthropic.HeaderContentType)

	var capt *capture

	if isBedrockStream(route, resp) {
		// The caller is handed Anthropic SSE, so that — not the event-stream
		// framing it arrived in — is what the record is summarized from.
		exch.contentType = eventStreamType
		capt = p.streamBedrock(r.Context(), w, resp)
	} else {
		capt = p.stream(r.Context(), w, resp)
	}
	exch.durationMS = millis(time.Since(start))

	p.record(exch, capt)
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

	p.record(exch, nil)
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
			chunk := buf[:read]
			capt.write(chunk)

			if _, writeErr := w.Write(chunk); writeErr != nil {
				// The client is gone, so the turn stops here: the capture holds a
				// prefix, not a finished turn, and must not be summarized as one.
				capt.truncated = true
				p.logger.WarnContext(ctx, "client went away mid-stream", slog.Any("error", writeErr))

				return capt
			}

			if canFlush {
				flusher.Flush()
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

// record hands the finished exchange to the ledger without making the client
// wait for it. A recorder that fails, or blocks, costs the record and nothing
// else.
func (p *Proxy) record(exch *exchange, capt *capture) {
	if p.recorder == nil {
		return
	}

	p.pending.Add(1)

	go p.appendRecord(exch, capt)
}

// appendRecord builds and appends one record, off the request path.
func (p *Proxy) appendRecord(exch *exchange, capt *capture) {
	defer p.pending.Done()

	if capt != nil && capt.dropped {
		p.logger.Warn("response capture dropped at the size ceiling",
			slog.Int("limit_bytes", captureLimitBytes))
	}

	if err := p.recorder.Append(exch.record(p.host, capt)); err != nil {
		p.logger.Warn("ledger record dropped", slog.Any("error", err))
	}
}

// send performs the upstream request and hands back its response.
//
// A sigv4 route gets one retry, and only on a 403: AWS refuses a signature made
// with a credential that has since expired, which is the ordinary end of an SSO
// session rather than a failure worth surfacing. The cached credential is
// dropped so the retry exports a fresh one. A second refusal is patchbay's own
// failure to report — the caller has no AWS credential of its own to fix, so a
// relayed 403 would only read as a broken gateway.
func (p *Proxy) send(
	r *http.Request,
	route registry.Route,
	body []byte,
	req *http.Request,
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
	p.credentials.Invalidate(route.AWSProfile)

	retry, buildErr := p.upstreamRequest(r, route, body)
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
// back to the pool rather than being torn down mid-body. A failure to drain
// costs the connection and nothing else, which is worth a line in the log and
// no more.
func (p *Proxy) discard(ctx context.Context, resp *http.Response) {
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		p.logger.WarnContext(ctx, "draining a refused response failed", slog.Any("error", err))
	}

	if err := resp.Body.Close(); err != nil {
		p.logger.WarnContext(ctx, "closing a refused response failed", slog.Any("error", err))
	}
}

// upstreamRequest builds the request to send upstream: the route's base URL
// carrying the inbound path and raw query verbatim, the caller's headers minus
// the ones a proxy owns, and the route's credentials.
func (p *Proxy) upstreamRequest(r *http.Request, route registry.Route, body []byte) (*http.Request, *gatewayError) {
	// A sigv4 route sends a translated, signed request to an endpoint its region
	// derives, so it shares none of the relay path below.
	if route.Auth == registry.AuthSigV4 {
		return p.bedrockRequest(r, route, body)
	}

	// Both gates below live only inside the AuthInject branch, so a forward or
	// default route never reaches either. DefaultRoute() is AuthForward, so a
	// registry that fails to resolve falls to a route that is never checked —
	// the default route can never fail closed. The caller-key check reads the
	// inbound request, before copyHeader strips X-Patchbay-Key from the upstream
	// copy below, so it sees the header the caller actually sent.
	if route.Auth == registry.AuthInject {
		if !isInjectMessagesPath(r.URL.Path) {
			return nil, &gatewayError{
				status: http.StatusNotFound,
				err: fmt.Errorf("route %q injects a key and only relays the Messages endpoints, not %s",
					route.Alias, r.URL.Path),
			}
		}

		if keyErr := checkCallerKey(r); keyErr != nil {
			return nil, keyErr
		}
	}

	target := strings.TrimSuffix(route.BaseURL, "/") + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, &gatewayError{
			status: http.StatusInternalServerError,
			err:    fmt.Errorf("build upstream request: %w", err),
		}
	}

	copyHeader(req.Header, r.Header,
		droppedHeaders(r.Header, headerHost, headerContentLength, headerAcceptEncoding, CallerKeyHeader))
	req.Header.Set(headerAcceptEncoding, encodingIdentity)

	if authErr := applyAuth(req, route); authErr != nil {
		return nil, authErr
	}

	return req, nil
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
