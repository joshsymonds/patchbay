package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/billing"
	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/server"
	"github.com/joshsymonds/patchbay/internal/testlog"
	"github.com/joshsymonds/patchbay/internal/usage"
)

const testCallerKey = "cli-caller-key"

type output struct {
	code           int
	stdout, stderr string
}

func (o output) contains(t *testing.T, stream, want string) {
	t.Helper()
	got := o.stdout
	if stream == "stderr" {
		got = o.stderr
	}
	if !strings.Contains(got, want) {
		t.Fatalf("%s=%q missing %q", stream, got, want)
	}
}
func invoke(t *testing.T, env map[string]string, args ...string) output {
	t.Helper()
	return invokeWith(t, nil, env, args...)
}

// invokeWith is invoke with an HTTP client of the test's choosing, which is how
// a test decides what reaching the daemon does. A nil client is what production
// passes, so invoke above is this with nothing chosen.
func invokeWith(t *testing.T, client *http.Client, env map[string]string, args ...string) output {
	t.Helper()
	var out, err bytes.Buffer
	code := dispatch(
		t.Context(),
		args,
		cli{
			stdout:    &out,
			stderr:    &err,
			lookupEnv: envMap(env),
			client:    client,
			logger:    quietLogger(),
		},
	)
	return output{code, out.String(), err.String()}
}

func callerKeyFile(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "caller.key")
	writeFile(t, path, testCallerKey)
	t.Setenv(proxy.CallerKeyEnvFile, path)
}

type fakeDaemon struct {
	server *httptest.Server
	addr   string
}

