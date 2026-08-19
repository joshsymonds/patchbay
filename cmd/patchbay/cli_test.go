package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/server"
)

// testCallerKey is the shared secret the CLI presents and the fake daemon
// expects.
const testCallerKey = "cli-caller-key"

// output is what one CLI invocation wrote and what it exited with.
type output struct {
	code   int
	stdout string
	stderr string
}

// contains fails unless the stream holds want.
func (o output) contains(t *testing.T, stream, want string) {
	t.Helper()

	got := o.stdout
	if stream == "stderr" {
		got = o.stderr
	}

	if !strings.Contains(got, want) {
		t.Fatalf("%s = %q; want it to contain %q", stream, got, want)
	}
}

// invoke runs one CLI invocation against env, capturing both streams.
func invoke(t *testing.T, env map[string]string, args ...string) output {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := dispatch(t.Context(), args, cli{
		stdout:    &stdout,
		stderr:    &stderr,
		lookupEnv: envMap(env),
		client:    nil,
		logger:    quietLogger(),
	})

	return output{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// callerKeyFile writes a caller-key file and names it in the environment, the
// same double indirection the daemon's own gate reads.
func callerKeyFile(t *testing.T, key string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "caller.key")
	writeFile(t, path, key)
	t.Setenv(proxy.CallerKeyEnvFile, path)
}

// fakeDaemon is a stand-in patchbay: it serves one introspection document to a
// caller that presents the right key, and refuses everyone else exactly as the
// real endpoint does.
type fakeDaemon struct {
	server *httptest.Server
	addr   string
}

// newFakeDaemon starts a stand-in serving doc.
func newFakeDaemon(t *testing.T, doc server.Document) *fakeDaemon {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != server.RegistryEndpoint {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		if r.Header.Get(proxy.CallerKeyHeader) != testCallerKey {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"patchbay: caller key"}}`)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)

	return &fakeDaemon{server: srv, addr: strings.TrimPrefix(srv.URL, "http://")}
}

// env returns the environment an invocation needs to find this daemon.
func (d *fakeDaemon) env() map[string]string {
	return map[string]string{envListen: d.addr}
}

// sampleDocument is the routing a fake daemon describes: a personal context on
// the built-in forward with one inject alias, and a work context whose default
// route is Bedrock.
func sampleDocument() server.Document {
	return server.Document{
		RegistryPath:   "/home/tester/.config/patchbay/routes.json",
		RegistryLoaded: true,
		Listen:         "127.0.0.1:4100",
		Contexts: map[string]server.ContextSummary{
			"personal": {
				DefaultRoute: server.RouteSummary{
					Auth:     string(registry.AuthForward),
					Upstream: "https://api.anthropic.com",
				},
				Routes: map[string]server.RouteSummary{
					"luna-fast": {
						Auth:           string(registry.AuthInject),
						Upstream:       "https://openrouter.ai/api",
						Model:          "openai/gpt-5.6-luna",
						MaxInputTokens: 262144,
						KeyEnvFile:     "OPENROUTER_API_KEY_FILE",
						KeyFile:        "/run/agenix/openrouter.key",
					},
				},
			},
			"work": {
				DefaultRoute: server.RouteSummary{
					Auth:            string(registry.AuthSigV4),
					Upstream:        "https://bedrock-runtime.us-west-2.amazonaws.com",
					AWSProfile:      "work-bedrock",
					AWSRegion:       "us-west-2",
					ModelMapEnvFile: "PATCHBAY_MODEL_MAP",
					ModelMapFile:    "/run/agenix/bedrock-models.json",
				},
				Routes: map[string]server.RouteSummary{
					"bedrock/opus": {
						Auth:            string(registry.AuthSigV4),
						Upstream:        "https://bedrock-runtime.us-west-2.amazonaws.com",
						Model:           "claude-opus-4-5",
						MaxInputTokens:  200000,
						AWSProfile:      "work-bedrock",
						AWSRegion:       "us-west-2",
						ModelMapEnvFile: "PATCHBAY_MODEL_MAP",
						ModelMapFile:    "/run/agenix/bedrock-models.json",
					},
				},
			},
		},
	}
}

func TestBareInvocationRefusesToServe(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run(nil, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("run() with no arguments = %d; want %d", code, exitUsage)
	}

	if !strings.Contains(stderr.String(), "usage: patchbay") {
		t.Fatalf("stderr = %q; want the usage text", stderr.String())
	}

	if !strings.Contains(stderr.String(), "serve") {
		t.Fatalf("stderr = %q; want it to name the serve subcommand", stderr.String())
	}

	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want nothing on a usage error", stdout.String())
	}
}

func TestUnknownSubcommandIsAUsageError(t *testing.T) {
	got := invoke(t, nil, "frobnicate")

	if got.code != exitUsage {
		t.Fatalf("code = %d; want %d", got.code, exitUsage)
	}

	got.contains(t, "stderr", "frobnicate")
	got.contains(t, "stderr", "usage: patchbay")
}

func TestHelpIsNotAnError(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run(arg, func(t *testing.T) {
			got := invoke(t, nil, arg)

			if got.code != exitOK {
				t.Fatalf("code = %d; want %d", got.code, exitOK)
			}

			got.contains(t, "stdout", "usage: patchbay")
		})
	}
}

// TestServeSubcommandServes runs the gateway through the same dispatch a shell
// would, and proves it is serving by asking it about itself. Nothing here
// reaches the network: the introspection endpoint is answered from the
// registry alone.
func TestServeSubcommandServes(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "routes.json")
	writeFile(t, registryPath, lunaRegistry)
	callerKeyFile(t, testCallerKey)

	logs := newLogRecorder()
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
			}),
			client: nil,
			logger: slog.New(logs),
		})
	}()

	addr := logs.waitFor(t, "patchbay listening")["listen"]

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+addr+server.RegistryEndpoint, nil)
	if err != nil {
		t.Fatalf("build introspection request: %v", err)
	}

	req.Header.Set(proxy.CallerKeyHeader, testCallerKey)

	resp, doErr := http.DefaultClient.Do(req)
	if doErr != nil {
		t.Fatalf("GET the introspection endpoint: %v", doErr)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("introspection status = %d; want %d", resp.StatusCode, http.StatusOK)
	}

	var doc server.Document
	if decodeErr := json.NewDecoder(resp.Body).Decode(&doc); decodeErr != nil {
		t.Fatalf("decode introspection document: %v", decodeErr)
	}

	if doc.Listen != addr || doc.RegistryPath != registryPath {
		t.Fatalf("document = %+v; want the address and registry this gateway serves", doc)
	}

	cancel()

	if code := <-done; code != exitOK {
		t.Fatalf("serve exited %d; want %d on a clean shutdown", code, exitOK)
	}
}

func TestServeReportsAFatalStartupFailure(t *testing.T) {
	var stderr bytes.Buffer

	code := dispatch(t.Context(), []string{"serve"}, cli{
		stdout:    io.Discard,
		stderr:    &stderr,
		lookupEnv: envMap(map[string]string{envListen: "not-an-address"}),
		client:    nil,
		logger:    quietLogger(),
	})

	if code != exitFailure {
		t.Fatalf("code = %d; want %d when the gateway cannot start", code, exitFailure)
	}
}

func TestSubcommandsReportAnUnreachableDaemon(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	// A port nothing is listening on: the connection is refused immediately.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(closed.URL, "http://")
	closed.Close()

	for _, args := range [][]string{{"contexts"}, {"routes"}, {"resolve", "personal", "claude-opus-5"}} {
		t.Run(args[0], func(t *testing.T) {
			got := invoke(t, map[string]string{envListen: addr}, args...)

			if got.code != exitFailure {
				t.Fatalf("code = %d; want %d", got.code, exitFailure)
			}

			got.contains(t, "stderr", "is patchbay running?")
			got.contains(t, "stderr", addr)
		})
	}
}

func TestSubcommandsReportARefusedKey(t *testing.T) {
	callerKeyFile(t, "the-wrong-key")

	daemon := newFakeDaemon(t, sampleDocument())
	got := invoke(t, daemon.env(), "contexts")

	if got.code != exitFailure {
		t.Fatalf("code = %d; want %d", got.code, exitFailure)
	}

	got.contains(t, "stderr", proxy.CallerKeyEnvFile)
	got.contains(t, "stderr", "does not match")
}

func TestSubcommandsReportAMissingKeyFile(t *testing.T) {
	t.Setenv(proxy.CallerKeyEnvFile, "")

	got := invoke(t, map[string]string{envListen: "127.0.0.1:4100"}, "contexts")

	if got.code != exitFailure {
		t.Fatalf("code = %d; want %d", got.code, exitFailure)
	}

	got.contains(t, "stderr", proxy.CallerKeyEnvFile)
}

func TestContextsListsEveryContext(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	daemon := newFakeDaemon(t, sampleDocument())
	got := invoke(t, daemon.env(), "contexts")

	if got.code != exitOK {
		t.Fatalf("code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}

	personal := lineFor(t, got.stdout, "personal")
	if !strings.Contains(personal, "built-in anthropic forward") || !strings.HasSuffix(personal, "1") {
		t.Fatalf("personal row = %q; want the built-in default route and one alias", personal)
	}

	work := lineFor(t, got.stdout, "work")
	if !strings.Contains(work, string(registry.AuthSigV4)) ||
		!strings.Contains(work, "bedrock-runtime.us-west-2") {
		t.Fatalf("work row = %q; want its sigv4 default route", work)
	}
}

// lineFor returns the single output line whose first field is name.
func lineFor(t *testing.T, out, name string) string {
	t.Helper()

	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == name {
			return strings.TrimSpace(line)
		}
	}

	t.Fatalf("output %q has no row for %q", out, name)

	return ""
}

func TestRoutesListsOneContext(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	daemon := newFakeDaemon(t, sampleDocument())

	// With no --ctx, the personal context is the one a bare request rides.
	got := invoke(t, daemon.env(), "routes")
	if got.code != exitOK {
		t.Fatalf("code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}

	row := lineFor(t, got.stdout, "luna-fast")
	for _, want := range []string{
		string(registry.AuthInject),
		"https://openrouter.ai/api",
		"openai/gpt-5.6-luna",
		"262144",
		"OPENROUTER_API_KEY_FILE",
	} {
		if !strings.Contains(row, want) {
			t.Fatalf("luna-fast row = %q; want it to carry %q", row, want)
		}
	}

	if strings.Contains(got.stdout, "/run/agenix/openrouter.key") {
		t.Fatalf("stdout = %q; the routes table names the variable, not the file", got.stdout)
	}

	// The work context is named explicitly, and answers with its own aliases.
	got = invoke(t, daemon.env(), "routes", "--ctx", "work")
	if got.code != exitOK {
		t.Fatalf("routes --ctx work code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}

	if strings.Contains(got.stdout, "luna-fast") {
		t.Fatalf("stdout = %q; want nothing from the personal context", got.stdout)
	}

	// A signing route pays with AWS credentials the gateway exports for a
	// profile, so the profile is what its credential source names — there is no
	// environment variable holding that key, and the model map is not one.
	bedrock := lineFor(t, got.stdout, "bedrock/opus")
	if !strings.Contains(bedrock, "aws profile work-bedrock") {
		t.Fatalf("bedrock/opus row = %q; want it to name the AWS profile it pays with", bedrock)
	}

	if strings.Contains(bedrock, "PATCHBAY_MODEL_MAP") {
		t.Fatalf("bedrock/opus row = %q; the model map is not what the route pays with", bedrock)
	}
}

func TestRoutesRefusesAnUnknownContext(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	daemon := newFakeDaemon(t, sampleDocument())
	got := invoke(t, daemon.env(), "routes", "--ctx", "bogus")

	if got.code != exitFailure {
		t.Fatalf("code = %d; want %d", got.code, exitFailure)
	}

	got.contains(t, "stderr", "bogus")
}

func TestResolveNamesOneRequestsFate(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	daemon := newFakeDaemon(t, sampleDocument())

	// A model an alias claims rides that alias.
	got := invoke(t, daemon.env(), "resolve", "personal", "luna-fast")
	if got.code != exitOK {
		t.Fatalf("code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}

	got.contains(t, "stdout", "alias luna-fast")
	got.contains(t, "stdout", string(registry.AuthInject))
	got.contains(t, "stdout", "https://openrouter.ai/api")
	got.contains(t, "stdout", "openai/gpt-5.6-luna")
	got.contains(t, "stdout", "/run/agenix/openrouter.key")

	// A model no alias claims rides the context's default route.
	got = invoke(t, daemon.env(), "resolve", "personal", "claude-opus-5")
	if got.code != exitOK {
		t.Fatalf("code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}

	got.contains(t, "stdout", "default route")
}

func TestResolveReadsTheModelMapWhenItCan(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	mapPath := filepath.Join(t.TempDir(), "models.json")
	writeFile(t, mapPath, `{"claude-opus-5":"arn:aws:bedrock:us-west-2::foundation-model/secret"}`)

	doc := sampleDocument()
	work := doc.Contexts["work"]
	work.DefaultRoute.ModelMapFile = mapPath
	doc.Contexts["work"] = work

	daemon := newFakeDaemon(t, doc)

	// A model the map claims: the daemon still does the lookup per request, and
	// the mapped Bedrock id is never printed.
	got := invoke(t, daemon.env(), "resolve", "work", "claude-opus-5")
	if got.code != exitOK {
		t.Fatalf("code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}

	got.contains(t, "stdout", mapPath)
	got.contains(t, "stdout", "mapped")

	if strings.Contains(got.stdout, "arn:aws:bedrock") {
		t.Fatalf("stdout = %q; a mapped Bedrock id is never printed", got.stdout)
	}

	// A model nothing maps is a request the daemon would refuse.
	got = invoke(t, daemon.env(), "resolve", "work", "claude-haiku-5")
	if got.code != exitOK {
		t.Fatalf("code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}

	got.contains(t, "stdout", "not mapped")
	got.contains(t, "stdout", "400")
}

func TestResolveRefusesAnUnknownContext(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	daemon := newFakeDaemon(t, sampleDocument())
	got := invoke(t, daemon.env(), "resolve", "bogus", "claude-opus-5")

	if got.code != exitFailure {
		t.Fatalf("code = %d; want %d", got.code, exitFailure)
	}

	got.contains(t, "stderr", "bogus")
}

func TestResolveNeedsBothArguments(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	for _, args := range [][]string{{"resolve"}, {"resolve", "personal"}, {"resolve", "a", "b", "c"}} {
		got := invoke(t, nil, args...)
		if got.code != exitUsage {
			t.Fatalf("%v code = %d; want %d", args, got.code, exitUsage)
		}
	}
}

func TestStrayArgumentsAreAUsageError(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	tests := [][]string{
		{"contexts", "extra"},
		{"routes", "extra"},
		{"check", "extra"},
		{"project", "one", "two"},
		{"routes", "--nonesuch"},
	}

	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got := invoke(t, nil, args...)
			if got.code != exitUsage {
				t.Fatalf("code = %d; want %d (stderr %q)", got.code, exitUsage, got.stderr)
			}
		})
	}
}

func TestCheckValidatesARegistryFile(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.json")
	writeFile(t, valid, lunaRegistry)

	got := invoke(t, nil, "check", "--registry", valid)
	if got.code != exitOK {
		t.Fatalf("code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}

	got.contains(t, "stdout", valid)
	got.contains(t, "stdout", "personal")
	got.contains(t, "stdout", "work")
}

func TestCheckReportsTheValidationError(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.json")
	writeFile(t, broken, `{"contexts":{"personal":{"routes":{"bad":{"auth":"nonsense"}}}}}`)

	got := invoke(t, nil, "check", "--registry", broken)
	if got.code != exitFailure {
		t.Fatalf("code = %d; want %d", got.code, exitFailure)
	}

	got.contains(t, "stderr", "auth")
	got.contains(t, "stderr", "nonsense")
}

func TestCheckReportsAMissingFile(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.json")

	got := invoke(t, nil, "check", "--registry", absent)
	if got.code != exitFailure {
		t.Fatalf("code = %d; want %d", got.code, exitFailure)
	}

	got.contains(t, "stderr", absent)
}

func TestCheckFallsBackToTheDaemonsOwnRegistryPath(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "routes.json")
	writeFile(t, valid, lunaRegistry)

	// No --registry: the path is resolved exactly the way the daemon resolves
	// its own.
	got := invoke(t, map[string]string{envRegistry: valid}, "check")
	if got.code != exitOK {
		t.Fatalf("code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}

	got.contains(t, "stdout", valid)
}

func TestCheckReportsAnUnresolvableRegistryPath(t *testing.T) {
	got := invoke(t, map[string]string{}, "check")
	if got.code != exitFailure {
		t.Fatalf("code = %d; want %d", got.code, exitFailure)
	}
}

// projectDir writes a .claude settings file under dir and returns its path.
func projectDir(t *testing.T, dir, name, body string) string {
	t.Helper()

	claude := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", claude, err)
	}

	path := filepath.Join(claude, name)
	writeFile(t, path, body)

	return path
}

// settingsBody is a Claude Code settings file that sets ANTHROPIC_BASE_URL.
func settingsBody(baseURL string) string {
	return `{"env":{"ANTHROPIC_BASE_URL":"` + baseURL + `"}}`
}

func TestProjectAppliesSettingsPrecedence(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	daemon := newFakeDaemon(t, sampleDocument())
	home := t.TempDir()
	dir := t.TempDir()

	userFile := projectDir(t, home, "settings.json", settingsBody("http://"+daemon.addr))
	projectFile := projectDir(t, dir, "settings.json", settingsBody("http://"+daemon.addr+"/ctx/personal"))
	localFile := projectDir(t, dir, "settings.local.json", settingsBody("http://"+daemon.addr+"/ctx/work"))

	env := daemon.env()
	env[envHome] = home

	got := invoke(t, env, "project", dir)
	if got.code != exitOK {
		t.Fatalf("code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}

	// Every file in the chain is named, whether or not it won.
	for _, path := range []string{localFile, projectFile, userFile} {
		got.contains(t, "stdout", path)
	}

	// The most local file wins, and its /ctx prefix names the context.
	got.contains(t, "stdout", "http://"+daemon.addr+"/ctx/work")
	got.contains(t, "stdout", "context: work")

	if strings.Contains(got.stdout, "context: personal") {
		t.Fatalf("stdout = %q; the project file must not override the local one", got.stdout)
	}
}

func TestProjectFallsBackToPersonal(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	daemon := newFakeDaemon(t, sampleDocument())
	env := daemon.env()
	env[envHome] = t.TempDir()

	// A directory with no settings at all selects the personal context, which is
	// what a bare Claude Code invocation rides.
	got := invoke(t, env, "project", t.TempDir())
	if got.code != exitOK {
		t.Fatalf("code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}

	got.contains(t, "stdout", "context: personal")
	got.contains(t, "stdout", "luna-fast")
}

func TestProjectWarnsAboutAnUnknownContext(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	daemon := newFakeDaemon(t, sampleDocument())
	dir := t.TempDir()
	projectDir(t, dir, "settings.json", settingsBody("http://"+daemon.addr+"/ctx/bogus"))

	env := daemon.env()
	env[envHome] = t.TempDir()

	got := invoke(t, env, "project", dir)
	if got.code != exitFailure {
		t.Fatalf("code = %d; want %d for a context the daemon does not serve", got.code, exitFailure)
	}

	got.contains(t, "stderr", "bogus")
}

func TestProjectNoticesAForeignBaseURL(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	daemon := newFakeDaemon(t, sampleDocument())
	dir := t.TempDir()
	projectDir(t, dir, "settings.json", settingsBody("https://api.anthropic.com"))

	env := daemon.env()
	env[envHome] = t.TempDir()

	got := invoke(t, env, "project", dir)
	if got.code != exitOK {
		t.Fatalf("code = %d; want %d — a foreign base URL is a notice, not a failure", got.code, exitOK)
	}

	got.contains(t, "stderr", "api.anthropic.com")
	got.contains(t, "stdout", "context: personal")
}

func TestProjectToleratesAMalformedSettingsFile(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	daemon := newFakeDaemon(t, sampleDocument())
	dir := t.TempDir()
	broken := projectDir(t, dir, "settings.local.json", `{"env":`)
	projectDir(t, dir, "settings.json", settingsBody("http://"+daemon.addr+"/ctx/work"))

	env := daemon.env()
	env[envHome] = t.TempDir()

	got := invoke(t, env, "project", dir)
	if got.code != exitOK {
		t.Fatalf("code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}

	got.contains(t, "stderr", broken)
	got.contains(t, "stdout", "context: work")
}

// A base URL ending at the bare context prefix names no context of its own.
// Claude Code appends its own endpoint path to it, so the gateway reads the
// first segment of THAT — "v1" — as the context name and refuses it. The
// command has to diagnose exactly what the gateway would do, which is why it
// splits the path with the gateway's own code rather than a second copy of the
// rule.
func TestProjectDiagnosesABareContextPrefix(t *testing.T) {
	callerKeyFile(t, testCallerKey)

	daemon := newFakeDaemon(t, sampleDocument())
	dir := t.TempDir()
	projectDir(t, dir, "settings.json", settingsBody("http://"+daemon.addr+"/ctx"))

	env := daemon.env()
	env[envHome] = t.TempDir()

	got := invoke(t, env, "project", dir)
	if got.code != exitFailure {
		t.Fatalf("code = %d; want %d for a base URL that names a context nothing serves", got.code, exitFailure)
	}

	got.contains(t, "stdout", "context: v1")
	got.contains(t, "stderr", "v1")
}

// check validates the registry patchbay actually ships as its example, and says
// exactly what the documentation says it says.
func TestCheckValidatesTheShippedExampleRegistry(t *testing.T) {
	const example = "../../examples/routes.json"

	got := invoke(t, nil, "check", "--registry", example)
	if got.code != exitOK {
		t.Fatalf("code = %d; want %d (stderr %q)", got.code, exitOK, got.stderr)
	}

	want := "OK " + example + ": 2 contexts (personal, work), 4 routes\n"
	if got.stdout != want {
		t.Fatalf("stdout = %q; want %q", got.stdout, want)
	}
}
