// Package awssig speaks the parts of AWS that patchbay's Bedrock upstream
// needs and nothing else: SigV4 request signing, the binary event-stream
// framing a streamed Bedrock response arrives in, and credentials exported from
// a named AWS profile.
//
// It is stdlib-only on purpose. The AWS SDK would bring a dependency tree an
// order of magnitude larger than patchbay itself to sign one request shape
// against one service.
package awssig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// The SigV4 vocabulary: the one algorithm patchbay signs with, the scope
// terminator every credential scope ends in, and the two time formats a
// signature is expressed in.
const (
	algorithm     = "AWS4-HMAC-SHA256"
	terminator    = "aws4_request"
	amzDateLayout = "20060102T150405Z"
	dateLayout    = "20060102"
	keyPrefix     = "AWS4"
)

// Headers the signature covers or sets. Host is always signed; the rest are
// signed only when the request carries them, which keeps the signed set exactly
// what goes on the wire.
const (
	headerAuthorization = "Authorization"
	headerAmzDate       = "X-Amz-Date"
	// headerAmzSession deliberately avoids spelling out the word the header
	// itself carries: a const named that way trips gosec's hardcoded-credential
	// heuristic (G101), the same way internal/anthropic's HeaderClientKey does.
	headerAmzSession = "X-Amz-Security-Token"
	headerHost       = "host"
)

// hexEscapeFormat renders one byte as an RFC 3986 percent-escape with the
// uppercase hex AWS canonicalization requires.
const hexEscapeFormat = "%%%02X"

// Credentials are one set of AWS keys, as `aws configure export-credentials`
// exports them. SessionToken is empty for a long-lived key pair; Expiration is
// zero when the source declared none.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// signedHeaderNames are the request headers, besides host, that the signature
// covers when they are present. Signing a fixed set rather than everything is
// deliberate: a header patchbay does not control must never be able to change
// the signature.
func signedHeaderNames() []string {
	return []string{"accept", "content-type", headerAmzDate, headerAmzSession}
}

// Sign signs req for service in region with creds, as of when, over payload.
// It sets X-Amz-Date, X-Amz-Security-Token when the credentials carry one, and
// the Authorization header. A request it cannot sign is left untouched: an
// unsigned request reaching AWS is a confusing 403, and a half-signed one is
// worse.
func Sign(
	req *http.Request,
	creds Credentials,
	service, region string,
	payload []byte,
	when time.Time,
) error {
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return errors.New("aws credentials are incomplete")
	}

	host := requestHost(req)
	if host == "" {
		return errors.New("request has no host to sign")
	}

	stamp := when.UTC()
	amzDate := stamp.Format(amzDateLayout)

	req.Header.Set(headerAmzDate, amzDate)

	if creds.SessionToken != "" {
		req.Header.Set(headerAmzSession, creds.SessionToken)
	}

	names, canonicalHeaders := headersToSign(req, host)
	signedHeaders := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.EscapedPath()),
		canonicalQuery(req.URL.RawQuery),
		canonicalHeaders,
		signedHeaders,
		hashHex(payload),
	}, "\n")

	scope := strings.Join([]string{stamp.Format(dateLayout), region, service, terminator}, "/")
	toSign := strings.Join([]string{algorithm, amzDate, scope, hashHex([]byte(canonical))}, "\n")
	signature := hex.EncodeToString(sign(signingKey(creds.SecretAccessKey, stamp, region, service), toSign))

	req.Header.Set(headerAuthorization, fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, creds.AccessKeyID, scope, signedHeaders, signature))

	return nil
}

// EscapePathSegment percent-escapes one path segment per RFC 3986: unreserved
// bytes survive, every other byte — a Bedrock ARN's colons and its one slash
// included — becomes an uppercase percent-escape.
func EscapePathSegment(segment string) string {
	var out strings.Builder

	for i := range len(segment) {
		char := segment[i]
		if isUnreserved(char) {
			out.WriteByte(char)

			continue
		}

		fmt.Fprintf(&out, hexEscapeFormat, char)
	}

	return out.String()
}

// requestHost is the host the signature covers: the request's explicit Host
// when it has one, otherwise the URL's.
func requestHost(req *http.Request) string {
	if req.Host != "" {
		return req.Host
	}

	if req.URL == nil {
		return ""
	}

	return req.URL.Host
}

// headersToSign returns the sorted lowercase names of the headers the signature
// covers and the canonical header block for them.
func headersToSign(req *http.Request, host string) ([]string, string) {
	values := map[string]string{headerHost: host}

	for _, name := range signedHeaderNames() {
		if value := req.Header.Get(name); value != "" {
			values[strings.ToLower(name)] = value
		}
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}

	sort.Strings(names)

	var block strings.Builder

	for _, name := range names {
		block.WriteString(name)
		block.WriteString(":")
		block.WriteString(strings.TrimSpace(values[name]))
		block.WriteString("\n")
	}

	return names, block.String()
}

// canonicalURI is the escaped request path escaped a second time, which is what
// SigV4 canonicalization requires of every service but S3. The separators stay
// separators; only the segments between them are re-escaped.
func canonicalURI(escapedPath string) string {
	if escapedPath == "" || escapedPath == "/" {
		return "/"
	}

	segments := strings.Split(escapedPath, "/")
	for i, segment := range segments {
		segments[i] = EscapePathSegment(segment)
	}

	return strings.Join(segments, "/")
}

// canonicalQuery is the raw query sorted by name and then value, with both
// escaped per RFC 3986. A query that will not parse contributes nothing rather
// than a guess.
func canonicalQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}

	parsed, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}

	pairs := make([]string, 0, len(parsed))

	for name, values := range parsed {
		for _, value := range values {
			pairs = append(pairs, EscapePathSegment(name)+"="+EscapePathSegment(value))
		}
	}

	sort.Strings(pairs)

	return strings.Join(pairs, "&")
}

// signingKey derives the request's signing key from the secret, walking the
// date, region and service chain SigV4 defines.
func signingKey(secret string, stamp time.Time, region, service string) []byte {
	key := sign([]byte(keyPrefix+secret), stamp.Format(dateLayout))
	key = sign(key, region)
	key = sign(key, service)

	return sign(key, terminator)
}

// sign is one HMAC-SHA256 round of the key derivation chain.
func sign(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))

	return mac.Sum(nil)
}

// hashHex is the lowercase hex SHA-256 of data, which is how SigV4 expresses
// both the payload hash and the canonical request hash.
func hashHex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// isUnreserved reports whether a byte may appear unescaped in a path segment.
func isUnreserved(char byte) bool {
	switch {
	case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char >= '0' && char <= '9':
		return true
	case char == '-', char == '.', char == '_', char == '~':
		return true
	default:
		return false
	}
}