func newFakeDaemon(t *testing.T, doc server.Document) *fakeDaemon {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != server.RegistryEndpoint {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get(proxy.CallerKeyHeader) != testCallerKey {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return &fakeDaemon{srv, strings.TrimPrefix(srv.URL, "http://")}
}
func (d *fakeDaemon) env() map[string]string { return map[string]string{envListen: d.addr} }

func sampleDocument() server.Document {
	def := "personal"
	return server.Document{
		DefaultContext: &def,
		RegistryValid:  true,
		RegistryPath:   "/config/patchbay/routes.json",
		Listen:         "127.0.0.1:4100",
		Seats: map[string]server.SeatSummary{
			"anthropic": {AuthMode: "forward", Upstream: "https://api.anthropic.com"},
			"luna": {
				AuthMode:       "inject",
				Upstream:       "https://openrouter.ai/api",
				Model:          "openai/gpt-5.6-luna",
				APIKeyEnvFile:  "OPENROUTER_API_KEY_FILE",
				MaxInputTokens: 262144,
			},
			"worker": {
				AuthMode:      "inject",
				Upstream:      "https://openrouter.ai/api",
				Model:         "openrouter/worker",
				APIKeyEnvFile: "OPENROUTER_API_KEY_FILE",
			},
		},
		Contexts: map[string]server.ContextSummary{
			"personal": {
				DefaultSeat: "anthropic",
				Models:      map[string]string{"luna-fast": "luna"},
				Subagents: &server.SubagentsSummary{
					DefaultSeat: "worker",
					Models:      map[string]string{"requested": "worker"},
				},
			},
			"work": {DefaultSeat: "luna", Models: map[string]string{}},
		},
	}
}

// opaqueValues are model selectors and Seat rewrite targets that are perfectly
// legal and hostile to a plain-text table: the token absence used to be spelled
// with, the word JSON spells absence with, and three values carrying the two
// bytes a table is built out of.
func opaqueValues() []string {
	return []string{"-", "null", "with\ttab", "with\nnewline", "   "}
}

// lines splits captured output into its lines, dropping the trailing empty one.
func lines(out string) []string {
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// tableRows normalizes a rendered table into its cells, discarding the padding
// tabwriter aligned the columns with.
//
// Splitting on whitespace recovers the cells exactly here because no value in
// these fixtures carries a space: every cell is a bare header word, a decimal,
// the unquoted null, or a quoted Go literal of a spaceless string. So the only
// whitespace left in the output is the padding, and what comes back is the
// matrix the command meant to print.
func tableRows(out string) [][]string {
	rows := make([][]string, 0, len(lines(out)))
	for _, line := range lines(out) {
		rows = append(rows, strings.Fields(line))
	}
	return rows
}

// tableDocument is sampleDocument carrying the two things a whole-table
// assertion needs and the shared fixture does not: a SigV4 Seat, so every
// credential source the Seat table can print is present at once, and a second
// public binding, so sorted order and selector-to-Seat pairing are two claims
// the binding table has to satisfy separately rather than one row that happens
// to satisfy both. The second binding is written before the first it sorts
// after, and bound to a different Seat than the one it sorts beside.
func tableDocument() server.Document {
	doc := sampleDocument()
	doc.Seats["bedrock"] = server.SeatSummary{
		AuthMode:       "sigv4",
		Upstream:       "https://bedrock-runtime.us-east-1.amazonaws.com",
		Model:          "anthropic.claude-sonnet-4-5-20250929-v1:0",
		AWSProfile:     "patchbay",
		AWSRegion:      "us-east-1",
		MaxInputTokens: 400000,
	}
	doc.Contexts["personal"] = server.ContextSummary{
		DefaultSeat: "anthropic",
		Models: map[string]string{
			"luna-fast":  "luna",
			"alpha-fast": "worker",
		},
		Subagents: &server.SubagentsSummary{
			DefaultSeat: "worker",
			Models:      map[string]string{"requested": "worker"},
		},
	}
	return doc
}

// opaqueDocument is a gateway document whose personal Context binds every
// opaque value above to a Seat that rewrites to the same bytes.
func opaqueDocument() server.Document {
	doc := sampleDocument()
	models := map[string]string{}
	for index, value := range opaqueValues() {
		id := fmt.Sprintf("opaque-%d", index)
		doc.Seats[id] = server.SeatSummary{
			AuthMode:      "inject",
			Upstream:      "https://openrouter.ai/api",
			Model:         value,
			APIKeyEnvFile: "OPENROUTER_API_KEY_FILE",
		}
		models[value] = id
	}
	doc.Contexts["personal"] = server.ContextSummary{DefaultSeat: "anthropic", Models: models}
	return doc
}

// TestTablesQuoteOpaqueValuesOntoOneLine pins the table half of the CLI's string
// convention.
//
// A model selector is an opaque exact string: whatever bytes an operator wrote,
// tabs and newlines included. Interpolated raw into a tab-separated table those
// bytes ARE the table — a newline splits one binding across two rows, a tab
// invents a column — so every string cell is written as a quoted Go literal,
// which has no way to spell either.
func TestTablesQuoteOpaqueValuesOntoOneLine(t *testing.T) {
	callerKeyFile(t)
	daemon := newFakeDaemon(t, opaqueDocument())

	bindings := invoke(t, daemon.env(), "bindings", "--ctx", "personal")
	if bindings.code != exitOK {
		t.Fatalf("%+v", bindings)
	}

	// One header and one row per binding, however the selectors are spelled.
	if got := len(lines(bindings.stdout)); got != len(opaqueValues())+1 {
		t.Errorf("bindings printed %d lines; want %d\n%s",
			got, len(opaqueValues())+1, bindings.stdout)
	}
	for _, value := range opaqueValues() {
		bindings.contains(t, "stdout", strconv.Quote(value))
	}
	for _, raw := range []string{"with\ttab", "with\nnewline"} {
		if strings.Contains(bindings.stdout, raw) {
			t.Errorf("bindings emitted %q raw, so its bytes are the table's: %q", raw, bindings.stdout)
		}
	}

	seats := invoke(t, daemon.env(), "seats")
	if seats.code != exitOK {
		t.Fatalf("%+v", seats)
	}
	if got, want := len(lines(seats.stdout)), len(opaqueDocument().Seats)+1; got != want {
		t.Errorf("seats printed %d lines; want %d\n%s", got, want, seats.stdout)
	}
	for _, value := range opaqueValues() {
		seats.contains(t, "stdout", strconv.Quote(value))
	}

	// A Seat that rewrites nothing has no model, which is unquoted null — the one
	// rendering no quoted string can collide with. So is a ceiling of zero.
	seats.contains(t, "stdout", `"anthropic"`)
	if !strings.Contains(seats.stdout, "null") {
		t.Errorf("seats printed no null for the Seat that rewrites nothing: %q", seats.stdout)
	}
}

// TestResolveSeparatesNullFromQuotedStrings pins the line half of the same
// convention. `-` used to mean "no value here", which made a Seat rewriting to a
// model an operator named `-` indistinguishable from one rewriting to nothing.
func TestResolveSeparatesNullFromQuotedStrings(t *testing.T) {
	callerKeyFile(t)

	doc := sampleDocument()
	doc.Seats["dashed"] = server.SeatSummary{
		AuthMode:      "inject",
		Upstream:      "https://openrouter.ai/api",
		Model:         "-",
		APIKeyEnvFile: "OPENROUTER_API_KEY_FILE",
	}
	doc.Contexts["personal"] = server.ContextSummary{
		DefaultSeat: "anthropic",
		Models:      map[string]string{"-": "dashed"},
	}
	daemon := newFakeDaemon(t, doc)

	dashed := invoke(t, daemon.env(), "resolve", "personal", "-")
	if dashed.code != exitOK {
		t.Fatalf("%+v", dashed)
	}
	for _, want := range []string{
		`context: "personal"`,
		`request_model: "-"`,
		`selection: "model"`,
		`selection_model: "-"`,
		`seat: "dashed"`,
		`auth_mode: "inject"`,
		`upstream: "https://openrouter.ai/api"`,
		`model: "-"`,
		`config_source: "OPENROUTER_API_KEY_FILE"`,
		`aws_profile: null`,
		`aws_region: null`,
		`model_map_env_file: null`,
		`max_input_tokens: null`,
		"insecure: false",
	} {
		dashed.contains(t, "stdout", want)
	}

	// The ordinary Context default names no model at all, which is the null the
	// exact selector above must never be confused with.
	fallback := invoke(t, daemon.env(), "resolve", "personal", "unbound")
	if fallback.code != exitOK {
		t.Fatalf("%+v", fallback)
	}
	fallback.contains(t, "stdout", "selection_model: null")
	fallback.contains(t, "stdout", `selection: "default"`)
	if strings.Contains(fallback.stdout, `selection_model: "`) {
		t.Errorf("an absent selection model was quoted like a real one: %q", fallback.stdout)
	}
}

// TestResolveKeepsOpaqueModelsOnOneLine is the same claim for the line-oriented
// output: a selector carrying a newline must not become two fields.
func TestResolveKeepsOpaqueModelsOnOneLine(t *testing.T) {
	callerKeyFile(t)
	daemon := newFakeDaemon(t, opaqueDocument())

	for _, value := range opaqueValues() {
		got := invoke(t, daemon.env(), "resolve", "personal", value)
		if got.code != exitOK {
			t.Fatalf("resolve %q: %+v", value, got)
		}
		for _, key := range []string{"request_model", "selection_model", "model"} {
			got.contains(t, "stdout", key+": "+strconv.Quote(value))
		}
		if strings.Contains(got.stdout, "\n\n") {
			t.Errorf("resolve %q emitted a blank line, so a value ended one: %q", value, got.stdout)
		}
		// One line per field, whatever bytes the selector carries.
		if want := 20; len(lines(got.stdout)) != want {
			t.Errorf("resolve %q printed %d lines; want %d\n%s",
				value, len(lines(got.stdout)), want, got.stdout)
		}
	}
}

func TestCLIHelpUsesSeatsBindingsAndSubagentResolve(t *testing.T) {
	got := invoke(t, nil, "help")
	if got.code != exitOK {
		t.Fatal(got.code)
	}
	for _, want := range []string{"seats", "bindings --ctx <name>", "resolve [--subagent]"} {
		got.contains(t, "stdout", want)
	}
	if strings.Contains(got.stdout, "patchbay routes") ||
		strings.Contains(got.stdout, "routes [--ctx") {
		t.Fatalf("legacy routes help: %s", got.stdout)
	}
}

// TestContextsTableCountsBindingsAndSubagentBindings pins the successful
// listing whole: the columns it has, the order the rows come in, and what each
// count means. Contexts sort by ID, BINDINGS counts public `models` entries,
// and SUBAGENT_BINDINGS counts exact marked bindings plus the marked default —
// zero for a Context carrying no marked policy at all.
//
// Every cell here is a registry ID or a count, so the whole table is compared
// as parsed fields rather than searched for substrings: a row out of order, a
// miscount, a dropped column, or an extra row all fail this.
func TestContextsTableCountsBindingsAndSubagentBindings(t *testing.T) {
	callerKeyFile(t)
	daemon := newFakeDaemon(t, sampleDocument())

	got := invoke(t, daemon.env(), "contexts")
	if got.code != exitOK {
		t.Fatalf("%+v", got)
	}

	want := [][]string{
		{"CONTEXT", "DEFAULT_SEAT", "BINDINGS", "SUBAGENT_BINDINGS"},
		{"personal", "anthropic", "1", "2"},
		{"work", "luna", "0", "0"},
	}
	if rows := tableRows(got.stdout); !reflect.DeepEqual(rows, want) {
		t.Fatalf("contexts printed %q; want %q\n%s", rows, want, got.stdout)
	}
}

// TestSeatsAndBindingsTables pins both successful listings whole: the columns
// each one has, the order its rows come in, and which cells belong to which
// row.
//
// Every field in a Seat row is only meaningful beside the Seat it describes —
// an upstream, a model rewrite and a credential source read off three different
// rows describe a deployment nobody configured — so each table is compared as a
// matrix rather than searched for the strings it ought to contain. A renamed
// column, a row out of order, a field paired with the wrong Seat, an extra row
// and a missing one all fail this; none of them fails a substring search.
//
// The Seat table's own vocabulary is pinned with it. An absent model rewrite
// and a ceiling of zero are both the unquoted null no quoted value can collide
// with, a real ceiling is the decimal itself, and every credential source a
// Seat can have is a quoted literal naming it: the caller's own key, the
// environment variable holding a provider key, or the profile SigV4 signs with.
func TestSeatsAndBindingsTables(t *testing.T) {
	callerKeyFile(t)
	daemon := newFakeDaemon(t, tableDocument())

	seats := invoke(t, daemon.env(), "seats")
	if seats.code != exitOK {
		t.Fatalf("%+v", seats)
	}
	wantSeats := [][]string{
		{"SEAT", "AUTH_MODE", "UPSTREAM", "MODEL", "MAX_INPUT_TOKENS", "CONFIG_SOURCE"},
		{`"anthropic"`, `"forward"`, `"https://api.anthropic.com"`, "null", "null", `"caller"`},
		{
			`"bedrock"`, `"sigv4"`, `"https://bedrock-runtime.us-east-1.amazonaws.com"`,
			`"anthropic.claude-sonnet-4-5-20250929-v1:0"`, "400000", `"aws_profile:patchbay"`,
		},
		{
			`"luna"`, `"inject"`, `"https://openrouter.ai/api"`,
			`"openai/gpt-5.6-luna"`, "262144", `"OPENROUTER_API_KEY_FILE"`,
		},
		{
			`"worker"`, `"inject"`, `"https://openrouter.ai/api"`,
			`"openrouter/worker"`, "null", `"OPENROUTER_API_KEY_FILE"`,
		},
	}
	if rows := tableRows(seats.stdout); !reflect.DeepEqual(rows, wantSeats) {
		t.Errorf("seats printed %q; want %q\n%s", rows, wantSeats, seats.stdout)
	}

	bindings := invoke(t, daemon.env(), "bindings", "--ctx", "personal")
	if bindings.code != exitOK {
		t.Fatalf("%+v", bindings)
	}
	wantBindings := [][]string{
		{"MODEL", "SEAT"},
		{`"alpha-fast"`, `"worker"`},
		{`"luna-fast"`, `"luna"`},
	}
	if rows := tableRows(bindings.stdout); !reflect.DeepEqual(rows, wantBindings) {
		t.Errorf("bindings printed %q; want %q\n%s", rows, wantBindings, bindings.stdout)
	}

	// The two rows above are the whole table, and the marked-subagent policy the
	// Context also carries is not one of them. Called out on its own because the
	// fixture does carry one: leaving it out is a decision the command makes,
	// not an absence the document arranged.
	if strings.Contains(bindings.stdout, "requested") {
		t.Errorf("subagent policy in public bindings: %s", bindings.stdout)
	}

	if got := invoke(t, daemon.env(), "bindings"); got.code != exitUsage {
		t.Fatalf("bindings without --ctx code=%d", got.code)
	}
}

func TestResolvePrintsObservationClass(t *testing.T) {
	callerKeyFile(t)
	daemon := newFakeDaemon(t, sampleDocument())
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "ordinary",
			args: []string{"resolve", "personal", "luna-fast"},
			want: "context: \"personal\"\n" +
				"request_model: \"luna-fast\"\n" +
				"observation: \"none\"\n" +
				"selection: \"model\"\n" +
				"selection_model: \"luna-fast\"\n" +
				"seat: \"luna\"\n" +
				"auth_mode: \"inject\"\n" +
				"upstream: \"https://openrouter.ai/api\"\n" +
				"model: \"openai/gpt-5.6-luna\"\n" +
				"speed: null\n" +
				"config_source: \"OPENROUTER_API_KEY_FILE\"\n" +
				"api_key_env_file: \"OPENROUTER_API_KEY_FILE\"\n" +
				"aws_profile: null\n" +
				"aws_region: null\n" +
				"model_map_env_file: null\n" +
				"max_input_tokens: 262144\n" +
				"max_output_tokens: null\n" +
				"insecure: false\n" +
				"strip_cache_control: false\n" +
				"capture_bodies: false\n",
		},
		{
			name: "subagent",
			args: []string{"resolve", "--subagent", "personal", "requested"},
			want: "context: \"personal\"\n" +
				"request_model: \"requested\"\n" +
				"observation: \"subagent\"\n" +
				"selection: \"subagent\"\n" +
				"selection_model: \"requested\"\n" +
				"seat: \"worker\"\n" +
				"auth_mode: \"inject\"\n" +
				"upstream: \"https://openrouter.ai/api\"\n" +
				"model: \"openrouter/worker\"\n" +
				"speed: null\n" +
				"config_source: \"OPENROUTER_API_KEY_FILE\"\n" +
				"api_key_env_file: \"OPENROUTER_API_KEY_FILE\"\n" +
				"aws_profile: null\n" +
				"aws_region: null\n" +
				"model_map_env_file: null\n" +
				"max_input_tokens: null\n" +
				"max_output_tokens: null\n" +
				"insecure: false\n" +
				"strip_cache_control: false\n" +
				"capture_bodies: false\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := invoke(t, daemon.env(), tc.args...)
			if got.code != exitOK {
				t.Fatalf("%+v", got)
			}
			if got.stdout != tc.want {
				t.Fatalf("stdout:\n%s\nwant:\n%s", got.stdout, tc.want)
			}
		})
	}
}

