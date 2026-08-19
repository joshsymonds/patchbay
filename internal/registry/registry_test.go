package registry_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joshsymonds/patchbay/internal/registry"
)

// lunaRoute is the brief's example inject route, written compactly so the
// preserved extra_body bytes are predictable.
const lunaRoute = `"luna-fast":{"base_url":"https://openrouter.ai/api",` +
	`"model":"openai/gpt-5.6-luna","auth":"inject",` +
	`"api_key_env_file":"OPENROUTER_API_KEY_FILE",` +
	`"extra_body":{"reasoning_effort":"low"},` +
	`"max_input_tokens":262144,"max_output_tokens":65536}`

// deepseekRoute exercises strip_cache_control and capture_bodies.
const deepseekRoute = `"deepseek-flash-low":{"base_url":"https://openrouter.ai/api",` +
	`"model":"deepseek/deepseek-v4-flash-0731","auth":"inject",` +
	`"api_key_env_file":"OPENROUTER_API_KEY_FILE",` +
	`"strip_cache_control":true,"capture_bodies":true}`

// solRoute uses the directory-style namespaced alias convention. The slash is
// an ordinary character in an alias key, not a path or a namespace lookup.
const solRoute = `"openrouter/sol":{"base_url":"https://openrouter.ai/api",` +
	`"model":"openrouter/sol-1","auth":"inject",` +
	`"api_key_env_file":"OPENROUTER_API_KEY_FILE"}`

// injectDefault is a default_route paid for with a foreign key: the shape that
// makes a whole context ride somewhere other than Anthropic.
const injectDefault = `"default_route":{"base_url":"https://openrouter.ai/api",` +
	`"model":"openai/gpt-5.6-luna","auth":"inject",` +
	`"api_key_env_file":"OPENROUTER_API_KEY_FILE","max_input_tokens":262144}`

// personalDoc wraps route entries as the routes of the personal context — the
// context every bare, unprefixed path resolves against.
func personalDoc(routes string) string {
	return `{"contexts":{"personal":{"routes":{` + routes + `}}}}`
}

// contextDoc wraps whole context entries as a registry document.
func contextDoc(contexts string) string {
	return `{"contexts":{` + contexts + `}}`
}

// writeRegistry writes body to path and returns path.
func writeRegistry(t *testing.T, path, body string) string {
	t.Helper()

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write registry %s: %v", path, err)
	}

	return path
}

// newRegistry writes body into a fresh temp dir and returns a Registry over it.
func newRegistry(t *testing.T, body string) *registry.Registry {
	t.Helper()

	return registry.New(writeRegistry(t, filepath.Join(t.TempDir(), "registry.json"), body))
}

// resolve resolves model within the named context, failing on any error.
func resolve(t *testing.T, reg *registry.Registry, sel registry.Selection, model string) registry.Resolution {
	t.Helper()

	res, err := reg.Resolve(sel, model)
	if err != nil {
		t.Fatalf("Resolve(%+v, %q) = %v; want no error", sel, model, err)
	}

	return res
}

// bare resolves model the way a request carrying no context prefix does.
func bare(t *testing.T, reg *registry.Registry, model string) registry.Route {
	t.Helper()

	return resolve(t, reg, registry.Selection{}, model).Route
}

// bareAliases lists the aliases a bare, unprefixed request sees.
func bareAliases(t *testing.T, reg *registry.Registry) []registry.Route {
	t.Helper()

	return resolve(t, reg, registry.Selection{}, "").Aliases
}

// assertDefault fails unless resolving model yields exactly the built-in route.
func assertDefault(t *testing.T, reg *registry.Registry, model string) {
	t.Helper()

	if got := bare(t, reg, model); !reflect.DeepEqual(got, registry.DefaultRoute()) {
		t.Fatalf("Resolve(%q) = %+v; want the default route %+v", model, got, registry.DefaultRoute())
	}
}

func TestDefaultRouteIsTheFixedBuiltIn(t *testing.T) {
	t.Parallel()

	want := registry.Route{
		Alias:             "",
		BaseURL:           "https://api.anthropic.com",
		Model:             "",
		Auth:              registry.AuthForward,
		APIKeyEnvFile:     "",
		ExtraBody:         nil,
		MaxInputTokens:    0,
		MaxOutputTokens:   0,
		StripCacheControl: false,
		CaptureBodies:     false,
	}

	if got := registry.DefaultRoute(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultRoute() = %+v; want %+v", got, want)
	}
}

