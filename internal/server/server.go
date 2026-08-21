// Package server is patchbay's HTTP surface: the routing that decides which
// requests the gateway answers from its own knowledge and which ones ride the
// proxy pipeline.
//
// Almost everything rides the pipeline. It is the catch-all rather than a
// fallback for paths worth enumerating, because a request naming no model
// resolves to its Context's own default Seat — so /v1/messages,
// /v1/messages/count_tokens and every endpoint nobody here has heard of are all
// served by forwarding them.
//
// The two model-listing endpoints are the exceptions, and only because their
// honest answer is a merge. The public bindings patchbay serves are gateway-local
// knowledge no upstream can report; the models an account may use are
// knowledge patchbay does not have. Neither half is invented — but the two
// halves fail differently, and only one of them degrades.
//
// A stable registry is a requirement of every request, listings included. A
// registry that will not validate is refused with 503 and a Context nothing
// defines with 404, here exactly as on the proxy path: there is no Seat outside
// the registry to answer from, so falling back would answer a question nobody
// asked. Only the upstream half is optional. An Anthropic that is unreachable,
// refuses the caller's credentials or answers with something that is not a
// listing costs those entries and is logged, and the caller still gets the
// Context's own bindings, with 200.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// The paths the gateway answers itself. Everything else rides the proxy,
// including these two paths under any other method.
const (
	modelsPath    = "/v1/models"
	modelsPattern = "GET " + modelsPath
	modelPattern  = "GET " + modelsPath + "/{id}"
	rootPattern   = "/"
)

// contextPrefix is how a request names the context it wants:
// "/ctx/work/v1/messages" asks the work context for /v1/messages. The prefix is
// stripped as the context is selected, so every gate and every upstream URL
// below the selection sees the request exactly as it would have arrived without
// one.
const contextPrefix = "/ctx"

// fetchTimeout bounds one upstream models call. Nothing on this surface
// streams — a listing is a small document that either arrives promptly or is
// not worth waiting on, which is the opposite of a Messages turn.
const fetchTimeout = 5 * time.Second

// responseLimitBytes caps the upstream body the gateway will hold in memory. A
// model listing is a few kilobytes; anything past this ceiling is not one.
const responseLimitBytes = 4 << 20

// Config is everything the HTTP surface needs from its host process.
type Config struct {
	// Proxy serves every request the gateway does not answer itself, which is
	// nearly all of them. Required.
	Proxy http.Handler

	// Registry supplies the public bindings the model endpoints advertise. Required.
	Registry *registry.Registry

	// Client fetches from Anthropic on the caller's behalf. A nil Client gets
	// one with no timeout of its own, since every fetch this package makes is
	// already bounded by a context.
	Client *http.Client

	// Logger receives the warnings a degraded listing produces, and the
	// diagnostics the gateway keeps off every surface a caller can read — the
	// detail behind a registry that stopped validating under a running gateway
	// above all. A nil Logger gets slog.Default(), which is what the host process
	// configured for everything else.
	Logger *slog.Logger

	// Listen is the address the host process bound, reported by the
	// introspection endpoint so a caller reading the document knows which
	// gateway answered it.
	Listen string
}

// gateway answers the endpoints patchbay serves from its own knowledge. It is
// safe for concurrent use: the registry and the client are themselves
// concurrent, every request carries its own state, and the one thing requests do
// share — the hot-reload failure already reported to the operator — carries its
// own lock.
type gateway struct {
	registry *registry.Registry
	client   *http.Client
	logger   *slog.Logger
	listen   string
	invalid  invalidRegistry
}

// invalidRegistryMessage is the stable line an operator greps a gateway's logs
// for. Nothing a caller can read ever carries it.
const invalidRegistryMessage = "registry invalid; requests fail closed"

