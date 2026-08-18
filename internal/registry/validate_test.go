package registry_test

import (
	"strings"
	"testing"
)

// siblingRoute is a valid route that shares every fixture file. A file-level
// validation failure must take it down too: validation is whole-file.
const siblingRoute = `"sibling-ok":{"base_url":"https://openrouter.ai/api",` +
	`"model":"openai/gpt-5.6-luna","auth":"inject","api_key_env_file":"OPENROUTER_API_KEY_FILE"}`

// fixture wraps one route under alias next to the always-valid sibling.
func fixture(alias, route string) string {
	return `{"routes":{` + siblingRoute + `,"` + alias + `":` + route + `}}`
}

// assertErrMentions fails unless err is non-nil and names every want fragment.
func assertErrMentions(t *testing.T, err error, wants []string) {
	t.Helper()

	if err == nil {
		t.Fatalf("Err() = nil; want an error mentioning %q", wants)
	}

	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Err() = %q; want it to mention %q", err.Error(), want)
		}
	}
}

// assertRejected checks the fail-closed contract: the whole file is discarded,
// both aliases fall through to the default route, and Err explains why.
func assertRejected(t *testing.T, body, alias string, wants []string) {
	t.Helper()

	reg := newRegistry(t, body)

	assertDefault(t, reg, alias)
	assertDefault(t, reg, "sibling-ok")
	assertErrMentions(t, reg.Err(), wants)
}

// assertAccepted checks the file loads and alias resolves to its own route.
func assertAccepted(t *testing.T, body, alias string) {
	t.Helper()

	reg := newRegistry(t, body)

	if err := reg.Err(); err != nil {
		t.Fatalf("Err() = %v; want nil for a valid registry", err)
	}

	if got := reg.Resolve(alias).Alias; got != alias {
		t.Fatalf("Resolve(%q) returned alias %q; want the aliased route", alias, got)
	}

	if got := reg.Resolve("sibling-ok").Alias; got != "sibling-ok" {
		t.Fatalf("Resolve(%q) returned alias %q; want the sibling route", "sibling-ok", got)
	}
}

// validationCase is one route object judged against the whole-file rules.
type validationCase struct {
	name  string
	alias string
	route string
	wants []string // error fragments; empty means the route must be accepted
}

func runValidationCases(t *testing.T, tests []validationCase) {
	t.Helper()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body := fixture(tc.alias, tc.route)
			if len(tc.wants) == 0 {
				assertAccepted(t, body, tc.alias)

				return
			}

			assertRejected(t, body, tc.alias, tc.wants)
		})
	}
}

// Rule 1: auth must be "forward" or "inject".
func TestValidateAuthMode(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{
			name:  "unknown mode rejected",
			alias: "probe",
			route: `{"base_url":"https://api.anthropic.com","auth":"bearer"}`,
			wants: []string{`"probe"`, "auth", "bearer"},
		},
		{
			name:  "missing auth rejected",
			alias: "probe",
			route: `{"base_url":"https://api.anthropic.com"}`,
			wants: []string{`"probe"`, "auth"},
		},
		{
			name:  "mixed case rejected",
			alias: "probe",
			route: `{"base_url":"https://api.anthropic.com","auth":"Forward"}`,
			wants: []string{`"probe"`, "auth"},
		},
		{
			name:  "forward accepted",
			alias: "probe",
			route: `{"base_url":"https://api.anthropic.com","auth":"forward"}`,
		},
		{
			name:  "inject accepted",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject","api_key_env_file":"KEY_FILE"}`,
		},
	})
}

// Rule 2 (D1): a forward route carries the caller's own bearer token, so it may
// only ever point at Anthropic.
func TestValidateForwardBaseURLIsAnthropicOnly(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{
			name:  "foreign host rejected",
			alias: "probe",
			route: `{"base_url":"https://evil.example","auth":"forward"}`,
			wants: []string{`"probe"`, "forward", "base_url", "https://evil.example"},
		},
		{
			name:  "trailing slash rejected",
			alias: "probe",
			route: `{"base_url":"https://api.anthropic.com/","auth":"forward"}`,
			wants: []string{`"probe"`, "forward", "base_url"},
		},
		{
			name:  "path suffix rejected",
			alias: "probe",
			route: `{"base_url":"https://api.anthropic.com/v1","auth":"forward"}`,
			wants: []string{`"probe"`, "forward", "base_url"},
		},
		{
			name:  "lookalike subdomain rejected",
			alias: "probe",
			route: `{"base_url":"https://api.anthropic.com.evil.example","auth":"forward"}`,
			wants: []string{`"probe"`, "forward", "base_url"},
		},
		{
			name:  "localhost rejected for forward",
			alias: "probe",
			route: `{"base_url":"http://127.0.0.1:8080","auth":"forward"}`,
			wants: []string{`"probe"`, "forward", "base_url"},
		},
		{
			name:  "exact anthropic base accepted",
			alias: "probe",
			route: `{"base_url":"https://api.anthropic.com","auth":"forward"}`,
		},
	})
}

