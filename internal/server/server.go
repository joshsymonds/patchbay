// Package server is patchbay's HTTP surface: the routing that decides which
// requests the gateway answers from its own knowledge and which ones ride the
// proxy pipeline.
//
// Almost everything rides the pipeline. It is the catch-all rather than a
// fallback for paths worth enumerating, because a request naming no model
// resolves to the built-in default route and reaches Anthropic with the
// caller's own credentials — so /v1/messages, /v1/messages/count_tokens and
// every endpoint nobody here has heard of are all served by forwarding them.
//
// The two model-listing endpoints are the exceptions, and only because their
// honest answer is a merge. The aliases patchbay serves are gateway-local
// knowledge no upstream can report; the models an account may use are
// knowledge patchbay does not have. Neither half is invented and neither half
// can take down the other: a broken registry costs the aliases, an unreachable
// upstream costs the upstream models, and the caller still gets a listing.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/usage"
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
// stripped before anything else looks at the path, so every gate and every
// upstream URL downstream sees the request exactly as it would have arrived
// without one.
const contextPrefix = "/ctx"

// fetchTimeout bounds one upstream models call. Nothing on this surface
// streams — a listing is a small document that either arrives promptly or is
// not worth waiting on, which is the opposite of a Messages turn.
const fetchTimeout = 5 * time.Second

// responseLimitBytes caps the upstream body the gateway will hold in memory. A
// model listing is a few kilobytes; anything past this ceiling is not one.
const responseLimitBytes = 4 << 20

// UsageSummaryStore supplies the local usage-accounting aggregate the gateway
// exposes on its caller-key-gated introspection surface.
type UsageSummaryStore interface {
	SummarizeUsage(context.Context, usage.SummaryFilter) (usage.Summary, error)
}

// Config is everything the HTTP surface needs from its host process.
type Config struct {
	// Proxy serves every request the gateway does not answer itself, which is
	// nearly all of them. Required.
	Proxy http.Handler

	// Registry supplies the aliases the model endpoints advertise. Required.
	Registry *registry.Registry

	// Usage supplies durable request-accounting summaries. A nil Usage leaves the
	// gateway available but makes the local usage endpoint report 503.
	Usage UsageSummaryStore

	// Client fetches from Anthropic on the caller's behalf. A nil Client gets
	// one with no timeout of its own, since every fetch this package makes is
	// already bounded by a context.
	Client *http.Client

	// Logger receives the warnings a degraded listing produces. A nil Logger
	// gets slog.Default(), which is what the host process configured for
	// everything else.
	Logger *slog.Logger

	// Listen is the address the host process bound, reported by the
	// introspection endpoint so a caller reading the document knows which
	// gateway answered it.
	Listen string
}

// gateway answers the endpoints patchbay serves from its own knowledge. It is
// safe for concurrent use: the registry and the client are themselves
// concurrent, and every request carries its own state.
type gateway struct {
	registry *registry.Registry
	usage    UsageSummaryStore
	client   *http.Client
	logger   *slog.Logger
	listen   string
}

// Handler returns the gateway's HTTP surface: the introspection endpoint, the
// two model endpoints answered locally, and cfg.Proxy for every other path and
// every other method.
//
// Context selection runs first and introspection immediately after it, on the
// path the selection left behind. The introspection prefix is patchbay's own —
// no Anthropic endpoint lives under it and no context can put one there — so a
// path under it that nothing serves is answered here rather than becoming a
// request to Anthropic that the caller never made, and that holds for
// "/ctx/work/_patchbay/anything" exactly as it does for the bare form.
func Handler(cfg Config) http.Handler {
	gw := &gateway{
		registry: cfg.Registry,
		usage:    cfg.Usage,
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

	return gw.selectContext(gw.selectIntrospection(mux))
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
			if r.URL.Path == UsageSummaryEndpoint {
				gw.usageSummary(w, r)
			} else {
				gw.introspect(w, r)
			}

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
// A request that named no context still resolves, implicitly, to the personal
// context, and keeps working with no registry at all. A request that DID name
// one is refused unless the current registry can produce it — an unknown name, a
// broken file, a missing file. Falling through to another context, or to the
// built-in default route, would answer a question nobody asked, and the whole
// point of a context is that it is a boundary.
func (gw *gateway) selectContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sel, stripped := splitContext(r.URL)

		res, err := gw.registry.Resolve(sel, "")
		if err != nil {
			gw.writeUnknownContext(r.Context(), w, err)

			return
		}

		ctx := registry.WithResolution(registry.WithSelection(r.Context(), sel), res)
		routed := r.WithContext(ctx)

		if sel.Explicit {
			routed.URL = stripped
		}

		next.ServeHTTP(w, routed)
	})
}

// ContextFromPath reports the context a request for path selects, by exactly
// the split the gateway routes with. It is exported because patchbay's own CLI
// has to say which context a configured base URL lands a project in, and a
// second implementation of the rule is how the tool and the gateway come to
// disagree about what a path means.
//
// A path naming no context selects the personal one. A path whose prefix names
// nothing selects the empty name, which no registry can produce and which the
// gateway refuses.
func ContextFromPath(path string) string {
	sel, _ := splitContext(&url.URL{Path: path})

	return sel.Name
}

// splitContext separates the context a request named from the path it actually
// asked for. A path with no prefix selects the personal context implicitly and
// is returned unchanged. A prefix naming nothing — "/ctx", "/ctx/", "/ctx//x",
// or a name that will not come cleanly off the escaped path — is an explicit
// selection of no context at all, which fails closed like any other name the
// registry cannot produce.
func splitContext(target *url.URL) (registry.Selection, *url.URL) {
	implicit := registry.Selection{Name: registry.PersonalContext, Explicit: false}

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

// writeUnknownContext refuses a request naming a context patchbay cannot
// resolve. It answers 404 in Anthropic's own not-found taxonomy rather than a
// type of patchbay's invention, so a client that has never heard of contexts
// still gets an error it knows how to handle, and names the context it refused
// so an operator can see which one. It never names anything else about the
// request.
func (gw *gateway) writeUnknownContext(ctx context.Context, w http.ResponseWriter, err error) {
	gw.logger.WarnContext(ctx, "request named a context that cannot be resolved", slog.Any("error", err))
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