func TestDefaultRouteIgnoresTheRegistryFile(t *testing.T) {
	t.Parallel()

	// A file that redefines the empty alias must not move the built-in route.
	reg := newRegistry(t, personalDoc(`"":{"base_url":"https://evil.example"}`))

	assertDefault(t, reg, "claude-sonnet-5")

	if err := reg.Err(); err == nil {
		t.Fatal("Err() = nil; want an error rejecting the empty alias key")
	}
}

func TestResolveFallsBackToDefaultRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		body  string
		write bool
		model string
	}{
		{
			name:  "missing file",
			write: false,
			model: "claude-sonnet-5",
		},
		{
			name:  "unparsable file",
			body:  `{"contexts":{`,
			write: true,
			model: "claude-sonnet-5",
		},
		{
			name: "file failing validation",
			body: personalDoc(lunaRoute +
				`,"bad":{"base_url":"https://evil.example","auth":"forward"}`),
			write: true,
			model: "luna-fast",
		},
		{
			name:  "valid file with no matching alias",
			body:  personalDoc(lunaRoute),
			write: true,
			model: "claude-opus-5",
		},
		{
			name:  "empty routes object",
			body:  personalDoc(``),
			write: true,
			model: "anything",
		},
		{
			name:  "file without a contexts key",
			body:  `{}`,
			write: true,
			model: "anything",
		},
		{
			name:  "document with no personal context",
			body:  contextDoc(`"work":{"routes":{` + lunaRoute + `}}`),
			write: true,
			model: "luna-fast",
		},
		{
			name:  "v1 document the v2 parser refuses whole",
			body:  `{"routes":{` + lunaRoute + `}}`,
			write: true,
			model: "luna-fast",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "registry.json")
			if tc.write {
				writeRegistry(t, path, tc.body)
			}

			assertDefault(t, registry.New(path), tc.model)
		})
	}
}

func TestResolveReturnsTheExactAliasedRoute(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t, personalDoc(lunaRoute+`,`+deepseekRoute+`,`+solRoute))

	tests := []struct {
		name  string
		model string
		want  registry.Route
	}{
		{
			name:  "every field carried through",
			model: "luna-fast",
			want: registry.Route{
				Alias:           "luna-fast",
				BaseURL:         "https://openrouter.ai/api",
				Model:           "openai/gpt-5.6-luna",
				Auth:            registry.AuthInject,
				APIKeyEnvFile:   "OPENROUTER_API_KEY_FILE",
				ExtraBody:       []byte(`{"reasoning_effort":"low"}`),
				MaxInputTokens:  262144,
				MaxOutputTokens: 65536,
			},
		},
		{
			name:  "directory-style namespaced alias",
			model: "openrouter/sol",
			want: registry.Route{
				Alias:         "openrouter/sol",
				BaseURL:       "https://openrouter.ai/api",
				Model:         "openrouter/sol-1",
				Auth:          registry.AuthInject,
				APIKeyEnvFile: "OPENROUTER_API_KEY_FILE",
			},
		},
		{
			name:  "namespace prefix alone matches nothing",
			model: "openrouter",
			want:  registry.DefaultRoute(),
		},
		{
			name:  "booleans and omitted fields",
			model: "deepseek-flash-low",
			want: registry.Route{
				Alias:             "deepseek-flash-low",
				BaseURL:           "https://openrouter.ai/api",
				Model:             "deepseek/deepseek-v4-flash-0731",
				Auth:              registry.AuthInject,
				APIKeyEnvFile:     "OPENROUTER_API_KEY_FILE",
				StripCacheControl: true,
				CaptureBodies:     true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := bare(t, reg, tc.model); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Resolve(%q) = %+v; want %+v", tc.model, got, tc.want)
			}
		})
	}
}

