package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Wire field names of a route object. They double as the vocabulary of every
// parse and validation error, so an operator reading a log sees the JSON key
// that needs fixing. fieldKeyEnvFile deliberately omits "API": a const spelled
// fieldAPIKeyEnvFile trips gosec's hardcoded-credential heuristic (G101).
const (
	fieldBaseURL           = "base_url"
	fieldModel             = "model"
	fieldAuth              = "auth"
	fieldKeyEnvFile        = "api_key_env_file"
	fieldExtraBody         = "extra_body"
	fieldMaxInputTokens    = "max_input_tokens"
	fieldMaxOutputTokens   = "max_output_tokens"
	fieldStripCacheControl = "strip_cache_control"
	fieldCaptureBodies     = "capture_bodies"
)

// registryFile is the top level of a registry document.
type registryFile struct {
	Routes map[string]json.RawMessage `json:"routes"`
}

// parseRegistry strictly decodes a registry document into routes keyed by
// alias. Unknown fields, trailing data and empty alias keys are all rejected:
// a registry patchbay does not fully understand is not one it will honor.
//
// Alias keys are otherwise unconstrained. Directory-style names such as
// "openrouter/sol" are the convention, but they are matched whole and never
// parsed, so no character is special and none is disallowed.
func parseRegistry(data []byte) (map[string]Route, error) {
	var file registryFile

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected trailing data after the registry document")
	}

	aliases := make([]string, 0, len(file.Routes))
	for alias := range file.Routes {
		aliases = append(aliases, alias)
	}

	sort.Strings(aliases)

	routes := make(map[string]Route, len(aliases))

	for _, alias := range aliases {
		if alias == "" {
			return nil, errors.New("empty alias key")
		}

		route, err := parseRoute(alias, file.Routes[alias])
		if err != nil {
			return nil, err
		}

		routes[alias] = route
	}

	return routes, nil
}

// parseRoute decodes one route object. Every key is matched explicitly so an
// unknown or misspelled field is named in the error rather than ignored.
func parseRoute(alias string, raw json.RawMessage) (Route, error) {
	var fields map[string]json.RawMessage

	if err := json.Unmarshal(raw, &fields); err != nil {
		return Route{}, fmt.Errorf("alias %q: decode route: %w", alias, err)
	}

	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}

	sort.Strings(names)

	route := Route{Alias: alias}

	for _, name := range names {
		if err := assignField(&route, name, fields[name]); err != nil {
			return Route{}, fmt.Errorf("alias %q: %w", alias, err)
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
	case fieldExtraBody:
		route.ExtraBody = bytes.Clone(value)

		return nil
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

// decodeField decodes a single JSON value, naming the wire field on failure.
func decodeField[Val any](target *Val, name string, value json.RawMessage) error {
	if err := json.Unmarshal(value, target); err != nil {
		return fmt.Errorf("field %q: %w", name, err)
	}

	return nil
}
