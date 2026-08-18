package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// claudeMarker is the substring an inject route may never carry. A foreign key
// paying for something that reads as a Claude model is a billing lie.
const claudeMarker = "claude"

// loopbackHost and localhostHost are the only hosts allowed to drop TLS.
const (
	loopbackHost  = "127.0.0.1"
	localhostHost = "localhost"
)

// validateRoutes checks every route and fails the whole file on the first
// violation. Validation is deliberately all-or-nothing: a registry with one bad
// route is a registry the operator did not mean to ship, and honoring the rest
// of it would silently apply a configuration nobody reviewed.
func validateRoutes(routes map[string]Route) error {
	aliases := make([]string, 0, len(routes))
	for alias := range routes {
		aliases = append(aliases, alias)
	}

	sort.Strings(aliases)

	for _, alias := range aliases {
		if err := validateRoute(routes[alias]); err != nil {
			return fmt.Errorf("alias %q: %w", alias, err)
		}
	}

	return nil
}

// validateRoute applies every rule to a single route.
func validateRoute(route Route) error {
	switch route.Auth {
	case AuthForward:
		if err := validateForward(route); err != nil {
			return err
		}
	case AuthInject:
		if err := validateInject(route); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s %q is not a valid auth mode (want %q or %q)",
			fieldAuth, route.Auth, AuthForward, AuthInject)
	}

	if err := validateBaseURL(route.BaseURL); err != nil {
		return err
	}

	return validateExtraBody(route.ExtraBody)
}

// validateForward enforces the exfiltration rule: a forward route sends the
// caller's own credentials, so it may only ever reach Anthropic, and it has no
// business naming a key file.
func validateForward(route Route) error {
	if route.BaseURL != anthropicBaseURL {
		return fmt.Errorf("%s routes must use %s %q, got %q",
			AuthForward, fieldBaseURL, anthropicBaseURL, route.BaseURL)
	}

	if route.APIKeyEnvFile != "" {
		return fmt.Errorf("%s routes must not set %s", AuthForward, fieldKeyEnvFile)
	}

	return nil
}

// validateInject enforces the misbilling rule: a route paid for with a foreign
// key must not present itself as a Claude model, in its alias or its rewrite
// target, and it needs a key file to inject from.
func validateInject(route Route) error {
	if route.APIKeyEnvFile == "" {
		return fmt.Errorf("%s routes require a non-empty %s", AuthInject, fieldKeyEnvFile)
	}

	if strings.Contains(strings.ToLower(route.Alias), claudeMarker) {
		return fmt.Errorf("%s routes must not reference %q; found it in the alias",
			AuthInject, claudeMarker)
	}

	if strings.Contains(strings.ToLower(route.Model), claudeMarker) {
		return fmt.Errorf("%s routes must not reference %q; found it in %s %q",
			AuthInject, claudeMarker, fieldModel, route.Model)
	}

	return nil
}

// validateBaseURL requires an absolute https URL. Plain http is allowed only
// for loopback, where there is no network to eavesdrop on, so tests can point a
// route at a local stub server.
func validateBaseURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s %q is not a valid URL: %w", fieldBaseURL, raw, err)
	}

	if !parsed.IsAbs() || parsed.Host == "" {
		return fmt.Errorf("%s %q must be an absolute URL with a scheme and host", fieldBaseURL, raw)
	}

	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if host := parsed.Hostname(); host != loopbackHost && host != localhostHost {
			return fmt.Errorf("%s %q may only use http for %s or %s",
				fieldBaseURL, raw, loopbackHost, localhostHost)
		}

		return nil
	default:
		return fmt.Errorf("%s %q must use https", fieldBaseURL, raw)
	}
}

// validateExtraBody requires a JSON object, since the proxy merges it into the
// request object at forward time. An absent extra_body is fine.
func validateExtraBody(body json.RawMessage) error {
	if len(body) == 0 {
		return nil
	}

	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("%s must be a JSON object", fieldExtraBody)
	}

	return nil
}
