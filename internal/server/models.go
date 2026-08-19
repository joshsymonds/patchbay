package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/registry"
)

// modelList is the /v1/models envelope. Entries are held as encoded values so
// an upstream model is relayed as the bytes it arrived as, with every field it
// carried, while an alias entry is a struct of this package's own.
type modelList struct {
	Data    []any   `json:"data"`
	HasMore bool    `json:"has_more"`
	FirstID *string `json:"first_id"`
	LastID  *string `json:"last_id"`
}

// epochCreatedAt is the created_at every alias entry carries. The gateway does
// not know when an alias came into existence and will not invent a date — but
// the field is not optional to a strict SDK decoder, and Anthropic's own
// contract for it says it may be set to an epoch value when the release date is
// unknown. So an alias says exactly that, in the sanctioned way.
const epochCreatedAt = "1970-01-01T00:00:00Z"

// modelEntry describes one model patchbay itself serves. The token ceilings are
// omitted when the route sets none; created_at is always present, carrying the
// epoch sentinel.
type modelEntry struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	DisplayName    string `json:"display_name"`
	CreatedAt      string `json:"created_at"`
	MaxInputTokens int    `json:"max_input_tokens,omitempty"`
	MaxTokens      int    `json:"max_tokens,omitempty"`
}

// upstreamListing is the little of an upstream listing the merge reads: the
// entries, and the pagination the merged envelope has to keep telling the truth
// about. Each entry stays raw so nothing about it is lost in the round trip.
type upstreamListing struct {
	Data    []json.RawMessage `json:"data"`
	HasMore bool              `json:"has_more"`
	LastID  *string           `json:"last_id"`
}

// upstreamEntry is the one field of an upstream model entry the merge reads:
// the id an alias may shadow.
type upstreamEntry struct {
	ID string `json:"id"`
}

// listModels answers GET /v1/models for the context the request selected: the
// aliases that context serves, followed — when the context still answers from
// Anthropic — by the models the caller's own account can see and no alias
// already claims.
//
// A context whose default route rides somewhere else gets a listing patchbay
// synthesizes whole. Anthropic has nothing to say about a context that never
// reaches it, so nothing is asked of it: the honest listing is the context's
// own aliases plus the model its default route rewrites to.
//
// Either way the answer is gateway-local: no ledger record is written, because
// nothing was proxied. It also never fails. An upstream that is unreachable,
// refuses the caller's credentials or answers with something that is not a
// listing costs the upstream half and is logged; the caller still gets the
// aliases, with 200.
func (gw *gateway) listModels(w http.ResponseWriter, r *http.Request) {
	res, err := gw.resolution(r)
	if err != nil {
		gw.writeUnknownContext(r.Context(), w, err)

		return
	}

	if status, keyErr := gw.checkContextKey(r); keyErr != nil {
		gw.writeError(r.Context(), w, status, keyErr)

		return
	}

	if regErr := gw.registry.Err(); regErr != nil {
		gw.warnRegistry(r.Context(), regErr)
	}

	aliases := res.Aliases()

	if !answersFromAnthropic(res.DefaultRoute) {
		gw.writeJSON(r.Context(), w, gw.synthesizedList(r, res, aliases))

		return
	}

	data := make([]any, 0, len(aliases))
	ids := make([]string, 0, len(aliases))
	claimed := make(map[string]bool, len(aliases))

	// The alias page is only ever the first page. A request carrying a
	// pagination cursor is asking the upstream for a later page, so prepending
	// the aliases again would repeat them on every page and report an alias as
	// that page's first_id. Skip them and let the upstream's own page stand.
	if !r.URL.Query().Has("after_id") {
		for _, route := range aliases {
			data = append(data, aliasEntry(route))
			ids = append(ids, route.Alias)
			claimed[route.Alias] = true
		}
	}

	listing := gw.upstreamModels(r)

	for _, entry := range listing.Data {
		id := entryID(entry)
		if claimed[id] {
			continue
		}

		data = append(data, entry)
		ids = append(ids, id)
	}

	gw.writeJSON(r.Context(), w, mergedList(data, ids, listing))
}

// answersFromAnthropic reports whether a context's default route still reaches
// Anthropic on the caller's own credentials. A forward route may target no
// other upstream — validation admits none — so the auth mode is the whole test.
func answersFromAnthropic(route registry.Route) bool {
	return route.Auth == registry.AuthForward
}

