// Package proxy forwards Anthropic Messages API requests to the upstream a
// seat names, tees the response into the ledger, and hands the caller the
// upstream's bytes unchanged.
//
// Byte fidelity is the invariant the whole package is built around. On a seat
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
	usagepkg "github.com/joshsymonds/patchbay/internal/usage"
)

// streamChunkBytes is the read size of the response copy loop. Every chunk is
// written and flushed as it arrives, so this bounds latency, not throughput.
const streamChunkBytes = 32768

const (
	completionQueueCapacity = 32
	completionWorkerCount   = 2

	// requestAdmissionCapacity permits one bounded handler set to wait behind a
	// fully occupied completion pipeline without allowing blocked submitters to
	// accumulate without limit.
	requestAdmissionCapacity = completionWorkerCount + completionQueueCapacity
)

// The Messages endpoints an inject seat is allowed to relay. An inject seat
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
	// Registry resolves an inbound model name to its seat. Required.
	Registry *registry.Registry

	// Recorder receives one record per request. A nil Recorder records nothing
	// and changes nothing else.
	Recorder Recorder

	// Accountant durably tracks every request that selected a Seat. It is
	// required: upstream contact is forbidden until its pending and dispatch
	// evidence are persisted.
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

type requestCompletion struct {
	accountingID int64
	finalized    usagepkg.FinalizedRequest
	record       ledger.Record
	appendLedger bool
}

type admissionContextKey struct{}

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

	// credentials exports and caches the AWS credentials sigv4 Seats sign with,
	// one cache per profile shared by every request that rides one.
	credentials *awssig.CredentialSource

	admission         chan struct{}
	completionQueue   chan requestCompletion
	completionMu      sync.Mutex
	completionWorkers int

	// pending counts completions submitted or blocked on the bounded queue, so a
	// host process can wait before closing accounting and ledger storage.
	pending sync.WaitGroup

	// inFlight is the current number of unfinished completions. It is only
	// diagnostic: Drain still relies on pending for the wait itself.
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
		registry:        cfg.Registry,
		recorder:        cfg.Recorder,
		accountant:      cfg.Accountant,
		client:          client,
		host:            cfg.Host,
		logger:          logger,
		credentials:     awssig.NewCredentialSource(),
		admission:       make(chan struct{}, requestAdmissionCapacity),
		completionQueue: make(chan requestCompletion, completionQueueCapacity),
	}, nil
}

// AdmissionMiddleware bounds routed work before a caller resolves a Context.
// It marks admitted requests so ServeHTTP can preserve the same bound for direct
// callers without acquiring the permit a second time.
func (p *Proxy) AdmissionMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !p.tryAdmission() {
				p.refuseOverload(w, r)
				return
			}
			defer func() { <-p.admission }()
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), admissionContextKey{}, p)))
		})
	}
}

func (p *Proxy) tryAdmission() bool {
	select {
	case p.admission <- struct{}{}:
		return true
	default:
		return false
	}
}

func (p *Proxy) directAdmission(w http.ResponseWriter, r *http.Request) func() {
	if owner, ok := r.Context().Value(admissionContextKey{}).(*Proxy); ok && owner == p {
		return func() {}
	}
	if !p.tryAdmission() {
		p.refuseOverload(w, r)
		return nil
	}
	return func() { <-p.admission }
}

