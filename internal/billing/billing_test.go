package billing_test

import (
	"testing"

	"github.com/joshsymonds/patchbay/internal/billing"
)

func TestParseClassAcceptsTheClosedVocabulary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want billing.Class
	}{
		{name: "subscription", raw: "subscription", want: billing.ClassSubscription},
		{name: "metered", raw: "metered", want: billing.ClassMetered},
		{name: "self hosted", raw: "self_hosted", want: billing.ClassSelfHosted},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := billing.ParseClass(tc.raw)
			if err != nil {
				t.Fatalf("ParseClass(%q) = %v; want no error", tc.raw, err)
			}

			if got != tc.want {
				t.Fatalf("ParseClass(%q) = %q; want %q", tc.raw, got, tc.want)
			}

			if !got.Valid() {
				t.Fatalf("Class(%q).Valid() = false; want true", got)
			}

			if got.String() != tc.raw {
				t.Fatalf("Class(%q).String() = %q; want %q", got, got.String(), tc.raw)
			}
		})
	}
}

func TestParseClassRejectsValuesOutsideTheVocabulary(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "unknown", "Subscription", "self-hosted"} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			got, err := billing.ParseClass(raw)
			if err == nil {
				t.Fatalf("ParseClass(%q) = %q, nil; want an error", raw, got)
			}

			if got.Valid() {
				t.Fatalf("ParseClass(%q) returned valid class %q on error", raw, got)
			}
		})
	}
}

func TestParsePolicyAcceptsTheClosedVocabulary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want billing.Policy
	}{
		{name: "explicit", raw: "explicit", want: billing.PolicyExplicit},
		{name: "credential derived", raw: "credential_derived", want: billing.PolicyCallerDerived},
		{name: "sigv4 metered", raw: "sigv4_metered", want: billing.PolicySigV4Metered},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := billing.ParsePolicy(tc.raw)
			if err != nil {
				t.Fatalf("ParsePolicy(%q) = %v; want no error", tc.raw, err)
			}

			if got != tc.want {
				t.Fatalf("ParsePolicy(%q) = %q; want %q", tc.raw, got, tc.want)
			}

			if !got.Valid() {
				t.Fatalf("Policy(%q).Valid() = false; want true", got)
			}

			if got.String() != tc.raw {
				t.Fatalf("Policy(%q).String() = %q; want %q", got, got.String(), tc.raw)
			}
		})
	}
}

func TestParsePolicyRejectsValuesOutsideTheVocabulary(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "unknown", "metered", "sigv4-metered"} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			got, err := billing.ParsePolicy(raw)
			if err == nil {
				t.Fatalf("ParsePolicy(%q) = %q, nil; want an error", raw, got)
			}

			if got.Valid() {
				t.Fatalf("ParsePolicy(%q) returned valid policy %q on error", raw, got)
			}
		})
	}
}
