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
	"log/slog"
	"net/http"
	"time"

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

	// Registry supplies the aliases the model endpoints advertise. Required.
	Registry *registry.Registry

	// Client fetches from Anthropic on the caller's behalf. A nil Client gets
	// one with no timeout of its own, since every fetch this package makes is
	// already bounded by a context.
	Client *http.Client

	// Logger receives the warnings a degraded listing produces. A nil Logger
	// gets slog.Default(), which is what the host process configured for
	// everything else.
	Logger *slog.Logger
}

// gateway answers the endpoints patchbay serves from its own knowledge. It is
// safe for concurrent use: the registry and the client are themselves
// concurrent, and every request carries its own state.
type gateway struct {
	registry *registry.Registry
	client   *http.Client
	logger   *slog.Logger
}

// Handler returns the gateway's HTTP surface: the two model endpoints answered
// locally, and cfg.Proxy for every other path and every other method.
func Handler(cfg Config) http.Handler {
	gw := &gateway{
		registry: cfg.Registry,
		client:   cfg.Client,
		logger:   cfg.Logger,
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

	return mux
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
