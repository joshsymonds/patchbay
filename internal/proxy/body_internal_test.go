package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/registry"
)

// TestForwardBodyRefusesJSONNull covers forwardBody's nil-map guard directly. A
// null body is production-reachable through ServeHTTP whenever the Seat it lands
// on rewrites — the registry admits a Context whose default_seat injects a model
// of its own — and the whole point of the guard is that such a request is
// refused rather than written into a nil map. The wire half of that claim is
// TestNullBodyOnARewritingDefaultSeatIsRefused; this half holds the function to
// the exact status and message it answers with.
func TestForwardBodyRefusesJSONNull(t *testing.T) {
	t.Parallel()

	body, gwErr := forwardBody([]byte("null"), registry.Seat{Model: "y"})
	if gwErr == nil {
		t.Fatal("forwardBody(null) error = nil; want the body refused rather than written into")
	}

	if body != nil {
		t.Fatalf("forwardBody(null) body = %q; want nothing to forward", body)
	}

	if gwErr.status != http.StatusBadRequest {
		t.Fatalf(
			"status = %d; want %d — a null body is the caller's mistake",
			gwErr.status,
			http.StatusBadRequest,
		)
	}

	if !strings.Contains(gwErr.Error(), "JSON null rather than an object") {
		t.Fatalf(
			"message = %q; want it to say the body is JSON null rather than an object",
			gwErr.Error(),
		)
	}
}

// TestCallerFacingSeatErrorsNameNoSeatID pins the wording of every refusal that
// blames the Seat a request landed on.
//
// A Seat ID is private: it is the operator's own name for a row in a registry no
// caller can read, and the pipeline deliberately keeps it out of everything it
// answers. These messages used to satisfy that by formatting the literal string
// "selected" through %q — which reads to a caller as a Seat genuinely named
// selected, and reads to an operator grepping their registry for it as a Seat
// that does not exist. Saying "the selected Seat" says the same true thing and
// invents no identifier to say it with.
func TestCallerFacingSeatErrorsNameNoSeatID(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		want string
		fail func() *gatewayError
	}{
		{
			name: "a body that is not a JSON object",
			want: "the selected Seat rewrites the request, which is not a JSON object",
			fail: func() *gatewayError {
				_, gwErr := forwardBody([]byte("not json at all"), registry.Seat{Model: "y"})

				return gwErr
			},
		},
		{
			name: "a body that is JSON null",
			want: "the selected Seat rewrites the request, which is JSON null rather than an object",
			fail: func() *gatewayError {
				_, gwErr := forwardBody([]byte("null"), registry.Seat{Model: "y"})

				return gwErr
			},
		},
		{
			name: "an extra_body that is not an object",
			want: "the selected Seat has an unusable extra_body",
			fail: func() *gatewayError {
				_, gwErr := forwardBody(
					[]byte(`{}`),
					registry.Seat{ExtraBody: json.RawMessage(`[1,2]`)},
				)

				return gwErr
			},
		},
		{
			name: "a model map that cannot be read",
			want: "the selected Seat has an unusable model map",
			fail: func() *gatewayError {
				_, gwErr := bedrockTarget(
					registry.Seat{ModelMapEnvFile: "PATCHBAY_TEST_ABSENT_MODEL_MAP"},
					"claude-opus-5",
				)

				return gwErr
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gwErr := tc.fail()
			if gwErr == nil {
				t.Fatal("error = nil; want the request refused")
			}

			message := gwErr.Error()
			if !strings.Contains(message, tc.want) {
				t.Errorf("message = %q; want it to contain %q", message, tc.want)
			}

			if strings.Contains(message, `"selected"`) {
				t.Errorf("message = %q; want no fabricated Seat ID quoted into it", message)
			}
		})
	}
}
