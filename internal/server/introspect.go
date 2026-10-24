package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/registry"
)

const (
	introspectRoot   = "/_patchbay"
	introspectPrefix = introspectRoot + "/"
)

// RegistryEndpoint is the caller-key-gated registry introspection path.
const RegistryEndpoint = introspectPrefix + "registry"

const introspectionMethodRefusal = "introspection endpoints answer GET and HEAD only"

// Document is the caller-key-gated non-secret operator view: every Context
// patchbay serves, the Seats those Contexts bind to, how each Seat pays, and
// where the registry behind them came from. It is built from the same state the
// proxy resolves against, so it cannot drift from what the gateway actually
// does.
//
// It never carries a secret. A Seat that pays with a key or maps a model
// through a file names the environment variable it reads, which is what an
// operator needs to know which variable was consulted. Neither the path that
// variable resolves to nor the file behind it appears here.
type Document struct {
	DefaultContext *string                   `json:"default_context"`
	Seats          map[string]SeatSummary    `json:"seats"`
	Contexts       map[string]ContextSummary `json:"contexts"`
	RegistryValid  bool                      `json:"registry_valid"`
	RegistryError  string                    `json:"validation_error,omitempty"`
	RegistryPath   string                    `json:"registry_path"`
	Listen         string                    `json:"listen"`
}

// ContextSummary is the non-secret public and marked-subagent binding view.
type ContextSummary struct {
	DefaultSeat string            `json:"default_seat"`
	Models      map[string]string `json:"models"`
	Subagents   *SubagentsSummary `json:"subagents,omitempty"`
}

// SubagentsSummary is the optional positively marked traffic policy.
type SubagentsSummary struct {
	DefaultSeat string            `json:"default_seat"`
	AgentTypes  map[string]string `json:"agent_types"`
	Models      map[string]string `json:"models"`
}

// SeatSummary is a global Seat with secrets and resolved paths omitted. Fields
// a Seat does not set are omitted rather than reported as an empty answer.
//
// BillingPolicy is always reported, because every Seat has one: it follows from
// the auth mode rather than being declared. BillingClass appears only where the
// policy is explicit, which is the only mode that declares a class — on the
// modes that derive one from the credential, an omitted field says the question
// is settled elsewhere rather than left unanswered.
type SeatSummary struct {
	AuthMode          string `json:"auth_mode"`
	BillingPolicy     string `json:"billing_policy"`
	BillingClass      string `json:"billing_class,omitempty"`
	Upstream          string `json:"upstream"`
	Model             string `json:"model,omitempty"`
	APIKeyEnvFile     string `json:"api_key_env_file,omitempty"`
	AWSProfile        string `json:"aws_profile,omitempty"`
	AWSRegion         string `json:"aws_region,omitempty"`
	ModelMapEnvFile   string `json:"model_map_env_file,omitempty"`
	MaxInputTokens    int    `json:"max_input_tokens,omitempty"`
	MaxOutputTokens   int    `json:"max_output_tokens,omitempty"`
	Insecure          bool   `json:"insecure,omitempty"`
	StripCacheControl bool   `json:"strip_cache_control,omitempty"`
	CaptureBodies     bool   `json:"capture_bodies,omitempty"`
	Speed             string `json:"speed,omitempty"`
	CacheTTL          string `json:"cache_ttl,omitempty"`
}

func (gw *gateway) introspect(w http.ResponseWriter, r *http.Request) {
	if err := registry.ResolutionErrorFrom(r.Context()); err != nil {
		gw.writeResolutionError(r.Context(), w, err)
		return
	}
	if r.URL.Path != RegistryEndpoint {
		gw.writeTyped(
			r.Context(),
			w,
			http.StatusNotFound,
			anthropic.ErrorTypeNotFound,
			fmt.Errorf("patchbay serves no %s", r.URL.Path),
		)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
		gw.writeError(
			r.Context(),
			w,
			http.StatusMethodNotAllowed,
			errors.New(introspectionMethodRefusal),
		)
		return
	}
	if status, err := gw.checkCallerKey(r); err != nil {
		gw.writeError(r.Context(), w, status, err)
		return
	}
	gw.writeJSON(r.Context(), w, gw.document(r.Context()))
}

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

// document builds the caller-key-gated view of the current registry, and takes
// the state it reads as the moment to tell the operator what this document
// deliberately will not say.
//
// Bare "/_patchbay/registry" is answered ahead of context selection, so an
// invalid registry may be observed here first and nowhere else. The report is
// made from behind the caller-key gate — every caller that reaches this point
// has already presented the key — because what it logs is what the rejected file
// contains, and an unauthenticated prober must not be able to make the gateway
// write that down.
//
// State and diagnostic are acquired together. Prefixed introspection reuses the
// acquisition context selection already performed; bare introspection acquires
// it here.
func (gw *gateway) document(ctx context.Context) Document {
	var state registry.State
	if carried, ok := ctx.Value(registryDocumentStateKey{}).(registryDocumentState); ok {
		state = carried.state
	} else {
		var observed registry.Observation
		state, observed = gw.registry.InspectObserved()
		gw.observeRegistry(ctx, observed)
	}
	doc := Document{
		Seats:         map[string]SeatSummary{},
		Contexts:      map[string]ContextSummary{},
		RegistryValid: state.Valid,
		RegistryError: state.Error,
		RegistryPath:  gw.registry.Path(),
		Listen:        gw.listen,
	}
	if !state.Valid {
		return doc
	}
	defaultContext := state.DefaultContext
	doc.DefaultContext = &defaultContext
	for id, seat := range state.Seats {
		doc.Seats[id] = seatSummary(seat)
	}
	for id, ctx := range state.Contexts {
		summary := ContextSummary{DefaultSeat: ctx.DefaultSeat, Models: ctx.Models}
		if ctx.Subagents != nil {
			summary.Subagents = &SubagentsSummary{
				DefaultSeat: ctx.Subagents.DefaultSeat,
				AgentTypes:  ctx.Subagents.AgentTypes,
				Models:      ctx.Subagents.Models,
			}
		}
		doc.Contexts[id] = summary
	}
	return doc
}

// seatSummary describes one Seat. The key and model-map environment variables
// are named, never opened and never resolved: an operator chasing a
// misconfiguration needs to know which variable was consulted, and the path it
// points at is one step closer to the secret than this endpoint goes.
//
// The AWS profile and region a signing Seat uses are named too. Neither is a
// secret — a profile is a name in the operator's own ~/.aws config, and the
// region is already visible in the upstream this reports — and they are the two
// things a refused signature is diagnosed from.
func seatSummary(seat registry.Seat) SeatSummary {
	summary := SeatSummary{
		AuthMode:          string(seat.AuthMode),
		BillingPolicy:     seat.BillingPolicy().String(),
		BillingClass:      "",
		Upstream:          seat.EffectiveUpstream(),
		Model:             seat.Model,
		APIKeyEnvFile:     seat.APIKeyEnvFile,
		AWSProfile:        seat.AWSProfile,
		AWSRegion:         seat.AWSRegion,
		ModelMapEnvFile:   seat.ModelMapEnvFile,
		MaxInputTokens:    seat.MaxInputTokens,
		MaxOutputTokens:   seat.MaxOutputTokens,
		Insecure:          seat.Insecure,
		StripCacheControl: seat.StripCacheControl,
		CaptureBodies:     seat.CaptureBodies,
		Speed:             seat.Speed,
		CacheTTL:          seat.CacheTTL,
	}

	if seat.BillingClassSet {
		summary.BillingClass = seat.BillingClass.String()
	}

	return summary
}
