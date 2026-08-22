package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
		usageDB:   "/home/tester/.local/state/patchbay/usage.sqlite",
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
				usageDB:   "/xdg/state/patchbay/usage.sqlite",
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
				envUsageDB:   "/var/lib/patchbay/usage.sqlite",
				envRateCards: "/etc/patchbay/rates.json",
			},
			want: config{
				listen:    "127.0.0.1:9999",
				registry:  "/etc/patchbay/routes.json",
				ledgerDir: "/var/lib/patchbay",
				usageDB:   "/var/lib/patchbay/usage.sqlite",
				rateCards: "/etc/patchbay/rates.json",
			},
		},
		{
			name: "an empty listen override falls back to the default",
			env:  map[string]string{envHome: "/home/tester", envListen: ""},
			want: homeConfig(),
		},
		{
			// Every path named explicitly: there is nothing left to resolve, so
			// the absence of a home directory costs nothing. The usage database
			// counts among them — it resolves against the same state directory the
			// ledger does, so naming only two of the three still needs a home.
			name: "explicit registry, ledger and usage database need no home at all",
			env: map[string]string{
				envRegistry:  "/etc/patchbay/routes.json",
				envLedgerDir: "/var/lib/patchbay",
				envUsageDB:   "/var/lib/patchbay/usage.sqlite",
			},
			want: config{
				listen:    defaultListen,
				registry:  "/etc/patchbay/routes.json",
				ledgerDir: "/var/lib/patchbay",
				usageDB:   "/var/lib/patchbay/usage.sqlite",
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
		{
			// The usage database is the third path, not a free rider on the other
			// two: with the registry and ledger named but no state directory to
			// resolve it against, there is still nowhere honest to put it.
			name: "registry and ledger named but the usage database unresolvable",
			env: map[string]string{
				envRegistry:  "/etc/patchbay/routes.json",
				envLedgerDir: "/var/lib/patchbay",
			},
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
				envUsageDB:   filepath.Join(dir, "usage.sqlite"),
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
				envUsageDB:   filepath.Join(dir, "usage.sqlite"),
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
					envUsageDB:   filepath.Join(t.TempDir(), "usage.sqlite"),
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

// servableRegistry writes a valid registry and returns its path, so a test
// about a later startup step really reaches that step. Registry loading is the
// first fatal check, so a test pointing at a file that does not exist would
// fail there and prove nothing about what it meant to pin.
func servableRegistry(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routes.json")
	writeFile(t, path, testRegistry)
	return path
}

func TestLoadRateCardsFileParsesExactJSONRates(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "rates.json")
	writeFile(t, path, `[
		{
			"model":"openrouter/model",
			"effective_from":"2026-05-01T12:00:00Z",
			"source":"provider price sheet",
			"rates_usd_per_million":{
				"input":"3.00","output":"15.00","cache_read":"0.30",
				"cache_creation":"3.75","cache_creation_5m":"3.75","cache_creation_1h":"6.00"
			}
		}
	]`)
	cards, err := loadRateCardsFile(path)
	if err != nil {
		t.Fatalf("loadRateCardsFile() returned error: %v", err)
	}
	if len(cards) != 1 || cards[0].InputNanoUSDPerMillion != 3_000_000_000 ||
		cards[0].EffectiveFrom.Format(time.RFC3339) != "2026-05-01T12:00:00Z" {
		t.Fatalf("parsed cards = %#v; want exact rate and timestamp", cards)
	}
}

func TestLoadRateCardsFileRejectsMissingOrNonStringRates(t *testing.T) {
	t.Parallel()

	for _, contents := range []string{
		`[{"model":"model","effective_from":"2026-05-01T12:00:00Z","source":"test","rates_usd_per_million":{}}]`,
		`[{"model":"model","effective_from":"2026-05-01T12:00:00Z","source":"test","rates_usd_per_million":{"input":3}}]`,
	} {
		path := filepath.Join(t.TempDir(), "rates.json")
		writeFile(t, path, contents)
		if _, err := loadRateCardsFile(path); err == nil {
			t.Fatal("loadRateCardsFile() returned nil error")
		}
	}
}

// TestServeFailsClosedOnAnUnusableRateCardsFile pins both halves of the
// rate-card startup contract. A file that is valid JSON but not a list of cards
// is as fatal as one that is not there at all: rates that cannot be established
// would silently become unknown cost on every request, and a gateway that will
// not price its traffic does not serve.
func TestServeFailsClosedOnAnUnusableRateCardsFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	null := filepath.Join(dir, "null-rates.json")
	writeFile(t, null, "null")

	for _, tc := range []struct{ name, rateCards string }{
		{name: "a JSON null is not a list of cards", rateCards: null},
		{name: "no file at all", rateCards: filepath.Join(dir, "missing-rates.json")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := serveGateway(t.Context(), envMap(map[string]string{
				envListen:    "127.0.0.1:0",
				envRegistry:  servableRegistry(t),
				envLedgerDir: filepath.Join(t.TempDir(), "ledger"),
				envUsageDB:   filepath.Join(t.TempDir(), "usage.sqlite"),
				envRateCards: tc.rateCards,
			}), quietLogger(), nil)
			if err == nil || !strings.Contains(err.Error(), "load rate cards") {
				t.Fatalf("serveGateway() error = %v; want a fatal rate-card load error", err)
			}
		})
	}
}

// TestServeFailsOnAnUnusableUsageDatabase pins the accounting store as a fatal
// startup dependency rather than a best-effort one. The ledger costs only its
// records when it cannot be opened; the usage database is the durable evidence
// that upstream contact is allowed, so a machine that cannot account does not
// serve at all.
func TestServeFailsOnAnUnusableUsageDatabase(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	writeFile(t, blocker, "in the way")

	err := serveGateway(t.Context(), envMap(map[string]string{
		envListen:    "127.0.0.1:0",
		envRegistry:  servableRegistry(t),
		envLedgerDir: filepath.Join(dir, "ledger"),
		envUsageDB:   filepath.Join(blocker, "usage.sqlite"),
	}), quietLogger(), nil)
	if err == nil || !strings.Contains(err.Error(), "usage database") {
		t.Fatalf("serveGateway() error = %v; want the unusable usage database named", err)
	}
	if strings.Contains(err.Error(), "listen on") {
		t.Fatalf("serveGateway() = %v; want the failure before the bind", err)
	}
}

// TestServeFailsWhenHostnameCannotBeResolved pins the last fatal startup step.
// Every accounting row and every ledger record is stamped with the machine that
// made the request, so a machine that cannot name itself would attribute its
// spending to nobody.
func TestServeFailsWhenHostnameCannotBeResolved(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		look func() (string, error)
	}{
		{
			name: "the lookup fails",
			look: func() (string, error) { return "", errors.New("hostname unavailable") },
		},
		{
			name: "the lookup succeeds with nothing",
			look: func() (string, error) { return "", nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			err := serveGatewayWithHostname(t.Context(), envMap(map[string]string{
				envListen:    "127.0.0.1:0",
				envRegistry:  servableRegistry(t),
				envLedgerDir: filepath.Join(dir, "ledger"),
				envUsageDB:   filepath.Join(dir, "usage.sqlite"),
			}), quietLogger(), nil, tc.look)
			if err == nil || !strings.Contains(err.Error(), "hostname") {
				t.Fatalf(
					"serveGatewayWithHostname() error = %v; want the hostname resolution named",
					err,
				)
			}
		})
	}
}
