package jsonstrict_test

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/jsonstrict"
)

// TestUniqueDocumentsAreAccepted pins what the rule is NOT about. It owns one
// structural question — is any member name said twice — so every shape a
// document can otherwise take passes through it untouched: nesting of any
// depth, arrays of anything, names differing only by case, repeated scalars,
// and numbers no float64 could hold.
func TestUniqueDocumentsAreAccepted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, data string }{
		{name: "an empty object", data: `{}`},
		{name: "an empty array", data: `[]`},
		{name: "a bare scalar", data: `"gpt-5.6-sol"`},
		{name: "a bare null", data: `null`},
		{name: "one flat object", data: `{"effort":"low","temperature":1}`},
		{
			name: "names differing only by case are different members",
			data: `{"effort":"low","Effort":"high","EFFORT":"medium"}`,
		},
		{
			name: "repeated equal scalars are not repeated members",
			data: `{"stop":["x","x","x"],"flags":[true,true,null,null]}`,
		},
		{
			name: "the same name once per object at every depth",
			data: `{"name":"root","child":{"name":"inner","child":{"name":"deepest"}},` +
				`"list":[{"name":"first"},{"name":"second"}]}`,
		},
		{
			name: "objects nested through arrays of arrays",
			data: `{"matrix":[[{"cell":1},{"cell":2}],[{"cell":3}]]}`,
		},
		{
			name: "a number no float64 holds is never parsed",
			data: `{"huge":1e999,"precise":123456789012345678901234567890}`,
		},
		{name: "surrounding whitespace", data: "  \n{\"effort\":\"low\"}\t\n "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := jsonstrict.RejectDuplicateNames([]byte(tc.data)); err != nil {
				t.Fatalf("RejectDuplicateNames(%s) = %v; want the document accepted", tc.data, err)
			}
		})
	}
}

// TestRepeatedMemberNamesAreRejected is the whole of the rule: a name said
// twice, wherever it sits. encoding/json keeps the last value of a repeated
// name, so every case here would otherwise decode as its second saying with
// nothing left to show for the first — including through arrays, which carry
// objects like any other value.
//
// The error has to be actionable, so each case pins the member it names and the
// path it was found at.
func TestRepeatedMemberNamesAreRejected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, data, member, path string }{
		{
			name:   "at the root",
			data:   `{"effort":"low","effort":"high"}`,
			member: `"effort"`,
			path:   "$",
		},
		{
			name:   "inside a nested object",
			data:   `{"reasoning":{"effort":"low","effort":"high"}}`,
			member: `"effort"`,
			path:   "$.reasoning",
		},
		{
			name:   "inside an object in an array",
			data:   `{"tools":[{"name":"first"},{"name":"second","name":"third"}]}`,
			member: `"name"`,
			path:   "$.tools[1]",
		},
		{
			name:   "deep under an array of arrays",
			data:   `{"matrix":[[{"cell":1},{"cell":2,"cell":3}]]}`,
			member: `"cell"`,
			path:   "$.matrix[0][1]",
		},
		{
			name:   "in an object under an array at the root",
			data:   `[0,{"reasoning":{"effort":"low","effort":"high"}}]`,
			member: `"effort"`,
			path:   "$[1].reasoning",
		},
		{
			name:   "spelled with an escape the second time",
			data:   "{\"effort\":\"low\",\"\\u0065ffort\":\"high\"}",
			member: `"effort"`,
			path:   "$",
		},
		{
			name:   "a repeated object-valued member",
			data:   `{"reasoning":{"effort":"low"},"reasoning":{"effort":"high"}}`,
			member: `"reasoning"`,
			path:   "$",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := jsonstrict.RejectDuplicateNames([]byte(tc.data))
			if err == nil {
				t.Fatalf("RejectDuplicateNames(%s) = nil; want the repeat refused", tc.data)
			}
			for _, want := range []string{"duplicate", tc.member, tc.path} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %s", err, want)
				}
			}
		})
	}
}

