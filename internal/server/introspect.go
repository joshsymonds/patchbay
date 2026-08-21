package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// The introspection surface: one endpoint under a prefix reserved for
// patchbay's own answers. The whole prefix is claimed, not just the endpoint,
// so a path under it that nothing serves is refused here rather than forwarded
// to Anthropic as a request the caller never meant to make.
const (
	introspectRoot   = "/_patchbay"
	introspectPrefix = introspectRoot + "/"
)

// RegistryEndpoint is the one path patchbay describes its own routing at. It is
// exported because patchbay's CLI is its only caller: one declaration, so the
// tool and the gateway can never disagree about where the document lives.
const RegistryEndpoint = introspectPrefix + "registry"

// Document is everything patchbay will say about its own routing: every context
// it serves, where each route in them goes and how it pays, and where the
// registry behind them came from. It is built from the same resolution the proxy
// forwards by, so it cannot drift from what the gateway actually does.
//
// It never carries a secret. A route that pays with a key or maps a model
// through a file names the environment variable and the path it resolved to,
// which is what an operator needs to find the file — the contents of that file
// are the one thing this endpoint exists to describe without disclosing.
type Document struct {
	Contexts       map[string]ContextSummary `json:"contexts"`
	RegistryPath   string                    `json:"registry_path"`
	RegistryLoaded bool                      `json:"registry_loaded"`
	Listen         string                    `json:"listen"`
}

// ContextSummary is one named context: the route every model its aliases do not
// claim rides, and the aliases themselves.
type ContextSummary struct {
	DefaultRoute RouteSummary            `json:"default_route"`
	Routes       map[string]RouteSummary `json:"routes"`
}

// RouteSummary is one route's fate for a request that rides it. Fields a route
// does not set are omitted rather than reported as an empty answer.
type RouteSummary struct {
	Auth            string `json:"auth"`
	BillingPolicy   string `json:"billing_policy"`
	BillingClass    string `json:"billing_class,omitempty"`
	Upstream        string `json:"upstream"`
	Model           string `json:"model,omitempty"`
	MaxInputTokens  int    `json:"max_input_tokens,omitempty"`
	AWSProfile      string `json:"aws_profile,omitempty"`
	AWSRegion       string `json:"aws_region,omitempty"`
	KeyEnvFile      string `json:"api_key_env_file,omitempty"`
	KeyFile         string `json:"api_key_file,omitempty"`
	ModelMapEnvFile string `json:"model_map_env_file,omitempty"`
	ModelMapFile    string `json:"model_map_file,omitempty"`
}

// introspect answers the one endpoint patchbay describes itself with.
//
// It is gated on the same caller key an inject route is: the document names
// every upstream this machine can spend money at, which is not something any
// local process gets to enumerate just by asking. The gate is deliberately last
// — a path nothing serves and a method this endpoint does not answer are refused
// on their face, so a mistyped path reads as a mistyped path rather than as a
// key problem.
func (gw *gateway) introspect(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != RegistryEndpoint {
		gw.writeTyped(r.Context(), w, http.StatusNotFound, anthropic.ErrorTypeNotFound,
			fmt.Errorf("patchbay serves no %s", r.URL.Path))

		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
		gw.writeError(r.Context(), w, http.StatusMethodNotAllowed,
			fmt.Errorf("%s only answers %s", RegistryEndpoint, http.MethodGet))

		return
	}

	if status, err := gw.checkCallerKey(r); err != nil {
		gw.writeError(r.Context(), w, status, err)

		return
	}

	gw.writeJSON(r.Context(), w, gw.document())
}

// checkCallerKey applies the proxy's own caller-key gate to an introspection
// request, and reports the status its failure deserves: a key file the host
// process never configured is a server-side problem, and anything the caller
// did or did not present is a refusal.
func (gw *gateway) checkCallerKey(r *http.Request) (int, error) {
	expected, err := proxy.CallerKey()
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("caller key unavailable: %w", err)
	}

	if !proxy.CallerKeyMatches(r.Header.Get(proxy.CallerKeyHeader), expected) {
		return http.StatusForbidden, errors.New("caller key missing or mismatched")
	}

	return http.StatusOK, nil
}

// document describes every context the registry currently serves.
func (gw *gateway) document() Document {
	contexts, loaded := gw.registry.Contexts()

	doc := Document{
		Contexts:       make(map[string]ContextSummary, len(contexts)),
		RegistryPath:   gw.registry.Path(),
		RegistryLoaded: loaded,
		Listen:         gw.listen,
	}

	for _, res := range contexts {
		aliases := res.Aliases()
		summary := ContextSummary{
			DefaultRoute: routeSummary(res.DefaultRoute),
			Routes:       make(map[string]RouteSummary, len(aliases)),
		}

		for _, route := range aliases {
			summary.Routes[route.Alias] = routeSummary(route)
		}

		doc.Contexts[res.Context] = summary
	}

	return doc
}

// routeSummary describes one route. The key and model-map files are named twice
// — the environment variable the registry declared, and the path that variable
// currently resolves to — because an operator chasing a misconfiguration needs
// to know both which variable was consulted and what it pointed at. Neither
// file is ever opened here.
//
// The AWS profile and region a signing route uses are named too. Neither is a
// secret — a profile is a name in the operator's own ~/.aws config, and the
// region is already visible in the upstream this reports — and they are the two
// things a refused signature is diagnosed from.
func routeSummary(route registry.Route) RouteSummary {
	summary := RouteSummary{
		Auth:            string(route.Auth),
		BillingPolicy:   route.BillingPolicy().String(),
		Upstream:        route.Upstream(),
		Model:           route.Model,
		MaxInputTokens:  route.MaxInputTokens,
		AWSProfile:      route.AWSProfile,
		AWSRegion:       route.AWSRegion,
		KeyEnvFile:      route.APIKeyEnvFile,
		KeyFile:         "",
		ModelMapEnvFile: route.ModelMapEnvFile,
		ModelMapFile:    "",
	}

	if route.BillingClassSet {
		summary.BillingClass = route.BillingClass.String()
	}

	if route.APIKeyEnvFile != "" {
		summary.KeyFile = os.Getenv(route.APIKeyEnvFile)
	}

	if route.ModelMapEnvFile != "" {
		summary.ModelMapFile = os.Getenv(route.ModelMapEnvFile)
	}

	return summary
}
