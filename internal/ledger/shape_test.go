package ledger_test

import (
	"reflect"
	"testing"

	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/ledger"
)

func TestHasDropSignature(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		shape []string
		want  bool
	}{
		{name: "empty", shape: nil, want: false},
		{name: "empty slice", shape: []string{}, want: false},
		{name: "single thinking", shape: []string{"thinking"}, want: false},
		{name: "single redacted thinking", shape: []string{"redacted_thinking"}, want: false},
		{name: "text only", shape: []string{"text"}, want: false},
		{name: "thinking then text", shape: []string{"thinking", "text"}, want: false},
		{name: "thinking text thinking", shape: []string{"thinking", "text", "thinking"}, want: false},
		{name: "thinking tool_use thinking", shape: []string{"thinking", "tool_use", "thinking"}, want: false},
		{name: "adjacent thinking", shape: []string{"thinking", "thinking"}, want: true},
		{
			name:  "adjacent thinking mid sequence",
			shape: []string{"text", "thinking", "thinking", "tool_use"},
			want:  true,
		},
		{name: "thinking then redacted", shape: []string{"thinking", "redacted_thinking"}, want: true},
		{name: "redacted then thinking", shape: []string{"redacted_thinking", "thinking"}, want: true},
		{name: "adjacent redacted", shape: []string{"redacted_thinking", "redacted_thinking"}, want: true},
		{
			name:  "redacted separated by text",
			shape: []string{"redacted_thinking", "text", "redacted_thinking"},
			want:  false,
		},
		{
			name:  "trailing adjacent pair after a clean run",
			shape: []string{"thinking", "text", "thinking", "text", "thinking", "thinking"},
			want:  true,
		},
		{name: "unknown block types", shape: []string{"server_tool_use", "web_search_tool_result"}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := ledger.HasDropSignature(tc.shape); got != tc.want {
				t.Fatalf("HasDropSignature(%v) = %t; want %t", tc.shape, got, tc.want)
			}
		})
	}
}

func TestHasDropSignatureDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	shape := []string{"thinking", "thinking", "text"}
	want := []string{"thinking", "thinking", "text"}

	ledger.HasDropSignature(shape)

	if !reflect.DeepEqual(shape, want) {
		t.Fatalf("HasDropSignature mutated its input: got %v; want %v", shape, want)
	}
}

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

// TestDropSignatureMatchesRecordedShape ties the two halves together: the flag
// stored on a record is the one HasDropSignature derives from its shape.
func TestDropSignatureMatchesRecordedShape(t *testing.T) {
	t.Parallel()

	shapes := [][]string{
		{"thinking", "text"},
		{"thinking", "thinking"},
		{"text", "redacted_thinking", "redacted_thinking"},
	}
	for _, blockTypes := range shapes {
		shape := ledger.BlockShape(blockTypes)
		rec := ledger.Record{
			Timestamp:       testTimestamp(t, testDay),
			Host:            "gnomon",
			Context:         "personal",
			CredentialClass: billing.CredentialOAuth,

			Upstream:      "https://api.anthropic.com",
			Model:         "claude-fable-5",
			RequestModel:  "claude-fable-5",
			AuthMode:      "forward",
			Status:        200,
			BlockShape:    shape,
			DropSignature: ledger.HasDropSignature(shape),
		}
		if err := rec.Validate(); err != nil {
			t.Fatalf("Validate() on a shape-derived record returned error: %v", err)
		}
		if rec.DropSignature != ledger.HasDropSignature(rec.BlockShape) {
			t.Fatalf("record drop_signature %t disagrees with its shape %v", rec.DropSignature, rec.BlockShape)
		}
	}
}
