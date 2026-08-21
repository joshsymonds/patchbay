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
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ledger.HasDropSignature(tc.shape); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestBlockShapeCopiesInput(t *testing.T) {
	t.Parallel()
	source := []string{"thinking", "text"}
	shape := ledger.BlockShape(source)
	source[0] = "changed"
	if !reflect.DeepEqual(shape, []string{"thinking", "text"}) {
		t.Fatalf("shape=%v", shape)
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