func TestAliasKeysAreArbitraryNonEmptyStrings(t *testing.T) {
	t.Parallel()

	// Aliases are opaque keys. Directory-style namespacing is a naming
	// convention the registry does not interpret: nothing about a key is
	// validated beyond it being non-empty, and nothing is split on the slash.
	// Context names are the opposite — they ride in a URL path and are checked.
	const forwardRoute = `{"base_url":"https://api.anthropic.com","model":"claude-opus-5","auth":"forward"}`
	const injectRoute = `{"base_url":"https://openrouter.ai/api","model":"vendor/model-1",` +
		`"auth":"inject","api_key_env_file":"KEY_FILE"}`

	aliases := []struct {
		name  string
		alias string
		route string
	}{
		{name: "vendor namespace", alias: "openrouter/sol", route: injectRoute},
		{name: "second vendor namespace", alias: "chatgpt/luna", route: injectRoute},
		{name: "anthropic namespace pinning a claude model", alias: "anthropic/opus", route: forwardRoute},
		{name: "deeply nested", alias: "deep/nested/path/model", route: injectRoute},
		{name: "spaces", alias: "an alias with spaces", route: injectRoute},
		{name: "dots and colons", alias: "vendor.io:model-1", route: injectRoute},
		{name: "non ascii", alias: "模型/一号", route: injectRoute},
		{name: "leading slash", alias: "/leading", route: injectRoute},
		{name: "trailing slash", alias: "trailing/", route: injectRoute},
	}

	parts := make([]string, 0, len(aliases))
	for _, entry := range aliases {
		parts = append(parts, `"`+entry.alias+`":`+entry.route)
	}

	reg := newRegistry(t, personalDoc(strings.Join(parts, ",")))

	if err := reg.Err(); err != nil {
		t.Fatalf("Err() = %v; want nil — alias keys carry no character rules", err)
	}

	for _, entry := range aliases {
		t.Run(entry.name, func(t *testing.T) {
			t.Parallel()

			if got := bare(t, reg, entry.alias).Alias; got != entry.alias {
				t.Fatalf("Resolve(%q) returned alias %q; want the aliased route", entry.alias, got)
			}
		})
	}
}

func TestResolveAliasShadowsAClaudeModelID(t *testing.T) {
	t.Parallel()

	// Pinning: a forward route aliased to a real Claude model id rewrites it to
	// another Claude model. Rule 3 constrains inject routes only.
	reg := newRegistry(t, personalDoc(`"claude-sonnet-5":{`+
		`"base_url":"https://api.anthropic.com","model":"claude-opus-5","auth":"forward"}`))

	got := bare(t, reg, "claude-sonnet-5")
	want := registry.Route{
		Alias:   "claude-sonnet-5",
		BaseURL: "https://api.anthropic.com",
		Model:   "claude-opus-5",
		Auth:    registry.AuthForward,
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve(%q) = %+v; want the pinned route %+v", "claude-sonnet-5", got, want)
	}

	if err := reg.Err(); err != nil {
		t.Fatalf("Err() = %v; want nil for a valid pinning route", err)
	}
}

// aliasNames returns the Alias field of every route in order, so a listing's
// order is compared without printing whole routes.
func aliasNames(routes []registry.Route) []string {
	names := make([]string, 0, len(routes))
	for _, route := range routes {
		names = append(names, route.Alias)
	}

	return names
}

func TestAliasesListsEveryRouteSortedByAlias(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t, personalDoc(solRoute+`,`+lunaRoute+`,`+deepseekRoute))

	got := bareAliases(t, reg)
	want := []string{"deepseek-flash-low", "luna-fast", "openrouter/sol"}

	if !reflect.DeepEqual(aliasNames(got), want) {
		t.Fatalf("Aliases = %v; want %v sorted by alias", aliasNames(got), want)
	}

	luna := registry.Route{
		Alias:           "luna-fast",
		BaseURL:         "https://openrouter.ai/api",
		Model:           "openai/gpt-5.6-luna",
		Auth:            registry.AuthInject,
		APIKeyEnvFile:   "OPENROUTER_API_KEY_FILE",
		ExtraBody:       []byte(`{"reasoning_effort":"low"}`),
		MaxInputTokens:  262144,
		MaxOutputTokens: 65536,
	}

	if !reflect.DeepEqual(got[1], luna) {
		t.Fatalf("Aliases[1] = %+v; want the whole route %+v", got[1], luna)
	}
}

