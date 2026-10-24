package registry_test

import (
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/registry"
)

func TestStrictNewGrammarAndLegacyRejection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body string
		wants      []string
	}{
		{
			name:  "missing default_context",
			body:  `{"seats":{},"contexts":{}}`,
			wants: []string{"default_context"},
		},
		{
			name:  "missing seats",
			body:  `{"default_context":"personal","contexts":{}}`,
			wants: []string{"seats"},
		},
		{
			name:  "missing contexts",
			body:  `{"default_context":"personal","seats":{}}`,
			wants: []string{"contexts"},
		},
		{
			name:  "null seats",
			body:  `{"default_context":"personal","seats":null,"contexts":{}}`,
			wants: []string{"seats", "null"},
		},
		{
			name:  "null contexts",
			body:  `{"default_context":"personal","seats":{},"contexts":null}`,
			wants: []string{"contexts", "null"},
		},
		{
			name:  "unknown root",
			body:  `{"default_context":"personal","seats":{},"contexts":{},"bogus":1}`,
			wants: []string{"unknown field", "bogus"},
		},
		{
			name:  "trailing data",
			body:  `{"default_context":"personal","seats":{},"contexts":{}} {}`,
			wants: []string{"trailing"},
		},
		{name: "legacy routes root", body: `{"routes":{}}`, wants: []string{"routes"}},
		{
			name:  "legacy default_route",
			body:  `{"default_context":"personal","seats":{},"contexts":{"personal":{"default_route":{},"models":{}}}}`,
			wants: []string{"default_route"},
		},
		{
			name:  "legacy context routes",
			body:  `{"default_context":"personal","seats":{},"contexts":{"personal":{"default_seat":"x","routes":{}}}}`,
			wants: []string{"routes"},
		},
		{
			name:  "legacy seat base_url",
			body:  `{"default_context":"personal","seats":{"x":{"base_url":"https://api.anthropic.com","auth_mode":"forward"}},"contexts":{"personal":{"default_seat":"x","models":{}}}}`,
			wants: []string{"base_url"},
		},
		{
			name:  "legacy seat auth",
			body:  `{"default_context":"personal","seats":{"x":{"upstream":"https://api.anthropic.com","auth":"forward"}},"contexts":{"personal":{"default_seat":"x","models":{}}}}`,
			wants: []string{"auth"},
		},
		{
			name:  "inline seat",
			body:  `{"default_context":"personal","seats":{"x":{"upstream":"https://api.anthropic.com","auth_mode":"forward"}},"contexts":{"personal":{"default_seat":"x","models":{"m":{"seat":"x"}}}}}`,
			wants: []string{"models", "string"},
		},
		{
			name:  "null models",
			body:  `{"default_context":"personal","seats":{"x":{"upstream":"https://api.anthropic.com","auth_mode":"forward"}},"contexts":{"personal":{"default_seat":"x","models":null}}}`,
			wants: []string{"models", "null"},
		},
		{
			name:  "null subagents",
			body:  `{"default_context":"personal","seats":{"x":{"upstream":"https://api.anthropic.com","auth_mode":"forward"}},"contexts":{"personal":{"default_seat":"x","models":{},"subagents":null}}}`,
			wants: []string{"subagents", "null"},
		},
		{
			name:  "null subagent models",
			body:  `{"default_context":"personal","seats":{"x":{"upstream":"https://api.anthropic.com","auth_mode":"forward"}},"contexts":{"personal":{"default_seat":"x","models":{},"subagents":{"default_seat":"x","models":null}}}}`,
			wants: []string{"subagents", "models", "null"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeRegistry(t, t.TempDir()+"/registry.json", tc.body)
			_, err := registry.New(path)
			if err == nil {
				t.Fatal("New() = nil error")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

func TestSubagentsModelsMayBeOmitted(t *testing.T) {
	t.Parallel()
	body := `{"default_context":"personal","seats":{` +
		`"anthropic":{"upstream":"https://api.anthropic.com","auth_mode":"forward"},` +
		`"worker":{"upstream":"https://openrouter.ai/api","model":"worker-model",` +
		`"auth_mode":"inject","billing":"metered","api_key_env_file":"WORKER_KEY_FILE"}},` +
		`"contexts":{"personal":{"default_seat":"anthropic","models":{},` +
		`"subagents":{"default_seat":"worker"}}}}`
	reg, err := registry.New(writeRegistry(t, t.TempDir()+"/registry.json", body))
	if err != nil {
		t.Fatalf("New() = %v; want default-only subagents admitted", err)
	}

	state := reg.State()
	if got := state.Contexts["personal"].Subagents.Models; len(got) != 0 {
		t.Fatalf("subagents models = %v; want empty exact-binding map", got)
	}
	res, err := reg.Resolve(
		registry.Selection{Name: "personal", Explicit: true},
		"unbound-model",
		&registry.SubagentObservation{AgentID: "test-agent"},
	)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if res.SeatID != "worker" || res.Match.Kind != registry.SelectionSubagent || res.Match.Model != nil {
		t.Fatalf("Resolve() = seat %q match %+v; want worker subagent default", res.SeatID, res.Match)
	}
}

func TestSubagentsStrictFields(t *testing.T) {
	t.Parallel()
	const prefix = `{"default_context":"personal","seats":{"x":{"upstream":"https://api.anthropic.com","auth_mode":"forward"}},"contexts":{"personal":{"default_seat":"x","models":{},"subagents":`
	for _, tc := range []struct {
		name, subagents string
		wants           []string
	}{
		{name: "missing default_seat", subagents: `{"models":{}}`, wants: []string{"default_seat"}},
		{name: "null models", subagents: `{"default_seat":"x","models":null}`, wants: []string{"models", "null"}},
		{name: "unknown field", subagents: `{"default_seat":"x","bogus":true}`, wants: []string{"unknown field", "bogus"}},
		{name: "null agent_types", subagents: `{"default_seat":"x","agent_types":null}`, wants: []string{"agent_types", "null"}},
		{name: "agent type unknown seat", subagents: `{"default_seat":"x","agent_types":{"Explore":"missing"}}`, wants: []string{"agent_types", "Explore", "missing"}},
		{name: "agent type with a space", subagents: `{"default_seat":"x","agent_types":{"Ex plore":"x"}}`, wants: []string{"agent_types", "Ex plore"}},
		{name: "empty agent type", subagents: `{"default_seat":"x","agent_types":{"":"x"}}`, wants: []string{"agent_types"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := registry.New(writeRegistry(t, t.TempDir()+"/registry.json", prefix+tc.subagents+`}}}`))
			if err == nil {
				t.Fatal("New() = nil error")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

func TestReferenceIDAndSelectorValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, mutation string
		wants          []string
	}{
		{
			name:     "bad seat id",
			mutation: strings.Replace(validRegistry, `"anthropic":`, `"Bad":`, 1),
			wants:    []string{"Bad", "seat"},
		},
		{
			name: "bad context id",
			mutation: strings.Replace(
				strings.Replace(validRegistry, `"personal":{`, `"bad/name":{`, 1),
				`"default_context":"personal"`,
				`"default_context":"bad/name"`,
				1,
			),
			wants: []string{"bad/name"},
		},
		{
			name: "unknown default context",
			mutation: strings.Replace(
				validRegistry,
				`"default_context":"personal"`,
				`"default_context":"missing"`,
				1,
			),
			wants: []string{"default_context", "missing"},
		},
		{
			name: "unknown context default seat",
			mutation: strings.Replace(
				validRegistry,
				`"default_seat":"anthropic"`,
				`"default_seat":"missing"`,
				1,
			),
			wants: []string{"default_seat", "missing"},
		},
		{
			name: "unknown public binding",
			mutation: strings.Replace(
				validRegistry,
				`"luna-fast":"luna"`,
				`"luna-fast":"missing"`,
				1,
			),
			wants: []string{"luna-fast", "missing"},
		},
		{
			name: "unknown subagent default",
			mutation: strings.Replace(
				validRegistry,
				`"default_seat":"sol"`,
				`"default_seat":"missing"`,
				1,
			),
			wants: []string{"subagents", "missing"},
		},
		{
			name: "unknown subagent binding",
			mutation: strings.Replace(
				validRegistry,
				`"claude-opus-5":"bedrock"`,
				`"claude-opus-5":"missing"`,
				1,
			),
			wants: []string{"claude-opus-5", "missing"},
		},
		{
			name:     "empty public selector",
			mutation: strings.Replace(validRegistry, `"luna-fast":"luna"`, `"":"luna"`, 1),
			wants:    []string{"empty", "selector"},
		},
		{
			name: "empty subagent selector",
			mutation: strings.Replace(
				validRegistry,
				`"claude-opus-5":"bedrock"`,
				`"":"bedrock"`,
				1,
			),
			wants: []string{"empty", "selector"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := registry.New(writeRegistry(t, t.TempDir()+"/registry.json", tc.mutation))
			if err == nil {
				t.Fatal("New() = nil error")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

func TestSeatAuthAndSafetyValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body string
		wants      []string
	}{
		{
			name:  "forward foreign upstream",
			body:  oneSeat(`{"upstream":"https://evil.example","auth_mode":"forward"}`),
			wants: []string{"forward", "upstream"},
		},
		{
			name: "inject missing key",
			body: oneSeat(
				`{"upstream":"https://openrouter.ai","auth_mode":"inject","billing":"metered","model":"gpt"}`,
			),
			wants: []string{"inject", "api_key_env_file"},
		},
		{
			name: "inject claude rewrite",
			body: oneSeat(
				`{"upstream":"https://openrouter.ai","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY","model":"Claude-clone"}`,
			),
			wants: []string{"inject", "claude"},
		},
		{
			name: "public claude selector on inject",
			body: oneSeatContext(
				`{"upstream":"https://openrouter.ai","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY","model":"gpt"}`,
				`{"claude-opus-5":"x"}`,
			),
			wants: []string{"claude-opus-5", "inject"},
		},
		{
			name: "subagent inject exact needs rewrite",
			body: subagentSeat(
				`{"upstream":"https://openrouter.ai","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY"}`,
				`"x"`,
				`{"claude-opus-5":"x"}`,
			),
			wants: []string{"subagents", "model"},
		},
		{
			name: "subagent inject default needs rewrite",
			body: subagentSeat(
				`{"upstream":"https://openrouter.ai","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY"}`,
				`"x"`,
				`{}`,
			),
			wants: []string{"subagents", "default", "model"},
		},
		{
			name:  "sigv4 fields required",
			body:  oneSeat(`{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2"}`),
			wants: []string{"model_map_env_file"},
		},
		{
			name: "sigv4 upstream forbidden",
			body: oneSeat(
				`{"upstream":"https://bedrock.example","auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2","model_map_env_file":"MAP"}`,
			),
			wants: []string{"sigv4", "upstream"},
		},
		{
			name: "negative ceiling",
			body: oneSeat(
				`{"upstream":"https://api.anthropic.com","auth_mode":"forward","max_output_tokens":-1}`,
			),
			wants: []string{"max_output_tokens"},
		},
		{
			name: "null extra body",
			body: oneSeat(
				`{"upstream":"https://api.anthropic.com","auth_mode":"forward","extra_body":null}`,
			),
			wants: []string{"extra_body"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := registry.New(writeRegistry(t, t.TempDir()+"/registry.json", tc.body))
			if err == nil {
				t.Fatal("New() = nil error")
			}
			for _, want := range tc.wants {
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

// TestDuplicateJSONMemberNamesAreRejectedAtEveryObject pins strict duplicate
// rejection at every object the registry grammar owns. Encoding/json keeps the
// last member of a repeated name, so a document whose reviewed value is shadowed
// by a second one further down would load as something nobody read.
func TestDuplicateJSONMemberNamesAreRejectedAtEveryObject(t *testing.T) {
	t.Parallel()
	const seat = `{"upstream":"https://api.anthropic.com","auth_mode":"forward"}`
	tests := []struct {
		name, body string
		want       string
	}{
		{
			name: "root",
			body: `{"default_context":"personal","default_context":"personal",` +
				`"seats":{"x":` + seat + `},"contexts":{"personal":{"default_seat":"x","models":{}}}}`,
			want: "default_context",
		},
		{
			name: "seats map",
			body: `{"default_context":"personal","seats":{"x":` + seat + `,"x":` + seat + `},` +
				`"contexts":{"personal":{"default_seat":"x","models":{}}}}`,
			want: "x",
		},
		{
			name: "seat object",
			body: `{"default_context":"personal","seats":{"x":{"upstream":"https://api.anthropic.com",` +
				`"upstream":"https://api.anthropic.com","auth_mode":"forward"}},` +
				`"contexts":{"personal":{"default_seat":"x","models":{}}}}`,
			want: "upstream",
		},
		{
			name: "contexts map",
			body: `{"default_context":"personal","seats":{"x":` + seat + `},"contexts":{` +
				`"personal":{"default_seat":"x","models":{}},"personal":{"default_seat":"x","models":{}}}}`,
			want: "personal",
		},
		{
			name: "context object",
			body: `{"default_context":"personal","seats":{"x":` + seat + `},"contexts":{` +
				`"personal":{"default_seat":"x","default_seat":"x","models":{}}}}`,
			want: "default_seat",
		},
		{
			name: "public binding map",
			body: `{"default_context":"personal","seats":{"x":` + seat + `},"contexts":{` +
				`"personal":{"default_seat":"x","models":{"m":"x","m":"x"}}}}`,
			want: "m",
		},
		{
			name: "subagents object",
			body: `{"default_context":"personal","seats":{"x":` + seat + `},"contexts":{` +
				`"personal":{"default_seat":"x","models":{},"subagents":{"default_seat":"x",` +
				`"default_seat":"x","models":{}}}}}`,
			want: "default_seat",
		},
		{
			name: "subagent binding map",
			body: `{"default_context":"personal","seats":{"x":` + seat + `},"contexts":{` +
				`"personal":{"default_seat":"x","models":{},"subagents":{"default_seat":"x",` +
				`"models":{"m":"x","m":"x"}}}}}`,
			want: "m",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := registry.New(writeRegistry(t, t.TempDir()+"/registry.json", tc.body))
			if err == nil {
				t.Fatal("New() = nil error for a duplicate member name")
			}
			if !strings.Contains(err.Error(), "duplicate") ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the duplicate %q", err, tc.want)
			}
		})
	}
}

// TestAPIKeyEnvFileUsesEnvironmentVariableGrammar pins registry admission to the
// same environment-variable-name grammar the ledger validates config_source
// with, so a Seat that loads can never produce a record the ledger refuses.
func TestAPIKeyEnvFileUsesEnvironmentVariableGrammar(t *testing.T) {
	t.Parallel()
	injectSeat := func(envFile string) string {
		return oneSeat(
			`{"upstream":"https://openrouter.ai","auth_mode":"inject","billing":"metered","model":"gpt",` +
				`"api_key_env_file":"` + envFile + `"}`,
		)
	}
	for _, name := range []string{"KEY", "_key", "PATCHBAY_OPENROUTER_KEY_FILE9", "k"} {
		if _, err := registry.New(
			writeRegistry(t, t.TempDir()+"/registry.json", injectSeat(name)),
		); err != nil {
			t.Errorf("api_key_env_file %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"", "1KEY", "KEY-FILE", "KEY FILE", "KEY/FILE", "aws_profile:work"} {
		_, err := registry.New(writeRegistry(t, t.TempDir()+"/registry.json", injectSeat(name)))
		if err == nil {
			t.Errorf("api_key_env_file %q accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "api_key_env_file") {
			t.Errorf("api_key_env_file %q error %q does not name the field", name, err)
		}
	}
}

// TestValidEnvNameIsTheOneSharedGrammar pins the exported validator the ledger
// reuses, so the two ends of the config_source contract cannot drift.
func TestValidEnvNameIsTheOneSharedGrammar(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		valid bool
	}{
		{"KEY", true}, {"_KEY", true}, {"k9", true}, {"_", true},
		{"", false}, {"9KEY", false}, {"KEY-FILE", false}, {"KEY.FILE", false},
		{"KEY FILE", false}, {"caller", true}, {"aws_profile:work", false},
	} {
		if got := registry.ValidEnvName(tc.name) == nil; got != tc.valid {
			t.Errorf("ValidEnvName(%q) valid=%v, want %v", tc.name, got, tc.valid)
		}
	}
}

func TestValidContextAndSeatIDsShareExactRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		id    string
		valid bool
	}{{"a", true}, {"a-1", true}, {"1-a", true}, {"a-", true}, {"", false}, {"A", false}, {"a_b", false}, {"-a", false}, {"a/b", false}} {
		if got := registry.ValidID(tc.id) == nil; got != tc.valid {
			t.Errorf("ValidID(%q) valid=%v", tc.id, got)
		}
	}
}

func oneSeat(seat string) string { return oneSeatContext(seat, `{}`) }
func oneSeatContext(seat, models string) string {
	return `{"default_context":"personal","seats":{"x":` + seat + `},"contexts":{"personal":{"default_seat":"x","models":` + models + `}}}`
}
func subagentSeat(seat, defaultSeat, models string) string {
	return `{"default_context":"personal","seats":{"x":` + seat + `},"contexts":{"personal":{"default_seat":"x","models":{},"subagents":{"default_seat":` + defaultSeat + `,"models":` + models + `}}}}`
}

// seatCase is one Seat body and what admission must say about it.
type seatCase struct {
	name, seat string
	wants      []string
	forbids    []string
}

// runSeatCases loads each Seat as the only Seat of a one-Context document. A
// case with no wants must be admitted; a case with wants must be refused by an
// error naming every one of them.
func runSeatCases(t *testing.T, cases []seatCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := registry.New(writeRegistry(t, t.TempDir()+"/registry.json", oneSeat(tc.seat)))
			if len(tc.wants) == 0 {
				if err != nil {
					t.Fatalf("New() = %v; want the Seat admitted", err)
				}
				return
			}
			if err == nil {
				t.Fatal("New() = nil error; want the Seat refused")
			}
			for _, want := range tc.wants {
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
			for _, forbid := range tc.forbids {
				if strings.Contains(err.Error(), forbid) {
					t.Errorf("error %q must not mention %q", err, forbid)
				}
			}
		})
	}
}

// TestSeatAuthFieldPairings pins every required-and-forbidden field pairing the
// three auth modes carry. Each mode spends a different credential, so a field
// belonging to another mode is not merely unused — it is a Seat whose operator
// believes something about it that is not true.
func TestSeatAuthFieldPairings(t *testing.T) {
	t.Parallel()
	runSeatCases(t, []seatCase{
		{
			name:  "unknown auth mode",
			seat:  `{"upstream":"https://api.anthropic.com","auth_mode":"passthrough"}`,
			wants: []string{"auth_mode", "passthrough"},
		},
		{name: "missing auth mode", seat: `{"upstream":"https://api.anthropic.com"}`, wants: []string{"auth_mode"}},

		{name: "forward to anthropic accepted", seat: `{"upstream":"https://api.anthropic.com","auth_mode":"forward"}`},
		{
			name:  "forward elsewhere rejected",
			seat:  `{"upstream":"https://evil.example","auth_mode":"forward"}`,
			wants: []string{"forward", "upstream"},
		},
		{
			name:  "forward with a key file rejected",
			seat:  `{"upstream":"https://api.anthropic.com","auth_mode":"forward","api_key_env_file":"KEY_FILE"}`,
			wants: []string{"forward", "api_key_env_file"},
		},
		{
			name:  "forward with aws_profile rejected",
			seat:  `{"upstream":"https://api.anthropic.com","auth_mode":"forward","aws_profile":"work"}`,
			wants: []string{"forward", "aws_profile"},
		},
		{
			name:  "forward with aws_region rejected",
			seat:  `{"upstream":"https://api.anthropic.com","auth_mode":"forward","aws_region":"us-east-2"}`,
			wants: []string{"forward", "aws_region"},
		},
		{
			name:  "forward with model_map_env_file rejected",
			seat:  `{"upstream":"https://api.anthropic.com","auth_mode":"forward","model_map_env_file":"MAP_FILE"}`,
			wants: []string{"forward", "model_map_env_file"},
		},

		{
			name: "inject with a key file accepted",
			seat: `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"}`,
		},
		{
			name:  "inject without a key file rejected",
			seat:  `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered"}`,
			wants: []string{"inject", "api_key_env_file"},
		},
		{
			name:  "inject with an empty key file rejected",
			seat:  `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":""}`,
			wants: []string{"inject", "api_key_env_file"},
		},
		{
			name:  "inject naming claude rejected",
			seat:  `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","model":"Claude-clone"}`,
			wants: []string{"inject", "claude"},
		},
		{
			name:  "inject with model_map_env_file rejected",
			seat:  `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","model_map_env_file":"MAP_FILE"}`,
			wants: []string{"inject", "model_map_env_file"},
		},
		{
			name:  "inject with aws_region rejected",
			seat:  `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","aws_region":"us-east-2"}`,
			wants: []string{"inject", "aws_region"},
		},
		{
			name:  "inject with aws_profile rejected",
			seat:  `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","aws_profile":"work"}`,
			wants: []string{"inject", "aws_profile"},
		},

		{
			name: "complete sigv4 accepted",
			seat: `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2","model_map_env_file":"BEDROCK_MODEL_MAP_FILE","model":"claude-sonnet-5"}`,
		},
		{
			name: "sigv4 with dots and underscores in aws_profile accepted",
			seat: `{"auth_mode":"sigv4","aws_profile":"work_bedrock.prod-1","aws_region":"us-east-2","model_map_env_file":"MAP_FILE"}`,
		},
		{
			name:  "sigv4 without aws_profile rejected",
			seat:  `{"auth_mode":"sigv4","aws_region":"us-east-2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{"sigv4", "aws_profile"},
		},
		{
			name:  "sigv4 with an empty aws_profile rejected",
			seat:  `{"auth_mode":"sigv4","aws_profile":"","aws_region":"us-east-2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{"sigv4", "aws_profile"},
		},
		{
			name:  "sigv4 without aws_region rejected",
			seat:  `{"auth_mode":"sigv4","aws_profile":"work","model_map_env_file":"MAP_FILE"}`,
			wants: []string{"sigv4", "aws_region"},
		},
		{
			name:  "sigv4 with an empty aws_region rejected",
			seat:  `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"","model_map_env_file":"MAP_FILE"}`,
			wants: []string{"sigv4", "aws_region"},
		},
		{
			name:  "sigv4 without model_map_env_file rejected",
			seat:  `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2"}`,
			wants: []string{"sigv4", "model_map_env_file"},
		},
		{
			name:  "sigv4 with an empty model_map_env_file rejected",
			seat:  `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2","model_map_env_file":""}`,
			wants: []string{"sigv4", "model_map_env_file"},
		},
		{
			name:  "sigv4 with an upstream rejected",
			seat:  `{"upstream":"https://bedrock-runtime.us-east-2.amazonaws.com","auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{"sigv4", "upstream", "aws_region"},
		},
		{
			name:  "sigv4 with an api key file rejected",
			seat:  `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2","model_map_env_file":"MAP_FILE","api_key_env_file":"KEY_FILE"}`,
			wants: []string{"sigv4", "api_key_env_file"},
		},
		{
			name: "sigv4 naming a claude model accepted",
			seat: `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2","model_map_env_file":"MAP_FILE","model":"claude-sonnet-5","max_input_tokens":200000}`,
		},
	})
}

// TestSigV4NamesAreConstrainedToASafeAlphabet keeps a hostile region or profile
// out of a signed endpoint and an exec'd AWS CLI invocation: the region becomes
// a hostname and the profile a command argument.
func TestSigV4NamesAreConstrainedToASafeAlphabet(t *testing.T) {
	t.Parallel()
	runSeatCases(t, []seatCase{
		{
			name:  "hostile region",
			seat:  `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2.example.com/x?","model_map_env_file":"MAP_FILE"}`,
			wants: []string{"aws_region", "us-east-2.example.com/x?"},
		},
		{
			name:  "uppercase region",
			seat:  `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"US-EAST-2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{"aws_region"},
		},
		{
			name:  "dotted region",
			seat:  `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us.east.2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{"aws_region"},
		},
		{
			name:  "spaced profile",
			seat:  `{"auth_mode":"sigv4","aws_profile":"work bedrock","aws_region":"us-east-2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{"aws_profile", "work bedrock"},
		},
		{
			name:  "traversing profile",
			seat:  `{"auth_mode":"sigv4","aws_profile":"../other","aws_region":"us-east-2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{"aws_profile"},
		},
		{
			name:  "uppercase profile",
			seat:  `{"auth_mode":"sigv4","aws_profile":"Work","aws_region":"us-east-2","model_map_env_file":"MAP_FILE"}`,
			wants: []string{"aws_profile"},
		},
	})
}

// TestSeatUpstreamMustBeAnAbsoluteHTTPSURL pins the destination rule. The
// upstream is where a provider key gets spent, so a relative, hostless or
// unsupported reference is refused rather than resolved into something.
func TestSeatUpstreamMustBeAnAbsoluteHTTPSURL(t *testing.T) {
	t.Parallel()
	runSeatCases(t, []seatCase{
		{
			name:  "missing upstream",
			seat:  `{"auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"}`,
			wants: []string{"upstream"},
		},
		{
			name:    "relative path",
			seat:    `{"upstream":"/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"}`,
			wants:   []string{"upstream", "absolute URL"},
			forbids: []string{"/api"},
		},
		{
			name:    "bare host",
			seat:    `{"upstream":"openrouter.ai","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"}`,
			wants:   []string{"upstream", "absolute URL"},
			forbids: []string{"openrouter.ai"},
		},
		{
			name:  "scheme without a host",
			seat:  `{"upstream":"https://","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"}`,
			wants: []string{"upstream"},
		},
		{
			name:  "unparsable url",
			seat:  `{"upstream":"https://%zz","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"}`,
			wants: []string{"upstream"},
		},
		{
			name:    "non-http scheme",
			seat:    `{"upstream":"ftp://files.example","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"}`,
			wants:   []string{"upstream", "must use https"},
			forbids: []string{"files.example"},
		},
		{
			name:    "plain http for a public host",
			seat:    `{"upstream":"http://openrouter.ai","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"}`,
			wants:   []string{"upstream", "may only use http", "insecure"},
			forbids: []string{"openrouter.ai"},
		},
		{
			name: "https accepted",
			seat: `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"}`,
		},
		{
			name: "loopback http accepted",
			seat: `{"upstream":"http://127.0.0.1:8080","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"}`,
		},
		{
			name: "localhost http accepted",
			seat: `{"upstream":"http://localhost:1234/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"}`,
		},
	})
}

// TestSeatInsecureOnlyBuysNonLoopbackHTTP pins insecure to the one thing it
// does. Anywhere it would change nothing it is refused, so it can never sit in a
// registry looking like it is protecting something.
func TestSeatInsecureOnlyBuysNonLoopbackHTTP(t *testing.T) {
	t.Parallel()
	runSeatCases(t, []seatCase{
		{
			name: "lan host with insecure accepted",
			seat: `{"upstream":"http://gpubox.lan:11434","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","insecure":true}`,
		},
		{
			name:    "insecure on https rejected",
			seat:    `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","insecure":true}`,
			wants:   []string{"insecure", "upstream", "https"},
			forbids: []string{"openrouter.ai"},
		},
		{
			name:    "insecure on loopback rejected",
			seat:    `{"upstream":"http://127.0.0.1:8080","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","insecure":true}`,
			wants:   []string{"insecure", "upstream", "loopback"},
			forbids: []string{"127.0.0.1"},
		},
		{
			name:  "insecure on localhost rejected",
			seat:  `{"upstream":"http://localhost:1234/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","insecure":true}`,
			wants: []string{"insecure", "upstream", "loopback"},
		},
		{
			name:  "insecure on sigv4 rejected",
			seat:  `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2","model_map_env_file":"MAP_FILE","insecure":true}`,
			wants: []string{"sigv4", "insecure"},
		},
	})
}

// TestSeatSpeedIsFastOrNothing pins speed to the one tier an upstream names,
// on the two auth modes whose bodies can carry the field.
func TestSeatSpeedIsFastOrNothing(t *testing.T) {
	t.Parallel()
	runSeatCases(t, []seatCase{
		{
			name: "fast on inject accepted",
			seat: `{"upstream":"http://127.0.0.1:8317","auth_mode":"inject","billing":"subscription","api_key_env_file":"KEY_FILE","speed":"fast"}`,
		},
		{
			name: "fast on forward accepted",
			seat: `{"upstream":"https://api.anthropic.com","auth_mode":"forward","speed":"fast"}`,
		},
		{
			name:    "other tier rejected",
			seat:    `{"upstream":"http://127.0.0.1:8317","auth_mode":"inject","billing":"subscription","api_key_env_file":"KEY_FILE","speed":"slow"}`,
			wants:   []string{"speed", "fast", "omitted"},
			forbids: []string{"slow"},
		},
		{
			name:  "empty string rejected",
			seat:  `{"upstream":"http://127.0.0.1:8317","auth_mode":"inject","billing":"subscription","api_key_env_file":"KEY_FILE","speed":""}`,
			wants: []string{"speed", "fast", "omitted"},
		},
		{
			name:  "fast on sigv4 rejected",
			seat:  `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2","model_map_env_file":"MAP_FILE","speed":"fast"}`,
			wants: []string{"sigv4", "speed"},
		},
	})
}

// TestSeatCacheTTLIsOneHourOnTheAnthropicForward pins cache_ttl to the one
// extended lifetime, on the one auth mode that reaches Anthropic's prompt
// cache, and never beside a strip that would leave it nothing to write into.
func TestSeatCacheTTLIsOneHourOnTheAnthropicForward(t *testing.T) {
	t.Parallel()
	runSeatCases(t, []seatCase{
		{
			name: "one hour on forward accepted",
			seat: `{"upstream":"https://api.anthropic.com","auth_mode":"forward","cache_ttl":"1h"}`,
		},
		{
			name:    "other lifetime rejected",
			seat:    `{"upstream":"https://api.anthropic.com","auth_mode":"forward","cache_ttl":"5m"}`,
			wants:   []string{"cache_ttl", "1h", "omitted"},
			forbids: []string{"5m"},
		},
		{
			name:  "empty string rejected",
			seat:  `{"upstream":"https://api.anthropic.com","auth_mode":"forward","cache_ttl":""}`,
			wants: []string{"cache_ttl", "1h", "omitted"},
		},
		{
			name:  "non-string rejected",
			seat:  `{"upstream":"https://api.anthropic.com","auth_mode":"forward","cache_ttl":3600}`,
			wants: []string{"cache_ttl"},
		},
		{
			name:  "one hour on inject rejected",
			seat:  `{"upstream":"http://127.0.0.1:8317","auth_mode":"inject","billing":"subscription","api_key_env_file":"KEY_FILE","cache_ttl":"1h"}`,
			wants: []string{"cache_ttl", "inject"},
		},
		{
			name:  "one hour on sigv4 rejected",
			seat:  `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2","model_map_env_file":"MAP_FILE","cache_ttl":"1h"}`,
			wants: []string{"cache_ttl", "sigv4"},
		},
		{
			name:  "one hour beside strip_cache_control rejected",
			seat:  `{"upstream":"https://api.anthropic.com","auth_mode":"forward","cache_ttl":"1h","strip_cache_control":true}`,
			wants: []string{"cache_ttl", "strip_cache_control"},
		},
	})
}

// TestSeatStripBetasIsBedrockOnly keeps strip_betas on the one auth mode whose
// translation rewrites the caller's beta flags, and holds every entry to a
// name the translation could actually match.
func TestSeatStripBetasIsBedrockOnly(t *testing.T) {
	t.Parallel()
	const sigv4Head = `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2","model_map_env_file":"MAP_FILE"`
	runSeatCases(t, []seatCase{
		{
			name: "names on sigv4 accepted",
			seat: sigv4Head + `,"strip_betas":["advisor-tool-2026-03-01","prompt-caching-scope-2026-01-05"]}`,
		},
		{
			name: "empty list on sigv4 accepted",
			seat: sigv4Head + `,"strip_betas":[]}`,
		},
		{
			name:  "names on forward rejected",
			seat:  `{"upstream":"https://api.anthropic.com","auth_mode":"forward","strip_betas":["advisor-tool-2026-03-01"]}`,
			wants: []string{"strip_betas", "forward"},
		},
		{
			name:  "names on inject rejected",
			seat:  `{"upstream":"http://127.0.0.1:8317","auth_mode":"inject","billing":"subscription","api_key_env_file":"KEY_FILE","strip_betas":["advisor-tool-2026-03-01"]}`,
			wants: []string{"strip_betas", "inject"},
		},
		{
			name:  "blank entry rejected",
			seat:  sigv4Head + `,"strip_betas":[""]}`,
			wants: []string{"strip_betas", "beta flag name"},
		},
		{
			name:  "padded entry rejected",
			seat:  sigv4Head + `,"strip_betas":[" advisor-tool-2026-03-01"]}`,
			wants: []string{"strip_betas", "whitespace"},
		},
		{
			name:  "non-string entry rejected",
			seat:  sigv4Head + `,"strip_betas":[1]}`,
			wants: []string{"strip_betas"},
		},
	})
}

func TestSeatUpstreamRejectsCredentialAndRoutingMaterialWithoutEchoingIt(t *testing.T) {
	t.Parallel()
	const injectTail = `","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"}`
	runSeatCases(t, []seatCase{
		{
			name:    "userinfo",
			seat:    `{"upstream":"https://ledgeruser:ledgerpass@example.test/v1` + injectTail,
			wants:   []string{"upstream", "must not contain user information"},
			forbids: []string{"ledgeruser", "ledgerpass"},
		},
		{
			name:    "query",
			seat:    `{"upstream":"https://example.test/v1?token=sk-query-placeholder` + injectTail,
			wants:   []string{"upstream", "must not contain query parameters"},
			forbids: []string{"sk-query-placeholder"},
		},
		{
			name:    "fragment",
			seat:    `{"upstream":"https://example.test/v1#fragment-placeholder` + injectTail,
			wants:   []string{"upstream", "must not contain a fragment"},
			forbids: []string{"fragment-placeholder"},
		},
		{
			name:    "malformed URL",
			seat:    `{"upstream":"https://%zz/secret-placeholder` + injectTail,
			wants:   []string{"upstream", "is not a valid URL"},
			forbids: []string{"%zz", "secret-placeholder"},
		},
		{
			name:    "relative URL",
			seat:    `{"upstream":"/relative-secret-placeholder` + injectTail,
			wants:   []string{"upstream", "absolute URL"},
			forbids: []string{"relative-secret-placeholder"},
		},
		{
			name:    "unsupported scheme",
			seat:    `{"upstream":"ftp://files.example/secret-placeholder` + injectTail,
			wants:   []string{"upstream", "must use https"},
			forbids: []string{"secret-placeholder"},
		},
		{
			name:    "public HTTP",
			seat:    `{"upstream":"http://files.example/secret-placeholder` + injectTail,
			wants:   []string{"upstream", "may only use http", "insecure"},
			forbids: []string{"secret-placeholder"},
		},
		{
			name:    "forward userinfo checked before exact destination",
			seat:    `{"upstream":"https://ledgeruser:ledgerpass@api.anthropic.com","auth_mode":"forward"}`,
			wants:   []string{"upstream", "must not contain user information"},
			forbids: []string{"ledgeruser", "ledgerpass"},
		},
		{
			name:    "wrong forward path",
			seat:    `{"upstream":"https://api.anthropic.com/secret-placeholder","auth_mode":"forward"}`,
			wants:   []string{"forward", "upstream", "https://api.anthropic.com"},
			forbids: []string{"secret-placeholder"},
		},
	})
}

// TestSeatTokenCeilingsAreNotNegative pins both ceilings. A negative one would
// silently disable the output clamp and advertise a nonsense max_input_tokens on
// the model endpoints.
func TestSeatTokenCeilingsAreNotNegative(t *testing.T) {
	t.Parallel()
	runSeatCases(t, []seatCase{
		{
			name:  "negative max_input_tokens",
			seat:  `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","max_input_tokens":-1}`,
			wants: []string{"max_input_tokens"},
		},
		{
			name:  "negative max_output_tokens",
			seat:  `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","max_output_tokens":-1}`,
			wants: []string{"max_output_tokens"},
		},
		{
			name: "zero ceilings accepted",
			seat: `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","max_input_tokens":0,"max_output_tokens":0}`,
		},
		{
			name: "positive ceilings accepted",
			seat: `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","max_input_tokens":262144,"max_output_tokens":32000}`,
		},
	})
}

// TestSeatExtraBodyMustBeAnObject pins the one field merged over the caller's
// own request: anything that is not an object cannot be merged, and would fail
// per-request instead of at admission.
func TestSeatExtraBodyMustBeAnObject(t *testing.T) {
	t.Parallel()
	const base = `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"`
	runSeatCases(t, []seatCase{
		{name: "array rejected", seat: base + `,"extra_body":[1,2]}`, wants: []string{"extra_body"}},
		{name: "string rejected", seat: base + `,"extra_body":"reasoning"}`, wants: []string{"extra_body"}},
		{name: "number rejected", seat: base + `,"extra_body":7}`, wants: []string{"extra_body"}},
		{name: "null rejected", seat: base + `,"extra_body":null}`, wants: []string{"extra_body"}},
		{name: "object accepted", seat: base + `,"extra_body":{"reasoning_effort":"low"}}`},
		{name: "empty object accepted", seat: base + `,"extra_body":{}}`},
		{name: "absent accepted", seat: base + `}`},
	})
}

func TestSeatExtraBodyReservesTopLevelModel(t *testing.T) {
	t.Parallel()
	runSeatCases(t, []seatCase{
		{
			name:  "inject seat with rewrite",
			seat:  `{"upstream":"https://openrouter.ai/api","model":"openai/gpt-6-luna","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","extra_body":{"model":"x"}}`,
			wants: []string{"extra_body", "model"},
		},
		{
			name:  "forward seat",
			seat:  `{"upstream":"https://api.anthropic.com","auth_mode":"forward","extra_body":{"model":"x"}}`,
			wants: []string{"extra_body", "model"},
		},
		{
			name:  "sigv4 seat",
			seat:  `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-west-2","model_map_env_file":"MODEL_MAP_FILE","extra_body":{"model":"x"}}`,
			wants: []string{"extra_body", "model"},
		},
		{
			name: "nested model accepted",
			seat: `{"upstream":"https://openrouter.ai/api","model":"openai/gpt-6-luna","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","extra_body":{"provider":{"model":"x"}}}`,
		},
		{
			name: "case-sensitive Model accepted",
			seat: `{"upstream":"https://openrouter.ai/api","model":"openai/gpt-6-luna","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE","extra_body":{"Model":"x"}}`,
		},
	})
}

// TestSeatExtraBodyRefusesRepeatedMembers holds the one arbitrary document the
// registry carries to the same canonical rule as the document around it.
//
// Its keys are not schema-known — an operator merges whatever the provider
// takes — so nothing here judges a name. What it cannot be is ambiguous: a
// repeated member is two values for one key, and merging keeps whichever the
// decoder happened to land on, at any depth and inside any array the operator
// wrote.
func TestSeatExtraBodyRefusesRepeatedMembers(t *testing.T) {
	t.Parallel()
	const base = `{"upstream":"https://openrouter.ai/api","auth_mode":"inject","billing":"metered","api_key_env_file":"KEY_FILE"`
	runSeatCases(t, []seatCase{
		{
			name:  "repeated member at the root",
			seat:  base + `,"extra_body":{"reasoning_effort":"low","reasoning_effort":"high"}}`,
			wants: []string{"extra_body", "duplicate", "reasoning_effort"},
		},
		{
			name:  "repeated member inside a nested object",
			seat:  base + `,"extra_body":{"reasoning":{"effort":"low","effort":"high"}}}`,
			wants: []string{"extra_body", "duplicate", "effort"},
		},
		{
			name:  "repeated member inside an object in an array",
			seat:  base + `,"extra_body":{"tools":[{"name":"first"},{"name":"second","name":"third"}]}}`,
			wants: []string{"extra_body", "duplicate", "name"},
		},
		{
			name:  "repeated member deep under an array of arrays",
			seat:  base + `,"extra_body":{"matrix":[[{"cell":1,"cell":2}]]}}`,
			wants: []string{"extra_body", "duplicate", "cell"},
		},
		{
			name: "a deeply nested unique document is admitted whole",
			seat: base + `,"extra_body":{"reasoning":{"effort":"low","summary":{"style":"concise"}},` +
				`"tools":[{"name":"first","args":{"depth":1}},{"name":"second"}],` +
				`"flags":[true,false,null],"Effort":"case is a different member","temperature":1}}`,
		},
		{
			name: "repeated equal scalars in an array are not a repeated member",
			seat: base + `,"extra_body":{"stop":["x","x","x"]}}`,
		},
	})
}

// TestSeatBillingDeclarationPairing pins which Seats declare who pays for them.
// An inject Seat spends an operator key that nothing downstream can classify,
// so it must name its class; a forward Seat spends the caller's own credential
// and a sigv4 Seat is metered against the account that signed it, so a
// declaration on either is a second answer to a settled question.
func TestSeatBillingDeclarationPairing(t *testing.T) {
	t.Parallel()
	const injectSeat = `{"upstream":"https://openrouter.ai/api","auth_mode":"inject",` +
		`"api_key_env_file":"KEY_FILE"`
	runSeatCases(t, []seatCase{
		{name: "inject subscription accepted", seat: injectSeat + `,"billing":"subscription"}`},
		{name: "inject metered accepted", seat: injectSeat + `,"billing":"metered"}`},
		{name: "inject self hosted accepted", seat: injectSeat + `,"billing":"self_hosted"}`},
		{
			name:  "inject without a class rejected",
			seat:  injectSeat + `}`,
			wants: []string{"inject", "billing"},
		},
		{
			name:  "forward with a class rejected",
			seat:  `{"upstream":"https://api.anthropic.com","auth_mode":"forward","billing":"subscription"}`,
			wants: []string{"forward", "billing"},
		},
		{
			name: "sigv4 with a class rejected",
			seat: `{"auth_mode":"sigv4","aws_profile":"work","aws_region":"us-east-2",` +
				`"model_map_env_file":"MAP_FILE","billing":"metered"}`,
			wants: []string{"sigv4", "billing"},
		},
	})
}

// TestSeatBillingRejectsValuesOutsideTheVocabulary holds the declaration to the
// closed billing-class vocabulary. A class nothing recognizes would be carried
// onto every accounting row the Seat produces, so it is refused at admission
// rather than stored and puzzled over later.
func TestSeatBillingRejectsValuesOutsideTheVocabulary(t *testing.T) {
	t.Parallel()
	const base = `{"upstream":"https://openrouter.ai/api","auth_mode":"inject",` +
		`"api_key_env_file":"KEY_FILE"`
	runSeatCases(t, []seatCase{
		{
			name:  "unknown class",
			seat:  base + `,"billing":"prepaid"}`,
			wants: []string{"billing", "prepaid"},
		},
		{name: "blank class", seat: base + `,"billing":""}`, wants: []string{"billing"}},
		{name: "wrong JSON type", seat: base + `,"billing":5}`, wants: []string{"billing"}},
		{name: "null", seat: base + `,"billing":null}`, wants: []string{"billing"}},
	})
}