// invalidRegistry remembers the hot-reload failure the operator has already been
// told about, by the detail that describes it.
//
// A registry that will not validate refuses every request that arrives while it
// stays that way, and the detail behind each of those refusals is the same
// detail. Logging it per request would bury the one line an operator needs under
// the traffic it stopped, so it is logged once per distinct failure. A different
// failure is a different line; the same failure after the file was repaired is
// news a second time, which is what an observation finding nothing wrong is for.
type invalidRegistry struct {
	mu       sync.Mutex
	reported string
}

// reportCurrent observes the registry's current failure through current, decides
// whether it is news, records it and hands it to emit — all inside one critical
// section. Reporting and forgetting are the same operation because they are the
// same question asked at different moments, and one entry point is what puts
// every answer in a single order.
//
// The observation has to happen under the lock rather than before it. Asked
// outside, a report can read a failure, lose the lock to an observation that saw
// the file repaired, and then take the lock and write its own stale detail back
// over the cleared marker — after which every refusal for the failure actually
// in front of the operator is suppressed as already-told. Held throughout, a
// recovery racing a report either waits for it and lands after it, or lands
// before the observation that supersedes it; there is no order in which the
// older reading wins.
//
// A nil current detail is that recovery: nothing is emitted, and the marker is
// forgotten so the next failure is news even when it is the same failure. The
// empty marker means nothing is outstanding, which no detailed error can be.
func (i *invalidRegistry) reportCurrent(current func() error, emit func(error)) {
	i.mu.Lock()
	defer i.mu.Unlock()

	detail := current()
	if detail == nil {
		i.reported = ""

		return
	}

	text := detail.Error()
	if i.reported == text {
		return
	}

	i.reported = text

	emit(detail)
}

// observeRegistry tells the operator what is wrong with the registry right now:
// the file it was read from, the field that was rejected, and the value that
// file named — or nothing at all, when nothing is wrong with it.
//
// This is the only place that detail is spoken aloud. Resolve, the introspected
// State, every response built from either and every ledger record kept of one
// carry the bare sentinel instead, because a validation error quotes what the
// rejected file contains and a request is entitled to none of it. An operator
// reading the gateway's own logs is entitled to it, and is the only audience
// that can act on it.
//
// It is one operation, and it asks the registry rather than the caller, because
// those are the same mistake twice. A request resolves once and carries that
// result for the rest of its life; a request that resolved a valid document and
// then did anything at all before reaching this point is holding a fact about a
// registry that may have been replaced since. Deciding the diagnostic from that
// fact lets the stale request erase a failure a newer one already reported, and
// the very next refusal for that same failure then reports it again — one
// failure logged twice, with a false recovery between them. So every caller asks
// the same question, and the answer, not the caller, decides whether anything is
// said.
//
// Both the asking and the telling are handed to the tracker rather than done
// here, so they happen inside its lock: a concurrent observation must not be
// able to land between this look at the registry and the marker this look
// produces.
func (gw *gateway) observeRegistry(ctx context.Context) {
	gw.invalid.reportCurrent(gw.registry.Err, func(detail error) {
		gw.logger.ErrorContext(ctx, invalidRegistryMessage, slog.Any("error", detail))
	})
}

// Handler returns the gateway's HTTP surface: the introspection endpoint, the
// two model endpoints answered locally, and cfg.Proxy for every other path and
// every other method.
//
// Introspection wraps context selection on both sides, because the two forms of
// a patchbay-private path arrive at different points. The outer wrapper reads
// the path as it arrived and answers the bare "/_patchbay/..." form before any
// context is selected at all. The inner wrapper reads the path selection left
// behind and answers "/ctx/work/_patchbay/...", which only looks like an
// introspection path once the prefix naming the context has come off it.
//
// The introspection prefix is patchbay's own — no Anthropic endpoint lives
// under it and no context can put one there — so between the two wrappers,
// neither form can reach the proxy and become a request to Anthropic that the
// caller never made, including a path under the prefix that nothing serves.
func Handler(cfg Config) http.Handler {
	gw := &gateway{
		registry: cfg.Registry,
		client:   cfg.Client,
		logger:   cfg.Logger,
		listen:   cfg.Listen,
	}

	if gw.client == nil {
		gw.client = defaultClient()
	}

	if gw.logger == nil {
		gw.logger = slog.Default()
	}

	mux := http.NewServeMux()
	mux.HandleFunc(modelsPattern, gw.listModels)
	mux.HandleFunc(modelPattern, gw.showModel)
	mux.Handle(rootPattern, cfg.Proxy)

	return gw.selectIntrospection(gw.selectContext(gw.selectIntrospection(mux)))
}