func TestAliasesReturnsAnIsolatedSlice(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t, personalDoc(lunaRoute))

	first := bareAliases(t, reg)
	first[0].Model = "tampered"
	first[0].Alias = "tampered"

	second := bareAliases(t, reg)
	if second[0].Model != "openai/gpt-5.6-luna" || second[0].Alias != "luna-fast" {
		t.Fatalf("Aliases after mutating an earlier result = %+v; want the registry untouched", second[0])
	}

	// A copied struct still points at the registry's own bytes. Writing through
	// that slice would corrupt the extra_body every later request merges, which
	// is exactly the isolation the doc comment promises.
	const want = `{"reasoning_effort":"low"}`

	extra := second[0].ExtraBody
	if string(extra) != want {
		t.Fatalf("Aliases extra_body = %s; want %s", extra, want)
	}

	for i := range extra {
		extra[i] = 'x'
	}

	if got := string(bare(t, reg, "luna-fast").ExtraBody); got != want {
		t.Fatalf("Resolve extra_body after writing through an Aliases result = %s; want %s", got, want)
	}

	if got := string(bareAliases(t, reg)[0].ExtraBody); got != want {
		t.Fatalf("Aliases extra_body after writing through an earlier result = %s; want %s", got, want)
	}
}

func TestAliasesReflectsAReload(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, personalDoc(lunaRoute))

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)

	reg := registry.New(path)
	if got := aliasNames(bareAliases(t, reg)); !reflect.DeepEqual(got, []string{"luna-fast"}) {
		t.Fatalf("first Aliases = %v; want [luna-fast]", got)
	}

	writeRegistry(t, path, personalDoc(lunaRoute+`,`+solRoute))
	chtimes(t, path, base.Add(time.Hour))

	if got := aliasNames(bareAliases(t, reg)); !reflect.DeepEqual(got, []string{"luna-fast", "openrouter/sol"}) {
		t.Fatalf("Aliases after a reload = %v; want [luna-fast openrouter/sol]", got)
	}
}

func TestAliasesIsEmptyWhenThereAreNoForeignRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		write   bool
		wantErr bool
	}{
		{name: "missing file", write: false},
		{name: "empty routes object", body: personalDoc(``), write: true},
		{name: "unparsable file", body: `{"contexts":{`, write: true, wantErr: true},
		{
			name:    "file failing validation",
			body:    personalDoc(`"bad":{"auth":"nonsense"}`),
			write:   true,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "registry.json")
			if tc.write {
				writeRegistry(t, path, tc.body)
			}

			reg := registry.New(path)
			if got := bareAliases(t, reg); len(got) != 0 {
				t.Fatalf("Aliases = %+v; want an empty slice", got)
			}

			if err := reg.Err(); (err != nil) != tc.wantErr {
				t.Fatalf("Err() = %v; wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestErrReportsRegistryHealth(t *testing.T) {
	t.Parallel()

	t.Run("missing file is healthy", func(t *testing.T) {
		t.Parallel()

		reg := registry.New(filepath.Join(t.TempDir(), "absent.json"))
		if err := reg.Err(); err != nil {
			t.Fatalf("Err() = %v; want nil for a missing registry file", err)
		}
	})

	t.Run("valid file is healthy", func(t *testing.T) {
		t.Parallel()

		reg := newRegistry(t, personalDoc(lunaRoute))
		if err := reg.Err(); err != nil {
			t.Fatalf("Err() = %v; want nil for a valid registry file", err)
		}
	})

	t.Run("broken file reports without a prior resolve", func(t *testing.T) {
		t.Parallel()

		reg := newRegistry(t, `{"contexts":{`)
		if err := reg.Err(); err == nil {
			t.Fatal("Err() = nil; want a parse error before any Resolve call")
		}
	})
}

func TestRegistryRecoversWhenTheFileIsFixed(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, personalDoc(`"bad":{"auth":"nonsense"}`))
	reg := registry.New(path)

	assertDefault(t, reg, "luna-fast")

	if err := reg.Err(); err == nil {
		t.Fatal("Err() = nil; want a validation error for the broken file")
	}

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)
	writeRegistry(t, path, personalDoc(lunaRoute))
	chtimes(t, path, base.Add(time.Hour))

	if got := bare(t, reg, "luna-fast"); got.Model != "openai/gpt-5.6-luna" {
		t.Fatalf("Resolve after repair = %+v; want the luna-fast route", got)
	}

	if err := reg.Err(); err != nil {
		t.Fatalf("Err() = %v; want nil once the file is valid again", err)
	}
}

