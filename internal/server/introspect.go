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

// Document is the caller-key-gated non-secret operator view.
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
	Models      map[string]string `json:"models"`
}

// SeatSummary is a global Seat with secrets and resolved paths omitted.
type SeatSummary struct {
	AuthMode          string `json:"auth_mode"`
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
			fmt.Errorf("%s only answers %s", RegistryEndpoint, http.MethodGet),
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
				Models:      ctx.Subagents.Models,
			}
		}
		doc.Contexts[id] = summary
	}
	return doc
}

func seatSummary(seat registry.Seat) SeatSummary {
	return SeatSummary{
		AuthMode:          string(seat.AuthMode),
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
	}
}