func TestResolveUsesSharedPrecedenceGuardAndSyntheticSubagent(t *testing.T) {
	callerKeyFile(t)
	doc := sampleDocument()
	daemon := newFakeDaemon(t, doc)
	ordinary := invoke(t, daemon.env(), "resolve", "personal", "luna-fast")
	if ordinary.code != exitOK {
		t.Fatalf("%+v", ordinary)
	}
	for _, want := range []string{`selection: "model"`, `selection_model: "luna-fast"`, `seat: "luna"`, `auth_mode: "inject"`, `config_source: "OPENROUTER_API_KEY_FILE"`} {
		ordinary.contains(t, "stdout", want)
	}
	sub := invoke(t, daemon.env(), "resolve", "--subagent", "personal", "requested")
	if sub.code != exitOK {
		t.Fatalf("%+v", sub)
	}
	sub.contains(t, "stdout", `selection: "subagent"`)
	sub.contains(t, "stdout", `seat: "worker"`)

	guardDoc := sampleDocument()
	guardDoc.Seats["luna"] = server.SeatSummary{
		AuthMode:      "inject",
		Upstream:      "https://openrouter.ai/api",
		Model:         "target",
		APIKeyEnvFile: "KEY",
	}
	guardDoc.Contexts["personal"] = server.ContextSummary{
		DefaultSeat: "anthropic",
		Models:      map[string]string{"a": "luna"},
	}
	guard := invoke(t, newFakeDaemon(t, guardDoc).env(), "resolve", "personal", "target")
	if guard.code != exitFailure {
		t.Fatalf("guard code=%d out=%s", guard.code, guard.stdout)
	}
	guard.contains(t, "stderr", "request the selector")
}

func TestProjectUsesIntrospectedDefaultContext(t *testing.T) {
	callerKeyFile(t)
	doc := sampleDocument()
	def := "work"
	doc.DefaultContext = &def
	daemon := newFakeDaemon(t, doc)
	env := daemon.env()
	env[envHome] = t.TempDir()
	got := invoke(t, env, "project", t.TempDir())
	if got.code != exitOK {
		t.Fatalf("%+v", got)
	}
	got.contains(t, "stdout", `context: "work"`)
}

// writeSettings writes a Claude Code settings file naming base as the gateway
// Claude Code should call, and returns the directory holding it.
func writeSettings(t *testing.T, dir, name, base string) {
	t.Helper()
	claude := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(claude, name), `{"env":{"ANTHROPIC_BASE_URL":"`+base+`"}}`)
}

// writeSettingsValue writes a settings file whose base URL is base exactly,
// encoded rather than pasted, so a value carrying the bytes JSON has to escape
// still reaches the command as the bytes it names.
func writeSettingsValue(t *testing.T, dir, name, base string) {
	t.Helper()
	claude := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(settingsFile{Env: map[string]string{baseURLKey: base}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(claude, name), string(encoded))
}

// TestProjectRefusesBaseURLsThatAreNotAbsoluteHTTP pins the base-URL contract.
// ANTHROPIC_BASE_URL is what Claude Code will actually dial, so a value that is
// not an absolute http(s) URL with a host does not select a Context at all — and
// reporting one anyway would tell an operator their project is wired up when it
// is not.
//
// The settings chain still prints: which files were read and what they said is
// the diagnosis. What must not print is anything downstream of a selection that
// never happened — a context line, or the bindings of whichever Context the
// gateway happens to call default.
func TestProjectRefusesBaseURLsThatAreNotAbsoluteHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, base string
	}{
		{name: "relative path", base: "/ctx/work"},
		{name: "scheme relative", base: "//127.0.0.1:4100/ctx/work"},
		{name: "bare host", base: "127.0.0.1:4100"},
		{name: "hostless", base: "http:///ctx/work"},
		{name: "unsupported scheme", base: "ftp://127.0.0.1:4100/ctx/work"},
		{name: "unparsable host", base: "http://[::1"},
		{name: "unparsable control byte", base: "http://127.0.0.1:4100/ctx/\x7f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callerKeyFile(t)
			daemon := newFakeDaemon(t, sampleDocument())
			dir := t.TempDir()
			writeSettings(t, dir, settingsName, tc.base)
			env := daemon.env()
			env[envHome] = t.TempDir()

			got := invoke(t, env, "project", dir)
			if got.code != exitFailure {
				t.Fatalf("code=%d stdout=%q stderr=%q", got.code, got.stdout, got.stderr)
			}
			got.contains(t, "stderr", baseURLKey)

			// The diagnosis itself is still owed to the operator.
			got.contains(t, "stdout", "settings ")
			got.contains(t, "stdout", "base_url: ")

			// Nothing downstream of a selection that never happened. "MODEL" and
			// "SEAT" are the binding table's own header, so neither the table nor
			// the Seat IDs under it can be printed without tripping this.
			for _, forbidden := range []string{"context:", "MODEL", "SEAT"} {
				if strings.Contains(got.stdout, forbidden) {
					t.Fatalf(
						"stdout described a Context this base URL never selects (%q): %q",
						forbidden,
						got.stdout,
					)
				}
			}
		})
	}
}