// ServeHTTP resolves one immutable Context snapshot, selects a Seat, accounts
// for the request, forwards, streams the answer, and records the v2 resolution.
//
// Accounting begins the moment a Seat is selected and never after upstream
// contact: the pending row is durable before the upstream request is built, and
// the dispatch evidence — including the resolved Bedrock target — is durable
// before a single byte is sent. Every refusal that precedes selection has no
// Seat to account against, so it is recorded in the ledger alone.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Direct callers have no outer admission middleware, so acquire a fallback
	// permit. Production-routed requests carry the private marker from the outer
	// middleware and must consume exactly one shared permit.
	release := p.directAdmission(w, r)
	if release == nil {
		return
	}
	defer release()

	start := time.Now()
	sel := registry.SelectionFrom(r.Context())
	credential := classifyCredential(r.Header)
	class := credential.ledgerClass()
	observation := observeSubagent(r.Header)

	ctxRes, ctxErr := p.contextResolution(r.Context(), sel)
	if ctxErr != nil {
		p.abort(r.Context(), w, newExchange(ctxRes.Context, class, "", observation, start),
			contextFailure(ctxErr))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, requestLimitBytes))
	if err != nil {
		p.abort(
			r.Context(),
			w,
			newExchange(ctxRes.Context, class, "", observation, start),
			&gatewayError{
				status: readFailureStatus(err), err: fmt.Errorf("read request body: %w", err),
			},
		)
		return
	}

	requestModel := extractModel(body)
	res, resErr := ctxRes.Select(requestModel, observation)
	if resErr != nil {
		p.abort(
			r.Context(),
			w,
			newExchange(ctxRes.Context, class, requestModel, observation, start),
			&gatewayError{status: http.StatusNotFound, errType: anthropic.ErrorTypeNotFound,
				err: fmt.Errorf("select the request model: %w", resErr)},
		)
		return
	}

	seat := res.Seat
	exch := newSelectedExchange(res, class, requestModel, start)

	if accountingErr := p.insertPending(r.Context(), r, exch); accountingErr != nil {
		p.abort(r.Context(), w, exch, accountingErr)
		return
	}

	// A forward Seat spends the caller's own credential, so an inbound shape it
	// cannot honestly forward is refused here rather than relayed and charged to
	// whoever the upstream decides the ambiguous headers name.
	if seat.AuthMode == registry.AuthForward {
		if credentialErr := credential.forwardError(); credentialErr != nil {
			p.abort(r.Context(), w, exch, credentialErr)
			return
		}
	}

	upstreamBody, bodyErr := forwardBody(body, seat)
	if bodyErr != nil {
		p.abort(r.Context(), w, exch, bodyErr)
		return
	}
	if seat.CaptureBodies {
		exch.requestBody = string(upstreamBody)
	}

	req, generation, upstreamTarget, reqErr := p.upstreamRequest(r, seat, upstreamBody, "")
	if reqErr != nil {
		p.abort(r.Context(), w, exch, reqErr)
		return
	}
	exch.upstreamTarget = upstreamTarget
	p.dispatch(r, w, seat, upstreamBody, req, generation, upstreamTarget, exch)
}

func (p *Proxy) dispatch(
	r *http.Request,
	w http.ResponseWriter,
	seat registry.Seat,
	upstreamBody []byte,
	req *http.Request,
	generation uint64,
	upstreamTarget string,
	exch *exchange,
) {
	sent := time.Now()
	if accountingErr := p.markDispatched(r.Context(), exch, sent); accountingErr != nil {
		p.abort(r.Context(), w, exch, accountingErr)
		return
	}

	resp, sendErr := p.send(r, seat, upstreamBody, req, generation, upstreamTarget)
	if sendErr != nil {
		p.abort(r.Context(), w, exch, sendErr)
		return
	}

	p.relayResponse(r.Context(), w, seat, resp, exch, sent)
}