// chtimes pins both timestamps of path so reload tests never depend on
// filesystem clock granularity.
func chtimes(t *testing.T, path string, when time.Time) {
	t.Helper()

	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// pinnedContext is a one-route personal context whose alias "pin" rewrites to
// model, used by the reload tests where only the model may differ.
func pinnedContext(model string) string {
	return personalDoc(`"pin":{"base_url":"https://api.anthropic.com","model":"` + model + `","auth":"forward"}`)
}

func TestReloadOnModTimeChange(t *testing.T) {
	t.Parallel()

	// Both bodies are byte-identical in length, so size cannot explain a reload.
	first := pinnedContext("model-one")
	second := pinnedContext("model-two")

	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, first)

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)

	reg := registry.New(path)
	if got := bare(t, reg, "pin").Model; got != "model-one" {
		t.Fatalf("first Resolve model = %q; want %q", got, "model-one")
	}

	writeRegistry(t, path, second)
	chtimes(t, path, base.Add(time.Hour))

	if got := bare(t, reg, "pin").Model; got != "model-two" {
		t.Fatalf("Resolve after mtime change = %q; want the reloaded %q", got, "model-two")
	}
}

func TestResolveUsesTheCacheWhenNothingChanged(t *testing.T) {
	t.Parallel()

	first := pinnedContext("model-one")
	second := pinnedContext("model-two")

	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, first)

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)

	reg := registry.New(path)
	if got := bare(t, reg, "pin").Model; got != "model-one" {
		t.Fatalf("first Resolve model = %q; want %q", got, "model-one")
	}

	// Same size, same mtime, different content: the cached parse must stand.
	writeRegistry(t, path, second)
	chtimes(t, path, base)

	if got := bare(t, reg, "pin").Model; got != "model-one" {
		t.Fatalf("Resolve with an unchanged stat = %q; want the cached %q", got, "model-one")
	}
}

func TestReloadOnSymlinkRetarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	one := writeRegistry(t, filepath.Join(dir, "one.json"), pinnedContext("model-one"))
	two := writeRegistry(t, filepath.Join(dir, "two.json"), pinnedContext("model-two"))

	// Identical size and mtime on both targets: only the resolved symlink target
	// distinguishes them, which is what a /nix/store swap looks like.
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, one, base)
	chtimes(t, two, base)

	link := filepath.Join(dir, "current.json")
	if err := os.Symlink(one, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, one, err)
	}

	reg := registry.New(link)
	if got := bare(t, reg, "pin").Model; got != "model-one" {
		t.Fatalf("first Resolve model = %q; want %q", got, "model-one")
	}

	if err := os.Remove(link); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}

	if err := os.Symlink(two, link); err != nil {
		t.Fatalf("retarget symlink %s -> %s: %v", link, two, err)
	}

	if got := bare(t, reg, "pin").Model; got != "model-two" {
		t.Fatalf("Resolve after symlink retarget = %q; want the reloaded %q", got, "model-two")
	}
}

func TestReloadWhenTheFileDisappears(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, personalDoc(lunaRoute))

	reg := registry.New(path)
	if got := bare(t, reg, "luna-fast").Alias; got != "luna-fast" {
		t.Fatalf("first Resolve alias = %q; want %q", got, "luna-fast")
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove registry: %v", err)
	}

	assertDefault(t, reg, "luna-fast")

	if err := reg.Err(); err != nil {
		t.Fatalf("Err() = %v; want nil after the registry file is removed", err)
	}
}

func TestUnreadableRegistryIsFailClosed(t *testing.T) {
	t.Parallel()

	// A directory where the registry file belongs: present, stat-able, and
	// impossible to read. Like every other broken registry, it costs the foreign
	// aliases and nothing else.
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}

	reg := registry.New(path)

	assertDefault(t, reg, "luna-fast")

	if err := reg.Err(); err == nil {
		t.Fatal("Err() = nil; want a read error for an unreadable registry path")
	}
}

