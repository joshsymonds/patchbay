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
// carried, while a binding entry is a struct of this package's own.
type modelList struct {
	Data    []any   `json:"data"`
	HasMore bool    `json:"has_more"`
	FirstID *string `json:"first_id"`
	LastID  *string `json:"last_id"`
}

// epochCreatedAt is the created_at every binding entry carries. The gateway does
// not know when a binding came into existence and will not invent a date — but
// the field is not optional to a strict SDK decoder, and Anthropic's own
// contract for it says it may be set to an epoch value when the release date is
// unknown. So a binding says exactly that, in the sanctioned way.
const epochCreatedAt = "1970-01-01T00:00:00Z"

// modelEntry describes one model patchbay itself serves. The token ceilings are
// omitted when the Seat sets none; created_at is always present, carrying the
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
// about. Both cursors are read, because Anthropic pages forward from last_id and
// back from first_id and the merged envelope owes a client both directions. Each
// entry stays raw so nothing about it is lost in the round trip.
type upstreamListing struct {
	Data    []json.RawMessage `json:"data"`
	HasMore bool              `json:"has_more"`
	FirstID *string           `json:"first_id"`
	LastID  *string           `json:"last_id"`
}

// upstreamEntry is the one field of an upstream model entry the merge reads:
// the id a binding may shadow.
type upstreamEntry struct {
	ID string `json:"id"`
}

