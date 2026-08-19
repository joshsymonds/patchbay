package awssig_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/awssig"
)

// The credentials, instant and request of AWS's own published SigV4 example.
// Reproducing its signature is what proves the canonicalization is AWS's and
// not merely self-consistent.
const (
	exampleKeyID  = "AKIDEXAMPLE"
	exampleSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	exampleStamp  = "2015-08-30T12:36:00Z"
)

// signingTime parses a fixed instant, so every signature in this file is
// reproducible.
func signingTime(t *testing.T) time.Time {
	t.Helper()

	when, err := time.Parse(time.RFC3339, exampleStamp)
	if err != nil {
		t.Fatalf("parse %q: %v", exampleStamp, err)
	}

	return when
}

// signatureOf pulls the Signature= value out of an Authorization header.
func signatureOf(t *testing.T, authorization string) string {
	t.Helper()

	_, sig, found := strings.Cut(authorization, "Signature=")
	if !found {
		t.Fatalf("Authorization %q carries no Signature=", authorization)
	}

	return sig
}

// TestSignReproducesTheAWSExampleVector pins the canonicalization against the
// get-header-value-trim/iam example AWS publishes: same credentials, same
// instant, same request, same signature.
func TestSignReproducesTheAWSExampleVector(t *testing.T) {
	t.Parallel()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	creds := awssig.Credentials{AccessKeyID: exampleKeyID, SecretAccessKey: exampleSecret}

	if signErr := awssig.Sign(req, creds, "iam", "us-east-1", nil, signingTime(t)); signErr != nil {
		t.Fatalf("Sign() = %v; want no error", signErr)
	}

	authorization := req.Header.Get("Authorization")

	const wantCredential = "Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request"

	const wantSigned = "SignedHeaders=content-type;host;x-amz-date"

	const wantSignature = "5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"

	if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization = %q; want the AWS4-HMAC-SHA256 scheme", authorization)
	}

	if !strings.Contains(authorization, wantCredential) {
		t.Errorf("Authorization = %q; want it to carry %q", authorization, wantCredential)
	}

	if !strings.Contains(authorization, wantSigned) {
		t.Errorf("Authorization = %q; want it to carry %q", authorization, wantSigned)
	}

	if got := signatureOf(t, authorization); got != wantSignature {
		t.Errorf("signature = %q; want %q", got, wantSignature)
	}

	if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Errorf("X-Amz-Date = %q; want %q", got, "20150830T123600Z")
	}
}

// TestSignBedrockARNPathIsStable is the regression pin for the path patchbay
// actually signs: an application-inference-profile ARN, percent-escaped in the
// request line and escaped a second time in the canonical request. The expected
// signature was computed once from this implementation and is asserted stable;
// what makes it meaningful is that the same implementation reproduces AWS's own
// published vector above.
func TestSignBedrockARNPathIsStable(t *testing.T) {
	t.Parallel()

	const arn = "arn:aws:bedrock:us-east-2:123456789012:application-inference-profile/abcdef123456"

	escaped := awssig.EscapePathSegment(arn)
	if strings.ContainsAny(escaped, ":/") {
		t.Fatalf("EscapePathSegment(%q) = %q; want every %q and %q percent-escaped", arn, escaped, ":", "/")
	}

	target := "https://bedrock-runtime.us-east-2.amazonaws.com/model/" + escaped + "/invoke"

	body := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":16}`)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	creds := awssig.Credentials{
		AccessKeyID:     exampleKeyID,
		SecretAccessKey: exampleSecret,
		SessionToken:    "session-token-value",
	}

	if signErr := awssig.Sign(req, creds, "bedrock", "us-east-2", body,
		signingTime(t)); signErr != nil {
		t.Fatalf("Sign() = %v; want no error", signErr)
	}

	authorization := req.Header.Get("Authorization")

	const wantSigned = "SignedHeaders=accept;content-type;host;x-amz-date;x-amz-security-token"

	const wantSignature = "b3f0e2944892fd2fae3ad5a8fddd42e0bb7e64b55ed6163674551e3b69903ef9"

	if !strings.Contains(authorization, wantSigned) {
		t.Errorf("Authorization = %q; want it to carry %q", authorization, wantSigned)
	}

	if got := req.Header.Get("X-Amz-Security-Token"); got != "session-token-value" {
		t.Errorf("X-Amz-Security-Token = %q; want the session token", got)
	}

	if got := req.URL.EscapedPath(); !strings.Contains(got, "%3A") {
		t.Errorf("request path = %q; want the ARN colons escaped", got)
	}

	if got := signatureOf(t, authorization); got != wantSignature {
		t.Errorf("signature = %q; want the pinned %q", got, wantSignature)
	}
}

// A signature over a different body must differ: the payload hash is part of
// the canonical request, so a tampered body cannot ride a stale signature.
func TestSignCoversThePayload(t *testing.T) {
	t.Parallel()

	sign := func(body string) string {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			"https://bedrock-runtime.us-east-2.amazonaws.com/model/m/invoke", strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}

		req.Header.Set("Content-Type", "application/json")

		creds := awssig.Credentials{AccessKeyID: exampleKeyID, SecretAccessKey: exampleSecret}
		if signErr := awssig.Sign(req, creds, "bedrock", "us-east-2", []byte(body),
			signingTime(t)); signErr != nil {
			t.Fatalf("Sign() = %v; want no error", signErr)
		}

		return signatureOf(t, req.Header.Get("Authorization"))
	}

	if first, second := sign(`{"a":1}`), sign(`{"a":2}`); first == second {
		t.Errorf("signatures over different bodies are both %q; want them to differ", first)
	}
}

// Sign refuses credentials it cannot sign with rather than sending an
// unsigned — or half-signed — request upstream.
func TestSignRejectsIncompleteCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		creds awssig.Credentials
	}{
		{name: "no access key id", creds: awssig.Credentials{SecretAccessKey: exampleSecret}},
		{name: "no secret", creds: awssig.Credentials{AccessKeyID: exampleKeyID}},
		{name: "nothing at all", creds: awssig.Credentials{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				"https://bedrock-runtime.us-east-2.amazonaws.com/model/m/invoke", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}

			if signErr := awssig.Sign(req, tc.creds, "bedrock", "us-east-2", nil,
				signingTime(t)); signErr == nil {
				t.Fatal("Sign() = nil; want an error for incomplete credentials")
			}

			if got := req.Header.Get("Authorization"); got != "" {
				t.Errorf("Authorization = %q; want no header on a refused signature", got)
			}
		})
	}
}

// EscapePathSegment implements RFC 3986 segment escaping: unreserved bytes
// survive, everything else is percent-escaped with uppercase hex.
func TestEscapePathSegment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "unreserved survives", in: "aZ09-._~", want: "aZ09-._~"},
		{name: "colon escaped", in: "a:b", want: "a%3Ab"},
		{name: "slash escaped", in: "a/b", want: "a%2Fb"},
		{name: "space escaped", in: "a b", want: "a%20b"},
		{name: "percent escaped", in: "a%b", want: "a%25b"},
		{name: "plus escaped", in: "a+b", want: "a%2Bb"},
		{name: "utf8 escaped per byte", in: "é", want: "%C3%A9"},
		{name: "empty", in: "", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := awssig.EscapePathSegment(tc.in); got != tc.want {
				t.Errorf("EscapePathSegment(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
