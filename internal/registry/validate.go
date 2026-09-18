package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/joshsymonds/patchbay/internal/anthropic"
	"github.com/joshsymonds/patchbay/internal/jsonstrict"
)

const claudeMarker = "claude"
const (
	regionAlphabet  = "lowercase letters, digits and dashes"
	profileAlphabet = "lowercase letters, digits, dashes, dots and underscores"
	loopbackHost    = "127.0.0.1"
	localhostHost   = "localhost"
)

func validateDocument(doc *document) error {
	if err := ValidID(doc.DefaultContext); err != nil {
		return fmt.Errorf("default_context %q %w", doc.DefaultContext, err)
	}
	if _, ok := doc.Contexts[doc.DefaultContext]; !ok {
		return fmt.Errorf("default_context %q does not reference a context", doc.DefaultContext)
	}

	for _, id := range sortedKeys(doc.Seats) {
		if err := ValidID(id); err != nil {
			return fmt.Errorf("seat %q %w", id, err)
		}
		if err := validateSeat(doc.Seats[id]); err != nil {
			return fmt.Errorf("seat %q: %w", id, err)
		}
	}
	for _, id := range sortedKeys(doc.Contexts) {
		if err := ValidID(id); err != nil {
			return fmt.Errorf("context %q %w", id, err)
		}
		ctx := doc.Contexts[id]
		if err := validateContext(id, &ctx, doc.Seats); err != nil {
			return err
		}
		ctx.foreignTargets = foreignTargets(ctx, doc.Seats)
		doc.Contexts[id] = ctx
	}
	return nil
}

// ValidID enforces the exact Context and Seat ID grammar.
func ValidID(id string) error {
	if id == "" {
		return fmt.Errorf("must match ^[a-z0-9][a-z0-9-]*$: empty ID")
	}
	for index, char := range id {
		if index == 0 {
			if !isLowerAlphanumeric(char) {
				return fmt.Errorf("must match ^[a-z0-9][a-z0-9-]*$; found %q", char)
			}
			continue
		}
		if !isLowerAlphanumeric(char) && char != '-' {
			return fmt.Errorf("must match ^[a-z0-9][a-z0-9-]*$; found %q", char)
		}
	}
	return nil
}

// ValidEnvName enforces the one environment-variable-name grammar patchbay
// admits. It is exported because the ledger validates the config_source it
// persists against the same rule: a Seat the registry admits must never produce
// a record the ledger refuses, and two copies of the grammar is exactly how that
// happens.
func ValidEnvName(name string) error {
	if name == "" {
		return errors.New("must match ^[A-Za-z_][A-Za-z0-9_]*$: empty name")
	}
	for index, char := range name {
		if index == 0 {
			if !isEnvStart(char) {
				return fmt.Errorf("must match ^[A-Za-z_][A-Za-z0-9_]*$; found %q", char)
			}
			continue
		}
		if !isEnvChar(char) {
			return fmt.Errorf("must match ^[A-Za-z_][A-Za-z0-9_]*$; found %q", char)
		}
	}
	return nil
}

func isEnvStart(char rune) bool {
	return char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char == '_'
}

func isEnvChar(char rune) bool {
	return isEnvStart(char) || char >= '0' && char <= '9'
}

func validateContext(id string, ctx *Context, seats map[string]Seat) error {
	if err := requireSeat(id, "default_seat", ctx.DefaultSeat, seats); err != nil {
		return err
	}
	for _, selector := range sortedKeys(ctx.Models) {
		if selector == "" {
			return fmt.Errorf("context %q models has an empty selector key", id)
		}
		seatID := ctx.Models[selector]
		if err := requireSeat(
			id,
			fmt.Sprintf("models selector %q", selector),
			seatID,
			seats,
		); err != nil {
			return err
		}
		if seats[seatID].AuthMode == AuthInject &&
			strings.Contains(strings.ToLower(selector), claudeMarker) {
			return fmt.Errorf(
				"context %q models selector %q cannot bind inject seat %q",
				id,
				selector,
				seatID,
			)
		}
	}
	if ctx.Subagents == nil {
		return nil
	}
	if err := requireSeat(
		id,
		"subagents default_seat",
		ctx.Subagents.DefaultSeat,
		seats,
	); err != nil {
		return err
	}
	if err := validateSubagentDestination(
		id,
		"subagents default",
		ctx.Subagents.DefaultSeat,
		seats,
	); err != nil {
		return err
	}
	for _, selector := range sortedKeys(ctx.Subagents.Models) {
		if selector == "" {
			return fmt.Errorf("context %q subagents models has an empty selector key", id)
		}
		seatID := ctx.Subagents.Models[selector]
		if err := requireSeat(
			id,
			fmt.Sprintf("subagents models selector %q", selector),
			seatID,
			seats,
		); err != nil {
			return err
		}
		if err := validateSubagentDestination(
			id,
			fmt.Sprintf("subagents model %q", selector),
			seatID,
			seats,
		); err != nil {
			return err
		}
	}
	return nil
}

