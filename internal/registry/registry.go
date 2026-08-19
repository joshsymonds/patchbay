// Package registry resolves an inbound request to the route that serves it.
//
// Every request rides exactly one Route, chosen inside exactly one Context. A
// context is a named world of its own: its aliases, and the default route every
// model those aliases do not claim rides. Requests select a context by URL path
// prefix, and bare paths — everything Claude Code sends without knowing
// contexts exist — select the personal one.
//
// The contexts come from a JSON registry file managed declaratively by
// home-manager, which may well be a symlink into /nix/store; the file is
// re-read whenever its modification time or symlink target changes, so a
// rebuild takes effect on the next request with nothing to restart.
//
// The file is honored all-or-nothing. One route that breaks a validation rule
// invalidates the whole document, and a document that fails to parse or
// validate contributes no contexts at all. What it can never do is break the
// bare path: a request that named no context still resolves to the built-in
// DefaultRoute, and Err reports why the rest went away. A request that DID name
// a context gets the opposite bargain — it fails closed with ErrUnknownContext
// rather than being served by a world it did not ask for.
package registry

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// PersonalContext is the context a request with no path prefix resolves
// against. Claude Code addresses patchbay as a plain Anthropic endpoint, so the
// unprefixed path has to mean something, and it means this.
const PersonalContext = "personal"

// ErrUnknownContext is returned when a request names a context the currently
// loaded registry cannot produce — an unknown name, or any name at all when the
// file is missing or broken. It is never softened into a fallback.
var ErrUnknownContext = errors.New("unknown context")

// Selection is the context one request chose, and whether it said so. The zero
// value is what a bare, unprefixed path selects: the personal context, with the
// built-in default route standing behind it if the registry cannot answer.
type Selection struct {
	// Name is the context the request named.
	Name string

	// Explicit reports whether the name came from the request's own path. An
	// explicit name that cannot be resolved fails closed; an implicit one falls
	// back to the built-in default route.
	Explicit bool
}

// Resolution is everything one request's context and model resolve to: the
// route that serves it, and the context metadata the model endpoints describe
// that same context with. Both come from one lookup so a listing can never
// describe a context differently from how the pipeline forwards it.
type Resolution struct {
	// Context is the resolved context's name.
	Context string

	// Route is the route serving the model that was resolved.
	Route Route

	// DefaultRoute is the context's effective default route: its own if it
	// declared one, otherwise the built-in.
	DefaultRoute Route

	// Aliases is every route the context defines, sorted by alias. It is the
	// caller's own copy, so nothing a caller does to it reaches the registry.
	Aliases []Route
}

// selectionKey is the private type of the request-context key a Selection is
// carried under, so no other package can collide with it.
type selectionKey struct{}

// Registry resolves requests against a registry file, reloading it when the
// file changes. It is safe for concurrent use.
type Registry struct {
	path string

	mu       sync.RWMutex
	loaded   bool
	stamp    fileStamp
	contexts map[string]Context
	err      error
}

// New returns a Registry backed by the JSON file at path. Nothing is read until
// the first Resolve or Err call, and a missing file is not an error: it simply
// means no context has anything of its own to add.
func New(path string) *Registry {
	return &Registry{path: path}
}

// WithSelection returns ctx carrying sel, so the context a request selected in
// its path is known to every handler downstream of the one that stripped it.
func WithSelection(ctx context.Context, sel Selection) context.Context {
	return context.WithValue(ctx, selectionKey{}, sel)
}

// SelectionFrom returns the selection ctx carries. A request that never named a
// context — every bare path — selects the personal one implicitly.
func SelectionFrom(ctx context.Context) Selection {
	if sel, ok := ctx.Value(selectionKey{}).(Selection); ok {
		return sel
	}

	return Selection{Name: PersonalContext, Explicit: false}
}

// Resolve returns the route serving model inside the selected context, with
// that context's own metadata alongside it. Aliases match as whole opaque
// strings, so a namespaced key like "openrouter/sol" resolves the same way any
// other key does; the slash carries no meaning here. An alias may deliberately
// shadow a real Claude model id — that is how a model gets pinned to a
// different upstream.
//
// Resolve re-reads the registry file when it has changed since the last call,
// so it always answers from the file as it stands now.
//
// It fails only one way: an explicitly named context the current file cannot
// produce returns ErrUnknownContext, and the caller must refuse the request
// rather than serve it from somewhere else. A bare selection never fails — a
// missing, unparsable or invalid registry file costs it only the foreign
// aliases, and the built-in default route always survives. The health of the
// file itself is observed through Err.
func (r *Registry) Resolve(sel Selection, model string) (Resolution, error) {
	r.refresh()

	r.mu.RLock()
	defer r.mu.RUnlock()

	name := sel.Name
	if name == "" && !sel.Explicit {
		name = PersonalContext
	}

	found, ok := r.contexts[name]
	if !ok && sel.Explicit {
		return Resolution{Context: name}, fmt.Errorf("%w %q", ErrUnknownContext, name)
	}

	res := resolution(name, found, ok)

	if route, matched := found.Routes[model]; matched {
		res.Route = route
	}

	return res, nil
}