// Rule 3 (D4): an inject route pays with a foreign key, so it must never look
// like a Claude model in either the alias or the rewrite target.
func TestValidateInjectNeverReferencesClaude(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{
			name:  "claude in alias rejected",
			alias: "claude-fast",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"claude-fast"`, "inject", "claude"},
		},
		{
			name:  "claude in alias rejected case-insensitively",
			alias: "my-CLAUDE-pin",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"my-CLAUDE-pin"`, "inject", "claude"},
		},
		{
			name:  "claude in model target rejected",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","model":"anthropic/claude-sonnet-5",` +
				`"auth":"inject","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"probe"`, "inject", "claude"},
		},
		{
			name:  "claude in model target rejected case-insensitively",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","model":"vendor/Claude-Clone",` +
				`"auth":"inject","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"probe"`, "inject", "claude"},
		},
		{
			name:  "claude embedded mid-token rejected",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","model":"vendor/notclaudeish",` +
				`"auth":"inject","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"probe"`, "inject", "claude"},
		},
		{
			name:  "claude inside a namespaced alias rejected",
			alias: "openrouter/claude-clone",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"openrouter/claude-clone"`, "inject", "claude"},
		},
		{
			name:  "namespaced foreign alias accepted",
			alias: "openrouter/sol",
			route: `{"base_url":"https://openrouter.ai/api","model":"openrouter/sol-1",` +
				`"auth":"inject","api_key_env_file":"KEY_FILE"}`,
		},
		{
			name:  "claude on a forward route accepted",
			alias: "claude-sonnet-5",
			route: `{"base_url":"https://api.anthropic.com","model":"claude-opus-5","auth":"forward"}`,
		},
		{
			name:  "foreign alias and model accepted",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","model":"openai/gpt-5.6-luna",` +
				`"auth":"inject","api_key_env_file":"KEY_FILE"}`,
		},
	})
}

// Rule 4: api_key_env_file is required by inject and forbidden on forward.
func TestValidateAPIKeyEnvFilePairing(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{
			name:  "inject without a key file rejected",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject"}`,
			wants: []string{`"probe"`, "inject", "api_key_env_file"},
		},
		{
			name:  "inject with an empty key file rejected",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject","api_key_env_file":""}`,
			wants: []string{`"probe"`, "inject", "api_key_env_file"},
		},
		{
			name:  "forward with a key file rejected",
			alias: "probe",
			route: `{"base_url":"https://api.anthropic.com","auth":"forward","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"probe"`, "forward", "api_key_env_file"},
		},
		{
			name:  "inject with a key file accepted",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject","api_key_env_file":"KEY_FILE"}`,
		},
		{
			name:  "forward without a key file accepted",
			alias: "probe",
			route: `{"base_url":"https://api.anthropic.com","auth":"forward"}`,
		},
	})
}

// Rule 5: base_url must be an absolute https URL; http only for loopback.
func TestValidateBaseURL(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{
			name:  "missing base_url rejected",
			alias: "probe",
			route: `{"auth":"inject","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"probe"`, "base_url"},
		},
		{
			name:  "relative path rejected",
			alias: "probe",
			route: `{"base_url":"/api","auth":"inject","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"probe"`, "base_url", "/api"},
		},
		{
			name:  "bare host rejected",
			alias: "probe",
			route: `{"base_url":"openrouter.ai","auth":"inject","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"probe"`, "base_url", "openrouter.ai"},
		},
		{
			name:  "scheme without a host rejected",
			alias: "probe",
			route: `{"base_url":"https://","auth":"inject","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"probe"`, "base_url"},
		},
		{
			name:  "unparsable url rejected",
			alias: "probe",
			route: `{"base_url":"https://%zz","auth":"inject","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"probe"`, "base_url"},
		},
		{
			name:  "non-http scheme rejected",
			alias: "probe",
			route: `{"base_url":"ftp://files.example","auth":"inject","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"probe"`, "base_url", "ftp://files.example"},
		},
		{
			name:  "plain http rejected for a public host",
			alias: "probe",
			route: `{"base_url":"http://openrouter.ai","auth":"inject","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"probe"`, "base_url", "http://openrouter.ai"},
		},
		{
			name:  "https accepted",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject","api_key_env_file":"KEY_FILE"}`,
		},
		{
			name:  "http accepted for 127.0.0.1",
			alias: "probe",
			route: `{"base_url":"http://127.0.0.1:8080","auth":"inject","api_key_env_file":"KEY_FILE"}`,
		},
		{
			name:  "http accepted for localhost",
			alias: "probe",
			route: `{"base_url":"http://localhost:1234/api","auth":"inject","api_key_env_file":"KEY_FILE"}`,
		},
	})
}