// synthesizedList is the whole listing for a context that never reaches
// Anthropic: its aliases, then every model its default route serves.
func (gw *gateway) synthesizedList(r *http.Request, res registry.Resolution, aliases []registry.Route) modelList {
	data := make([]any, 0, len(aliases)+1)
	ids := make([]string, 0, len(aliases)+1)
	claimed := make(map[string]bool, len(aliases))

	for _, route := range aliases {
		data = append(data, aliasEntry(route))
		ids = append(ids, route.Alias)
		claimed[route.Alias] = true
	}

	for _, model := range gw.defaultModels(r, res.DefaultRoute) {
		if claimed[model] {
			continue
		}

		data = append(data, routeEntry(res.DefaultRoute, model))
		ids = append(ids, model)
		claimed[model] = true
	}

	return mergedList(data, ids, upstreamListing{Data: nil, HasMore: false, LastID: nil})
}

// defaultModels is every model id a caller may name and actually reach a
// context's default route by, in order.
//
// A route that rewrites to one model advertises that one; a route that rewrites
// nothing has no model id to advertise and contributes none, because the gateway
// will not invent one.
//
// A sigv4 route is the interesting case: it serves exactly the models its model
// map has entries for, and the ids a caller may name are that map's KEYS. The
// values are Bedrock model ids — the one thing the map file exists to keep out
// of sight — so they are never listed. A map that cannot be read costs the
// listing nothing but those entries: the aliases still stand.
func (gw *gateway) defaultModels(r *http.Request, route registry.Route) []string {
	if route.Auth != registry.AuthSigV4 {
		if route.Model == "" {
			return nil
		}

		return []string{route.Model}
	}

	mapping, err := readModelMap(route.ModelMapEnvFile)
	if err != nil {
		gw.logger.WarnContext(r.Context(), "model map unavailable; a sigv4 context lists only its aliases",
			slog.Any("error", err))

		return nil
	}

	models := make([]string, 0, len(mapping))

	for model, target := range mapping {
		// An entry mapping to nothing is one the pipeline refuses, so listing it
		// would advertise a model this context cannot actually reach.
		if target != "" {
			models = append(models, model)
		}
	}

	slices.Sort(models)

	return models
}

// readModelMap resolves the double indirection a sigv4 route names its model
// map with: envName is the name of an environment variable whose value is the
// path of a JSON file mapping the model ids a caller may name to the Bedrock
// ones they ride. It is read per request, the same way the pipeline reads it, so
// a listing describes the file as it stands now.
func readModelMap(envName string) (map[string]string, error) {
	path := os.Getenv(envName)
	if path == "" {
		return nil, fmt.Errorf("environment variable %s is not set", envName)
	}

	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read the model map named by %s: %w", envName, err)
	}

	var mapping map[string]string
	if decodeErr := json.Unmarshal(contents, &mapping); decodeErr != nil {
		return nil, fmt.Errorf("decode the model map named by %s: %w", envName, decodeErr)
	}

	return mapping, nil
}

// showModel answers GET /v1/models/{id} for the context the request selected.
//
// An alias that context serves is described from the registry. Anything else
// depends on where the context's default route rides, exactly as the listing
// does: a context that still answers from Anthropic leaves the rest to the
// upstream, whose reply — a model, a 404, a rejected key — is relayed exactly as
// it came back, because the upstream's own answer is the honest one. A context
// that never reaches Anthropic is answered here or not at all: Anthropic has
// nothing to say about a model it will never be asked for, and asking it would
// describe a model this context cannot reach.
func (gw *gateway) showModel(w http.ResponseWriter, r *http.Request) {
	res, err := gw.resolution(r)
	if err != nil {
		gw.writeUnknownContext(r.Context(), w, err)

		return
	}

	if status, keyErr := gw.checkContextKey(r); keyErr != nil {
		gw.writeError(r.Context(), w, status, keyErr)

		return
	}

	id := r.PathValue("id")

	for _, route := range res.Aliases() {
		if route.Alias == id {
			gw.writeJSON(r.Context(), w, aliasEntry(route))

			return
		}
	}

	if answersFromAnthropic(res.DefaultRoute) {
		gw.relayModel(w, r)

		return
	}

	if slices.Contains(gw.defaultModels(r, res.DefaultRoute), id) {
		gw.writeJSON(r.Context(), w, routeEntry(res.DefaultRoute, id))

		return
	}

	gw.writeTyped(r.Context(), w, http.StatusNotFound, anthropic.ErrorTypeNotFound,
		fmt.Errorf("context %q serves no model %q", res.Context, id))
}

