package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func envMap(values map[string]string) lookupEnvFunc {
	return func(name string) (string, bool) { v, ok := values[name]; return v, ok }
}
func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// homeConfig is what every environment resolving through /home/tester's own
// conventional paths produces, which is most of the matrix below.
func homeConfig() config {
	return config{
		listen:    defaultListen,
		registry:  "/home/tester/.config/patchbay/routes.json",
		ledgerDir: "/home/tester/.local/state/patchbay/ledger",
	}
}

// TestResolveConfig walks every branch of the resolution: an explicit setting
// always wins, the XDG directories decide next, and the home directory's
// conventional paths are the last fallback.
func TestResolveConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		env  map[string]string
		want config
	}{
		{
			name: "xdg directories",
			env: map[string]string{
				envConfigXDG: "/xdg/config",
				envStateXDG:  "/xdg/state",
				envHome:      "/home/tester",
			},
			want: config{
				listen:    defaultListen,
				registry:  "/xdg/config/patchbay/routes.json",
				ledgerDir: "/xdg/state/patchbay/ledger",
			},
		},
		{
			name: "home fallback when xdg is unset",
			env:  map[string]string{envHome: "/home/tester"},
			want: homeConfig(),
		},
		{
			// An empty XDG variable is not a directory, and honoring it would
			// resolve the registry to "/patchbay/routes.json".
			name: "home fallback when xdg is set but empty",
			env: map[string]string{
				envConfigXDG: "",
				envStateXDG:  "",
				envHome:      "/home/tester",
			},
			want: homeConfig(),
		},
		{
			name: "every setting overridden",
			env: map[string]string{
				envConfigXDG: "/xdg/config",
				envStateXDG:  "/xdg/state",
				envHome:      "/home/tester",
				envListen:    "127.0.0.1:9999",
				envRegistry:  "/etc/patchbay/routes.json",
				envLedgerDir: "/var/lib/patchbay",
			},
			want: config{
				listen:    "127.0.0.1:9999",
				registry:  "/etc/patchbay/routes.json",
				ledgerDir: "/var/lib/patchbay",
			},
		},
		{
			name: "an empty listen override falls back to the default",
			env:  map[string]string{envHome: "/home/tester", envListen: ""},
			want: homeConfig(),
		},
		{
			// Both paths named explicitly: there is nothing left to resolve, so
			// the absence of a home directory costs nothing.
			name: "explicit registry and ledger need no home at all",
			env: map[string]string{
				envRegistry:  "/etc/patchbay/routes.json",
				envLedgerDir: "/var/lib/patchbay",
			},
			want: config{
				listen:    defaultListen,
				registry:  "/etc/patchbay/routes.json",
				ledgerDir: "/var/lib/patchbay",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveConfig(envMap(tc.env))
			if err != nil {
				t.Fatalf("resolveConfig() error = %v; want nil", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("resolveConfig() = %+v; want %+v", got, tc.want)
			}
		})
	}
}

// TestResolveConfigWithNowhereToLook pins the fatal half. With neither an
// override, nor an XDG directory, nor a home, there is no honest path to fall
// back to — and inventing one would write somebody's records somewhere they
// never asked for.
func TestResolveConfigWithNowhereToLook(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{name: "nothing set at all", env: map[string]string{}},
		{
			name: "registry override only",
			env:  map[string]string{envRegistry: "/etc/patchbay/routes.json"},
		},
		{
			name: "ledger override only",
			env:  map[string]string{envLedgerDir: "/var/lib/patchbay"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := resolveConfig(envMap(tc.env)); err == nil {
				t.Fatal("resolveConfig() error = nil; want a fatal misconfiguration")
			}
		})
	}
}

func TestServeValidatesRegistryBeforeListenerBind(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	err := serveGateway(
		t.Context(),
		envMap(
			map[string]string{
				envListen:    "not-an-address",
				envRegistry:  missing,
				envLedgerDir: filepath.Join(dir, "ledger"),
			},
		),
		quietLogger(),
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "registry") ||
		strings.Contains(err.Error(), "listen on") {
		t.Fatalf("error=%v", err)
	}

	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = serveGateway(
		t.Context(),
		envMap(
			map[string]string{
				envListen:    "not-an-address",
				envRegistry:  invalid,
				envLedgerDir: filepath.Join(dir, "ledger"),
			},
		),
		quietLogger(),
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "default_context") ||
		strings.Contains(err.Error(), "listen on") {
		t.Fatalf("error=%v", err)
	}
}

// TestServeFailsOnAnUnusableListenAddress pins the other fatal startup. The
// registry is written and valid, so the run really does get as far as
// net.Listen — a test pointing at a registry that does not exist would fail one
// step earlier and prove nothing about binding at all, while still passing.
// That is why each case asserts both halves: the address is named, and the
// registry never entered into it.
func TestServeFailsOnAnUnusableListenAddress(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "registry.json")
	writeFile(t, registryPath, testRegistry)

	for _, tc := range []struct{ name, listen string }{
		{name: "port out of range", listen: "127.0.0.1:99999"},
		{name: "not an address at all", listen: "not-an-address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := serveGateway(
				t.Context(),
				envMap(map[string]string{
					envListen:    tc.listen,
					envRegistry:  registryPath,
					envLedgerDir: filepath.Join(dir, "ledger"),
				}),
				quietLogger(),
				nil,
			)
			if err == nil {
				t.Fatalf("serveGateway() with listen %q error = nil; want a bind failure", tc.listen)
			}
			if !strings.Contains(err.Error(), "listen on") ||
				!strings.Contains(err.Error(), tc.listen) {
				t.Fatalf(
					"serveGateway() = %v; want a listen failure naming the address it could not bind",
					err,
				)
			}
			if strings.Contains(err.Error(), "load registry") {
				t.Fatalf("serveGateway() = %v; want the run to have reached the bind", err)
			}
		})
	}
}

// A gateway that reaches the listening state, serves real requests and shuts
// down cleanly is pinned end to end by TestGatewayServesEndToEnd, which observes
// all three. Nothing here cancels a context before serving to claim any of them.