// TestProjectChainCannotBeForgedBySettingsContent pins the last place untrusted
// text reaches stdout. A settings file belongs to whoever checked the project
// out, and the base URL it names is printed back verbatim — so a value carrying
// a newline could write whole lines of its own into a diagnosis an operator
// reads as patchbay's.
//
// Every settings path and every base URL is a quoted Go literal, which has no
// spelling for a newline that is a newline. The fixed notes are patchbay's own
// vocabulary and stay bare, so a reader can still tell "not present" from a file
// whose base URL happens to say that.
func TestProjectChainCannotBeForgedBySettingsContent(t *testing.T) {
	callerKeyFile(t)
	daemon := newFakeDaemon(t, sampleDocument())
	dir := t.TempDir()

	const forged = "http://127.0.0.1:4100/ctx/work\ncontext: forged\nMODEL\tSEAT\nevil\tevil"
	writeSettingsValue(t, dir, settingsName, forged)
	env := daemon.env()
	env[envHome] = t.TempDir()

	got := invoke(t, env, "project", dir)
	if got.code != exitFailure {
		t.Fatalf("code=%d stdout=%q stderr=%q", got.code, got.stdout, got.stderr)
	}

	// Three chain lines and one base_url line, whatever the file said.
	if want := 4; len(lines(got.stdout)) != want {
		t.Fatalf("project printed %d stdout lines; want %d\n%s",
			len(lines(got.stdout)), want, got.stdout)
	}
	if strings.Contains(got.stdout, "\ncontext: forged") ||
		strings.Contains(got.stdout, "\nMODEL") {
		t.Fatalf("settings content wrote its own lines into the diagnosis: %q", got.stdout)
	}
	got.contains(t, "stdout", strconv.Quote(forged))

	// The paths consulted are printed as quoted literals too, and the fixed notes
	// are not — they are patchbay's words, not the file's.
	local := filepath.Join(dir, claudeDir, settingsLocalName)
	got.contains(t, "stdout", "settings "+strconv.Quote(local)+": not present")
}

// TestProjectReportsNoBaseURLAsNull keeps the absent case in the same
// convention as every other absent string the CLI prints, so the field never
// carries a sentence a base URL could have been.
func TestProjectReportsNoBaseURLAsNull(t *testing.T) {
	callerKeyFile(t)
	doc := sampleDocument()
	def := "work"
	doc.DefaultContext = &def
	daemon := newFakeDaemon(t, doc)
	env := daemon.env()
	env[envHome] = t.TempDir()

	got := invoke(t, env, "project", t.TempDir())
	if got.code != exitOK {
		t.Fatalf("%+v", got)
	}
	got.contains(t, "stdout", "base_url: null")
	got.contains(t, "stderr", baseURLKey)
	if strings.Contains(got.stdout, `base_url: "`) {
		t.Errorf("an absent base URL was quoted like a real one: %q", got.stdout)
	}
}

// TestProjectReportsForeignHostsWithoutFailing keeps the one base URL that is
// merely unusual from being confused with the ones that are wrong: pointing
// Claude Code at some other gateway is a thing an operator may perfectly well
// have meant.
func TestProjectReportsForeignHostsWithoutFailing(t *testing.T) {
	callerKeyFile(t)
	daemon := newFakeDaemon(t, sampleDocument())
	dir := t.TempDir()
	writeSettings(t, dir, settingsName, "http://elsewhere.example:4100/ctx/work")
	env := daemon.env()
	env[envHome] = t.TempDir()

	got := invoke(t, env, "project", dir)
	if got.code != exitOK {
		t.Fatalf("code=%d stderr=%q", got.code, got.stderr)
	}
	got.contains(t, "stdout", `context: "work"`)
	got.contains(t, "stderr", "elsewhere.example:4100")
}

// TestProjectSettingsPrecedenceAndDiagnostics covers the rest of the chain: the
// most local file that sets a base URL wins, a broken file is reported and
// skipped rather than ending the command, and a base URL selecting a Context
// this gateway does not serve fails rather than printing someone else's
// bindings.
func TestProjectSettingsPrecedenceAndDiagnostics(t *testing.T) {
	t.Run("local overrides project", func(t *testing.T) {
		callerKeyFile(t)
		daemon := newFakeDaemon(t, sampleDocument())
		dir := t.TempDir()
		writeSettings(t, dir, settingsName, "http://"+daemon.addr+"/ctx/personal")
		writeSettings(t, dir, settingsLocalName, "http://"+daemon.addr+"/ctx/work")
		env := daemon.env()
		env[envHome] = t.TempDir()

		got := invoke(t, env, "project", dir)
		if got.code != exitOK {
			t.Fatalf("code=%d stderr=%q", got.code, got.stderr)
		}
		got.contains(t, "stdout", `context: "work"`)
	})

	t.Run("project overrides home", func(t *testing.T) {
		callerKeyFile(t)
		daemon := newFakeDaemon(t, sampleDocument())
		dir := t.TempDir()
		home := t.TempDir()
		writeSettings(t, home, settingsName, "http://"+daemon.addr+"/ctx/personal")
		writeSettings(t, dir, settingsName, "http://"+daemon.addr+"/ctx/work")
		env := daemon.env()
		env[envHome] = home

		got := invoke(t, env, "project", dir)
		if got.code != exitOK {
			t.Fatalf("code=%d stderr=%q", got.code, got.stderr)
		}
		got.contains(t, "stdout", `context: "work"`)
	})

	t.Run("malformed settings are skipped", func(t *testing.T) {
		callerKeyFile(t)
		daemon := newFakeDaemon(t, sampleDocument())
		dir := t.TempDir()
		claude := filepath.Join(dir, ".claude")
		if err := os.MkdirAll(claude, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(claude, settingsLocalName), `{"env":`)
		writeSettings(t, dir, settingsName, "http://"+daemon.addr+"/ctx/work")
		env := daemon.env()
		env[envHome] = t.TempDir()

		got := invoke(t, env, "project", dir)
		if got.code != exitOK {
			t.Fatalf("code=%d stderr=%q", got.code, got.stderr)
		}
		got.contains(t, "stdout", "malformed JSON, skipped")
		got.contains(t, "stdout", `context: "work"`)
	})

	// The base URLs that name a context with no name at all. An escaped
	// separator is the one the CLI could read differently from the gateway — the
	// decoded path says "work", the escaped path the gateway actually routes on
	// says a single segment nothing defines — and a doubled slash leaves the
	// prefix naming nothing. An escaped separator at the very end of the base URL
	// is the same thing wearing a trailing slash's clothes: it is not one, so
	// nothing may trim it.
	//
	// All of them come off the shared splitter as the empty context, which no
	// registry can produce and the gateway refuses. So does this: it is a base URL
	// that selects nothing, reported like every other one, with no context line
	// and no bindings under it.
	for _, tc := range []struct {
		name, path string
	}{
		{name: "encoded separator", path: "/ctx/work%2Feu"},
		{name: "encoded trailing separator", path: "/ctx/work%2F"},
		{name: "lowercase encoded trailing separator", path: "/ctx/work%2f"},
		{name: "prefix naming nothing", path: "/ctx//"},
	} {
		t.Run(tc.name+" fails closed", func(t *testing.T) {
			callerKeyFile(t)
			daemon := newFakeDaemon(t, sampleDocument())
			dir := t.TempDir()
			writeSettings(t, dir, settingsName, "http://"+daemon.addr+tc.path)
			env := daemon.env()
			env[envHome] = t.TempDir()

			got := invoke(t, env, "project", dir)
			if got.code != exitFailure {
				t.Fatalf("code=%d stdout=%q stderr=%q", got.code, got.stdout, got.stderr)
			}

			// The operator is told which setting is at fault, and still sees the
			// chain that produced it.
			got.contains(t, "stderr", baseURLKey)
			got.contains(t, "stdout", "base_url: ")

			// Nothing that would describe a Context — not the decoded name this
			// URL never reaches, not an empty one, and not the default's bindings.
			for _, forbidden := range []string{"context:", "MODEL", "SEAT"} {
				if strings.Contains(got.stdout, forbidden) {
					t.Fatalf(
						"stdout described a Context this base URL never selects (%q): %q",
						forbidden,
						got.stdout,
					)
				}
			}
		})
	}

	t.Run("unknown context fails", func(t *testing.T) {
		callerKeyFile(t)
		daemon := newFakeDaemon(t, sampleDocument())
		dir := t.TempDir()
		writeSettings(t, dir, settingsName, "http://"+daemon.addr+"/ctx/nowhere")
		env := daemon.env()
		env[envHome] = t.TempDir()

		got := invoke(t, env, "project", dir)
		if got.code != exitFailure {
			t.Fatalf("code=%d stdout=%q", got.code, got.stdout)
		}
		got.contains(t, "stdout", `context: "nowhere"`)
		got.contains(t, "stderr", "nowhere")
	})
}