// resolution is the one resolution this request made. The HTTP surface resolves
// the context before anything dispatches on it and attaches the result, so a
// handler reads that rather than asking the registry a second question it might
// answer from a different file.
func (gw *gateway) resolution(r *http.Request) (registry.Resolution, error) {
	if res, ok := registry.ResolutionFrom(r.Context()); ok {
		return res, nil
	}

	res, err := gw.registry.Resolve(registry.SelectionFrom(r.Context()), "")
	if err != nil {
		return res, fmt.Errorf("resolve the request context: %w", err)
	}

	return res, nil
}

// checkContextKey gates the model endpoints for a request that named a context
// in its own path, with the same caller key — and the same 403 and 500 — the
// introspection endpoint is gated on. Naming a context is asking what some other
// world of this machine's serves, which is the thing that key exists to protect.
//
// The bare path is deliberately not gated: it is what Claude Code sends, with no
// idea patchbay is there, and a /v1/models it cannot answer is a broken client.
// It describes only the personal context, which is the one the caller was
// already riding.
func (gw *gateway) checkContextKey(r *http.Request) (int, error) {
	if !registry.SelectionFrom(r.Context()).Explicit {
		return http.StatusOK, nil
	}

	return gw.checkCallerKey(r)
}

// relayModel forwards a single-model lookup to Anthropic with the caller's
// credentials and hands back its status and body untouched. An upstream that
// cannot be reached at all is the one thing patchbay must answer for itself,
// which it does in Anthropic's error shape, marked as its own.
func (gw *gateway) relayModel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), fetchTimeout)
	defer cancel()

	resp, err := gw.fetch(ctx, r.URL.Path, r.URL.EscapedPath(), r.URL.RawQuery, r.Header)
	if err != nil {
		gw.writeError(ctx, w, http.StatusBadGateway, err)

		return
	}

	defer resp.Body.Close()

	// Read one byte past the ceiling so an upstream that overruns it is caught
	// rather than silently truncated and relayed as a fragment under the
	// upstream's own 2xx.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, responseLimitBytes+1))
	if readErr != nil {
		gw.writeError(ctx, w, http.StatusBadGateway, fmt.Errorf("read upstream response: %w", readErr))

		return
	}

	if len(body) > responseLimitBytes {
		gw.writeError(ctx, w, http.StatusBadGateway,
			fmt.Errorf("upstream response exceeds the %d-byte ceiling", responseLimitBytes))

		return
	}

	w.Header().Set(anthropic.HeaderContentType, contentType(resp))
	w.WriteHeader(resp.StatusCode)
	gw.write(ctx, w, body)
}

// upstreamModels fetches the listing the caller's own credentials can see.
// Every way it can go wrong — a request that cannot be built, an upstream that
// never answers, a non-2xx reply, a body that is not a listing — returns no
// entries and logs why. The caller is never told about it: a gateway that
// cannot reach Anthropic still knows its own aliases.
func (gw *gateway) upstreamModels(r *http.Request) upstreamListing {
	ctx, cancel := context.WithTimeout(r.Context(), fetchTimeout)
	defer cancel()

	resp, err := gw.fetch(ctx, modelsPath, modelsPath, r.URL.RawQuery, r.Header)
	if err != nil {
		gw.warn(ctx, err)

		return upstreamListing{}
	}

	defer resp.Body.Close()

	// Read one byte past the ceiling to tell a listing that fits from one that
	// overruns it, so an oversized body is refused explicitly rather than left to
	// fail an opaque json.Unmarshal.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, responseLimitBytes+1))
	if readErr != nil {
		gw.warn(ctx, fmt.Errorf("read upstream response: %w", readErr))

		return upstreamListing{}
	}

	if len(body) > responseLimitBytes {
		gw.warn(ctx, fmt.Errorf("upstream response exceeds the %d-byte ceiling", responseLimitBytes))

		return upstreamListing{}
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		gw.warn(ctx, fmt.Errorf("upstream answered %d", resp.StatusCode))

		return upstreamListing{}
	}

	var listing upstreamListing
	if parseErr := json.Unmarshal(body, &listing); parseErr != nil {
		gw.warn(ctx, fmt.Errorf("upstream body is not a model listing: %w", parseErr))

		return upstreamListing{}
	}

	return listing
}

