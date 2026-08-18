package registry_test

import (
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

// assertDefault fails unless resolving model yields exactly the built-in route.
func assertDefault(t *testing.T, reg *registry.Registry, model string) {
	t.Helper()

	if got := reg.Resolve(model); !reflect.DeepEqual(got, registry.DefaultRoute()) {
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
	reg := newRegistry(t, `{"routes":{"":{"base_url":"https://evil.example"}}}`)

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
			body:  `{"routes":{`,
			write: true,
			model: "claude-sonnet-5",
		},
		{
			name:  "file failing validation",
			body:  `{"routes":{` + lunaRoute + `,"bad":{"base_url":"https://evil.example","auth":"forward"}}}`,
			write: true,
			model: "luna-fast",
		},
		{
			name:  "valid file with no matching alias",
			body:  `{"routes":{` + lunaRoute + `}}`,
			write: true,
			model: "claude-opus-5",
		},
		{
			name:  "empty routes object",
			body:  `{"routes":{}}`,
			write: true,
			model: "anything",
		},
		{
			name:  "file without a routes key",
			body:  `{}`,
			write: true,
			model: "anything",
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

	reg := newRegistry(t, `{"routes":{`+lunaRoute+`,`+deepseekRoute+`,`+solRoute+`}}`)

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

			if got := reg.Resolve(tc.model); !reflect.DeepEqual(got, tc.want) {
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

	reg := newRegistry(t, `{"routes":{`+strings.Join(parts, ",")+`}}`)

	if err := reg.Err(); err != nil {
		t.Fatalf("Err() = %v; want nil — alias keys carry no character rules", err)
	}

	for _, entry := range aliases {
		t.Run(entry.name, func(t *testing.T) {
			t.Parallel()

			if got := reg.Resolve(entry.alias).Alias; got != entry.alias {
				t.Fatalf("Resolve(%q) returned alias %q; want the aliased route", entry.alias, got)
			}
		})
	}
}

func TestResolveAliasShadowsAClaudeModelID(t *testing.T) {
	t.Parallel()

	// Pinning: a forward route aliased to a real Claude model id rewrites it to
	// another Claude model. Rule 3 constrains inject routes only.
	reg := newRegistry(t, `{"routes":{"claude-sonnet-5":{`+
		`"base_url":"https://api.anthropic.com","model":"claude-opus-5","auth":"forward"}}}`)

	got := reg.Resolve("claude-sonnet-5")
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

	reg := newRegistry(t, `{"routes":{`+solRoute+`,`+lunaRoute+`,`+deepseekRoute+`}}`)

	got := reg.Aliases()
	want := []string{"deepseek-flash-low", "luna-fast", "openrouter/sol"}

	if !reflect.DeepEqual(aliasNames(got), want) {
		t.Fatalf("Aliases() aliases = %v; want %v sorted by alias", aliasNames(got), want)
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
		t.Fatalf("Aliases()[1] = %+v; want the whole route %+v", got[1], luna)
	}
}

func TestAliasesReturnsAnIsolatedSlice(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t, `{"routes":{`+lunaRoute+`}}`)

	first := reg.Aliases()
	first[0].Model = "tampered"
	first[0].Alias = "tampered"

	second := reg.Aliases()
	if second[0].Model != "openai/gpt-5.6-luna" || second[0].Alias != "luna-fast" {
		t.Fatalf("Aliases() after mutating an earlier result = %+v; want the registry untouched", second[0])
	}

	// A copied struct still points at the registry's own bytes. Writing through
	// that slice would corrupt the extra_body every later request merges, which
	// is exactly the isolation the doc comment promises.
	const want = `{"reasoning_effort":"low"}`

	extra := second[0].ExtraBody
	if string(extra) != want {
		t.Fatalf("Aliases() extra_body = %s; want %s", extra, want)
	}

	for i := range extra {
		extra[i] = 'x'
	}

	if got := string(reg.Resolve("luna-fast").ExtraBody); got != want {
		t.Fatalf("Resolve() extra_body after writing through an Aliases() result = %s; want %s", got, want)
	}

	if got := string(reg.Aliases()[0].ExtraBody); got != want {
		t.Fatalf("Aliases() extra_body after writing through an earlier result = %s; want %s", got, want)
	}
}

func TestAliasesReflectsAReload(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, `{"routes":{`+lunaRoute+`}}`)

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)

	reg := registry.New(path)
	if got := aliasNames(reg.Aliases()); !reflect.DeepEqual(got, []string{"luna-fast"}) {
		t.Fatalf("first Aliases() = %v; want [luna-fast]", got)
	}

	writeRegistry(t, path, `{"routes":{`+lunaRoute+`,`+solRoute+`}}`)
	chtimes(t, path, base.Add(time.Hour))

	if got := aliasNames(reg.Aliases()); !reflect.DeepEqual(got, []string{"luna-fast", "openrouter/sol"}) {
		t.Fatalf("Aliases() after a reload = %v; want [luna-fast openrouter/sol]", got)
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
		{name: "empty routes object", body: `{"routes":{}}`, write: true},
		{name: "unparsable file", body: `{"routes":{`, write: true, wantErr: true},
		{name: "file failing validation", body: `{"routes":{"bad":{"auth":"nonsense"}}}`, write: true, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "registry.json")
			if tc.write {
				writeRegistry(t, path, tc.body)
			}

			reg := registry.New(path)
			if got := reg.Aliases(); len(got) != 0 {
				t.Fatalf("Aliases() = %+v; want an empty slice", got)
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

		reg := newRegistry(t, `{"routes":{`+lunaRoute+`}}`)
		if err := reg.Err(); err != nil {
			t.Fatalf("Err() = %v; want nil for a valid registry file", err)
		}
	})

	t.Run("broken file reports without a prior resolve", func(t *testing.T) {
		t.Parallel()

		reg := newRegistry(t, `{"routes":{`)
		if err := reg.Err(); err == nil {
			t.Fatal("Err() = nil; want a parse error before any Resolve call")
		}
	})
}