func requireSeat(contextID, source, seatID string, seats map[string]Seat) error {
	if _, ok := seats[seatID]; !ok {
		return fmt.Errorf("context %q %s references unknown seat %q", contextID, source, seatID)
	}
	return nil
}

func validateSubagentDestination(contextID, source, seatID string, seats map[string]Seat) error {
	seat := seats[seatID]
	if seat.AuthMode == AuthInject && seat.Model == "" {
		return fmt.Errorf(
			"context %q %s inject seat %q requires a non-empty model rewrite",
			contextID,
			source,
			seatID,
		)
	}
	if seat.AuthMode == AuthInject && strings.Contains(strings.ToLower(seat.Model), claudeMarker) {
		return fmt.Errorf(
			"context %q %s inject seat %q model must not contain %q",
			contextID,
			source,
			seatID,
			claudeMarker,
		)
	}
	return nil
}

func foreignTargets(ctx Context, seats map[string]Seat) map[string]string {
	targets := map[string]string{}
	defaultUpstream := seats[ctx.DefaultSeat].EffectiveUpstream()
	for _, selector := range sortedKeys(ctx.Models) {
		seat := seats[ctx.Models[selector]]
		if seat.Model == "" || seat.EffectiveUpstream() == defaultUpstream {
			continue
		}
		if _, exists := targets[seat.Model]; !exists {
			targets[seat.Model] = selector
		}
	}
	for _, seatID := range ctx.Models {
		seat := seats[seatID]
		if seat.Model != "" && seat.EffectiveUpstream() == defaultUpstream {
			delete(targets, seat.Model)
		}
	}
	return targets
}

func validateSeat(seat Seat) error {
	if seat.AuthMode != AuthSigV4 {
		if err := validateUpstream(seat.Upstream, seat.Insecure); err != nil {
			return err
		}
	}
	if err := validateAuthFields(seat); err != nil {
		return err
	}
	if err := validateBilling(seat); err != nil {
		return err
	}
	if err := validateSpeed(seat); err != nil {
		return err
	}
	if err := validateStripBetas(seat); err != nil {
		return err
	}
	if seat.MaxInputTokens < 0 {
		return fmt.Errorf("%s must not be negative", fieldMaxInputTokens)
	}
	if seat.MaxOutputTokens < 0 {
		return fmt.Errorf("%s must not be negative", fieldMaxOutputTokens)
	}
	return validateExtraBody(seat.ExtraBody)
}

// validateBilling holds a Seat to exactly one answer about who pays for it.
//
// An inject seat spends an operator key. Nothing downstream can look at the
// request and tell whether that key draws on a subscription, a metered account
// or the operator's own hardware, so the seat declares the class and admission
// requires the declaration. A forward seat spends the caller's own credential
// and a sigv4 seat is metered against the AWS account that signed it: both
// derive their class from the credential, so a declaration there would be a
// second answer to a question already settled, and is refused rather than
// silently ignored.
//
// It runs after validateAuthFields so a Seat with an unrecognized auth_mode is
// named for that, not for a billing rule derived from a mode that means nothing.
func validateBilling(seat Seat) error {
	if seat.AuthMode != AuthInject {
		if seat.BillingClassSet {
			return fmt.Errorf("%s seats must not set %s; their billing policy is %q",
				seat.AuthMode, fieldBilling, seat.BillingPolicy())
		}
		return nil
	}
	if !seat.BillingClassSet {
		return fmt.Errorf("inject seats require %s; their billing policy is %q",
			fieldBilling, seat.BillingPolicy())
	}
	// The parser admits only the closed vocabulary, so this catches a Seat built
	// in process — a ResolveState caller's own struct — rather than one read from
	// a file.
	if !seat.BillingClass.Valid() {
		return fmt.Errorf("%s %q is not a valid billing class", fieldBilling, seat.BillingClass)
	}
	return nil
}

func validateAuthFields(seat Seat) error {
	switch seat.AuthMode {
	case AuthForward:
		return validateForwardFields(seat)
	case AuthInject:
		return validateInjectFields(seat)
	case AuthSigV4:
		return validateSigV4Fields(seat)
	default:
		return fmt.Errorf(
			"%s %q is not valid (want %q, %q or %q)",
			fieldAuthMode,
			seat.AuthMode,
			AuthForward,
			AuthInject,
			AuthSigV4,
		)
	}
}

func validateForwardFields(seat Seat) error {
	if seat.Upstream != anthropic.BaseURL {
		return fmt.Errorf("forward seats must use upstream %q", anthropic.BaseURL)
	}
	if seat.APIKeyEnvFile != "" {
		return fmt.Errorf("forward seats must not set %s", fieldKeyEnvFile)
	}
	return validateNoAWSFields(seat)
}

func validateInjectFields(seat Seat) error {
	if err := ValidEnvName(seat.APIKeyEnvFile); err != nil {
		return fmt.Errorf("inject seat %s %q %w", fieldKeyEnvFile, seat.APIKeyEnvFile, err)
	}
	if strings.Contains(strings.ToLower(seat.Model), claudeMarker) {
		return fmt.Errorf("inject seat model %q must not contain %q", seat.Model, claudeMarker)
	}
	return validateNoAWSFields(seat)
}