// TestClaudeTargetKeepsBothHalvesOfAnEscapedPath pins the URL the diagnosis is
// made on. The gateway routes on a path's escaped form and strips a context off
// both halves of it, so the two halves of the URL the CLI builds have to agree:
// RawPath must stay an encoding of Path, or EscapedPath silently discards it and
// the CLI reads a context the request never reaches.
//
// The escaped separator at the end is the case that makes trimming the two
// halves separately wrong. "%2F" is a path byte, not the separator a trailing
// slash is, so nothing may trim it — and the target it produces asks for a
// segment no registry can name, which is exactly what the gateway refuses.
func TestClaudeTargetKeepsBothHalvesOfAnEscapedPath(t *testing.T) {
	for _, tc := range []struct {
		name, base, wantPath, wantEscaped string
	}{
		{
			name:        "ordinary trailing slash",
			base:        "http://127.0.0.1:4100/ctx/work/",
			wantPath:    "/ctx/work/v1/messages",
			wantEscaped: "/ctx/work/v1/messages",
		},
		{
			name:        "no trailing slash",
			base:        "http://127.0.0.1:4100/ctx/work",
			wantPath:    "/ctx/work/v1/messages",
			wantEscaped: "/ctx/work/v1/messages",
		},
		{
			name:        "bare root",
			base:        "http://127.0.0.1:4100",
			wantPath:    "/v1/messages",
			wantEscaped: "/v1/messages",
		},
		{
			name:        "embedded escaped separator",
			base:        "http://127.0.0.1:4100/ctx/work%2Feu",
			wantPath:    "/ctx/work/eu/v1/messages",
			wantEscaped: "/ctx/work%2Feu/v1/messages",
		},
		{
			name:        "trailing escaped separator",
			base:        "http://127.0.0.1:4100/ctx/work%2F",
			wantPath:    "/ctx/work//v1/messages",
			wantEscaped: "/ctx/work%2F/v1/messages",
		},
		{
			name:        "lowercase trailing escaped separator",
			base:        "http://127.0.0.1:4100/ctx/work%2f",
			wantPath:    "/ctx/work//v1/messages",
			wantEscaped: "/ctx/work%2f/v1/messages",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, parseErr := url.Parse(tc.base)
			if parseErr != nil {
				t.Fatalf("parse %q: %v", tc.base, parseErr)
			}

			target, err := claudeTarget(base)
			if err != nil {
				t.Fatalf("claudeTarget(%q) = %v", tc.base, err)
			}

			if target.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", target.Path, tc.wantPath)
			}
			if got := target.EscapedPath(); got != tc.wantEscaped {
				t.Errorf("EscapedPath() = %q, want %q", got, tc.wantEscaped)
			}

			// RawPath is a hint EscapedPath honors only while it encodes Path. One
			// that says anything else is the hint being thrown away.
			if target.RawPath != "" && target.RawPath != tc.wantEscaped {
				t.Errorf("RawPath = %q, want %q or none", target.RawPath, tc.wantEscaped)
			}
			decoded, unescapeErr := url.PathUnescape(target.EscapedPath())
			if unescapeErr != nil {
				t.Fatalf("PathUnescape(%q) = %v", target.EscapedPath(), unescapeErr)
			}
			if decoded != target.Path {
				t.Errorf("escaped path decodes to %q, but Path is %q", decoded, target.Path)
			}
		})
	}
}

// TestClaudeTargetPreservesEverythingButThePath keeps the rest of the base URL
// intact: only the path gains the endpoint.
func TestClaudeTargetPreservesEverythingButThePath(t *testing.T) {
	base, parseErr := url.Parse("https://gateway.example:4100/ctx/work%2F?beta=true#anchor")
	if parseErr != nil {
		t.Fatal(parseErr)
	}

	target, err := claudeTarget(base)
	if err != nil {
		t.Fatalf("claudeTarget() = %v", err)
	}

	if target.Scheme != "https" || target.Host != "gateway.example:4100" ||
		target.RawQuery != "beta=true" || target.Fragment != "anchor" {
		t.Fatalf("target = %+v; want the base URL's scheme, host, query and fragment", target)
	}
}

func TestCheckReportsSeatsContextsAndBindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	writeFile(t, path, testRegistry)
	got := invoke(t, nil, "check", "--registry", path)
	if got.code != exitOK {
		t.Fatalf("%+v", got)
	}
	for _, want := range []string{"2 seats", "2 contexts", "1 binding"} {
		got.contains(t, "stdout", want)
	}
	writeFile(t, path, `{"routes":{}}`)
	if got := invoke(t, nil, "check", "--registry", path); got.code != exitFailure {
		t.Fatalf("legacy registry code=%d", got.code)
	}
}

func TestSubcommandsRequireRunningValidIntrospection(t *testing.T) {
	callerKeyFile(t)
	doc := sampleDocument()
	doc.RegistryValid = false
	doc.DefaultContext = nil
	doc.Seats = map[string]server.SeatSummary{}
	doc.Contexts = map[string]server.ContextSummary{}
	doc.RegistryError = "broken"
	got := invoke(t, newFakeDaemon(t, doc).env(), "seats")
	if got.code != exitFailure {
		t.Fatalf("code=%d", got.code)
	}
	got.contains(t, "stderr", "broken")
}

// gatewayBacked names every subcommand that asks the running gateway, so a
// failure reaching it is asserted once for all of them rather than for whichever
// one a test happened to pick.
func gatewayBacked() [][]string {
	return [][]string{
		{"contexts"},
		{"seats"},
		{"bindings", "--ctx", "personal"},
		{"resolve", "personal", "luna-fast"},
		{"project"},
	}
}

// errNoDaemon is what an unreachable gateway answers with here. Naming the
// failure rather than borrowing one from the network is what makes "nothing is
// listening" a fact of this test instead of a fact about the machine running it.
var errNoDaemon = errors.New("no daemon answered")

// unreachableTransport refuses every request without dialing, and records the
// address each one named. The recording is the proof: the failure an operator
// reads came from here, and the address in it was never connected to.
type unreachableTransport struct{ targets []string }

func (u *unreachableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u.targets = append(u.targets, req.URL.Host)

	return nil, errNoDaemon
}

// TestSubcommandsReportAnUnreachableDaemon pins the first thing an operator
// needs to know when nothing answers: which address was tried.
//
// The refusal is injected rather than dialed. A test that reaches for a port it
// expects nothing to hold asserts something about the machine it runs on, not
// about patchbay — the port may be taken, and the failure it produces is
// whatever the kernel felt like. Here the configured address stays what it is in
// the diagnostic, a label, and the transport below guarantees nothing ever
// opens a socket to it.
func TestSubcommandsReportAnUnreachableDaemon(t *testing.T) {
	callerKeyFile(t)
	const addr = "127.0.0.1:1"
	env := map[string]string{envListen: addr, envHome: t.TempDir()}
	for _, args := range gatewayBacked() {
		transport := &unreachableTransport{}
		got := invokeWith(t, &http.Client{Transport: transport}, env, args...)
		if got.code != exitFailure {
			t.Errorf("%v code = %d; want %d", args, got.code, exitFailure)
		}
		got.contains(t, "stderr", addr)

		// The transport was asked for exactly the configured address, and the
		// diagnostic carries the failure it answered with — so no socket was
		// involved in producing either.
		if want := []string{addr}; !reflect.DeepEqual(transport.targets, want) {
			t.Errorf("%v asked the transport for %q; want %q", args, transport.targets, want)
		}
		got.contains(t, "stderr", errNoDaemon.Error())
	}
}

// TestSubcommandsReportARefusedKey calls out the one thing that actually goes
// wrong here: two different caller-key files.
func TestSubcommandsReportARefusedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caller.key")
	writeFile(t, path, "not-the-daemons-key")
	t.Setenv(proxy.CallerKeyEnvFile, path)

	daemon := newFakeDaemon(t, sampleDocument())
	env := daemon.env()
	env[envHome] = t.TempDir()
	for _, args := range gatewayBacked() {
		got := invoke(t, env, args...)
		if got.code != exitFailure {
			t.Errorf("%v code = %d; want %d", args, got.code, exitFailure)
		}
		got.contains(t, "stderr", "refused the caller key")
	}
}

// TestSubcommandsReportAMissingKeyFile fails before dialing at all: a key this
// process cannot read is a misconfiguration here, not a refusal there.
func TestSubcommandsReportAMissingKeyFile(t *testing.T) {
	t.Setenv(proxy.CallerKeyEnvFile, filepath.Join(t.TempDir(), "absent.key"))
	daemon := newFakeDaemon(t, sampleDocument())
	env := daemon.env()
	env[envHome] = t.TempDir()
	for _, args := range gatewayBacked() {
		got := invoke(t, env, args...)
		if got.code != exitFailure {
			t.Errorf("%v code = %d; want %d", args, got.code, exitFailure)
		}
		got.contains(t, "stderr", "caller key")
	}
}

