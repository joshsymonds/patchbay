// Package registry resolves an inbound model name to the route that serves it.
//
// Every request rides exactly one Route. Claude models ride the built-in
// default route returned by DefaultRoute — a fixed value that is never read
// from disk. Foreign aliases come from a JSON registry file managed
// declaratively by home-manager, which may well be a symlink into /nix/store;
// the file is re-read whenever its modification time or symlink target changes,
// so a rebuild takes effect on the next request with nothing to restart.
//
// The file is honored all-or-nothing. One route that breaks a validation rule
// invalidates the whole document, and a document that fails to parse or
// validate contributes no aliases at all. What it can never do is break the
// default route: Resolve keeps serving DefaultRoute for every model, and Err
// reports why the foreign aliases went away.
package registry

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Registry resolves model names against a registry file, reloading it when the
// file changes. It is safe for concurrent use.
type Registry struct {
	path string

	mu     sync.RWMutex
	loaded bool
	stamp  fileStamp
	routes map[string]Route
	err    error
}

// New returns a Registry backed by the JSON file at path. Nothing is read until
// the first Resolve or Err call, and a missing file is not an error: it simply
// means no foreign aliases exist.
func New(path string) *Registry {
	return &Registry{path: path}
}

// Resolve returns the route serving model: the exact alias from a valid
// registry file if one matches, otherwise the built-in default route. Aliases
// match as whole opaque strings, so a namespaced key like "openrouter/sol"
// resolves the same way any other key does; the slash carries no meaning here.
//
// An alias may deliberately shadow a real Claude model id — that is how a model
// gets pinned to a different upstream. Resolve re-reads the registry file when
// it has changed since the last call, so it always answers from the file as it
// stands now.
//
// Resolve never fails, which is why it returns no error. A missing, unparsable
// or invalid registry file costs only the foreign aliases; the default route
// always survives. The health of the file itself is observed through Err.
func (r *Registry) Resolve(model string) Route {
	r.refresh()

	r.mu.RLock()
	defer r.mu.RUnlock()

	if route, ok := r.routes[model]; ok {
		return route
	}

	return DefaultRoute()
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

// Aliases returns every foreign route the current registry file defines, sorted
// by alias. The built-in default route is not one of them: it answers for
// models no alias claims and has no name to list.
//
// Like Resolve, it answers from the file as it stands now, and it never fails —
// a missing, unparsable or invalid file simply has no aliases to list, which
// Err explains. The slice and the routes in it are the caller's own copy, so
// nothing a caller does to them reaches the registry.
func (r *Registry) Aliases() []Route {
	r.refresh()

	r.mu.RLock()
	defer r.mu.RUnlock()

	aliases := make([]Route, 0, len(r.routes))

	for _, route := range r.routes {
		// The struct copy still points at the registry's own extra_body bytes,
		// which every later request on this alias merges from. Cloning them is
		// what makes the isolation above true rather than nearly true.
		route.ExtraBody = bytes.Clone(route.ExtraBody)
		aliases = append(aliases, route)
	}

	slices.SortFunc(aliases, func(a, b Route) int { return cmp.Compare(a.Alias, b.Alias) })

	return aliases
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

	r.routes, r.err = loadRoutes(r.path)
	r.stamp = current
	r.loaded = true
}

// loadRoutes reads, parses and validates the registry file. An absent file
// yields no routes and no error. Any other failure yields no routes and an
// error naming the file, the offending alias and the rule it broke.
func loadRoutes(path string) (map[string]Route, error) {
	data, readErr := os.ReadFile(filepath.Clean(path))
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			return map[string]Route{}, nil
		}

		return nil, fmt.Errorf("registry %s: read: %w", path, readErr)
	}

	routes, parseErr := parseRegistry(data)
	if parseErr != nil {
		return nil, fmt.Errorf("registry %s: %w", path, parseErr)
	}

	if validateErr := validateRoutes(routes); validateErr != nil {
		return nil, fmt.Errorf("registry %s: %w", path, validateErr)
	}

	return routes, nil
}