// listModels answers GET /v1/models for the context the request selected: the
// public bindings that context serves, followed — when the context still answers from
// Anthropic — by the models the caller's own account can see and no binding
// already claims.
//
// A context whose default Seat rides somewhere else gets a listing patchbay
// synthesizes whole. Anthropic has nothing to say about a context that never
// reaches it, so nothing is asked of it: the honest listing is the context's
// own public bindings plus the model its default Seat rewrites to.
//
// Either way the answer is gateway-local: no ledger record is written, because
// nothing was proxied. What can still fail is everything ahead of the merge: an
// invalid registry is refused with 503, a Context nothing defines with 404, and
// a request that named a Context in its own path without the caller key gets
// the same 403 — or 500, when the key file cannot be read — introspection
// answers. Only the upstream half degrades. An Anthropic that is unreachable,
// refuses the caller's credentials or answers with something that is not a
// listing costs those entries and is logged; the caller still gets the public
// bindings, with 200.
func (gw *gateway) listModels(w http.ResponseWriter, r *http.Request) {
	res, err := gw.resolution(r)
	if err != nil {
		gw.writeResolutionError(r.Context(), w, err)

		return
	}

	if status, keyErr := gw.checkContextKey(r); keyErr != nil {
		gw.writeError(r.Context(), w, status, keyErr)

		return
	}

	bindings := res.PublicBindings()
	_, defaultSeat := res.DefaultSeat()

	if !answersFromAnthropic(defaultSeat) {
		gw.writeJSON(r.Context(), w, gw.synthesizedList(r, bindings, defaultSeat))
		return
	}

	// The public bindings are a page of their own, in front of the upstream's
	// first page, so they are emitted once and never repeated on a later page.
	// Anthropic pages forward from last_id and back from first_id, so a request
	// carrying either cursor is asking for a page of the upstream's own listing
	// and gets no bindings.
	//
	// What every page still owes is the shadowing: a selector this context serves
	// is what that name means here, whichever upstream page the colliding id
	// happens to arrive on.
	query := r.URL.Query()
	firstPage := !query.Has("after_id") && !query.Has("before_id")

	data := make([]any, 0, len(bindings))
	ids := make([]string, 0, len(bindings))
	claimed := make(map[string]bool, len(bindings))

	for _, binding := range bindings {
		claimed[binding.Selector] = true

		if firstPage {
			data = append(data, seatEntry(binding.Seat, binding.Selector))
			ids = append(ids, binding.Selector)
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

	gw.writeJSON(r.Context(), w, mergedList(data, ids, listing, firstPage))
}

// answersFromAnthropic reports whether a context's default Seat still reaches
// Anthropic on the caller's own credentials. A forward Seat may target no
// other upstream — validation admits none — so the auth mode is the whole test.
func answersFromAnthropic(seat registry.Seat) bool {
	return seat.AuthMode == registry.AuthForward
}

func (gw *gateway) synthesizedList(
	r *http.Request,
	bindings []registry.Binding,
	defaultSeat registry.Seat,
) modelList {
	data := make([]any, 0, len(bindings)+1)
	ids := make([]string, 0, len(bindings)+1)
	claimed := make(map[string]bool, len(bindings))
	for _, binding := range bindings {
		data = append(data, seatEntry(binding.Seat, binding.Selector))
		ids = append(ids, binding.Selector)
		claimed[binding.Selector] = true
	}
	for _, model := range gw.defaultModels(r, defaultSeat) {
		if claimed[model] {
			continue
		}
		data = append(data, seatEntry(defaultSeat, model))
		ids = append(ids, model)
		claimed[model] = true
	}
	// A synthesized listing is patchbay's own from end to end and is never
	// paginated, so it is always its own first and only page.
	return mergedList(data, ids, upstreamListing{}, true)
}

// defaultModels is every model id a caller may name and actually reach a
// context's default Seat by, in order.
//
// A Seat that rewrites to one model advertises that one; a Seat that rewrites
// nothing has no model id to advertise and contributes none, because the gateway
// will not invent one.
//
// A sigv4 Seat is the interesting case: it serves exactly the models its model
// map has entries for, and the ids a caller may name are that map's KEYS. The
// values are Bedrock model ids — the one thing the map file exists to keep out
// of sight — so they are never listed. A map that cannot be read costs the
// listing nothing but those entries: the public bindings still stand.
func (gw *gateway) defaultModels(r *http.Request, seat registry.Seat) []string {
	if seat.AuthMode != registry.AuthSigV4 {
		if seat.Model == "" {
			return nil
		}

		return []string{seat.Model}
	}

	mapping, err := readModelMap(seat.ModelMapEnvFile)
	if err != nil {
		gw.logger.WarnContext(
			r.Context(),
			"model map unavailable; a sigv4 context lists only its public bindings",
			slog.Any("error", err),
		)

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

// readModelMap resolves the double indirection a sigv4 Seat names its model
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
// A binding that context serves is described from the registry. Anything else
// depends on where the context's default Seat rides, exactly as the listing
// does: a context that still answers from Anthropic leaves the rest to the
// upstream, whose reply — a model, a 404, a rejected key — is relayed exactly as
// it came back, because the upstream's own answer is the honest one. A context
// that never reaches Anthropic is answered here or not at all: Anthropic has
// nothing to say about a model it will never be asked for, and asking it would
// describe a model this context cannot reach.
func (gw *gateway) showModel(w http.ResponseWriter, r *http.Request) {
	res, err := gw.resolution(r)
	if err != nil {
		gw.writeResolutionError(r.Context(), w, err)

		return
	}

	if status, keyErr := gw.checkContextKey(r); keyErr != nil {
		gw.writeError(r.Context(), w, status, keyErr)

		return
	}

	id := r.PathValue("id")
	for _, binding := range res.PublicBindings() {
		if binding.Selector == id {
			gw.writeJSON(r.Context(), w, seatEntry(binding.Seat, binding.Selector))
			return
		}
	}
	_, defaultSeat := res.DefaultSeat()
	if answersFromAnthropic(defaultSeat) {
		gw.relayModel(w, r)
		return
	}
	if slices.Contains(gw.defaultModels(r, defaultSeat), id) {
		gw.writeJSON(r.Context(), w, seatEntry(defaultSeat, id))
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
	if err := registry.ResolutionErrorFrom(r.Context()); err != nil {
		return registry.Resolution{}, fmt.Errorf("use attached Context resolution: %w", err)
	}
	if res, ok := registry.ResolutionFrom(r.Context()); ok {
		return res, nil
	}

	res, err := gw.registry.Resolve(registry.SelectionFrom(r.Context()), "", nil)
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
// It describes only the configured default_context, which is the one the caller
// was already riding.
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
		gw.writeError(
			ctx,
			w,
			http.StatusBadGateway,
			fmt.Errorf("read upstream response: %w", readErr),
		)

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
// cannot reach Anthropic still knows its own public bindings.
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
		gw.warn(
			ctx,
			fmt.Errorf("upstream response exceeds the %d-byte ceiling", responseLimitBytes),
		)

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
// 200 — which for these two endpoints, built from validated Seats and bytes
// that already parsed, is a failure that should never happen at all.
func (gw *gateway) writeJSON(ctx context.Context, w http.ResponseWriter, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		gw.writeError(
			ctx,
			w,
			http.StatusInternalServerError,
			fmt.Errorf("encode response: %w", err),
		)

		return
	}

	w.Header().Set(anthropic.HeaderContentType, jsonContentType)
	w.WriteHeader(http.StatusOK)
	gw.write(ctx, w, body)
}

// write hands body to the caller, noting a caller that has already gone.
func (gw *gateway) write(ctx context.Context, w http.ResponseWriter, body []byte) {
	if _, err := w.Write(body); err != nil {
		gw.logger.WarnContext(
			ctx,
			"client went away before the answer reached it",
			slog.Any("error", err),
		)
	}
}

// warn reports a degraded model listing. It is a warning rather than an error
// because the caller was still served.
func (gw *gateway) warn(ctx context.Context, err error) {
	gw.logger.WarnContext(ctx, "upstream model list unavailable", slog.Any("error", err))
}

// mergedList wraps the merged entries in the /v1/models envelope.
//
// has_more is the upstream's own. Patchbay puts a page of public bindings in front of
// the upstream's first page, and whether another page follows is the upstream's
// fact to report, not the gateway's to overwrite — claiming otherwise hides
// every model a paginated account has past the first page.
//
// The cursors are the merged listing's own ends only where the merged listing
// really does end there. A cursor-qualified page is none of patchbay's own from
// end to end — no binding leads it and none closes it — so both of its cursors
// are the upstream's, and both are kept whichever way the caller was paging and
// whatever has_more says. Anthropic pages forward from last_id and back from
// first_id, so a page reached by before_id still has a forward end the caller
// may turn around at, and has_more:false says only that nothing lies further
// back. Shadowing is what makes this a rule rather than a formality: it drops
// entries the upstream's own cursors name, and substituting the first or last
// survivor would page from an id the upstream never said anything about.
//
// Only firstPage genuinely begins where the merged listing begins, with
// patchbay's own bindings, and only there does the merged listing's own final id
// close it — when has_more is true the next page is the upstream's, so last_id
// must be the cursor Anthropic can page from rather than a binding id that would
// mean nothing to it.
func mergedList(data []any, ids []string, listing upstreamListing, firstPage bool) modelList {
	list := modelList{Data: data, HasMore: listing.HasMore, FirstID: nil, LastID: nil}

	if len(ids) > 0 {
		list.FirstID = &ids[0]
		list.LastID = &ids[len(ids)-1]
	}

	if firstPage {
		if listing.HasMore {
			list.LastID = listing.LastID
		}

		return list
	}

	if listing.FirstID != nil {
		list.FirstID = listing.FirstID
	}

	if listing.LastID != nil {
		list.LastID = listing.LastID
	}

	return list
}

// seatEntry renders one public selector, or one model a Context's default Seat
// serves, as a listing entry. The Seat supplies the token ceilings and nothing
// else: its ID is private, and a listing that named it would publish the
// registry's own vocabulary to every caller.
func seatEntry(seat registry.Seat, model string) modelEntry {
	return modelEntry{
		Type: "model", ID: model, DisplayName: model, CreatedAt: epochCreatedAt,
		MaxInputTokens: seat.MaxInputTokens, MaxTokens: seat.MaxOutputTokens,
	}
}

// entryID reads the id of an upstream listing entry. An entry that is not an
// object, or carries no id, has no id a binding could shadow — it is listed as
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