func TestConcurrentResolveIsSafe(t *testing.T) {
	t.Parallel()

	const workers = 8
	const rounds = 50

	reg := newRegistry(t, contextDoc(
		`"personal":{"routes":{`+lunaRoute+`}},"work":{`+injectDefault+`,"routes":{}}`))

	var wg sync.WaitGroup
	start := make(chan struct{})

	for range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()
			<-start

			for range rounds {
				if route := bare(t, reg, "luna-fast"); route.Model != "openai/gpt-5.6-luna" {
					t.Errorf("Resolve model = %q; want %q", route.Model, "openai/gpt-5.6-luna")

					return
				}

				if route := bare(t, reg, "claude-sonnet-5"); route.BaseURL != "https://api.anthropic.com" {
					t.Errorf("Resolve of an unmatched model = %+v; want the default route", route)

					return
				}

				work := registry.Selection{Name: "work", Explicit: true}
				if route := resolve(t, reg, work, "claude-sonnet-5").Route; route.Auth != registry.AuthInject {
					t.Errorf("Resolve in the work context = %+v; want its inject default route", route)

					return
				}

				if err := reg.Err(); err != nil {
					t.Errorf("Err() = %v; want nil for a valid registry", err)

					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()
}

// TestResolveSelectsTheNamedContext pins the core of the contexts feature: the
// same model name resolves to a different route in each context, and a context
// resolves its own default route for a model no alias in it claims.
func TestResolveSelectsTheNamedContext(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t, contextDoc(
		`"personal":{"routes":{`+lunaRoute+`}},`+
			`"work":{`+injectDefault+`,"routes":{`+deepseekRoute+`}},`+
			`"pinned":{"default_route":{"base_url":"https://api.anthropic.com",`+
			`"model":"claude-opus-5","auth":"forward"},"routes":{}}`))

	tests := []struct {
		name         string
		sel          registry.Selection
		model        string
		wantContext  string
		wantAlias    string
		wantUpstream string
		wantModel    string
	}{
		{
			name:         "bare path uses the personal context",
			sel:          registry.Selection{},
			model:        "luna-fast",
			wantContext:  "personal",
			wantAlias:    "luna-fast",
			wantUpstream: "https://openrouter.ai/api",
			wantModel:    "openai/gpt-5.6-luna",
		},
		{
			name:         "a personal alias is invisible to another context",
			sel:          registry.Selection{Name: "work", Explicit: true},
			model:        "luna-fast",
			wantContext:  "work",
			wantAlias:    "",
			wantUpstream: "https://openrouter.ai/api",
			wantModel:    "openai/gpt-5.6-luna",
		},
		{
			name:         "a named context resolves its own alias",
			sel:          registry.Selection{Name: "work", Explicit: true},
			model:        "deepseek-flash-low",
			wantContext:  "work",
			wantAlias:    "deepseek-flash-low",
			wantUpstream: "https://openrouter.ai/api",
			wantModel:    "deepseek/deepseek-v4-flash-0731",
		},
		{
			name:         "an unmatched model rides the context default route",
			sel:          registry.Selection{Name: "pinned", Explicit: true},
			model:        "claude-sonnet-5",
			wantContext:  "pinned",
			wantAlias:    "",
			wantUpstream: "https://api.anthropic.com",
			wantModel:    "claude-opus-5",
		},
		{
			name:         "a context with no default route falls to the built-in one",
			sel:          registry.Selection{Name: "personal", Explicit: true},
			model:        "claude-sonnet-5",
			wantContext:  "personal",
			wantAlias:    "",
			wantUpstream: "https://api.anthropic.com",
			wantModel:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res := resolve(t, reg, tc.sel, tc.model)

			if res.Context != tc.wantContext {
				t.Fatalf("Resolution.Context = %q; want %q", res.Context, tc.wantContext)
			}

			if res.Route.Alias != tc.wantAlias {
				t.Fatalf("Resolution.Route.Alias = %q; want %q", res.Route.Alias, tc.wantAlias)
			}

			if res.Route.BaseURL != tc.wantUpstream {
				t.Fatalf("Resolution.Route.BaseURL = %q; want %q", res.Route.BaseURL, tc.wantUpstream)
			}

			if res.Route.Model != tc.wantModel {
				t.Fatalf("Resolution.Route.Model = %q; want %q", res.Route.Model, tc.wantModel)
			}
		})
	}
}

// TestResolveCarriesTheContextDefaultAndAliases pins the metadata both the
// proxy and the model endpoints read off one resolution, so introspection and
// forwarding can never disagree about what a context holds.
func TestResolveCarriesTheContextDefaultAndAliases(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t, contextDoc(
		`"personal":{"routes":{`+lunaRoute+`}},`+
			`"work":{`+injectDefault+`,"routes":{`+solRoute+`,`+deepseekRoute+`}}`))

	personal := resolve(t, reg, registry.Selection{}, "")
	if !reflect.DeepEqual(personal.DefaultRoute, registry.DefaultRoute()) {
		t.Fatalf("personal DefaultRoute = %+v; want the built-in %+v",
			personal.DefaultRoute, registry.DefaultRoute())
	}

	if got := aliasNames(personal.Aliases); !reflect.DeepEqual(got, []string{"luna-fast"}) {
		t.Fatalf("personal Aliases = %v; want [luna-fast]", got)
	}

	work := resolve(t, reg, registry.Selection{Name: "work", Explicit: true}, "")
	if work.DefaultRoute.Auth != registry.AuthInject ||
		work.DefaultRoute.Model != "openai/gpt-5.6-luna" ||
		work.DefaultRoute.MaxInputTokens != 262144 {
		t.Fatalf("work DefaultRoute = %+v; want the declared inject default", work.DefaultRoute)
	}

	if got := aliasNames(work.Aliases); !reflect.DeepEqual(got, []string{"deepseek-flash-low", "openrouter/sol"}) {
		t.Fatalf("work Aliases = %v; want the work context's own aliases sorted", got)
	}
}

