package awssig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// The aws CLI invocation that exports a profile's credentials. Patchbay never
// reads ~/.aws itself: the CLI already knows how to refresh an SSO session, and
// duplicating that is how a gateway ends up with its own subtly wrong idea of
// what a profile means.
const (
	awsBinary     = "aws"
	configureVerb = "configure"
	exportVerb    = "export-credentials"
	profileEnv    = "AWS_PROFILE"
)

// exportTimeout bounds one aws invocation. Exporting credentials is a local
// operation on a warm SSO cache and a network one on a cold one, so the bound
// is generous enough for the latter and short enough that a hung CLI cannot
// hold a request open.
const exportTimeout = 10 * time.Second

// refreshSkew is how long before a credential's stated expiry it stops being
// used: a signature made in the last two minutes of a session could easily
// arrive after it has died.
const refreshSkew = 2 * time.Minute

// defaultLifetime is how long a credential with no stated expiry is cached. A
// long-lived key pair never expires at all, but re-reading it periodically is
// what makes a rotated one take effect without a restart.
const defaultLifetime = 15 * time.Minute

// Wire field names of the aws CLI's credential document. Only these are read;
// anything else it prints is ignored rather than decoded.
const (
	fieldAccessKeyID = "AccessKeyId"
	fieldSecretKey   = "SecretAccessKey"
	fieldSessionKey  = "SessionToken"
	fieldExpiration  = "Expiration"
)

// CredentialSource exports and caches AWS credentials per profile. It is safe
// for concurrent use, and a refresh several requests need at once costs exactly
// one aws process, not one per request.
type CredentialSource struct {
	mu       sync.Mutex
	profiles map[string]*profileCache
}

// profileCache is one profile's cached credential and the lock that serializes
// its refreshes. Holding this lock across the exec is what makes a refresh
// single-flight: everyone else waits and then finds the fresh value.
type profileCache struct {
	mu      sync.Mutex
	creds   Credentials
	expires time.Time
	valid   bool
}

// NewCredentialSource returns an empty source. Nothing is exported until a
// route actually needs credentials.
func NewCredentialSource() *CredentialSource {
	return &CredentialSource{profiles: map[string]*profileCache{}}
}

// Credentials returns the profile's credentials, exporting them if the cache
// has none or the cached ones are within the refresh skew of expiring.
func (s *CredentialSource) Credentials(ctx context.Context, profile string) (Credentials, error) {
	entry := s.entry(profile)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.valid && time.Now().Before(entry.expires) {
		return entry.creds, nil
	}

	creds, err := export(ctx, profile)
	if err != nil {
		return Credentials{}, err
	}

	entry.creds = creds
	entry.expires = cacheUntil(creds.Expiration)
	entry.valid = true

	return creds, nil
}

// Invalidate drops the profile's cached credential, so the next request exports
// a fresh one. It is what a rejected signature costs: the credential patchbay
// held is evidently not one AWS accepts any more.
func (s *CredentialSource) Invalidate(profile string) {
	entry := s.entry(profile)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	entry.valid = false
}

// entry returns the profile's cache, creating it on first use. The source lock
// is held only for the map lookup, never across an exec, so one profile's
// refresh never blocks another's.
func (s *CredentialSource) entry(profile string) *profileCache {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, ok := s.profiles[profile]
	if !ok {
		found = &profileCache{creds: Credentials{}, expires: time.Time{}, valid: false}
		s.profiles[profile] = found
	}

	return found
}

// cacheUntil is how long an exported credential may be reused: until the skew
// before its stated expiry, or the default lifetime when it stated none.
func cacheUntil(expiration time.Time) time.Time {
	if expiration.IsZero() {
		return time.Now().Add(defaultLifetime)
	}

	return expiration.Add(-refreshSkew)
}

// export runs the aws CLI and parses the credential document it prints.
//
// Every failure is reported as the same short, sanitized sentence naming only
// the profile. The CLI's own output is deliberately dropped: it can carry
// credential material, and everything patchbay reports ends up in a ledger
// record and in a caller-visible error body.
func export(ctx context.Context, profile string) (Credentials, error) {
	runCtx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()

	// Every argument is a constant: the profile is selected through the CLI's own
	// AWS_PROFILE variable rather than an argument built from configuration, so
	// nothing patchbay reads can ever become part of the command line.
	cmd := exec.CommandContext(runCtx, awsBinary, configureVerb, exportVerb)
	cmd.Env = append(os.Environ(), profileEnv+"="+profile)

	out, err := cmd.Output()
	if err != nil {
		return Credentials{}, fmt.Errorf("exporting aws credentials for profile %q failed", profile)
	}

	creds, err := parseCredentials(out)
	if err != nil {
		return Credentials{}, fmt.Errorf("aws credentials for profile %q could not be read", profile)
	}

	return creds, nil
}

// parseCredentials reads the fields patchbay needs out of the CLI's document,
// ignoring every other key rather than failing on one it has not seen.
func parseCredentials(document []byte) (Credentials, error) {
	var fields map[string]json.RawMessage

	if err := json.Unmarshal(document, &fields); err != nil {
		return Credentials{}, fmt.Errorf("decode credential document: %w", err)
	}

	creds := Credentials{
		AccessKeyID:     stringField(fields, fieldAccessKeyID),
		SecretAccessKey: stringField(fields, fieldSecretKey),
		SessionToken:    stringField(fields, fieldSessionKey),
		Expiration:      time.Time{},
	}

	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return Credentials{}, errors.New("credential document names no key pair")
	}

	if stamp := stringField(fields, fieldExpiration); stamp != "" {
		expiration, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			return Credentials{}, fmt.Errorf("parse credential expiration: %w", err)
		}

		creds.Expiration = expiration
	}

	return creds, nil
}

// stringField reads one string field, treating a missing field and one of some
// other type alike: the document did not say.
func stringField(fields map[string]json.RawMessage, name string) string {
	raw, present := fields[name]
	if !present {
		return ""
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}

	return value
}
