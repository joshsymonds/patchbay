package awssig

import "testing"

// The canonical URI is the request's already-escaped path escaped a second
// time: SigV4 for every service but S3 double-encodes, and a Bedrock ARN — full
// of colons and a slash — is where that stops being academic.
func TestCanonicalURIDoubleEscapes(t *testing.T) {
	t.Parallel()

	const arn = "arn:aws:bedrock:us-east-2:123456789012:application-inference-profile/abcdef123456"

	escapedPath := "/model/" + EscapePathSegment(arn) + "/invoke"

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty path is root", in: "", want: "/"},
		{name: "root", in: "/", want: "/"},
		{name: "plain segments survive", in: "/model/foo/invoke", want: "/model/foo/invoke"},
		{
			name: "already-escaped bytes escape again",
			in:   escapedPath,
			want: "/model/arn%253Aaws%253Abedrock%253Aus-east-2%253A123456789012%253A" +
				"application-inference-profile%252Fabcdef123456/invoke",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := canonicalURI(tc.in); got != tc.want {
				t.Errorf("canonicalURI(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The canonical query string is sorted by key and RFC 3986 escaped, so two
// requests that differ only in parameter order sign identically.
func TestCanonicalQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "sorted by key", in: "b=2&a=1", want: "a=1&b=2"},
		{name: "value escaped", in: "a=x y", want: "a=x%20y"},
		{name: "repeated key sorted by value", in: "a=2&a=1", want: "a=1&a=2"},
		{name: "empty value keeps its equals", in: "a=", want: "a="},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := canonicalQuery(tc.in); got != tc.want {
				t.Errorf("canonicalQuery(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
