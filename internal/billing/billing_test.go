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

func TestParseCredentialClassAcceptsTheClosedVocabulary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want billing.CredentialClass
	}{
		{name: "oauth", raw: "oauth", want: billing.CredentialOAuth},
		{name: "api key", raw: "api_key", want: billing.CredentialAPIKey},
		{name: "none", raw: "none", want: billing.CredentialNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := billing.ParseCredentialClass(tc.raw)
			if err != nil {
				t.Fatalf("ParseCredentialClass(%q) = %v; want no error", tc.raw, err)
			}

			if got != tc.want {
				t.Fatalf("ParseCredentialClass(%q) = %q; want %q", tc.raw, got, tc.want)
			}

			if !got.Valid() {
				t.Fatalf("CredentialClass(%q).Valid() = false; want true", got)
			}

			if got.String() != tc.raw {
				t.Fatalf("CredentialClass(%q).String() = %q; want %q", got, got.String(), tc.raw)
			}
		})
	}
}

func TestParseCredentialClassRejectsValuesOutsideTheVocabulary(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"",
		" ",
		"OAuth",
		"API_KEY",
		"unknown",
		"oauth ",
		"Bearer sk-ant-oat01-secret",
		"Authorization: Bearer secret",
		"X-Api-Key: sk-ant-api03-secret",
		"sk-ant-api03-secret",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			got, err := billing.ParseCredentialClass(raw)
			if err == nil {
				t.Fatalf("ParseCredentialClass(%q) = %q, nil; want an error", raw, got)
			}

			if got.Valid() {
				t.Fatalf("ParseCredentialClass(%q) returned valid class %q on error", raw, got)
			}
		})
	}
}

func TestRealizeClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		policy     billing.Policy
		explicit   billing.Class
		credential billing.CredentialClass
		want       billing.Class
		wantErr    bool
	}{
		{
			name:   "explicit subscription with oauth",
			policy: billing.PolicyExplicit, explicit: billing.ClassSubscription,
			credential: billing.CredentialOAuth, want: billing.ClassSubscription,
		},
		{
			name:   "explicit subscription with api key",
			policy: billing.PolicyExplicit, explicit: billing.ClassSubscription,
			credential: billing.CredentialAPIKey, want: billing.ClassSubscription,
		},
		{
			name:   "explicit subscription with no credential",
			policy: billing.PolicyExplicit, explicit: billing.ClassSubscription,
			credential: billing.CredentialNone, want: billing.ClassSubscription,
		},
		{
			name:   "explicit metered with oauth",
			policy: billing.PolicyExplicit, explicit: billing.ClassMetered,
			credential: billing.CredentialOAuth, want: billing.ClassMetered,
		},
		{
			name:   "explicit metered with api key",
			policy: billing.PolicyExplicit, explicit: billing.ClassMetered,
			credential: billing.CredentialAPIKey, want: billing.ClassMetered,
		},
		{
			name:   "explicit metered with no credential",
			policy: billing.PolicyExplicit, explicit: billing.ClassMetered,
			credential: billing.CredentialNone, want: billing.ClassMetered,
		},
		{
			name:   "explicit self hosted with oauth",
			policy: billing.PolicyExplicit, explicit: billing.ClassSelfHosted,
			credential: billing.CredentialOAuth, want: billing.ClassSelfHosted,
		},
		{
			name:   "explicit self hosted with api key",
			policy: billing.PolicyExplicit, explicit: billing.ClassSelfHosted,
			credential: billing.CredentialAPIKey, want: billing.ClassSelfHosted,
		},
		{
			name:   "explicit self hosted with no credential",
			policy: billing.PolicyExplicit, explicit: billing.ClassSelfHosted,
			credential: billing.CredentialNone, want: billing.ClassSelfHosted,
		},
		{
			name:   "caller-derived oauth",
			policy: billing.PolicyCallerDerived, credential: billing.CredentialOAuth,
			want: billing.ClassSubscription,
		},
		{
			name:   "caller-derived api key",
			policy: billing.PolicyCallerDerived, credential: billing.CredentialAPIKey,
			want: billing.ClassMetered,
		},
		{
			name:   "caller-derived no credential",
			policy: billing.PolicyCallerDerived, credential: billing.CredentialNone,
			wantErr: true,
		},
		{
			name:   "sigv4 oauth",
			policy: billing.PolicySigV4Metered, credential: billing.CredentialOAuth,
			want: billing.ClassMetered,
		},
		{
			name:   "sigv4 api key",
			policy: billing.PolicySigV4Metered, credential: billing.CredentialAPIKey,
			want: billing.ClassMetered,
		},
		{
			name:   "sigv4 no credential",
			policy: billing.PolicySigV4Metered, credential: billing.CredentialNone,
			want: billing.ClassMetered,
		},
		{
			name:       "empty policy",
			credential: billing.CredentialOAuth,
			wantErr:    true,
		},
		{
			name:   "unknown policy",
			policy: billing.Policy("future"), credential: billing.CredentialOAuth,
			wantErr: true,
		},
		{
			name:    "empty credential",
			policy:  billing.PolicyCallerDerived,
			wantErr: true,
		},
		{
			name:   "unknown credential",
			policy: billing.PolicySigV4Metered, credential: billing.CredentialClass("future"),
			wantErr: true,
		},
		{
			name:   "explicit policy with empty class",
			policy: billing.PolicyExplicit, credential: billing.CredentialOAuth,
			wantErr: true,
		},
		{
			name:   "explicit policy with unknown class",
			policy: billing.PolicyExplicit, explicit: billing.Class("future"),
			credential: billing.CredentialOAuth, wantErr: true,
		},
		{
			name:   "caller-derived with explicit subscription",
			policy: billing.PolicyCallerDerived, explicit: billing.ClassSubscription,
			credential: billing.CredentialOAuth, wantErr: true,
		},
		{
			name:   "caller-derived with explicit metered",
			policy: billing.PolicyCallerDerived, explicit: billing.ClassMetered,
			credential: billing.CredentialOAuth, wantErr: true,
		},
		{
			name:   "caller-derived with explicit self hosted",
			policy: billing.PolicyCallerDerived, explicit: billing.ClassSelfHosted,
			credential: billing.CredentialOAuth, wantErr: true,
		},
		{
			name:   "sigv4 with explicit subscription",
			policy: billing.PolicySigV4Metered, explicit: billing.ClassSubscription,
			credential: billing.CredentialNone, wantErr: true,
		},
		{
			name:   "sigv4 with explicit metered",
			policy: billing.PolicySigV4Metered, explicit: billing.ClassMetered,
			credential: billing.CredentialNone, wantErr: true,
		},
		{
			name:   "sigv4 with explicit self hosted",
			policy: billing.PolicySigV4Metered, explicit: billing.ClassSelfHosted,
			credential: billing.CredentialNone, wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := billing.RealizeClass(tc.policy, tc.explicit, tc.credential)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("RealizeClass(%q, %q, %q) = %q, nil; want an error",
						tc.policy, tc.explicit, tc.credential, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("RealizeClass(%q, %q, %q) = %v; want no error",
					tc.policy, tc.explicit, tc.credential, err)
			}

			if got != tc.want {
				t.Fatalf("RealizeClass(%q, %q, %q) = %q; want %q",
					tc.policy, tc.explicit, tc.credential, got, tc.want)
			}

			if !got.Valid() {
				t.Fatalf("RealizeClass(%q, %q, %q) returned invalid class %q on success",
					tc.policy, tc.explicit, tc.credential, got)
			}
		})
	}
}