// TestCommandLineErrorsExitTwo pins the usage taxonomy: a command line patchbay
// does not understand is exit 2 with usage on stderr, never a gateway and never
// a silent success.
func TestCommandLineErrorsExitTwo(t *testing.T) {
	callerKeyFile(t)
	daemon := newFakeDaemon(t, sampleDocument())
	for _, args := range [][]string{
		{},
		{"nonsense"},
		{"contexts", "extra"},
		{"seats", "extra"},
		{"bindings"},
		{"bindings", "--ctx", "personal", "extra"},
		{"bindings", "--nope", "x"},
		{"resolve"},
		{"resolve", "personal"},
		{"resolve", "personal", "model", "extra"},
		{"project", "one", "two"},
		{"check", "positional"},
		{"check", "--nope"},
	} {
		got := invoke(t, daemon.env(), args...)
		if got.code != exitUsage {
			t.Errorf("%v code = %d; want %d (stdout %q stderr %q)", args, got.code, exitUsage, got.stdout, got.stderr)
		}
		if got.stdout != "" {
			t.Errorf("%v wrote to stdout: %q", args, got.stdout)
		}
	}
}

// TestHelpIsNotAnError pins the one form that is a request rather than a
// mistake: it succeeds, and it goes to stdout so it can be piped.
func TestHelpIsNotAnError(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}} {
		got := invoke(t, nil, args...)
		if got.code != exitOK {
			t.Errorf("%v code = %d; want %d", args, got.code, exitOK)
		}
		got.contains(t, "stdout", "usage: patchbay <command>")
		got.contains(t, "stdout", "usage inventory-transcripts")
		got.contains(t, "stdout", "usage import-transcripts")
		if got.stderr != "" {
			t.Errorf("%v wrote usage to stderr: %q", args, got.stderr)
		}
	}
}

// TestUnknownContextDiagnosticsNameWhatIsServed pins the refusal an operator
// meets most: the CLI never falls back to another Context, for the same reason
// the gateway never does, so it says which ones exist instead.
func TestUnknownContextDiagnosticsNameWhatIsServed(t *testing.T) {
	callerKeyFile(t)
	daemon := newFakeDaemon(t, sampleDocument())
	for _, args := range [][]string{
		{"bindings", "--ctx", "nowhere"},
		{"resolve", "nowhere", "luna-fast"},
	} {
		got := invoke(t, daemon.env(), args...)
		if got.code != exitFailure {
			t.Errorf("%v code = %d; want %d", args, got.code, exitFailure)
		}
		got.contains(t, "stderr", "nowhere")
	}
}

// TestCheckFailures pins the one subcommand that needs no gateway. Each way of
// failing is exit 1 with the reason named, so an operator editing a registry
// learns what is wrong before anything reloads it.
func TestCheckFailures(t *testing.T) {
	dir := t.TempDir()

	missing := invoke(t, nil, "check", "--registry", filepath.Join(dir, "absent.json"))
	if missing.code != exitFailure {
		t.Errorf("missing file code = %d", missing.code)
	}
	missing.contains(t, "stderr", "registry file")

	broken := filepath.Join(dir, "broken.json")
	writeFile(t, broken, `{"default_context":"personal","seats":{},"contexts":{}}`)
	invalid := invoke(t, nil, "check", "--registry", broken)
	if invalid.code != exitFailure {
		t.Errorf("invalid registry code = %d", invalid.code)
	}
	invalid.contains(t, "stderr", "default_context")

	unparsable := filepath.Join(dir, "unparsable.json")
	writeFile(t, unparsable, `{`)
	if got := invoke(t, nil, "check", "--registry", unparsable); got.code != exitFailure {
		t.Errorf("unparsable registry code = %d", got.code)
	}

	// With no --registry and nothing in the environment to resolve one from,
	// there is nowhere honest to look.
	if got := invoke(t, map[string]string{}, "check"); got.code != exitFailure {
		t.Errorf("unresolvable registry path code = %d out = %q", got.code, got.stdout)
	}
}

// replacedValue is the distinctive string the registry below names in a field
// validation quotes back, so a diagnostic can be shown to have come from the
// replacement rather than from the file `check` opened.
const replacedValue = "sk-live-secret"

// replacedRegistry is what lands under `check` after it has already opened a
// valid registry: a document the gateway refuses, whose validation error names
// the field and the value behind the refusal.
const replacedRegistry = `{"default_context":"personal","seats":{` +
	`"luna":{"upstream":"https://openrouter.ai/api","model":"openai/gpt-5.6-luna",` +
	`"auth_mode":"inject","api_key_env_file":"` + replacedValue + `"}},` +
	`"contexts":{"personal":{"default_seat":"luna","models":{}}}}`

// TestCheckRefusesARegistryThatWentInvalidAfterItOpened pins the one verdict
// `check` must never print: OK for a file it cannot vouch for.
//
// Opening the registry and summarizing it are two reads of the same path, and a
// home-manager generation can swap the file between them. Summarized through a
// second read of its own, the replacement is reported as the empty document it
// is not — "0 seats, 0 contexts", exit 0 — for a file the gateway will refuse to
// load. The post-open half asks for one atomic inspection instead: a registry
// that no longer validates is refused with the field and the value behind it,
// and nothing is written to stdout at all.
func TestCheckRefusesARegistryThatWentInvalidAfterItOpened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	writeFile(t, path, testRegistry)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, base, base); err != nil {
		t.Fatal(err)
	}

	reg, err := registry.New(path)
	if err != nil {
		t.Fatalf("registry.New() = %v", err)
	}

	writeFile(t, path, replacedRegistry)
	if err := os.Chtimes(path, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	var verdict, diagnostic bytes.Buffer
	code := cli{stdout: &verdict, stderr: &diagnostic}.reportCheck(path, reg)

	if code != exitFailure {
		t.Fatalf("reportCheck() = %d; want %d", code, exitFailure)
	}
	if verdict.String() != "" {
		t.Errorf("check vouched for a registry it never validated: %q", verdict.String())
	}
	for _, detail := range []string{"api_key_env_file", replacedValue, path} {
		if !strings.Contains(diagnostic.String(), detail) {
			t.Errorf("stderr = %q; want the operator detail %q", diagnostic.String(), detail)
		}
	}
}

// TestCheckResolvesTheGatewaysOwnRegistryPath pins check reading the same file
// the gateway would, so it can never validate a different one.
func TestCheckResolvesTheGatewaysOwnRegistryPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	writeFile(t, path, testRegistry)

	got := invoke(t, map[string]string{envRegistry: path}, "check")
	if got.code != exitOK {
		t.Fatalf("code = %d stderr = %q", got.code, got.stderr)
	}
	got.contains(t, "stdout", path)
}

// TestCheckQuotesTheRegistryPath extends the CLI's string convention to the one
// value `check` prints. A registry path is a filename, and a filename may carry
// a tab or a newline: written raw into the verdict those bytes stop being the
// path and become the output's structure, splitting patchbay's single-line OK
// into lines an operator reads as patchbay's own.
//
// A quoted literal has no spelling for either, so the whole verdict stays one
// line and every byte of the path is still visible in it.
func TestCheckQuotesTheRegistryPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg\tistry\nfile.json")
	writeFile(t, path, testRegistry)

	got := invoke(t, nil, "check", "--registry", path)
	if got.code != exitOK {
		t.Fatalf("%+v", got)
	}

	if want := 1; len(lines(got.stdout)) != want {
		t.Errorf("check printed %d stdout lines; want %d\n%q",
			len(lines(got.stdout)), want, got.stdout)
	}
	for _, raw := range []string{"\t", "\n"} {
		if strings.Contains(strings.TrimSuffix(got.stdout, "\n"), raw) {
			t.Errorf("check emitted %q raw, so the path's bytes are the verdict's: %q",
				raw, got.stdout)
		}
	}
	got.contains(t, "stdout", strconv.Quote(path))

	// Quoting the path is not a change to what check counted.
	for _, want := range []string{"2 seats", "2 contexts", "1 binding"} {
		got.contains(t, "stdout", want)
	}
}

