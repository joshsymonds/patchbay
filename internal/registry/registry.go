// Package registry loads global Seats and resolves Context Seat bindings.
package registry

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// loadAttempts bounds how many times one acquisition starts over when the file
// is replaced underneath the read. Three is enough for the writers that
// actually do this — an editor's write-and-rename, a home-manager generation
// swap — and small enough that a file being rewritten in a loop costs a bounded
// refusal rather than a request that never returns.
const loadAttempts = 3

// Resolution and lifecycle sentinel errors.
var (
	ErrUnknownContext = errors.New("unknown context")
	ErrRewriteTarget  = errors.New("model is a rewrite target, not a binding")
	ErrInvalid        = errors.New("registry is invalid")
)

// State is the non-secret operator view of the current registry snapshot.
type State struct {
	Valid          bool
	Error          string
	DefaultContext string
	Seats          map[string]Seat
	Contexts       map[string]Context
}

// Registry owns the current valid document or fail-closed invalid sentinel.
type Registry struct {
	path string

	mu     sync.RWMutex
	stamp  fileStamp
	doc    *document
	err    error
	loaded bool
}

// New validates the initial registry synchronously. No caller should bind a listener before it succeeds.
func New(path string) (*Registry, error) {
	snap, err := loadSnapshot(path)
	if err != nil {
		return nil, fmt.Errorf("registry %s: %w", path, err)
	}
	if snap.err != nil {
		return nil, fmt.Errorf("registry %s: %w", path, snap.err)
	}
	return &Registry{path: path, stamp: snap.stamp, doc: snap.doc, loaded: true}, nil
}

// Resolve selects one Seat using public, marked-subagent, guard, then default precedence.
//
// A registry that went invalid under a running gateway is refused with the bare
// ErrInvalid sentinel and nothing else. This error is what an unauthenticated
// caller reads in a 503 and what the ledger persists as that refusal's body,
// while the detail behind it — the field, the value the file named, the path it
// was read from — is exactly what a validation error quotes. Err carries that
// detail to the operator surfaces entitled to it; a request gets the sentinel.
func (r *Registry) Resolve(
	sel Selection,
	model string,
	observation *SubagentObservation,
) (Resolution, error) {
	r.refresh()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.err != nil || r.doc == nil {
		return Resolution{Context: sel.Name}, ErrInvalid
	}

	return resolveDocument(*r.doc, sel, model, observation)
}

// ResolveState applies the runtime resolver to an introspected non-secret snapshot.
//
// The State belongs to the caller and stays theirs: it is a struct of exported
// maps, slices and pointers, passed by value, so the copy this receives still
// points at everything the caller can still write to. A Resolution is documented
// as an immutable snapshot and outlives this call — Select re-runs the
// precedence against it, and Models, SubagentPolicy, DefaultSeat and
// PublicBindings hand out copies of it — so the whole document is cloned here,
// before anything is derived from it, rather than aliased and read later.
func ResolveState(
	state State,
	sel Selection,
	model string,
	observation *SubagentObservation,
) (Resolution, error) {
	if !state.Valid {
		return Resolution{Context: sel.Name}, ErrInvalid
	}
	seats := cloneSeats(state.Seats)
	contexts := make(map[string]Context, len(state.Contexts))
	for id, ctx := range state.Contexts {
		cloned := cloneContext(ctx)
		cloned.foreignTargets = foreignTargets(cloned, seats)
		contexts[id] = cloned
	}
	return resolveDocument(
		document{DefaultContext: state.DefaultContext, Seats: seats, Contexts: contexts},
		sel,
		model,
		observation,
	)
}

func resolveDocument(
	doc document,
	sel Selection,
	model string,
	observation *SubagentObservation,
) (Resolution, error) {
	name := sel.Name
	if !sel.Explicit {
		name = doc.DefaultContext
	}
	ctx, ok := doc.Contexts[name]
	if !ok {
		return Resolution{Context: name}, fmt.Errorf("%w %q", ErrUnknownContext, name)
	}
	return resolution(name, ctx, doc.Seats).Select(model, observation)
}

// State returns an isolated, non-secret snapshot. Invalid registries expose no
// stale summaries, and no detail of the file that was rejected: Error carries
// the same sentinel text Resolve does, because this snapshot is what
// introspection publishes to every holder of the caller key rather than to an
// operator reading the gateway's own diagnostics.
func (r *Registry) State() State {
	r.refresh()
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.state()
}

// Inspect returns both operator views of one snapshot: the isolated non-secret
// State, and the detailed error behind an invalid one.
//
// Reading them separately reads the file twice, and between the two reads a
// home-manager generation can swap it. Whichever way it moves, the two halves
// then describe different versions — a summary of the document that opened
// beside a refusal from the one that replaced it, or a summary of a document
// nothing ever validated. So the refresh happens once and both halves are taken
// under the same lock: a valid snapshot inspects with a nil error, and an
// invalid one hands back the same sanitized empty State introspection publishes
// together with the field and the value that rejected the file.
func (r *Registry) Inspect() (State, error) {
	r.refresh()
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.state(), r.invalidErr()
}

// state builds the isolated non-secret snapshot with the read lock already
// held, which is what lets Inspect pair it with the error of the same version.
func (r *Registry) state() State {
	if r.err != nil || r.doc == nil {
		return State{
			Valid:    false,
			Error:    ErrInvalid.Error(),
			Seats:    map[string]Seat{},
			Contexts: map[string]Context{},
		}
	}
	contexts := make(map[string]Context, len(r.doc.Contexts))
	for id, ctx := range r.doc.Contexts {
		contexts[id] = cloneContext(ctx)
	}
	return State{
		Valid:          true,
		DefaultContext: r.doc.DefaultContext,
		Seats:          cloneSeats(r.doc.Seats),
		Contexts:       contexts,
	}
}