func validateSigV4Fields(seat Seat) error {
	if seat.Upstream != "" {
		return fmt.Errorf(
			"sigv4 seats must not set %s; endpoint derives from %s",
			fieldUpstream,
			fieldAWSRegion,
		)
	}
	if seat.APIKeyEnvFile != "" {
		return fmt.Errorf("sigv4 seats must not set %s", fieldKeyEnvFile)
	}
	if seat.Insecure {
		return fmt.Errorf("sigv4 seats must not set %s", fieldInsecure)
	}
	required := []struct{ name, value string }{
		{fieldAWSProfile, seat.AWSProfile},
		{fieldAWSRegion, seat.AWSRegion},
		{fieldModelMapEnvFile, seat.ModelMapEnvFile},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("sigv4 seats require a non-empty %s", field.name)
		}
	}
	if err := validateAWSName(
		fieldAWSRegion,
		seat.AWSRegion,
		isRegionChar,
		regionAlphabet,
	); err != nil {
		return err
	}
	return validateAWSName(fieldAWSProfile, seat.AWSProfile, isProfileChar, profileAlphabet)
}

func validateNoAWSFields(seat Seat) error {
	for _, field := range []struct{ name, value string }{{fieldAWSProfile, seat.AWSProfile}, {fieldAWSRegion, seat.AWSRegion}, {fieldModelMapEnvFile, seat.ModelMapEnvFile}} {
		if field.value != "" {
			return fmt.Errorf("%s seats must not set %s", seat.AuthMode, field.name)
		}
	}
	return nil
}

func validateAWSName(field, value string, allowed func(rune) bool, alphabet string) error {
	for _, char := range value {
		if !allowed(char) {
			return fmt.Errorf("%s %q must use only %s; found %q", field, value, alphabet, char)
		}
	}
	return nil
}
func isRegionChar(char rune) bool  { return isLowerAlphanumeric(char) || char == '-' }
func isProfileChar(char rune) bool { return isRegionChar(char) || char == '.' || char == '_' }
func isLowerAlphanumeric(char rune) bool {
	return (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9')
}

func validateUpstream(raw string, insecure bool) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL", fieldUpstream)
	}
	if parsed.User != nil {
		return fmt.Errorf("%s must not contain user information", fieldUpstream)
	}
	if parsed.RawQuery != "" {
		return fmt.Errorf("%s must not contain query parameters", fieldUpstream)
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("%s must not contain a fragment", fieldUpstream)
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		return fmt.Errorf("%s must be an absolute URL with a scheme and host", fieldUpstream)
	}
	switch parsed.Scheme {
	case "https":
		if insecure {
			return fmt.Errorf("%s is set but %s already uses https", fieldInsecure, fieldUpstream)
		}
	case "http":
		host := parsed.Hostname()
		if host == loopbackHost || host == localhostHost {
			if insecure {
				return fmt.Errorf("%s is set but %s is loopback", fieldInsecure, fieldUpstream)
			}
		} else if !insecure {
			return fmt.Errorf("%s may only use http for loopback or with %s", fieldUpstream, fieldInsecure)
		}
	default:
		return fmt.Errorf("%s must use https", fieldUpstream)
	}
	return nil
}

// validateExtraBody holds the arbitrary provider document a Seat carries to
// the rules that make it safe to merge over a request.
//
// It must be an object and canonical: a repeated member is two values under one
// key, and the merge would carry whichever the decoder happened to keep, at the
// root or anywhere below it. Its top-level model member is reserved because the
// selected Seat is the sole authority for an upstream model rewrite.
// validateSpeed keeps speed off the Bedrock path: the vocabulary is held at
// parse time, but the translation a sigv4 seat performs has no field to carry
// the tier at all.
func validateSpeed(seat Seat) error {
	if seat.Speed != "" && seat.AuthMode == AuthSigV4 {
		return fmt.Errorf("%s is not a Bedrock request field and cannot be set on a sigv4 seat", fieldSpeed)
	}
	return nil
}

// validateStripBetas keeps strip_betas on the Bedrock path: only the sigv4
// translation rewrites the caller's beta flags, so on any other seat the list
// would be policy that nothing applies.
func validateStripBetas(seat Seat) error {
	if len(seat.StripBetas) != 0 && seat.AuthMode != AuthSigV4 {
		return fmt.Errorf("%s only applies to the Bedrock translation and cannot be set on a %s seat",
			fieldStripBetas, seat.AuthMode)
	}
	return nil
}

func validateExtraBody(body json.RawMessage) error {
	if len(body) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("%s must be a JSON object", fieldExtraBody)
	}
	if err := jsonstrict.RejectDuplicateNames(body); err != nil {
		return fmt.Errorf("%s: %w", fieldExtraBody, err)
	}
	fields, err := object(body, fieldExtraBody)
	if err != nil {
		return err
	}
	if _, reserved := fields[fieldModel]; reserved {
		return fmt.Errorf("%s must not set reserved member %q", fieldExtraBody, fieldModel)
	}
	return nil
}