// selectIntrospection answers everything under patchbay's own path prefix and
// passes everything else to next.
//
// It is a plain prefix test rather than another ServeMux on purpose: a mux
// cleans the request path and redirects a caller whose path was not already
// clean, and every path that is not introspection is one the proxy has to
// forward exactly as it arrived.
func (gw *gateway) selectIntrospection(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == introspectRoot || strings.HasPrefix(r.URL.Path, introspectPrefix) {
			gw.introspect(w, r)

			return
		}

		next.ServeHTTP(w, r)
	})
}

// selectContext resolves the context a request named in its path, strips the
// prefix that named it, and hands the rest to next with the selection and the
// resolution it produced attached.
//
// This is the one place a request is resolved. Everything downstream — the
// model endpoints, the whole proxy pipeline — reads the resolution off the
// request rather than asking the registry again, so a registry file replaced
// mid-request cannot serve the first half of a request from one document and
// the second half from the next.
//
// A request that named no context still resolves, implicitly, to the registry's
// configured default_context. A request that DID name one is refused unless the
// current registry can produce it — an unknown name, a broken file, a missing
// file. So is the implicit one: there is no Seat outside the registry to fall
// back to, and falling through to another context would answer a question
// nobody asked, when the whole point of a context is that it is a boundary.
//
// It is also where the operator hears about a registry that stopped validating,
// because this is where a running gateway first finds out. The two refusals are
// not the same news: a file that will not validate is a broken machine and gets
// the detail behind it logged, while a context nothing defines is a request
// asking for something that was never there.
//
// The diagnostic is taken from the registry rather than from the resolution
// above it, and taken unconditionally. This request's result describes the
// registry as it was when the resolution ran; the operator needs the registry as
// it is, and an older request resuming here with a stale valid result must not
// be able to speak for it.
func (gw *gateway) selectContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sel, stripped := splitContext(r.URL)

		res, err := gw.registry.Resolve(sel, "", nil)
		ctx := registry.WithSelection(r.Context(), sel)

		gw.observeRegistry(ctx)

		if err != nil {
			ctx = registry.WithResolutionError(ctx, err)
		} else {
			ctx = registry.WithResolution(ctx, res)
		}
		routed := r.WithContext(ctx)

		if sel.Explicit {
			routed.URL = stripped
		}

		next.ServeHTTP(w, routed)
	})
}

// ContextFromURL reports the context a request for target selects, by exactly
// the split the gateway routes with. It is exported because patchbay's own CLI
// has to say which context a configured base URL lands a project in, and a
// second implementation of the rule is how the tool and the gateway come to
// disagree about what a path means.
//
// It takes the whole URL rather than a path string because the decoded Path and
// the raw RawPath are two different answers to the question: "/ctx/work%2Feu"
// decodes to a context named "work" under a sub-path and escapes to one segment
// naming neither. The gateway routes on the escaped form, so a caller handing
// over only the decoded half would report a context this URL never reaches.
//
// A URL naming no context selects the registry's configured default_context. One
// whose prefix names nothing — or whose name will not come cleanly off both
// halves of the path — selects the empty name, which no registry can produce and
// which the gateway refuses.
func ContextFromURL(target *url.URL, defaultContext string) string {
	sel, _ := splitContext(target)
	if !sel.Explicit {
		return defaultContext
	}
	return sel.Name
}