// cloneSeats copies the Seat map by value and the extra_body bytes each Seat
// carries, which are the only part of a Seat that is not a value.
func cloneSeats(in map[string]Seat) map[string]Seat {
	out := make(map[string]Seat, len(in))
	for id, seat := range in {
		seat.ExtraBody = bytes.Clone(seat.ExtraBody)
		out[id] = seat
	}
	return out
}

// cloneContext copies a Context's bindings and marked-subagent policy. The
// foreign rewrite targets are left behind: they are derived from a Seat map, so
// whoever holds the clone derives them against the Seats it holds.
func cloneContext(in Context) Context {
	return Context{
		DefaultSeat: in.DefaultSeat,
		Models:      cloneBindings(in.Models),
		Subagents:   cloneSubagents(in.Subagents),
	}
}

// Path returns the configured registry path.
func (r *Registry) Path() string { return r.path }

// Err reports the current hot-reload invalid sentinel, if any, with the detail
// behind it: the path, the field, and the value the rejected file named. It is
// the operator's surface — startup, the check subcommand, the gateway's own
// logs — and the one place that detail survives, because it is what an operator
// needs to repair the file and what no request may be told.
func (r *Registry) Err() error {
	r.refresh()
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.invalidErr()
}

// invalidErr renders the current refusal for an operator with the read lock
// already held, or nil while the registry is valid. It copies nothing: this is
// on the request path, and what an operator is owed here is a sentence about
// the file, not a document.
func (r *Registry) invalidErr() error {
	if r.err == nil {
		return nil
	}
	return fmt.Errorf("%w: registry %s: %w", ErrInvalid, r.path, r.err)
}

// refresh reloads the document when the file's stamp has moved, and does
// nothing at all when it has not.
//
// The reload keeps whatever loadSnapshot settled on — a document or the detailed
// error the file produced instead of one — under the stamp of the version it was
// actually read from. An acquisition that never settled is the one case that
// stores no stamp: it installs the fail-closed sentinel and clears loaded, so
// the next request tries again rather than treating a version nothing validated
// as the current one.
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
	if r.loaded && r.stamp.equal(current) {
		return
	}
	r.store(loadSnapshot(r.path))
}

// store installs one acquisition's outcome, with the write lock already held.
//
// A settled acquisition is kept under its own stamp, whichever way it settled.
// An acquisition that never settled keeps no stamp and clears loaded, which is
// what sends the next request back to the file: storing the stamp of a version
// this registry never read whole would mark it current forever.
func (r *Registry) store(snap snapshot, err error) {
	if err != nil {
		r.doc = nil
		r.err = err
		r.loaded = false

		return
	}

	r.doc = snap.doc
	r.err = snap.err
	r.stamp = snap.stamp
	r.loaded = true
}

func resolution(name string, ctx Context, seats map[string]Seat) Resolution {
	return Resolution{
		Context:       name,
		defaultSeatID: ctx.DefaultSeat,
		seats:         seats,
		models:        ctx.Models,
		subagents:     ctx.Subagents,
		foreign:       ctx.foreignTargets,
	}
}

// snapshot is one consistent acquisition of the registry file: what the file
// settled on — a validated document, or the detailed error it produced instead
// of one — paired with the stamp of the exact version both describe.
type snapshot struct {
	doc   *document
	err   error
	stamp fileStamp
}

// loadSnapshot acquires path's current version consistently.
func loadSnapshot(path string) (snapshot, error) {
	return acquire(
		func() fileStamp { return statFile(path) },
		func() (document, error) { return loadDocument(path) },
	)
}

// acquire pairs one load with the stamp of the version it actually read.
//
// Stamping only after the read is what pairs document A with stamp B when the
// file is replaced between the two — and since the cache is invalidated by the
// stamp alone, that pairing is permanent: the replacement's own stamp is already
// installed, so every later refresh sees it as current and the superseded
// document is served until something else moves the file. So the file is
// stamped on both sides of the read and the result is kept only when the two
// agree.
//
// A file that keeps moving is bounded rather than spun on. It yields no
// snapshot and no stamp at all, because the only thing worse than refusing an
// acquisition is trusting a stamp for bytes nobody validated.
func acquire(stat func() fileStamp, load func() (document, error)) (snapshot, error) {
	for range loadAttempts {
		before := stat()

		doc, err := load()

		after := stat()
		if !before.equal(after) {
			continue
		}

		return settled(doc, err, after), nil
	}

	return snapshot{}, fmt.Errorf(
		"changed while being read %d times running; no version could be read whole",
		loadAttempts,
	)
}

// settled builds the snapshot of one version read whole: the document it
// produced, or the detailed error it produced instead of one, under the stamp
// they both describe. A failure keeps no document, so nothing downstream can
// mistake a zero document for a loaded one.
func settled(doc document, err error, stamp fileStamp) snapshot {
	if err != nil {
		return snapshot{doc: nil, err: err, stamp: stamp}
	}

	return snapshot{doc: &doc, err: nil, stamp: stamp}
}

func loadDocument(path string) (document, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return document{}, fmt.Errorf("read: %w", err)
	}
	doc, err := parseRegistry(data)
	if err != nil {
		return document{}, err
	}
	if validateErr := validateDocument(&doc); validateErr != nil {
		return document{}, validateErr
	}
	return doc, nil
}