// fetch asks Anthropic for path on the caller's behalf. The base URL is a fixed
// constant and the caller's path is set on the parsed URL afterwards, so no
// caller-supplied string is ever parsed as a whole URL and the destination host
// cannot move.
func (gw *gateway) fetch(
	ctx context.Context,
	path, escapedPath, rawQuery string,
	header http.Header,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropic.BaseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	req.URL.Path = path
	req.URL.RawPath = escapedPath
	req.URL.RawQuery = rawQuery

	copyCallerAuth(req.Header, header)

	resp, doErr := gw.client.Do(req)
	if doErr != nil {
		return nil, fmt.Errorf("upstream request failed: %w", doErr)
	}

	return resp, nil
}

// writeJSON answers with value encoded as JSON. Encoding it whole before
// anything is written keeps a failure from reaching the caller as a truncated
// 200 — which for these two endpoints, built from validated routes and bytes
// that already parsed, is a failure that should never happen at all.
func (gw *gateway) writeJSON(ctx context.Context, w http.ResponseWriter, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		gw.writeError(ctx, w, http.StatusInternalServerError, fmt.Errorf("encode response: %w", err))

		return
	}

	w.Header().Set(anthropic.HeaderContentType, jsonContentType)
	w.WriteHeader(http.StatusOK)
	gw.write(ctx, w, body)
}

// write hands body to the caller, noting a caller that has already gone.
func (gw *gateway) write(ctx context.Context, w http.ResponseWriter, body []byte) {
	if _, err := w.Write(body); err != nil {
		gw.logger.WarnContext(ctx, "client went away before the answer reached it", slog.Any("error", err))
	}
}

// warn reports a degraded model listing. It is a warning rather than an error
// because the caller was still served.
func (gw *gateway) warn(ctx context.Context, err error) {
	gw.logger.WarnContext(ctx, "upstream model list unavailable", slog.Any("error", err))
}

// warnRegistry reports the other half of a degraded listing: a registry file
// that will not parse or validate, which costs every alias. The file is re-read
// on every request, so a rebuild can break it long after the process started
// and the warning is the only place that shows.
func (gw *gateway) warnRegistry(ctx context.Context, err error) {
	gw.logger.WarnContext(ctx, "registry file is broken; foreign aliases are unavailable",
		slog.Any("error", err))
}

// mergedList wraps the merged entries in the /v1/models envelope.
//
// has_more is the upstream's own. Patchbay puts a page of aliases in front of
// the upstream's first page, and whether another page follows is the upstream's
// fact to report, not the gateway's to overwrite — claiming otherwise hides
// every model a paginated account has past the first page. When there is a next
// page, last_id has to be the upstream's cursor too, since that is the only id
// Anthropic can page from; an alias id would mean nothing to it. Otherwise the
// merged listing is the whole listing, and its own ends are the honest answer.
func mergedList(data []any, ids []string, listing upstreamListing) modelList {
	list := modelList{Data: data, HasMore: listing.HasMore, FirstID: nil, LastID: nil}

	if len(ids) > 0 {
		list.FirstID = &ids[0]
		list.LastID = &ids[len(ids)-1]
	}

	if listing.HasMore {
		list.LastID = listing.LastID
	}

	return list
}

// aliasEntry renders one registry route as a listing entry. A ceiling the route
// does not set is left out rather than advertised as zero.
func aliasEntry(route registry.Route) modelEntry {
	return modelEntry{
		Type:           "model",
		ID:             route.Alias,
		DisplayName:    route.Alias,
		CreatedAt:      epochCreatedAt,
		MaxInputTokens: route.MaxInputTokens,
		MaxTokens:      route.MaxOutputTokens,
	}
}

// routeEntry renders one model a route serves as a listing entry, under the id
// a caller may actually name to reach it. The ceilings are the route's own: they
// are what the request will be held to whichever of its models is asked for.
func routeEntry(route registry.Route, model string) modelEntry {
	return modelEntry{
		Type:           "model",
		ID:             model,
		DisplayName:    model,
		CreatedAt:      epochCreatedAt,
		MaxInputTokens: route.MaxInputTokens,
		MaxTokens:      route.MaxOutputTokens,
	}
}

// entryID reads the id of an upstream listing entry. An entry that is not an
// object, or carries no id, has no id an alias could shadow — it is listed as
// it arrived.
func entryID(raw json.RawMessage) string {
	var entry upstreamEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return ""
	}

	return entry.ID
}

// contentType reports the content type to relay, defaulting to JSON when the
// upstream named none — every answer this endpoint relays is a JSON document.
func contentType(resp *http.Response) string {
	if declared := resp.Header.Get(anthropic.HeaderContentType); declared != "" {
		return declared
	}

	return jsonContentType
}