// splitContext separates the context a request named from the path it actually
// asked for. A path with no prefix selects the configured default_context
// implicitly and is returned unchanged. A prefix naming nothing — "/ctx", "/ctx/", "/ctx//x",
// or a name that will not come cleanly off the escaped path — is an explicit
// selection of no context at all, which fails closed like any other name the
// registry cannot produce.
func splitContext(target *url.URL) (registry.Selection, *url.URL) {
	implicit := registry.Selection{}

	after, found := strings.CutPrefix(target.EscapedPath(), contextPrefix)
	if !found || (after != "" && !strings.HasPrefix(after, "/")) {
		return implicit, target
	}

	name, _, _ := strings.Cut(strings.TrimPrefix(after, "/"), "/")
	if name == "" {
		return registry.Selection{Name: "", Explicit: true}, target
	}

	stripped, ok := stripPrefix(target, contextPrefix+"/"+name)
	if !ok {
		return registry.Selection{Name: "", Explicit: true}, target
	}

	return registry.Selection{Name: name, Explicit: true}, stripped
}

// stripPrefix removes prefix from both halves of a URL's path — the decoded
// Path and the raw RawPath — the way http.StripPrefix does, so a path carrying
// escaped bytes reaches the upstream as the bytes it arrived as. It reports
// false when the prefix does not come cleanly off both.
func stripPrefix(target *url.URL, prefix string) (*url.URL, bool) {
	trimmed := strings.TrimPrefix(target.Path, prefix)
	if len(trimmed) >= len(target.Path) {
		return nil, false
	}

	rawTrimmed := strings.TrimPrefix(target.RawPath, prefix)
	if target.RawPath != "" && len(rawTrimmed) >= len(target.RawPath) {
		return nil, false
	}

	// "/ctx/work" asked the work context for the root, not for the empty path.
	if trimmed == "" {
		trimmed = "/"
		rawTrimmed = ""
	}

	stripped := *target
	stripped.Path = trimmed
	stripped.RawPath = rawTrimmed

	return &stripped, true
}

// writeResolutionError refuses a request whose context did not resolve, in the
// two ways it can fail to. A registry that will not validate is the gateway's
// own condition and answers 503, so a caller knows to try again; a context the
// current registry simply does not define answers 404, because trying again
// will not produce it.
func (gw *gateway) writeResolutionError(ctx context.Context, w http.ResponseWriter, err error) {
	if errors.Is(err, registry.ErrInvalid) {
		gw.writeError(ctx, w, http.StatusServiceUnavailable, err)
		return
	}
	gw.writeUnknownContext(ctx, w, err)
}

// writeUnknownContext refuses a request naming a context patchbay cannot
// resolve. It answers 404 in Anthropic's own not-found taxonomy rather than a
// type of patchbay's invention, so a client that has never heard of contexts
// still gets an error it knows how to handle, and names the context it refused
// so an operator can see which one. It never names anything else about the
// request.
func (gw *gateway) writeUnknownContext(ctx context.Context, w http.ResponseWriter, err error) {
	gw.logger.WarnContext(
		ctx,
		"request named a context that cannot be resolved",
		slog.Any("error", err),
	)
	gw.writeTyped(ctx, w, http.StatusNotFound, anthropic.ErrorTypeNotFound, err)
}

// defaultClient is the client used when the host process supplies none. It
// carries no timeout, deliberately: each request here sets its own deadline,
// and a client timeout would only duplicate it less precisely.
//
// It never follows a redirect. Go's default policy follows up to ten hops and,
// crossing to another host, strips only Authorization, Www-Authenticate, Cookie
// and the Proxy-* headers — the caller's x-api-key, anthropic-version and
// anthropic-beta would ride along to whatever host the upstream named. A
// redirect is also an answer the single-model relay owes the caller verbatim,
// and following one would replace it with something else's.
func defaultClient() *http.Client {
	return &http.Client{
		Transport:     nil,
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
