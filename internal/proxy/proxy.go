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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// streamChunkBytes is the read size of the response copy loop. Every chunk is
// written and flushed as it arrives, so this bounds latency, not throughput.
const streamChunkBytes = 32768

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
		registry: cfg.Registry,
		recorder: cfg.Recorder,
		client:   client,
		host:     cfg.Host,
		logger:   logger,
	}
}

// ServeHTTP runs one request through the pipeline: read it, resolve its route,
// forward it, stream the answer back and tee a record into the ledger.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, requestLimitBytes))
	if err != nil {
		p.abort(w, newExchange(registry.DefaultRoute(), "", start), &gatewayError{
			status: readFailureStatus(err),
			err:    fmt.Errorf("read request body: %w", err),
		})

		return
	}

	requestModel := extractModel(body)
	route := p.registry.Resolve(requestModel)
	exch := newExchange(route, requestModel, start)

	upstreamBody, bodyErr := forwardBody(body, route)
	if bodyErr != nil {
		p.abort(w, exch, bodyErr)

		return
	}

	if route.CaptureBodies {
		exch.requestBody = string(upstreamBody)
	}

	req, reqErr := upstreamRequest(r, route, upstreamBody)
	if reqErr != nil {
		p.abort(w, exch, reqErr)

		return
	}

	sent := time.Now()

	resp, doErr := p.client.Do(req)
	if doErr != nil {
		p.abort(w, exch, &gatewayError{
			status: http.StatusBadGateway,
			err:    fmt.Errorf("upstream request failed: %w", doErr),
		})

		return
	}

	defer resp.Body.Close()

	exch.upstreamMS = millis(time.Since(sent))
	exch.status = resp.StatusCode
	exch.contentType = resp.Header.Get(headerContentType)

	capt := p.stream(w, resp)
	exch.durationMS = millis(time.Since(start))

	p.record(exch, capt)
}

// abort answers a failure patchbay originated, in Anthropic's error shape, and
// records it. An upstream's own error never comes through here: it is streamed
// back untouched.
func (p *Proxy) abort(w http.ResponseWriter, exch *exchange, gwErr *gatewayError) {
	body := errorBody(gwErr)

	w.Header().Set(headerContentType, "application/json")
	w.WriteHeader(gwErr.status)

	if _, writeErr := w.Write(body); writeErr != nil {
		p.logger.Warn("client went away before the error reached it", slog.Any("error", writeErr))
	}

	exch.status = gwErr.status
	exch.errorBody = string(body)
	exch.durationMS = millis(time.Since(exch.start))

	p.logger.Warn("gateway error", slog.String("alias", exch.route.Alias),
		slog.Int("status", gwErr.status), slog.Any("error", gwErr.err))

	p.record(exch, nil)
}

// stream copies the upstream response to the client, flushing every chunk so a
// streamed turn reaches the caller as it is produced, and tees a copy into the
// capture the record is built from.
func (p *Proxy) stream(w http.ResponseWriter, resp *http.Response) *capture {
	copyHeader(w.Header(), resp.Header, droppedHeaders(resp.Header))
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	capt := &capture{buf: nil, dropped: false}
	buf := make([]byte, streamChunkBytes)

	for {
		read, readErr := resp.Body.Read(buf)
		if read > 0 {
			chunk := buf[:read]
			capt.write(chunk)

			if _, writeErr := w.Write(chunk); writeErr != nil {
				p.logger.Warn("client went away mid-stream", slog.Any("error", writeErr))

				return capt
			}

			if canFlush {
				flusher.Flush()
			}
		}

		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				p.logger.Warn("upstream stream ended early", slog.Any("error", readErr))
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

	go p.appendRecord(exch, capt)
}

// appendRecord builds and appends one record, off the request path.
func (p *Proxy) appendRecord(exch *exchange, capt *capture) {
	if capt != nil && capt.dropped {
		p.logger.Warn("response capture dropped at the size ceiling",
			slog.Int("limit_bytes", captureLimitBytes))
	}

	if err := p.recorder.Append(exch.record(p.host, capt)); err != nil {
		p.logger.Warn("ledger record dropped", slog.Any("error", err))
	}
}

// upstreamRequest builds the request to send upstream: the route's base URL
// carrying the inbound path and raw query verbatim, the caller's headers minus
// the ones a proxy owns, and the route's credentials.
func upstreamRequest(r *http.Request, route registry.Route, body []byte) (*http.Request, *gatewayError) {
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

	copyHeader(req.Header, r.Header, droppedHeaders(r.Header, headerHost, headerContentLength, headerAcceptEncoding))
	req.Header.Set(headerAcceptEncoding, encodingIdentity)

	if authErr := applyAuth(req, route); authErr != nil {
		return nil, authErr
	}

	return req, nil
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
// none: bounded connection setup, unbounded response time.
func defaultClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: dialKeepAlive}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          maxIdleConns,
			IdleConnTimeout:       idleTimeout,
			TLSHandshakeTimeout:   handshakeTimeout,
			ExpectContinueTimeout: continueTimeout,
		},
		CheckRedirect: nil,
		Jar:           nil,
		Timeout:       0,
	}
}
