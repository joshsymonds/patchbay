package accounting_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/accounting"
)

const (
	maxNanoUSD = accounting.NanoUSD(9223372036854775807)
	minNanoUSD = accounting.NanoUSD(-9223372036854775807 - 1)
)

func TestNanoUSDIsSignedInt64(t *testing.T) {
	t.Parallel()

	if got := reflect.TypeOf(accounting.NanoUSD(0)).Kind(); got != reflect.Int64 {
		t.Fatalf("NanoUSD kind = %v; want %v", got, reflect.Int64)
	}

	if got := int64(accounting.NanoUSD(-1)); got != -1 {
		t.Fatalf("int64(NanoUSD(-1)) = %d; want -1", got)
	}
}

func TestParseUSDAcceptsJSONDecimalsAndRoundsHalfEven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want accounting.NanoUSD
	}{
		{name: "zero", text: "0", want: 0},
		{name: "one USD", text: "1", want: 1000000000},
		{name: "fraction", text: "1.25", want: 1250000000},
		{name: "negative exponent", text: "1e-9", want: 1},
		{name: "uppercase positive exponent", text: "1E+1", want: 10000000000},
		{name: "zero-padded exponent", text: "100e-002", want: 1000000000},
		{name: "half nano rounds to even zero", text: "0.0000000005", want: 0},
		{name: "one and a half nanos rounds to even two", text: "0.0000000015", want: 2},
		{name: "two and a half nanos rounds to even two", text: "0.0000000025", want: 2},
		{name: "half nano exponent", text: "5e-10", want: 0},
		{name: "one and a half nanos exponent", text: "15e-10", want: 2},
		{name: "two and a half nanos exponent", text: "25e-10", want: 2},
		{name: "just below half", text: "0.0000000004999999999", want: 0},
		{name: "just above half", text: "0.0000000005000000001", want: 1},
		{name: "just below odd tie", text: "0.0000000014999999999", want: 1},
		{name: "just above odd tie", text: "0.0000000015000000001", want: 2},
		{name: "large even tie", text: "1.2345678905", want: 1234567890},
		{name: "large odd tie", text: "1.2345678915", want: 1234567892},
		{name: "huge negative exponent", text: "1e-999999999999999999999999", want: 0},
		{name: "zero with huge positive exponent", text: "0e999999999999999999999999", want: 0},
		{name: "exact maximum", text: "9223372036.854775807", want: maxNanoUSD},
		{name: "below overflowing tie", text: "9223372036.8547758074", want: maxNanoUSD},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := accounting.ParseUSD(tc.text)
			if err != nil {
				t.Fatalf("ParseUSD(%q) = %v; want no error", tc.text, err)
			}

			if got != tc.want {
				t.Fatalf("ParseUSD(%q) = %d; want %d", tc.text, got, tc.want)
			}
		})
	}
}

func TestParseUSDRejectsInvalidProviderNumbers(t *testing.T) {
	t.Parallel()

	invalid := []string{
		"",
		" ",
		" 0",
		"0 ",
		"\t0",
		"+1",
		"-0",
		"-1",
		"00",
		"01",
		"00.1",
		".1",
		"1.",
		"1.e2",
		"1e",
		"1e+",
		"1e-",
		"e1",
		"NaN",
		"nan",
		"Inf",
		"Infinity",
		"0x1",
		"1_0",
		"1a",
		"1 0",
	}

	for _, text := range invalid {
		t.Run(text, func(t *testing.T) {
			t.Parallel()

			if got, err := accounting.ParseUSD(text); err == nil {
				t.Fatalf("ParseUSD(%q) = %d, nil; want an error", text, got)
			}
		})
	}
}

func TestParseUSDReportsOverflow(t *testing.T) {
	t.Parallel()

	for _, text := range []string{
		"9223372036.8547758075",
		"9223372036.8547758076",
		"9223372036.854775808",
		"9223372037",
		"1e999999999999999999999999",
	} {
		t.Run(text, func(t *testing.T) {
			t.Parallel()

			if got, err := accounting.ParseUSD(text); err == nil {
				t.Fatalf("ParseUSD(%q) = %d, nil; want overflow", text, got)
			}
		})
	}
}

func TestAddChecksSignedOverflow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		left    accounting.NanoUSD
		right   accounting.NanoUSD
		want    accounting.NanoUSD
		wantErr bool
	}{
		{name: "zero", left: 0, right: 0, want: 0},
		{name: "positive", left: 2, right: 3, want: 5},
		{name: "negative", left: -2, right: -3, want: -5},
		{name: "mixed", left: maxNanoUSD, right: minNanoUSD, want: -1},
		{name: "maximum plus zero", left: maxNanoUSD, right: 0, want: maxNanoUSD},
		{name: "minimum plus zero", left: minNanoUSD, right: 0, want: minNanoUSD},
		{name: "positive overflow", left: maxNanoUSD, right: 1, wantErr: true},
		{name: "negative overflow", left: minNanoUSD, right: -1, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := accounting.Add(tc.left, tc.right)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Add(%d, %d) = %d, nil; want overflow", tc.left, tc.right, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("Add(%d, %d) = %v; want no error", tc.left, tc.right, err)
			}

			if got != tc.want {
				t.Fatalf("Add(%d, %d) = %d; want %d", tc.left, tc.right, got, tc.want)
			}
		})
	}
}

