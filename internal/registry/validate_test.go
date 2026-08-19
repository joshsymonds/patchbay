package registry_test

import (
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/registry"
)

// siblingRoute is a valid route that shares every fixture file. A file-level
// validation failure must take it down too: validation is whole-file.
const siblingRoute = `"sibling-ok":{"base_url":"https://openrouter.ai/api",` +
	`"model":"openai/gpt-5.6-luna","auth":"inject","api_key_env_file":"OPENROUTER_API_KEY_FILE"}`

// fixture wraps one route under alias next to the always-valid sibling, in the
// personal context.
func fixture(alias, route string) string {
	return personalDoc(siblingRoute + `,"` + alias + `":` + route)
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

	if got := bare(t, reg, alias).Alias; got != alias {
		t.Fatalf("Resolve(%q) returned alias %q; want the aliased route", alias, got)
	}

	if got := bare(t, reg, "sibling-ok").Alias; got != "sibling-ok" {
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

// Rule 7: a token ceiling is a positive limit or absent (zero). A negative
// value would silently disable the output clamp and advertise a nonsense
// max_input_tokens, so it is rejected.
func TestValidateTokenCeilingsAreNotNegative(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{
			name:  "negative max_input_tokens rejected",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject",` +
				`"api_key_env_file":"KEY_FILE","max_input_tokens":-1}`,
			wants: []string{`"probe"`, "max_input_tokens"},
		},
		{
			name:  "negative max_output_tokens rejected",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject",` +
				`"api_key_env_file":"KEY_FILE","max_output_tokens":-100}`,
			wants: []string{`"probe"`, "max_output_tokens"},
		},
		{
			name:  "zero ceilings accepted",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject",` +
				`"api_key_env_file":"KEY_FILE","max_input_tokens":0,"max_output_tokens":0}`,
		},
		{
			name:  "positive ceilings accepted",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject",` +
				`"api_key_env_file":"KEY_FILE","max_input_tokens":262144,"max_output_tokens":65536}`,
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
			body:  `{"contexts":{},"bogus":1}`,
			wants: []string{"unknown field", "bogus"},
		},
		{
			name:  "v1 document keyed by routes",
			body:  `{"routes":{` + siblingRoute + `}}`,
			wants: []string{"unknown field", "routes"},
		},
		{
			name:  "unknown field inside a context",
			body:  contextDoc(`"personal":{"routes":{},"bogus":1}`),
			wants: []string{`context "personal"`, "unknown field", "bogus"},
		},
		{
			name:  "trailing json value",
			body:  `{"contexts":{}} {"contexts":{}}`,
			wants: []string{"trailing"},
		},
		{
			name:  "trailing garbage",
			body:  `{"contexts":{}}xyzzy`,
			wants: []string{"trailing"},
		},
		{
			name:  "empty alias key",
			body:  personalDoc(`"":{"base_url":"https://api.anthropic.com","auth":"forward"}`),
			wants: []string{"empty alias"},
		},
		{
			name:  "empty context name",
			body:  contextDoc(`"":{"routes":{}}`),
			wants: []string{"empty contexts key"},
		},
		{
			name:  "context name with an uppercase letter",
			body:  contextDoc(`"Work":{"routes":{}}`),
			wants: []string{`"Work"`, "lowercase"},
		},
		{
			name:  "context name with a slash",
			body:  contextDoc(`"work/eu":{"routes":{}}`),
			wants: []string{`"work/eu"`, "lowercase"},
		},
		{
			name:  "context name with a dot",
			body:  contextDoc(`"..":{"routes":{}}`),
			wants: []string{"lowercase"},
		},
		{
			name:  "contexts is not an object",
			body:  `{"contexts":[]}`,
			wants: []string{"contexts"},
		},
		{
			name:  "context value is not an object",
			body:  contextDoc(`"personal":5`),
			wants: []string{`context "personal"`},
		},
		{
			name:  "routes is not an object",
			body:  contextDoc(`"personal":{"routes":[]}`),
			wants: []string{"routes"},
		},
		{
			name:  "route value is not an object",
			body:  personalDoc(`"probe":5`),
			wants: []string{`"probe"`},
		},
		{
			name: "invalid default route",
			body: contextDoc(`"personal":{"default_route":` +
				`{"base_url":"https://evil.example","auth":"forward"},"routes":{}}`),
			wants: []string{"default_route", "forward"},
		},
		{
			name: "unknown field inside a default route",
			body: contextDoc(`"personal":{"default_route":` +
				`{"base_url":"https://api.anthropic.com","auth":"forward","bogus":1},"routes":{}}`),
			wants: []string{"default_route", "unknown field", "bogus"},
		},
		{
			name: "invalid route in a second context takes the whole file down",
			body: contextDoc(`"personal":{"routes":{` + siblingRoute + `}},` +
				`"work":{"routes":{"probe":{"auth":"nonsense"}}}`),
			wants: []string{`context "work"`, `"probe"`, "auth"},
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
			body:  `{"contexts":{`,
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

// Rule 8: a sigv4 route signs with AWS credentials, so it needs a profile, a
// region and a model map, and it has no base_url of its own — the Bedrock
// endpoint is derived from the region.
func TestValidateSigV4FieldPairing(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{
			name:  "complete sigv4 route accepted",
			alias: "bedrock/sonnet",
			route: `{"auth":"sigv4","aws_profile":"work","aws_region":"us-east-2",` +
				`"model_map_env_file":"BEDROCK_MODEL_MAP_FILE","model":"claude-sonnet-5"}`,
		},
		{
			name:  "sigv4 without aws_profile rejected",
			alias: "probe",
			route: `{"auth":"sigv4","aws_region":"us-east-2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{`"probe"`, "sigv4", "aws_profile"},
		},
		{
			name:  "sigv4 with an empty aws_profile rejected",
			alias: "probe",
			route: `{"auth":"sigv4","aws_profile":"","aws_region":"us-east-2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{`"probe"`, "sigv4", "aws_profile"},
		},
		{
			name:  "sigv4 without aws_region rejected",
			alias: "probe",
			route: `{"auth":"sigv4","aws_profile":"work","model_map_env_file":"MAP_FILE"}`,
			wants: []string{`"probe"`, "sigv4", "aws_region"},
		},
		{
			name:  "sigv4 with an empty aws_region rejected",
			alias: "probe",
			route: `{"auth":"sigv4","aws_profile":"work","aws_region":"","model_map_env_file":"MAP_FILE"}`,
			wants: []string{`"probe"`, "sigv4", "aws_region"},
		},
		{
			name:  "sigv4 without model_map_env_file rejected",
			alias: "probe",
			route: `{"auth":"sigv4","aws_profile":"work","aws_region":"us-east-2"}`,
			wants: []string{`"probe"`, "sigv4", "model_map_env_file"},
		},
		{
			name:  "sigv4 with an empty model_map_env_file rejected",
			alias: "probe",
			route: `{"auth":"sigv4","aws_profile":"work","aws_region":"us-east-2","model_map_env_file":""}`,
			wants: []string{`"probe"`, "sigv4", "model_map_env_file"},
		},
		{
			name:  "sigv4 with a hostile aws_region rejected",
			alias: "probe",
			route: `{"auth":"sigv4","aws_profile":"work","aws_region":"us-east-2.example.com/x?",` +
				`"model_map_env_file":"MAP_FILE"}`,
			wants: []string{`"probe"`, "aws_region", "us-east-2.example.com/x?"},
		},
		{
			name:  "sigv4 with an uppercase aws_region rejected",
			alias: "probe",
			route: `{"auth":"sigv4","aws_profile":"work","aws_region":"US-EAST-2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{`"probe"`, "aws_region"},
		},
		{
			name:  "sigv4 with a dotted aws_region rejected",
			alias: "probe",
			route: `{"auth":"sigv4","aws_profile":"work","aws_region":"us.east.2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{`"probe"`, "aws_region"},
		},
		{
			name:  "sigv4 with a spaced aws_profile rejected",
			alias: "probe",
			route: `{"auth":"sigv4","aws_profile":"work bedrock","aws_region":"us-east-2",` +
				`"model_map_env_file":"MAP_FILE"}`,
			wants: []string{`"probe"`, "aws_profile", "work bedrock"},
		},
		{
			name:  "sigv4 with a slashed aws_profile rejected",
			alias: "probe",
			route: `{"auth":"sigv4","aws_profile":"../other","aws_region":"us-east-2",` +
				`"model_map_env_file":"MAP_FILE"}`,
			wants: []string{`"probe"`, "aws_profile"},
		},
		{
			name:  "sigv4 with an uppercase aws_profile rejected",
			alias: "probe",
			route: `{"auth":"sigv4","aws_profile":"Work","aws_region":"us-east-2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{`"probe"`, "aws_profile"},
		},
		{
			name:  "sigv4 with dots and underscores in aws_profile accepted",
			alias: "bedrock/dots",
			route: `{"auth":"sigv4","aws_profile":"work_bedrock.prod-1","aws_region":"us-east-2",` +
				`"model_map_env_file":"MAP_FILE"}`,
		},
		{
			name:  "sigv4 with a base_url rejected",
			alias: "probe",
			route: `{"base_url":"https://bedrock-runtime.us-east-2.amazonaws.com","auth":"sigv4",` +
				`"aws_profile":"work","aws_region":"us-east-2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{`"probe"`, "sigv4", "base_url", "aws_region"},
		},
		{
			name:  "sigv4 with an api key file rejected",
			alias: "probe",
			route: `{"auth":"sigv4","aws_profile":"work","aws_region":"us-east-2",` +
				`"model_map_env_file":"MAP_FILE","api_key_env_file":"KEY_FILE"}`,
			wants: []string{`"probe"`, "sigv4", "api_key_env_file"},
		},
		{
			name:  "forward with aws_profile rejected",
			alias: "probe",
			route: `{"base_url":"https://api.anthropic.com","auth":"forward","aws_profile":"work"}`,
			wants: []string{`"probe"`, "forward", "aws_profile"},
		},
		{
			name:  "forward with aws_region rejected",
			alias: "probe",
			route: `{"base_url":"https://api.anthropic.com","auth":"forward","aws_region":"us-east-2"}`,
			wants: []string{`"probe"`, "forward", "aws_region"},
		},
		{
			name:  "inject with model_map_env_file rejected",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject","api_key_env_file":"KEY_FILE",` +
				`"model_map_env_file":"MAP_FILE"}`,
			wants: []string{`"probe"`, "inject", "model_map_env_file"},
		},
		{
			name:  "inject with aws_region rejected",
			alias: "probe",
			route: `{"base_url":"https://openrouter.ai/api","auth":"inject","api_key_env_file":"KEY_FILE",` +
				`"aws_region":"us-east-2"}`,
			wants: []string{`"probe"`, "inject", "aws_region"},
		},
		{
			name:  "sigv4 naming a claude model accepted",
			alias: "bedrock/claude",
			route: `{"auth":"sigv4","aws_profile":"work","aws_region":"us-east-2",` +
				`"model_map_env_file":"MAP_FILE","model":"claude-sonnet-5","max_input_tokens":200000}`,
		},
	})
}

// Rule 9: insecure permits plain http on a non-loopback host, and is rejected
// anywhere it would change nothing.
func TestValidateInsecure(t *testing.T) {
	t.Parallel()

	runValidationCases(t, []validationCase{
		{
			name:  "insecure http accepted for a lan host",
			alias: "local/qwen",
			route: `{"base_url":"http://gpubox.lan:11434","auth":"inject","api_key_env_file":"KEY_FILE",` +
				`"insecure":true}`,
		},
	})
}

// A sigv4 route's schema fields survive parsing into the resolved Route, since
// the pipeline signs and maps models from them.
func TestSigV4RouteFieldsResolve(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t, personalDoc(`"bedrock/sonnet":{"auth":"sigv4","aws_profile":"work",`+
		`"aws_region":"us-east-2","model_map_env_file":"MAP_FILE","model":"claude-sonnet-5"}`))

	if err := reg.Err(); err != nil {
		t.Fatalf("Err() = %v; want nil for a valid registry", err)
	}

	route := bare(t, reg, "bedrock/sonnet")

	if route.Auth != registry.AuthSigV4 {
		t.Errorf("Auth = %q; want %q", route.Auth, registry.AuthSigV4)
	}

	if route.AWSProfile != "work" {
		t.Errorf("AWSProfile = %q; want %q", route.AWSProfile, "work")
	}

	if route.AWSRegion != "us-east-2" {
		t.Errorf("AWSRegion = %q; want %q", route.AWSRegion, "us-east-2")
	}

	if route.ModelMapEnvFile != "MAP_FILE" {
		t.Errorf("ModelMapEnvFile = %q; want %q", route.ModelMapEnvFile, "MAP_FILE")
	}
}

// The context-name rule is declared once and exported, because the ledger holds
// a stored context name to exactly the same shape before accepting a record. A
// second copy of the rule is how the two ends drift.
func TestValidContextNameIsTheOneSharedRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		context string
		wantErr bool
	}{
		{name: "lowercase", context: "personal", wantErr: false},
		{name: "digits and dashes", context: "work-eu-2", wantErr: false},
		{name: "uppercase", context: "Work", wantErr: true},
		{name: "slash", context: "work/eu", wantErr: true},
		{name: "traversal", context: "..", wantErr: true},
		{name: "space", context: "work eu", wantErr: true},
		{name: "underscore", context: "work_eu", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := registry.ValidContextName(tc.context)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidContextName(%q) = %v; wantErr %v", tc.context, err, tc.wantErr)
			}
		})
	}
}