// TestOperationalDiagnosticsStayOnOneLine covers the stderr half of the same
// convention, where quoting is not the answer. A diagnostic is prose an operator
// reads — it names a path, an address, the sentence a syscall returned — and
// wrapping the whole line in quotes would make patchbay's own words look like
// data.
//
// But the values inside that prose are still byte strings nobody vetted. A
// registry path may carry a newline, and written raw it stops being part of the
// message: it ends the line, and everything after it reads as a second
// diagnostic patchbay never issued. So the control bytes are escaped where they
// sit and nothing else is.
//
// Both ways `check` can fail on a path go through that one writer, so both are
// checked here.
func TestOperationalDiagnosticsStayOnOneLine(t *testing.T) {
	const leaf = "reg\tistry\nfile.json"

	// The escaped spelling of leaf, written out rather than computed, so this
	// asserts against the convention instead of against the code implementing it.
	const escapedLeaf = `reg\tistry\nfile.json`

	for _, tc := range []struct {
		name  string
		write bool
	}{
		{name: "the registry file is missing", write: false},
		{name: "the registry file is invalid", write: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), leaf)
			if tc.write {
				writeFile(t, path, `{"default_context":"personal","seats":{},"contexts":{}}`)
			}

			got := invoke(t, nil, "check", "--registry", path)
			if got.code != exitFailure {
				t.Fatalf("%+v", got)
			}

			if want := 1; len(lines(got.stderr)) != want {
				t.Errorf("check wrote %d stderr lines; want %d\n%q",
					len(lines(got.stderr)), want, got.stderr)
			}
			for _, raw := range []string{"\t", "\n", "\r"} {
				if strings.Contains(strings.TrimSuffix(got.stderr, "\n"), raw) {
					t.Errorf("the diagnostic carried %q raw, so the path's bytes are its structure: %q",
						raw, got.stderr)
				}
			}
			got.contains(t, "stderr", escapedLeaf)
		})
	}
}

// TestNoticesAreEscapedLikeFailures pins the other operational writer. A notice
// is the half of stderr that does not stop the command — a settings file that
// would not parse, a base URL pointing elsewhere — and it names the same
// unvetted paths a failure does. It goes through the same escape, so two notices
// are two lines and no directory's name can make a third.
func TestNoticesAreEscapedLikeFailures(t *testing.T) {
	callerKeyFile(t)
	daemon := newFakeDaemon(t, sampleDocument())

	dir := filepath.Join(t.TempDir(), "pro\tject\ndir")
	claude := filepath.Join(dir, claudeDir)
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(claude, settingsLocalName), `{"env":`)

	env := daemon.env()
	env[envHome] = t.TempDir()

	got := invoke(t, env, "project", dir)
	if got.code != exitOK {
		t.Fatalf("%+v", got)
	}

	// One notice for the file that would not parse, one for the base URL nothing
	// set. A third would be the directory's own name having written it.
	noticed := lines(got.stderr)
	if want := 2; len(noticed) != want {
		t.Fatalf("project wrote %d stderr lines; want %d\n%q", len(noticed), want, got.stderr)
	}
	for _, notice := range noticed {
		if strings.ContainsAny(notice, "\t\r") {
			t.Errorf("a notice carried the directory's bytes raw: %q", notice)
		}
	}
	got.contains(t, "stderr", `pro\tject\ndir`)
}