func TestMultiplyPerMillionIsExactAndRoundsHalfEven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		rate   accounting.NanoUSD
		tokens int64
		want   accounting.NanoUSD
	}{
		{name: "zero rate", rate: 0, tokens: 9223372036854775807, want: 0},
		{name: "zero tokens", rate: maxNanoUSD, tokens: 0, want: 0},
		{name: "one dollar per million", rate: 1000000000, tokens: 1000000, want: 1000000000},
		{name: "one token", rate: 1000000000, tokens: 1, want: 1000},
		{name: "even quotient tie", rate: 1, tokens: 500000, want: 0},
		{name: "odd quotient tie", rate: 3, tokens: 500000, want: 2},
		{name: "second even quotient tie", rate: 5, tokens: 500000, want: 2},
		{name: "large product without intermediate overflow", rate: maxNanoUSD, tokens: 1000000, want: maxNanoUSD},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := accounting.MultiplyPerMillion(tc.rate, tc.tokens)
			if err != nil {
				t.Fatalf("MultiplyPerMillion(%d, %d) = %v; want no error", tc.rate, tc.tokens, err)
			}

			if got != tc.want {
				t.Fatalf("MultiplyPerMillion(%d, %d) = %d; want %d", tc.rate, tc.tokens, got, tc.want)
			}
		})
	}
}

func TestMultiplyPerMillionRejectsInvalidOrOverflowingProducts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		rate   accounting.NanoUSD
		tokens int64
	}{
		{name: "negative rate", rate: -1, tokens: 1},
		{name: "negative tokens", rate: 1, tokens: -1},
		{name: "overflow", rate: maxNanoUSD, tokens: 1000001},
		{name: "large overflow", rate: maxNanoUSD, tokens: 9223372036854775807},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got, err := accounting.MultiplyPerMillion(tc.rate, tc.tokens); err == nil {
				t.Fatalf("MultiplyPerMillion(%d, %d) = %d, nil; want an error", tc.rate, tc.tokens, got)
			}
		})
	}
}

func TestRoundCentsRoundsSignedHalfEven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		amount accounting.NanoUSD
		want   int64
	}{
		{name: "zero", amount: 0, want: 0},
		{name: "positive below half", amount: 4999999, want: 0},
		{name: "positive even tie", amount: 5000000, want: 0},
		{name: "positive above half", amount: 5000001, want: 1},
		{name: "positive odd tie", amount: 15000000, want: 2},
		{name: "positive second even tie", amount: 25000000, want: 2},
		{name: "negative below half", amount: -4999999, want: 0},
		{name: "negative even tie", amount: -5000000, want: 0},
		{name: "negative above half", amount: -5000001, want: -1},
		{name: "negative odd tie", amount: -15000000, want: -2},
		{name: "negative second even tie", amount: -25000000, want: -2},
		{name: "maximum int64", amount: maxNanoUSD, want: 922337203685},
		{name: "minimum int64", amount: minNanoUSD, want: -922337203685},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := accounting.RoundCents(tc.amount); got != tc.want {
				t.Fatalf("RoundCents(%d) = %d; want %d", tc.amount, got, tc.want)
			}
		})
	}
}

func TestTruncateCentsTruncatesTowardZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		amount accounting.NanoUSD
		want   int64
	}{
		{name: "zero", amount: 0, want: 0},
		{name: "positive partial cent", amount: 19999999, want: 1},
		{name: "negative partial cent", amount: -19999999, want: -1},
		{name: "positive exact cent", amount: 20000000, want: 2},
		{name: "negative exact cent", amount: -20000000, want: -2},
		{name: "maximum int64", amount: maxNanoUSD, want: 922337203685},
		{name: "minimum int64", amount: minNanoUSD, want: -922337203685},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := accounting.TruncateCents(tc.amount); got != tc.want {
				t.Fatalf("TruncateCents(%d) = %d; want %d", tc.amount, got, tc.want)
			}
		})
	}
}

func TestExportedSurfaceIsExact(t *testing.T) {
	t.Parallel()

	got := exportedDeclarations(t)
	want := []string{"Add", "MultiplyPerMillion", "NanoUSD", "ParseUSD", "RoundCents", "TruncateCents"}

	if !slices.Equal(got, want) {
		t.Fatalf("exported declarations = %q; want %q", got, want)
	}
}

func exportedDeclarations(t *testing.T) []string {
	t.Helper()

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	directory := filepath.Dir(testFile)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("ReadDir(%q) = %v", directory, err)
	}

	var names []string

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		filePath := filepath.Join(directory, entry.Name())
		file, parseErr := parser.ParseFile(token.NewFileSet(), filePath, nil, 0)
		if parseErr != nil {
			t.Fatalf("ParseFile(%q) = %v", filePath, parseErr)
		}

		for _, declaration := range file.Decls {
			names = append(names, exportedNames(declaration)...)
		}
	}

	sort.Strings(names)

	return names
}

func exportedNames(declaration ast.Decl) []string {
	switch declaration := declaration.(type) {
	case *ast.FuncDecl:
		if ast.IsExported(declaration.Name.Name) {
			return []string{declaration.Name.Name}
		}
	case *ast.GenDecl:
		var names []string

		for _, specification := range declaration.Specs {
			switch specification := specification.(type) {
			case *ast.TypeSpec:
				if ast.IsExported(specification.Name.Name) {
					names = append(names, specification.Name.Name)
				}
			case *ast.ValueSpec:
				for _, name := range specification.Names {
					if ast.IsExported(name.Name) {
						names = append(names, name.Name)
					}
				}
			}
		}

		return names
	}

	return nil
}