// TestResolveFailsClosedOnAnUnknownContext is the security property: a request
// that names a context patchbay cannot resolve is refused, never quietly served
// by another context or by the built-in default route.
func TestResolveFailsClosedOnAnUnknownContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		body  string
		write bool
		sel   registry.Selection
	}{
		{
			name:  "name no context claims",
			body:  contextDoc(`"personal":{"routes":{` + lunaRoute + `}}`),
			write: true,
			sel:   registry.Selection{Name: "work", Explicit: true},
		},
		{
			name:  "registry that failed to parse",
			body:  `{"contexts":{`,
			write: true,
			sel:   registry.Selection{Name: "personal", Explicit: true},
		},
		{
			name:  "registry that failed validation",
			body:  personalDoc(`"bad":{"auth":"nonsense"}`),
			write: true,
			sel:   registry.Selection{Name: "personal", Explicit: true},
		},
		{
			name:  "registry file that is not there",
			write: false,
			sel:   registry.Selection{Name: "personal", Explicit: true},
		},
		{
			name:  "empty name",
			body:  contextDoc(`"personal":{"routes":{}}`),
			write: true,
			sel:   registry.Selection{Name: "", Explicit: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "registry.json")
			if tc.write {
				writeRegistry(t, path, tc.body)
			}

			res, err := registry.New(path).Resolve(tc.sel, "claude-sonnet-5")
			if !errors.Is(err, registry.ErrUnknownContext) {
				t.Fatalf("Resolve(%+v) error = %v; want ErrUnknownContext", tc.sel, err)
			}

			if !strings.Contains(err.Error(), tc.sel.Name) {
				t.Fatalf("Resolve error = %q; want it to name the context %q", err, tc.sel.Name)
			}

			if !reflect.DeepEqual(res.Route, registry.Route{}) {
				t.Fatalf("Resolve returned route %+v; want no route at all", res.Route)
			}
		})
	}
}

// TestReloadAddsAndRemovesWholeContexts covers hot reload at the document
// level: the whole contexts map is re-read, so a context an operator adds is
// resolvable on the next request and one they remove fails closed on it.
func TestReloadAddsAndRemovesWholeContexts(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, contextDoc(`"personal":{"routes":{`+lunaRoute+`}}`))

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)

	reg := registry.New(path)
	work := registry.Selection{Name: "work", Explicit: true}

	if _, err := reg.Resolve(work, "claude-sonnet-5"); !errors.Is(err, registry.ErrUnknownContext) {
		t.Fatalf("Resolve before the context exists = %v; want ErrUnknownContext", err)
	}

	writeRegistry(t, path, contextDoc(
		`"personal":{"routes":{`+lunaRoute+`}},"work":{`+injectDefault+`,"routes":{`+solRoute+`}}`))
	chtimes(t, path, base.Add(time.Hour))

	added := resolve(t, reg, work, "openrouter/sol")
	if added.Route.Alias != "openrouter/sol" {
		t.Fatalf("Resolve after the context was added = %+v; want its own alias", added.Route)
	}

	if added.DefaultRoute.Model != "openai/gpt-5.6-luna" {
		t.Fatalf("DefaultRoute after the reload = %+v; want the declared default", added.DefaultRoute)
	}

	writeRegistry(t, path, contextDoc(`"personal":{"routes":{`+lunaRoute+`}}`))
	chtimes(t, path, base.Add(2*time.Hour))

	if _, err := reg.Resolve(work, "openrouter/sol"); !errors.Is(err, registry.ErrUnknownContext) {
		t.Fatalf("Resolve after the context was removed = %v; want ErrUnknownContext", err)
	}

	// Removing a context never costs the bare path its default route.
	assertDefault(t, reg, "claude-sonnet-5")
}