// Rule 6: extra_body, when present, must be a JSON object.
func TestValidateExtraBody(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{
			name:  "array rejected",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject",` +
				`"api_key_env_file":"KEY_FILE","extra_body":[1,2]}`,
			wants: []string{`"probe"`, "extra_body"},
		},
		{
			name:  "string rejected",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject",` +
				`"api_key_env_file":"KEY_FILE","extra_body":"reasoning"}`,
			wants: []string{`"probe"`, "extra_body"},
		},
		{
			name:  "null rejected",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject",` +
				`"api_key_env_file":"KEY_FILE","extra_body":null}`,
			wants: []string{`"probe"`, "extra_body"},
		},
		{
			name:  "object accepted",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject",` +
				`"api_key_env_file":"KEY_FILE","extra_body":{"reasoning_effort":"low"}}`,
		},
		{
			name:  "empty object accepted",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject",` +
				`"api_key_env_file":"KEY_FILE","extra_body":{}}`,
		},
		{
			name:  "absent extra_body accepted",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject","api_key_env_file":"KEY_FILE"}`,
		},
	})
}

func TestStrictDecodeRejectsMalformedRegistries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		body  string
		wants []string
	}{
		{
			name:  "unknown field inside a route",
			body:  fixture("probe", `{"base_url":"https://api.anthropic.com","auth":"forward","bogus":1}`),
			wants: []string{`"probe"`, "unknown field", "bogus"},
		},
		{
			name:  "camelCase spelling of a known field",
			body:  fixture("probe", `{"baseUrl":"https://api.anthropic.com","auth":"forward"}`),
			wants: []string{`"probe"`, "unknown field", "baseUrl"},
		},
		{
			name:  "unknown top-level field",
			body:  `{"routes":{},"bogus":1}`,
			wants: []string{"unknown field", "bogus"},
		},
		{
			name:  "trailing json value",
			body:  `{"routes":{}} {"routes":{}}`,
			wants: []string{"trailing"},
		},
		{
			name:  "trailing garbage",
			body:  `{"routes":{}}xyzzy`,
			wants: []string{"trailing"},
		},
		{
			name:  "empty alias key",
			body:  `{"routes":{"":{"base_url":"https://api.anthropic.com","auth":"forward"}}}`,
			wants: []string{"empty alias"},
		},
		{
			name:  "routes is not an object",
			body:  `{"routes":[]}`,
			wants: []string{"routes"},
		},
		{
			name:  "route value is not an object",
			body:  `{"routes":{"probe":5}}`,
			wants: []string{`"probe"`},
		},
		{
			name:  "field with the wrong json type",
			body:  fixture("probe", `{"base_url":5,"auth":"forward"}`),
			wants: []string{`"probe"`, "base_url"},
		},
		{
			name: "token limit is not a number",
			body: fixture("probe", `{"base_url":"https://api.anthropic.com","auth":"forward",`+
				`"max_input_tokens":"many"}`),
			wants: []string{`"probe"`, "max_input_tokens"},
		},
		{
			name: "boolean field is not a boolean",
			body: fixture("probe", `{"base_url":"https://api.anthropic.com","auth":"forward",`+
				`"strip_cache_control":"yes"}`),
			wants: []string{`"probe"`, "strip_cache_control"},
		},
		{
			name:  "truncated json",
			body:  `{"routes":{`,
			wants: []string{"registry"},
		},
		{
			name:  "empty file",
			body:  ``,
			wants: []string{"registry"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := newRegistry(t, tc.body)

			assertDefault(t, reg, "probe")
			assertDefault(t, reg, "sibling-ok")
			assertErrMentions(t, reg.Err(), tc.wants)
		})
	}
}