// TestEscapeControlsRewritesOnlyControlBytes pins the escape itself, because the
// values that reach it carry more than the tab and the newline a registry path
// happened to have. A lone carriage return rewrites the line a terminal already
// drew, an escape introducer repaints it in whatever colors follow, and DEL sits
// above the printable range where a "below space" test alone would miss it.
//
// The rule is exactly the ASCII controls and nothing else. Prose, a multi-byte
// rune, and a byte no rune starts all come through untouched — which is why the
// pass is byte-wise: decoding first would replace the undecodable bytes of a
// path with U+FFFD, and the path an operator is being sent to look at would stop
// being the path.
func TestEscapeControlsRewritesOnlyControlBytes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, in, want string }{
		{
			name: "prose is left alone",
			in:   "registry file: /etc/patchbay/routes.json: no such file or directory",
			want: "registry file: /etc/patchbay/routes.json: no such file or directory",
		},
		{name: "newline", in: "one\ntwo", want: `one\ntwo`},
		{name: "carriage return", in: "one\rtwo", want: `one\rtwo`},
		{name: "tab", in: "one\ttwo", want: `one\ttwo`},
		{name: "a line ending is both of its bytes", in: "one\r\ntwo", want: `one\r\ntwo`},
		{name: "nul", in: "one\x00two", want: `one\x00two`},
		{name: "escape introducer", in: "one\x1b[31mtwo", want: `one\x1b[31mtwo`},
		{name: "delete", in: "one\x7ftwo", want: `one\x7ftwo`},
		{name: "the rest of the control range", in: "\x01\x08\x0b\x0c\x0e\x1f", want: `\x01\x08\x0b\x0c\x0e\x1f`},
		{name: "space is printable", in: "one two", want: "one two"},
		{name: "multi-byte runes survive", in: "café ✓", want: "café ✓"},
		{name: "invalid utf-8 survives", in: "bad\xffbyte", want: "bad\xffbyte"},
		{name: "a byte no rune starts survives", in: "\x80\x9f", want: "\x80\x9f"},
		{name: "nothing to escape", in: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := escapeControls(tc.in); got != tc.want {
				t.Errorf("escapeControls(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// startServingDaemon runs the gateway in the background against usageDB and
// returns once it is listening, along with the stop that asserts a clean exit.
// The usage subcommands are held to working while the daemon is writing, which
// is the only interesting case: a stopped daemon leaves nothing to contend with.
func startServingDaemon(t *testing.T, usageDB string) func() {
	t.Helper()
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "routes.json")
	writeFile(t, registryPath, testRegistry)
	callerKeyFile(t)

	logs := testlog.New(lineBuffer)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() {
		done <- dispatch(ctx, []string{"serve"}, cli{
			stdout: io.Discard,
			stderr: io.Discard,
			lookupEnv: envMap(map[string]string{
				envListen:    "127.0.0.1:0",
				envRegistry:  registryPath,
				envLedgerDir: filepath.Join(dir, "ledger"),
				envUsageDB:   usageDB,
			}),
			logger: logs.Logger(),
		})
	}()
	logs.WaitFor(t, "patchbay listening", waitDeadline)

	return func() {
		cancel()
		if code := <-done; code != exitOK {
			t.Errorf("serve exited %d; want %d", code, exitOK)
		}
	}
}

// TestUsageCommandsResolveTheDaemonUsageDatabase proves the subcommands find the
// same database the daemon would, from the environment alone: neither a running
// gateway nor a caller key is involved.
func TestUsageCommandsResolveTheDaemonUsageDatabase(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	source := filepath.Join(state, appDir, usageLeafFile)
	store, err := usage.Open(source)
	if err != nil {
		t.Fatalf("open usage database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close usage database: %v", err)
	}

	env := map[string]string{envStateXDG: state, envHome: t.TempDir()}
	snapshot := filepath.Join(t.TempDir(), "usage.snapshot.sqlite")
	got := invoke(t, env, "usage", "snapshot", snapshot)
	if got.code != exitOK {
		t.Fatalf("usage snapshot code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}
	got.contains(t, "stdout", snapshot)

	export := filepath.Join(t.TempDir(), "usage.export.jsonl")
	got = invoke(t, env, "usage", "export", export)
	if got.code != exitOK {
		t.Fatalf("usage export code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}
	got.contains(t, "stdout", export)
}

func TestUsageCommandsRequireOneDestination(t *testing.T) {
	for _, args := range [][]string{
		{"usage"},
		{"usage", "snapshot"},
		{"usage", "export"},
		{"usage", "snapshot", "one", "two"},
		{"usage", "export", "one", "two"},
		{"usage", "unknown", "one"},
	} {
		got := invoke(t, nil, args...)
		if got.code != exitUsage {
			t.Fatalf("%v code = %d; want %d", args, got.code, exitUsage)
		}
	}
}

func TestUsageSnapshotCoexistsWithServingDaemon(t *testing.T) {
	usageDB := filepath.Join(t.TempDir(), "usage.sqlite")
	stop := startServingDaemon(t, usageDB)

	dest := filepath.Join(t.TempDir(), "usage.snapshot.sqlite")
	got := invoke(
		t,
		map[string]string{envUsageDB: usageDB, envHome: t.TempDir()},
		"usage", "snapshot", dest,
	)
	if got.code != exitOK {
		t.Fatalf(
			"usage snapshot with live daemon code = %d; want %d (stderr %q)",
			got.code, exitOK, got.stderr,
		)
	}
	export := filepath.Join(t.TempDir(), "usage.export.jsonl")
	got = invoke(
		t,
		map[string]string{envUsageDB: usageDB, envHome: t.TempDir()},
		"usage", "export", export,
	)
	if got.code != exitOK {
		t.Fatalf(
			"usage export with live daemon code = %d; want %d (stderr %q)",
			got.code, exitOK, got.stderr,
		)
	}
	stop()
}

func writeUsageLedger(t *testing.T, dir, name, contents string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, name), contents)
}

func legacyLedgerLine(timestamp string) string {
	return `{"timestamp":"` + timestamp + `","host":"host","upstream":"upstream",` +
		`"model":"model","request_model":"model","auth_mode":"inject","status":200,` +
		`"input_tokens":0,"output_tokens":1,"duration_ms":0,"upstream_ms":0,` +
		`"block_shape":[]}` + "\n"
}

// v2LedgerLine is one line in the shape the live ledger writes. A file's name
// says which schema it holds, so the v2-named file in the enumeration test has
// to actually hold v2 — that is what makes the test cover both decoders rather
// than running the v1 one twice.
func v2LedgerLine(timestamp string) string {
	return `{"timestamp":"` + timestamp + `","host":"host","context":"personal",` +
		`"selection":{"kind":"default","model":null},"seat":"anthropic",` +
		`"upstream":"https://api.anthropic.com","model":"model","request_model":"model",` +
		`"auth_mode":"inject","credential_class":"none","config_source":"PATCHBAY_KEY_FILE",` +
		`"status":200,"input_tokens":0,"output_tokens":1,"duration_ms":0,"upstream_ms":0,` +
		`"block_shape":[],"drop_signature":false}` + "\n"
}

func TestUsageImportEnumeratesSortedLedgerFiles(t *testing.T) {
	dir := t.TempDir()
	usageDB := filepath.Join(t.TempDir(), "usage.sqlite")
	writeUsageLedger(t, dir, "patchbay-v2-2026-08-21.jsonl", v2LedgerLine("2026-08-21T12:00:00Z"))
	writeUsageLedger(t, dir, "patchbay-2026-08-20.jsonl", legacyLedgerLine("2026-08-20T12:00:00Z"))
	writeUsageLedger(t, dir, "patchbay-invalid.jsonl", legacyLedgerLine("2026-08-22T12:00:00Z"))

	got := invoke(t, map[string]string{envUsageDB: usageDB, envHome: t.TempDir()}, "usage", "import", dir)
	if got.code != exitOK {
		t.Fatalf("usage import code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}
	first := strings.Index(got.stdout, "patchbay-2026-08-20.jsonl")
	second := strings.Index(got.stdout, "patchbay-v2-2026-08-21.jsonl")
	if first < 0 || second < 0 || first >= second || strings.Contains(got.stdout, "patchbay-invalid") {
		t.Fatalf("usage import stdout = %q; want sorted recognized ledger names only", got.stdout)
	}
	got.contains(t, "stdout", "imported=1")
}

func TestUsageImportRejectsMissingOrEmptyLedgerDirectory(t *testing.T) {
	missing := invoke(
		t,
		map[string]string{envUsageDB: filepath.Join(t.TempDir(), "usage.sqlite"), envHome: t.TempDir()},
		"usage",
		"import",
		filepath.Join(t.TempDir(), "missing"),
	)
	if missing.code != exitFailure {
		t.Fatalf("missing ledger directory code = %d; want %d", missing.code, exitFailure)
	}
	empty := invoke(
		t,
		map[string]string{envUsageDB: filepath.Join(t.TempDir(), "usage.sqlite"), envHome: t.TempDir()},
		"usage",
		"import",
		t.TempDir(),
	)
	if empty.code != exitFailure {
		t.Fatalf("empty ledger directory code = %d; want %d", empty.code, exitFailure)
	}
	empty.contains(t, "stderr", "no ledger files")
}

func TestUsageImportCoexistsWithServingDaemon(t *testing.T) {
	usageDB := filepath.Join(t.TempDir(), "usage.sqlite")
	stop := startServingDaemon(t, usageDB)

	ledgerDir := t.TempDir()
	writeUsageLedger(t, ledgerDir, "patchbay-2026-08-20.jsonl", legacyLedgerLine("2026-08-20T12:00:00Z"))
	got := invoke(
		t,
		map[string]string{envUsageDB: usageDB, envHome: t.TempDir()},
		"usage", "import", ledgerDir,
	)
	if got.code != exitOK {
		t.Fatalf("usage import with daemon code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}
	stop()
}

func TestRegistryStatePreservesSeatBillingDeclaration(t *testing.T) {
	t.Parallel()
	doc := server.Document{
		RegistryValid: true,
		Seats: map[string]server.SeatSummary{
			"worker": {
				AuthMode:      string(registry.AuthInject),
				BillingPolicy: billing.PolicyExplicit.String(),
				BillingClass:  billing.ClassSubscription.String(),
				Upstream:      "https://example.test",
			},
		},
		Contexts: map[string]server.ContextSummary{},
	}

	seat := registryState(doc).Seats["worker"]
	if seat.BillingClass != billing.ClassSubscription || !seat.BillingClassSet {
		t.Fatalf(
			"registryState() billing = %q, set=%v; want subscription, true",
			seat.BillingClass,
			seat.BillingClassSet,
		)
	}
}

func TestUsageTranscriptCommandsParseFlagsAndResolveDefaultDatabase(t *testing.T) {
	claude := t.TempDir()
	claudeProject := filepath.Join(claude, "project")
	if err := os.MkdirAll(claudeProject, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(claudeProject, "session.jsonl"),
		[]byte(
			`{"type":"assistant","uuid":"a","timestamp":"2026-09-01T00:00:00Z","message":{"id":"string-content","model":"claude-test","usage":{"input_tokens":1,"output_tokens":1},"content":"hello","stop_reason":"end_turn"}}`+"\n"+
				`{"type":"assistant","uuid":"bad","timestamp":"2026-09-01T00:00:01Z","message":{"content":{"type":"text"}}}`+"\n",
		),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	pi := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(pi, "session.jsonl"),
		[]byte(
			`{"type":"message","id":"u","timestamp":"2026-09-01T00:00:00Z","message":{"role":"assistant","model":"pi-test","usage":{"input":1,"output":1},"stopReason":"stop","content":[{"type":"text"}]}}`+"\n",
		),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	inventory := filepath.Join(t.TempDir(), "inventory.json")
	env := map[string]string{
		envUsageDB: filepath.Join(t.TempDir(), "usage.sqlite"),
		envHome:    t.TempDir(),
	}
	got := invoke(t, env, "usage", "inventory-transcripts",
		"--claude-projects", claude,
		"--pi-sessions", pi,
		"--out", inventory,
	)
	if got.code != exitOK {
		t.Fatalf("inventory command code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}
	got.contains(t, "stdout", "OK usage inventory-transcripts")
	got = invoke(t, env, "usage", "import-transcripts", "--inventory", inventory)
	if got.code != exitOK {
		t.Fatalf("import-transcripts command code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}
	got.contains(
		t,
		"stdout",
		"OK usage import-transcripts project/session.jsonl: imported=1 skipped-existing=0 skipped-changed=0",
	)
	if strings.Contains(got.stdout, "unreadable") {
		t.Fatalf("stdout changed format: %q", got.stdout)
	}
	if got.stderr != "patchbay: project/session.jsonl: 1 unreadable rows skipped\n" {
		t.Fatalf("stderr = %q", got.stderr)
	}
}

func TestUsageTranscriptCommandsRequireNamedFlags(t *testing.T) {
	for _, args := range [][]string{
		{"usage", "inventory-transcripts"},
		{"usage", "inventory-transcripts", "--out", "one"},
		{"usage", "import-transcripts"},
		{"usage", "import-transcripts", "--db", "one"},
	} {
		got := invoke(t, nil, args...)
		if got.code != exitUsage {
			t.Errorf("%v code = %d; want %d", args, got.code, exitUsage)
		}
	}
}
