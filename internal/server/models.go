package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

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

// listModels answers GET /v1/models with the aliases this gateway serves,
// followed by the models the caller's own Anthropic account can see and no
// alias already claims.
//
// The merge is gateway-local: no ledger record is written, because nothing was
// proxied. It also never fails. An upstream that is unreachable, refuses the
// caller's credentials or answers with something that is not a listing costs
// the upstream half and is logged; the caller still gets the aliases, with 200.
func (gw *gateway) listModels(w http.ResponseWriter, r *http.Request) {
	aliases := gw.registry.Aliases()
	if err := gw.registry.Err(); err != nil {
		gw.warnRegistry(r.Context(), err)
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

// showModel answers GET /v1/models/{id}. An alias patchbay serves is described
// from the registry; anything else is the upstream's to answer, and its reply —
// a model, a 404, a rejected key — is relayed exactly as it came back. The
// upstream's own answer is the honest one, so patchbay never substitutes a
// guess for it.
func (gw *gateway) showModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	for _, route := range gw.registry.Aliases() {
		if route.Alias == id {
			gw.writeJSON(r.Context(), w, aliasEntry(route))

			return
		}
	}

	gw.relayModel(w, r)
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
