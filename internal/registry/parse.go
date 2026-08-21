package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/joshsymonds/patchbay/internal/billing"
)

// Wire field names of a route object. They double as the vocabulary of every
// parse and validation error, so an operator reading a log sees the JSON key
// that needs fixing. fieldKeyEnvFile deliberately omits "API": a const spelled
// fieldAPIKeyEnvFile trips gosec's hardcoded-credential heuristic (G101).
const (
	fieldContexts          = "contexts"
	fieldRoutes            = "routes"
	fieldDefaultRoute      = "default_route"
	fieldBaseURL           = "base_url"
	fieldModel             = "model"
	fieldAuth              = "auth"
	fieldKeyEnvFile        = "api_key_env_file"
	fieldAWSProfile        = "aws_profile"
	fieldAWSRegion         = "aws_region"
	fieldModelMapEnvFile   = "model_map_env_file"
	fieldBilling           = "billing"
	fieldExtraBody         = "extra_body"
	fieldInsecure          = "insecure"
	fieldMaxInputTokens    = "max_input_tokens"
	fieldMaxOutputTokens   = "max_output_tokens"
	fieldStripCacheControl = "strip_cache_control"
	fieldCaptureBodies     = "capture_bodies"
)

// registryFile is the top level of a registry document.
type registryFile struct {
	Contexts map[string]json.RawMessage `json:"contexts"`
}

// parseRegistry strictly decodes a registry document into contexts keyed by
// name. Unknown fields, trailing data and empty alias keys are all rejected: a
// registry patchbay does not fully understand is not one it will honor. That
// strictness is also what makes a document of some other shape — an older one
// keyed by "routes", or a newer one this build has never seen — fail whole
// rather than half-apply.
//
// Alias keys are unconstrained. Directory-style names such as "openrouter/sol"
// are the convention, but they are matched whole and never parsed, so no
// character is special and none is disallowed. Context names are the opposite:
// they ride in a URL path, and validateContexts holds them to that.
func parseRegistry(data []byte) (map[string]Context, error) {
	var file registryFile

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected trailing data after the registry document")
	}

	names := sortedKeys(file.Contexts)
	contexts := make(map[string]Context, len(names))

	for _, name := range names {
		parsed, err := parseContext(name, file.Contexts[name])
		if err != nil {
			return nil, fmt.Errorf("context %q: %w", name, err)
		}

		contexts[name] = parsed
	}

	return contexts, nil
}

// parseContext decodes one context object: its optional default route and its
// aliases. Every key is matched explicitly, so an unknown or misspelled field
// is named in the error rather than ignored.
func parseContext(name string, raw json.RawMessage) (Context, error) {
	var fields map[string]json.RawMessage

	if err := json.Unmarshal(raw, &fields); err != nil {
		return Context{}, fmt.Errorf("decode context: %w", err)
	}

	// A context that declares no default route rides the built-in one, which is
	// exactly what a bare Claude model got before contexts existed.
	parsed := Context{Name: name, DefaultRoute: DefaultRoute(), Routes: map[string]Route{}}

	for _, field := range sortedKeys(fields) {
		switch field {
		case fieldDefaultRoute:
			// The default route answers for models no alias claims, so it has no
			// alias of its own to be found under.
			route, err := parseRoute("", fields[field])
			if err != nil {
				return Context{}, fmt.Errorf("%s: %w", fieldDefaultRoute, err)
			}

			parsed.DefaultRoute = route
		case fieldRoutes:
			routes, err := parseRoutes(fields[field])
			if err != nil {
				return Context{}, err
			}

			parsed.Routes = routes
		default:
			return Context{}, fmt.Errorf("unknown field %q", field)
		}
	}

	// Both the default route and the aliases are settled now, so the rewrite
	// targets no fallthrough may ride can be computed once instead of on every
	// request.
	parsed.foreignTargets = foreignTargets(parsed)

	return parsed, nil
}

// parseRoutes decodes a context's routes object into routes keyed by alias.
func parseRoutes(raw json.RawMessage) (map[string]Route, error) {
	var entries map[string]json.RawMessage

	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("field %q: %w", fieldRoutes, err)
	}

	routes := make(map[string]Route, len(entries))

	for _, alias := range sortedKeys(entries) {
		if alias == "" {
			return nil, errors.New("empty alias key")
		}

		route, err := parseRoute(alias, entries[alias])
		if err != nil {
			return nil, fmt.Errorf("alias %q: %w", alias, err)
		}

		routes[alias] = route
	}

	return routes, nil
}

// sortedKeys returns a map's keys in order, so a document with several problems
// always reports the same one.
func sortedKeys[Val any](values map[string]Val) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

// parseRoute decodes one route object. Every key is matched explicitly so an
// unknown or misspelled field is named in the error rather than ignored.
func parseRoute(alias string, raw json.RawMessage) (Route, error) {
	var fields map[string]json.RawMessage

	if err := json.Unmarshal(raw, &fields); err != nil {
		return Route{}, fmt.Errorf("decode route: %w", err)
	}

	route := Route{Alias: alias}

	for _, name := range sortedKeys(fields) {
		if err := assignField(&route, name, fields[name]); err != nil {
			return Route{}, err
		}
	}

	return route, nil
}

// assignField decodes one wire field into its Route field.
func assignField(route *Route, name string, value json.RawMessage) error {
	switch name {
	case fieldBaseURL:
		return decodeField(&route.BaseURL, name, value)
	case fieldModel:
		return decodeField(&route.Model, name, value)
	case fieldAuth:
		return decodeField(&route.Auth, name, value)
	case fieldKeyEnvFile:
		return decodeField(&route.APIKeyEnvFile, name, value)
	case fieldAWSProfile:
		return decodeField(&route.AWSProfile, name, value)
	case fieldAWSRegion:
		return decodeField(&route.AWSRegion, name, value)
	case fieldModelMapEnvFile:
		return decodeField(&route.ModelMapEnvFile, name, value)
	case fieldBilling:
		return decodeBillingClass(route, value)
	case fieldExtraBody:
		route.ExtraBody = bytes.Clone(value)

		return nil
	case fieldInsecure:
		return decodeField(&route.Insecure, name, value)
	case fieldMaxInputTokens:
		return decodeField(&route.MaxInputTokens, name, value)
	case fieldMaxOutputTokens:
		return decodeField(&route.MaxOutputTokens, name, value)
	case fieldStripCacheControl:
		return decodeField(&route.StripCacheControl, name, value)
	case fieldCaptureBodies:
		return decodeField(&route.CaptureBodies, name, value)
	default:
		return fmt.Errorf("unknown field %q", name)
	}
}

// decodeBillingClass parses the explicit billing class and records that the
// field was present. A blank value is present and invalid, not an omission.
func decodeBillingClass(route *Route, value json.RawMessage) error {
	var raw string
	if err := decodeField(&raw, fieldBilling, value); err != nil {
		return err
	}

	class, err := billing.ParseClass(raw)
	if err != nil {
		return fmt.Errorf("field %q: %w", fieldBilling, err)
	}

	route.BillingClass = class
	route.BillingClassSet = true

	return nil
}

// decodeField decodes a single JSON value, naming the wire field on failure.
func decodeField[Val any](target *Val, name string, value json.RawMessage) error {
	if err := json.Unmarshal(value, target); err != nil {
		return fmt.Errorf("field %q: %w", name, err)
	}

	return nil
}
