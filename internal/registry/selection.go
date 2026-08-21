package registry

import (
	"bytes"
	"context"
	"fmt"
	"slices"
)

// Selection is the Context selected by the request path.
type Selection struct {
	Name     string
	Explicit bool
}

// SelectionKind is the reason a seat was selected.
type SelectionKind string

// Selection kinds explain which binding class selected a Seat.
const (
	SelectionModel    SelectionKind = "model"
	SelectionSubagent SelectionKind = "subagent"
	SelectionDefault  SelectionKind = "default"
)

// Match describes the selector that selected a seat. Model is nil for defaults.
type Match struct {
	Kind  SelectionKind
	Model *string
}

// SubagentObservation is a positively validated marked-subagent observation.
type SubagentObservation struct {
	AgentID       string
	ParentAgentID string
}

// Resolution is an immutable Context snapshot and one selected Seat within it.
type Resolution struct {
	Context  string
	Match    Match
	SeatID   string
	Seat     Seat
	Subagent *SubagentObservation

	defaultSeatID string
	seats         map[string]Seat
	models        map[string]string
	subagents     *Subagents
	foreign       map[string]string
}

// Select applies the same precedence used by Registry.Resolve to this immutable snapshot.
func (r Resolution) Select(model string, observation *SubagentObservation) (Resolution, error) {
	r.Match = Match{}
	r.SeatID = ""
	r.Seat = Seat{}
	r.Subagent = cloneObservation(observation)

	if seatID, ok := r.models[model]; ok {
		matched := model
		return r.selected(seatID, Match{Kind: SelectionModel, Model: &matched}), nil
	}

	// Marked traffic that matched no exact subagent binding still fell to the
	// marked-subagent policy, not to the Context's ordinary default, so it is
	// reported as a subagent selection naming no model. The nil model is the
	// whole distinction between the unnamed fallback and an exact marked hit.
	if observation != nil && r.subagents != nil {
		if seatID, ok := r.subagents.Models[model]; ok {
			matched := model
			return r.selected(seatID, Match{Kind: SelectionSubagent, Model: &matched}), nil
		}
		return r.selected(r.subagents.DefaultSeat, Match{Kind: SelectionSubagent}), nil
	}

	if selector, guarded := r.foreign[model]; guarded {
		return r, fmt.Errorf("%w: %q is what model %q asks of its upstream; request the selector instead",
			ErrRewriteTarget, model, selector)
	}

	return r.selected(r.defaultSeatID, Match{Kind: SelectionDefault}), nil
}

// selected publishes one Seat on the Resolution. The Seat is copied by value but
// extra_body is a slice, so it is cloned: a Resolution is an immutable snapshot
// and the pipeline merges a request's own body over these bytes, which without
// the clone would rewrite the registry document every later request reads.
func (r Resolution) selected(seatID string, match Match) Resolution {
	seat := r.seats[seatID]
	seat.ExtraBody = bytes.Clone(seat.ExtraBody)
	r.SeatID = seatID
	r.Seat = seat
	r.Match = match
	return r
}

// Models returns the public Context bindings as an isolated copy.
func (r Resolution) Models() map[string]string {
	return cloneBindings(r.models)
}

// SubagentPolicy returns the marked-subagent bindings as an isolated copy.
func (r Resolution) SubagentPolicy() *Subagents {
	return cloneSubagents(r.subagents)
}

// DefaultSeat returns the Context default Seat ID and Seat.
func (r Resolution) DefaultSeat() (string, Seat) {
	seat := r.seats[r.defaultSeatID]
	seat.ExtraBody = bytes.Clone(seat.ExtraBody)
	return r.defaultSeatID, seat
}

// PublicBindings returns public selectors sorted lexically with their selected seats.
func (r Resolution) PublicBindings() []Binding {
	selectors := make([]string, 0, len(r.models))
	for selector := range r.models {
		selectors = append(selectors, selector)
	}
	slices.Sort(selectors)
	out := make([]Binding, 0, len(selectors))
	for _, selector := range selectors {
		seatID := r.models[selector]
		seat := r.seats[seatID]
		seat.ExtraBody = bytes.Clone(seat.ExtraBody)
		out = append(out, Binding{Selector: selector, SeatID: seatID, Seat: seat})
	}
	return out
}

// Binding is one public selector and the Seat it names.
type Binding struct {
	Selector string
	SeatID   string
	Seat     Seat
}

type selectionKey struct{}
type resolutionKey struct{}
type resolutionErrorKey struct{}

// WithSelection attaches the URL-selected Context to a request.
func WithSelection(ctx context.Context, sel Selection) context.Context {
	return context.WithValue(ctx, selectionKey{}, sel)
}

// SelectionFrom returns the URL-selected Context attached to a request.
func SelectionFrom(ctx context.Context) Selection {
	if sel, ok := ctx.Value(selectionKey{}).(Selection); ok {
		return sel
	}
	return Selection{}
}

// WithResolution attaches an immutable Context resolution to a request.
func WithResolution(ctx context.Context, res Resolution) context.Context {
	return context.WithValue(ctx, resolutionKey{}, res)
}

// ResolutionFrom returns the immutable Context resolution attached to a request.
func ResolutionFrom(ctx context.Context) (Resolution, bool) {
	res, ok := ctx.Value(resolutionKey{}).(Resolution)
	return res, ok
}

// WithResolutionError attaches a fail-closed Context resolution error to a request.
func WithResolutionError(ctx context.Context, err error) context.Context {
	return context.WithValue(ctx, resolutionErrorKey{}, err)
}

// ResolutionErrorFrom returns the fail-closed Context resolution error attached to a request.
func ResolutionErrorFrom(ctx context.Context) error {
	value := ctx.Value(resolutionErrorKey{})
	if value == nil {
		return nil
	}
	err, ok := value.(error)
	if !ok {
		return nil
	}
	return err
}

func cloneObservation(in *SubagentObservation) *SubagentObservation {
	if in == nil {
		return nil
	}
	cloned := *in
	return &cloned
}

func cloneBindings(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneSubagents(in *Subagents) *Subagents {
	if in == nil {
		return nil
	}
	return &Subagents{DefaultSeat: in.DefaultSeat, Models: cloneBindings(in.Models)}
}
