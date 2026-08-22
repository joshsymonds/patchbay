package ledger_test

import (
	"reflect"
	"testing"

	"github.com/joshsymonds/patchbay/internal/ledger"
)

func TestHasDropSignature(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		shape []string
		want  bool
	}{
		{name: "empty", shape: nil},
		{name: "separated", shape: []string{"thinking", "text", "thinking"}},
		{name: "adjacent thinking", shape: []string{"thinking", "thinking"}, want: true},
		{name: "thinking redacted", shape: []string{"thinking", "redacted_thinking"}, want: true},
		{name: "adjacent redacted", shape: []string{"redacted_thinking", "redacted_thinking"}, want: true},
		{
			name:  "redacted separated by text",
			shape: []string{"redacted_thinking", "text", "redacted_thinking"},
		},
		{
			name:  "trailing adjacent pair after a clean run",
			shape: []string{"thinking", "text", "thinking", "text", "thinking", "thinking"},
			want:  true,
		},
		{name: "unknown block types", shape: []string{"server_tool_use", "web_search_tool_result"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ledger.HasDropSignature(tc.shape); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

// TestHasDropSignatureDoesNotMutateInput keeps the predicate a predicate: it is
// handed the caller's own slice, so reordering or rewriting it to answer would
// change the shape the caller is about to record.
func TestHasDropSignatureDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	shape := []string{"thinking", "thinking", "text"}
	want := []string{"thinking", "thinking", "text"}

	ledger.HasDropSignature(shape)

	if !reflect.DeepEqual(shape, want) {
		t.Fatalf("HasDropSignature mutated its input: got %v; want %v", shape, want)
	}
}

// TestBlockShapeCopiesInput pins the copy in both directions. A shape is read
// off a live response and then held on a record until it is written, so sharing
// backing memory with the caller in either direction would let one of them
// silently rewrite the other.
func TestBlockShapeCopiesInput(t *testing.T) {
	t.Parallel()

	source := []string{"thinking", "text", "tool_use"}
	shape := ledger.BlockShape(source)

	if !reflect.DeepEqual(shape, source) {
		t.Fatalf("BlockShape(%v) = %v; want an equal copy", source, shape)
	}

	source[0] = "mutated"
	if shape[0] != "thinking" {
		t.Fatalf("BlockShape aliased its input: shape[0] = %q after mutating the source", shape[0])
	}

	shape[1] = "also-mutated"
	if source[1] != "text" {
		t.Fatalf("mutating the result changed the source: source[1] = %q", source[1])
	}
}

func TestBlockShapeOfEmptyInput(t *testing.T) {
	t.Parallel()

	if got := ledger.BlockShape(nil); len(got) != 0 {
		t.Fatalf("BlockShape(nil) = %v; want an empty shape", got)
	}
	if got := ledger.BlockShape([]string{}); len(got) != 0 {
		t.Fatalf("BlockShape([]) = %v; want an empty shape", got)
	}
}

// TestDropSignatureMustAgreeWithBlockShape pins the flag to the shape it
// summarizes. The whole point of drop_signature is that a later reader can
// select on it without re-deriving it, so a record whose flag disagrees with its
// own block_shape is a lie the ledger must not store — in either direction.
func TestDropSignatureMustAgreeWithBlockShape(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		shape     []string
		signature bool
		valid     bool
	}{
		{name: "derived true", shape: []string{"thinking", "thinking"}, signature: true, valid: true},
		{name: "derived false", shape: []string{"thinking", "text"}, valid: true},
		{name: "empty shape", shape: nil, valid: true},
		{
			name:      "false positive",
			shape:     []string{"thinking", "text"},
			signature: true,
		},
		{
			name:  "false negative",
			shape: []string{"thinking", "redacted_thinking"},
		},
		{
			name:      "empty shape claimed as a drop",
			shape:     nil,
			signature: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := validRecord(t)
			rec.BlockShape = ledger.BlockShape(tc.shape)
			rec.DropSignature = tc.signature
			if got := rec.Validate() == nil; got != tc.valid {
				t.Fatalf("Validate() valid=%v, want %v (shape=%v signature=%v)",
					got, tc.valid, tc.shape, tc.signature)
			}
		})
	}
}