// TestExactlyOneCompleteValueIsRequired covers the other half of reading raw
// bytes: a second value after the first is data nobody will look at, and a
// document that stops early is not one value either.
func TestExactlyOneCompleteValueIsRequired(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, data string }{
		{name: "a second object", data: `{"effort":"low"} {"effort":"high"}`},
		{name: "a second scalar", data: `{"effort":"low"} 7`},
		{name: "two scalars", data: `1 2`},
		{name: "trailing bytes that are not JSON", data: `{"effort":"low"} garbage`},
		{name: "nothing at all", data: ``},
		{name: "whitespace only", data: "  \n\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := jsonstrict.RejectDuplicateNames([]byte(tc.data)); err == nil {
				t.Fatalf("RejectDuplicateNames(%q) = nil; want one complete value required", tc.data)
			}
		})
	}
}

// TestTrailingBytesTellTwoFailuresApart separates the two ways a document can
// carry more than one value, because they are not the same fault.
//
// A second value that parses is surplus: the bytes are well-formed JSON and the
// only thing wrong with them is that there are more of them, which is the rule's
// own refusal to make. Bytes after the value that are not JSON at all are
// malformed, and what a caller is told about those is encoding/json's own
// account, wrapped the way every other syntax error here is rather than
// flattened into the generic refusal.
func TestTrailingBytesTellTwoFailuresApart(t *testing.T) {
	t.Parallel()

	for _, data := range []string{`{"effort":"low"} garbage`, `[1,2] ]`, `"done" :`} {
		err := jsonstrict.RejectDuplicateNames([]byte(data))
		var syntax *json.SyntaxError
		if !errors.As(err, &syntax) {
			t.Errorf("RejectDuplicateNames(%s) = %v; want a wrapped *json.SyntaxError", data, err)
		}
	}

	for _, data := range []string{`{"effort":"low"} {"effort":"high"}`, `{"effort":"low"} 7`, `1 2`} {
		err := jsonstrict.RejectDuplicateNames([]byte(data))
		if err == nil {
			t.Fatalf("RejectDuplicateNames(%s) = nil; want the second value refused", data)
		}
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			t.Errorf("RejectDuplicateNames(%s) = %v; want surplus JSON, not malformed bytes", data, err)
		}
		if !strings.Contains(err.Error(), "unexpected trailing data") {
			t.Errorf("RejectDuplicateNames(%s) = %v; want it to name the trailing data", data, err)
		}
	}
}

// TestMalformedJSONKeepsItsOwnError pins the wrapping. The rule adds one
// refusal of its own; everything else a caller is told about its bytes is
// encoding/json's own account of them, and a caller that inspects the error
// still can.
func TestMalformedJSONKeepsItsOwnError(t *testing.T) {
	t.Parallel()

	for _, data := range []string{`{"effort":}`, `{1:2}`, `{"effort" "low"}`, `[1,]`} {
		err := jsonstrict.RejectDuplicateNames([]byte(data))
		var syntax *json.SyntaxError
		if !errors.As(err, &syntax) {
			t.Errorf("RejectDuplicateNames(%s) = %v; want a wrapped *json.SyntaxError", data, err)
		}
	}

	// A document that stops mid-value runs the token stream out, which
	// encoding/json reports as io.EOF. The path carried alongside it is what
	// says where the bytes ran out.
	for _, tc := range []struct{ data, path string }{
		{data: `{`, path: "$"},
		{data: `{"reasoning":{"effort":"low"`, path: "$.reasoning"},
		{data: `[{"a":1}`, path: "$"},
	} {
		err := jsonstrict.RejectDuplicateNames([]byte(tc.data))
		if !errors.Is(err, io.EOF) {
			t.Errorf("RejectDuplicateNames(%s) = %v; want a wrapped io.EOF", tc.data, err)
		}
		if err != nil && !strings.Contains(err.Error(), tc.path) {
			t.Errorf("error %q does not say where the bytes ran out (%s)", err, tc.path)
		}
	}
}
