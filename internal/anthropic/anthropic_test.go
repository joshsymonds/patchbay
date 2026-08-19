package anthropic_test

import (
	"errors"
	"testing"

	"github.com/joshsymonds/patchbay/internal/anthropic"
)

func TestErrorBodyIsAnthropicShapedAndMarkedAsPatchbaysOwn(t *testing.T) {
	t.Parallel()

	got := string(anthropic.ErrorBody(errors.New("upstream request failed")))
	want := `{"type":"error","error":{"type":"api_error","message":"patchbay: upstream request failed"}}`

	if got != want {
		t.Fatalf("ErrorBody() = %s; want %s", got, want)
	}
}

func TestTypedErrorBodyCarriesTheNamedErrorType(t *testing.T) {
	t.Parallel()

	got := string(anthropic.TypedErrorBody(anthropic.ErrorTypeNotFound, errors.New(`unknown context "work"`)))
	want := `{"type":"error","error":{"type":"not_found_error","message":"patchbay: unknown context \"work\""}}`

	if got != want {
		t.Fatalf("TypedErrorBody() = %s; want %s", got, want)
	}
}

func TestErrorMessagePrefixMarksTheSender(t *testing.T) {
	t.Parallel()

	// Claude Code matches upstream error text to decide whether to retry, so the
	// prefix is a wire fact, not a cosmetic one.
	if anthropic.ErrorMessagePrefix != "patchbay: " {
		t.Fatalf("ErrorMessagePrefix = %q; want %q", anthropic.ErrorMessagePrefix, "patchbay: ")
	}
}

func TestHeaderNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "authorization", got: anthropic.HeaderAuthorization, want: "Authorization"},
		{name: "client key", got: anthropic.HeaderClientKey, want: "X-Api-Key"},
		{name: "version", got: anthropic.HeaderVersion, want: "Anthropic-Version"},
		{name: "beta", got: anthropic.HeaderBeta, want: "Anthropic-Beta"},
		{name: "content type", got: anthropic.HeaderContentType, want: "Content-Type"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if tc.got != tc.want {
				t.Fatalf("header name = %q; want %q", tc.got, tc.want)
			}
		})
	}
}