// Contexts returns every context the registry currently serves, sorted by name,
// alongside whether the registry file behind them actually loaded. It is the
// same lookup Resolve answers one request with, so nothing built from it can
// describe a context differently from how the pipeline forwards to it.
//
// The personal context is always present. A missing or broken file costs every
// other context, and reports loaded false, but a bare request still rides the
// built-in default route — so a document that left personal out would describe a
// gateway that does not exist.
func (r *Registry) Contexts() ([]Resolution, bool) {
	r.refresh()

	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.contexts)+1)
	for name := range r.contexts {
		names = append(names, name)
	}

	if _, ok := r.contexts[PersonalContext]; !ok {
		names = append(names, PersonalContext)
	}

	slices.Sort(names)

	all := make([]Resolution, 0, len(names))
	for _, name := range names {
		found, ok := r.contexts[name]
		all = append(all, resolution(name, found, ok))
	}

	return all, r.err == nil && r.stamp.exists
}

// Path is the registry file this Registry reads. It is fixed for the lifetime of
// the Registry — only the file's contents are ever reloaded.
func (r *Registry) Path() string {
	return r.path
}

// Err returns the parse or validation error from the current registry file, or
// nil when the file is valid or simply absent. It reflects the file as of the
// last check, refreshing first, so it can be called on its own to report
// registry health.
func (r *Registry) Err() error {
	r.refresh()

	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.err
}

// refresh reloads the registry file when its stamp has moved since the last
// load. Both outcomes are cached, success and failure alike, so a broken file
// is not re-read and re-reported on every single request.
func (r *Registry) refresh() {
	current := statFile(r.path)

	r.mu.RLock()
	fresh := r.loaded && r.stamp.equal(current)
	r.mu.RUnlock()

	if fresh {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Another goroutine may have reloaded while this one waited for the lock.
	if r.loaded && r.stamp.equal(current) {
		return
	}

	r.contexts, r.err = loadContexts(r.path)
	r.stamp = current
	r.loaded = true
}

// resolution describes one context: the one the file defines under name, or —
// when found is not a loaded context — the built-in world a bare request rides
// when the registry has nothing to add.
func resolution(name string, found Context, ok bool) Resolution {
	if !ok {
		return Resolution{
			Context:      name,
			Route:        DefaultRoute(),
			DefaultRoute: DefaultRoute(),
			Aliases:      []Route{},
		}
	}

	return Resolution{
		Context:      name,
		Route:        found.DefaultRoute,
		DefaultRoute: found.DefaultRoute,
		Aliases:      aliasList(found.Routes),
	}
}

// aliasList returns a context's routes sorted by alias, as the caller's own
// copy.
func aliasList(routes map[string]Route) []Route {
	aliases := make([]Route, 0, len(routes))

	for _, route := range routes {
		// The struct copy still points at the registry's own extra_body bytes,
		// which every later request on this alias merges from. Cloning them is
		// what makes the isolation above true rather than nearly true.
		route.ExtraBody = bytes.Clone(route.ExtraBody)
		aliases = append(aliases, route)
	}

	slices.SortFunc(aliases, func(a, b Route) int { return cmp.Compare(a.Alias, b.Alias) })

	return aliases
}

// loadContexts reads, parses and validates the registry file. An absent file
// yields no contexts and no error. Any other failure yields no contexts and an
// error naming the file, the offending context or alias, and the rule it broke.
func loadContexts(path string) (map[string]Context, error) {
	data, readErr := os.ReadFile(filepath.Clean(path))
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			return map[string]Context{}, nil
		}

		return nil, fmt.Errorf("registry %s: read: %w", path, readErr)
	}

	contexts, parseErr := parseRegistry(data)
	if parseErr != nil {
		return nil, fmt.Errorf("registry %s: %w", path, parseErr)
	}

	if validateErr := validateContexts(contexts); validateErr != nil {
		return nil, fmt.Errorf("registry %s: %w", path, validateErr)
	}

	return contexts, nil
}