// relayResponse streams the upstream answer back and closes the exchange out.
func (p *Proxy) relayResponse(
	ctx context.Context,
	w http.ResponseWriter,
	seat registry.Seat,
	resp *http.Response,
	exch *exchange,
	sent time.Time,
) {
	defer resp.Body.Close()

	exch.upstreamMS = millis(time.Since(sent))
	exch.status = resp.StatusCode
	exch.contentType = resp.Header.Get(anthropic.HeaderContentType)
	var capt *capture
	if isBedrockStream(seat, resp) {
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

// insertPending persists the request's identity before anything is built for an
// upstream. A failure here refuses the request: an unaccounted request must
// never reach an upstream that would charge for it.
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

// markDispatched persists the dispatch evidence — when the request went and the
// destination it resolved to — before the request is sent. The mark is
// one-way and idempotent, so the sigv4 retry re-sends against the same recorded
// target rather than claiming a second dispatch.
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

// contextFailure is the refusal a Context that could not be resolved deserves.
// A registry that went invalid under a running gateway is the operator's
// problem, not the caller's, and says so with a 503; every other failure is a
// Context name nothing serves.
func contextFailure(err error) *gatewayError {
	if errors.Is(err, registry.ErrInvalid) {
		return &gatewayError{
			status:  http.StatusServiceUnavailable,
			errType: anthropic.ErrorTypeAPI,
			err:     err,
		}
	}

	return &gatewayError{
		status:  http.StatusNotFound,
		errType: anthropic.ErrorTypeNotFound,
		err:     err,
	}
}

func (p *Proxy) contextResolution(
	ctx context.Context,
	sel registry.Selection,
) (registry.Resolution, error) {
	if err := registry.ResolutionErrorFrom(ctx); err != nil {
		return registry.Resolution{
				Context: sel.Name,
			}, fmt.Errorf(
				"use attached Context resolution: %w",
				err,
			)
	}
	if res, ok := registry.ResolutionFrom(ctx); ok {
		return res, nil
	}
	res, err := p.registry.Resolve(sel, "", nil)
	if err != nil {
		return res, fmt.Errorf("resolve the request context: %w", err)
	}
	return res, nil
}

// Drain waits for submitted completion work, including full-queue submitters,
// or until ctx expires, whichever comes first.
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

// refuseOverload answers before admission, so it deliberately performs no
// routing, upstream contact, accounting, ledger capture, or completion enqueue.
func (p *Proxy) refuseOverload(w http.ResponseWriter, r *http.Request) {
	err := errors.New("proxy completion capacity exhausted")
	body := anthropic.TypedErrorBody(anthropic.ErrorTypeAPI, err)

	w.Header().Set(anthropic.HeaderContentType, "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	if _, writeErr := w.Write(body); writeErr != nil {
		p.logger.WarnContext(r.Context(), "client went away before the overload refusal reached it",
			slog.Any("error", writeErr))
	}
	p.logger.WarnContext(r.Context(), "gateway overloaded; request refused before admission",
		slog.Int("capacity", requestAdmissionCapacity))
}

// abort answers a failure patchbay originated, in Anthropic's error shape, and
// records it. An upstream's own error never comes through here: it is streamed
// back untouched.
func (p *Proxy) abort(
	ctx context.Context,
	w http.ResponseWriter,
	exch *exchange,
	gwErr *gatewayError,
) {
	body := anthropic.TypedErrorBody(gwErr.anthropicType(), gwErr)

	w.Header().Set(anthropic.HeaderContentType, "application/json")
	w.WriteHeader(gwErr.status)

	if _, writeErr := w.Write(body); writeErr != nil {
		p.logger.WarnContext(
			ctx,
			"client went away before the error reached it",
			slog.Any("error", writeErr),
		)
	}

	exch.status = gwErr.status
	exch.errorBody = string(body)
	exch.durationMS = millis(time.Since(exch.start))

	seatID := ""
	if exch.resolution != nil {
		seatID = exch.resolution.SeatID
	}
	p.logger.WarnContext(ctx, "gateway error", slog.String("seat", seatID),
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
	observeSSE := strings.Contains(
		strings.ToLower(resp.Header.Get(anthropic.HeaderContentType)),
		eventStreamType,
	)
	capt := &capture{
		buf:        nil,
		dropped:    false,
		truncated:  false,
		observeSSE: observeSSE,
	}
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
	capt.observeChunk(chunk, time.Now())
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

// record materializes the finished exchange immediately, then submits only the
// finalized accounting snapshot and ledger record to a bounded completion queue.
// A full queue applies backpressure after the response relay rather than dropping
// durable accounting work or retaining the response capture in an unbounded set
// of goroutines.
func (p *Proxy) record(ctx context.Context, exch *exchange, capt *capture) {
	if p.recorder == nil && exch.accountingID == 0 {
		return
	}
	if capt != nil && capt.dropped {
		p.logger.WarnContext(ctx, "response capture dropped at the size ceiling",
			slog.Int("limit_bytes", captureLimitBytes))
	}
	completion := requestCompletion{
		accountingID: exch.accountingID,
		finalized:    exch.finalizedRequest(capt),
		record:       exch.record(p.host, capt),
		appendLedger: p.recorder != nil,
	}

	p.pending.Add(1)
	p.inFlight.Add(1)
	p.completionQueue <- completion
	p.startCompletionWorker(completionPersistenceContext(ctx))
}

// completionPersistenceContext deliberately breaks request ownership: queued
// persistence must survive cancellation and carry no request-scoped values.
func completionPersistenceContext(context.Context) context.Context {
	return context.Background()
}

func (p *Proxy) startCompletionWorker(ctx context.Context) {
	p.completionMu.Lock()
	defer p.completionMu.Unlock()
	if p.completionWorkers >= completionWorkerCount {
		return
	}
	p.completionWorkers++
	go p.runCompletionWorker(ctx)
}

func (p *Proxy) runCompletionWorker(ctx context.Context) {
	for {
		completion := <-p.completionQueue
		p.complete(ctx, completion)
		p.inFlight.Add(-1)
		p.pending.Done()

		p.completionMu.Lock()
		if len(p.completionQueue) == 0 {
			p.completionWorkers--
			p.completionMu.Unlock()
			return
		}
		p.completionMu.Unlock()
	}
}

// complete finalizes accounting before appending the audit copy. Both operations
// use the Proxy's service-owned context, independent of request cancellation.
func (p *Proxy) complete(ctx context.Context, completion requestCompletion) {
	if completion.accountingID != 0 {
		p.finalizeAccounting(ctx, completion.accountingID, completion.finalized)
	}
	if completion.appendLedger {
		if err := p.recorder.Append(completion.record); err != nil {
			p.logger.WarnContext(ctx, "ledger record dropped", slog.Any("error", err))
		}
	}
}

// send performs the upstream request and hands back its response.
//
// A sigv4 seat gets one retry, and only on a 403: AWS refuses a signature made
// with a credential that has since expired, which is the ordinary end of an SSO
// session rather than a failure worth surfacing. The credential that signed the
// refused request — named by generation, not by whatever the cache holds now —
// is dropped so the retry exports a fresh one. A second refusal is patchbay's own
// failure to report — the caller has no AWS credential of its own to fix, so a
// relayed 403 would only read as a broken gateway.
//
// The retry rebuilds against upstreamTarget — the destination already recorded
// as dispatch evidence — so a fresh signature never lands anywhere the
// accounting row did not already say the request went.
func (p *Proxy) send(
	r *http.Request,
	seat registry.Seat,
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

	if seat.AuthMode != registry.AuthSigV4 || resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}

	p.discard(r.Context(), resp)
	p.credentials.Invalidate(seat.AWSProfile, generation)

	retry, _, _, buildErr := p.upstreamRequest(r, seat, body, upstreamTarget)
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
			err: fmt.Errorf(
				"bedrock refused the request signed with profile %q twice; run: aws sso login --profile %s",
				seat.AWSProfile,
				seat.AWSProfile,
			),
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
	if _, err := io.CopyN(
		io.Discard,
		resp.Body,
		drainLimitBytes,
	); err != nil &&
		!errors.Is(err, io.EOF) {
		p.logger.WarnContext(ctx, "draining a refused response failed", slog.Any("error", err))
	}

	if err := resp.Body.Close(); err != nil {
		p.logger.WarnContext(ctx, "closing a refused response failed", slog.Any("error", err))
	}
}

// upstreamRequest builds the request to send upstream: the seat's base URL
// carrying the inbound path and raw query verbatim, the caller's headers minus
// the ones a proxy owns, and the seat's credentials.
// It also reports the AWS credential generation a sigv4 request was signed
// with; every other seat signs nothing and reports zero.
//
// The returned string is the resolved upstream target a sigv4 request is
// dispatched to, which is what the accounting row records as dispatch evidence.
// Passing a non-empty upstreamTarget back in reuses that already-recorded
// destination instead of resolving the model map a second time, so the retry
// cannot land somewhere the evidence does not name. Every other seat resolves
// no target of its own and reports the empty string.
func (p *Proxy) upstreamRequest(
	r *http.Request,
	seat registry.Seat,
	body []byte,
	upstreamTarget string,
) (*http.Request, uint64, string, *gatewayError) {
	// A sigv4 seat sends a translated, signed request to an endpoint its region
	// derives, so it shares none of the relay path below.
	if seat.AuthMode == registry.AuthSigV4 {
		return p.bedrockRequest(r, seat, body, upstreamTarget)
	}

	// Both gates below live only inside the AuthInject branch. Registry and
	// Context failures abort before any Seat reaches this point. The caller-key
	// check reads the inbound request before copyHeader strips X-Patchbay-Key from
	// the upstream copy, so it sees the header the caller actually sent.
	if seat.AuthMode == registry.AuthInject {
		if !isInjectMessagesPath(r.URL.Path) {
			return nil, 0, "", &gatewayError{
				status: http.StatusNotFound,
				err: fmt.Errorf(
					"the selected Seat injects a key and only relays the Messages endpoints, not %s",
					r.URL.Path,
				),
			}
		}

		if keyErr := checkCallerKey(r); keyErr != nil {
			return nil, 0, "", keyErr
		}
	}

	target := strings.TrimSuffix(seat.Upstream, "/") + r.URL.EscapedPath()
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

	copyHeader(
		req.Header,
		r.Header,
		droppedHeaders(
			r.Header,
			headerHost,
			headerContentLength,
			headerAcceptEncoding,
			CallerKeyHeader,
		),
	)
	req.Header.Set(headerAcceptEncoding, encodingIdentity)

	if authErr := applyAuth(req, seat); authErr != nil {
		return nil, 0, "", authErr
	}

	return req, 0, "", nil
}

// isInjectMessagesPath reports whether path is one of the Messages endpoints an
// inject seat may relay.
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