func TestRegistryRecoversWhenTheFileIsFixed(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, `{"routes":{"bad":{"auth":"nonsense"}}}`)
	reg := registry.New(path)

	assertDefault(t, reg, "luna-fast")

	if err := reg.Err(); err == nil {
		t.Fatal("Err() = nil; want a validation error for the broken file")
	}

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)
	writeRegistry(t, path, `{"routes":{`+lunaRoute+`}}`)
	chtimes(t, path, base.Add(time.Hour))

	if got := reg.Resolve("luna-fast"); got.Model != "openai/gpt-5.6-luna" {
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

func TestReloadOnModTimeChange(t *testing.T) {
	t.Parallel()

	// Both bodies are byte-identical in length, so size cannot explain a reload.
	const first = `{"routes":{"pin":{"base_url":"https://api.anthropic.com","model":"model-one","auth":"forward"}}}`
	const second = `{"routes":{"pin":{"base_url":"https://api.anthropic.com","model":"model-two","auth":"forward"}}}`

	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, first)

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)

	reg := registry.New(path)
	if got := reg.Resolve("pin").Model; got != "model-one" {
		t.Fatalf("first Resolve model = %q; want %q", got, "model-one")
	}

	writeRegistry(t, path, second)
	chtimes(t, path, base.Add(time.Hour))

	if got := reg.Resolve("pin").Model; got != "model-two" {
		t.Fatalf("Resolve after mtime change = %q; want the reloaded %q", got, "model-two")
	}
}

func TestResolveUsesTheCacheWhenNothingChanged(t *testing.T) {
	t.Parallel()

	const first = `{"routes":{"pin":{"base_url":"https://api.anthropic.com","model":"model-one","auth":"forward"}}}`
	const second = `{"routes":{"pin":{"base_url":"https://api.anthropic.com","model":"model-two","auth":"forward"}}}`

	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, first)

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	chtimes(t, path, base)

	reg := registry.New(path)
	if got := reg.Resolve("pin").Model; got != "model-one" {
		t.Fatalf("first Resolve model = %q; want %q", got, "model-one")
	}

	// Same size, same mtime, different content: the cached parse must stand.
	writeRegistry(t, path, second)
	chtimes(t, path, base)

	if got := reg.Resolve("pin").Model; got != "model-one" {
		t.Fatalf("Resolve with an unchanged stat = %q; want the cached %q", got, "model-one")
	}
}

func TestReloadOnSymlinkRetarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	one := writeRegistry(t, filepath.Join(dir, "one.json"),
		`{"routes":{"pin":{"base_url":"https://api.anthropic.com","model":"model-one","auth":"forward"}}}`)
	two := writeRegistry(t, filepath.Join(dir, "two.json"),
		`{"routes":{"pin":{"base_url":"https://api.anthropic.com","model":"model-two","auth":"forward"}}}`)

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
	if got := reg.Resolve("pin").Model; got != "model-one" {
		t.Fatalf("first Resolve model = %q; want %q", got, "model-one")
	}

	if err := os.Remove(link); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}

	if err := os.Symlink(two, link); err != nil {
		t.Fatalf("retarget symlink %s -> %s: %v", link, two, err)
	}

	if got := reg.Resolve("pin").Model; got != "model-two" {
		t.Fatalf("Resolve after symlink retarget = %q; want the reloaded %q", got, "model-two")
	}
}

func TestReloadWhenTheFileDisappears(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, `{"routes":{`+lunaRoute+`}}`)

	reg := registry.New(path)
	if got := reg.Resolve("luna-fast").Alias; got != "luna-fast" {
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

	reg := newRegistry(t, `{"routes":{`+lunaRoute+`}}`)

	var wg sync.WaitGroup
	start := make(chan struct{})

	for range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()
			<-start

			for range rounds {
				if route := reg.Resolve("luna-fast"); route.Model != "openai/gpt-5.6-luna" {
					t.Errorf("Resolve model = %q; want %q", route.Model, "openai/gpt-5.6-luna")

					return
				}

				if route := reg.Resolve("claude-sonnet-5"); route.BaseURL != "https://api.anthropic.com" {
					t.Errorf("Resolve of an unmatched model = %+v; want the default route", route)

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
