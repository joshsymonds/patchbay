package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// envMap turns a fixed map into a lookup function, so a test resolves
// configuration without touching the process environment.
func envMap(values map[string]string) lookupEnvFunc {
	return func(name string) (string, bool) {
		value, ok := values[name]

		return value, ok
	}
}

// quietLogger returns a logger that writes nowhere.
func quietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestResolveConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		want config
	}{
		{
			name: "xdg directories",
			env: map[string]string{
				"XDG_CONFIG_HOME": "/xdg/config",
				"XDG_STATE_HOME":  "/xdg/state",
				"HOME":            "/home/tester",
			},
			want: config{
				listen:    "127.0.0.1:4100",
				registry:  "/xdg/config/patchbay/routes.json",
				ledgerDir: "/xdg/state/patchbay/ledger",
				usageDB:   "/xdg/state/patchbay/usage.sqlite",
			},
		},
		{
			name: "home fallback when xdg is unset",
			env:  map[string]string{"HOME": "/home/tester"},
			want: config{
				listen:    "127.0.0.1:4100",
				registry:  "/home/tester/.config/patchbay/routes.json",
				ledgerDir: "/home/tester/.local/state/patchbay/ledger",
				usageDB:   "/home/tester/.local/state/patchbay/usage.sqlite",
			},
		},
		{
			name: "home fallback when xdg is set but empty",
			env: map[string]string{
				"XDG_CONFIG_HOME": "",
				"XDG_STATE_HOME":  "",
				"HOME":            "/home/tester",
			},
			want: config{
				listen:    "127.0.0.1:4100",
				registry:  "/home/tester/.config/patchbay/routes.json",
				ledgerDir: "/home/tester/.local/state/patchbay/ledger",
				usageDB:   "/home/tester/.local/state/patchbay/usage.sqlite",
			},
		},
		{
			name: "every setting overridden",
			env: map[string]string{
				"XDG_CONFIG_HOME":     "/xdg/config",
				"XDG_STATE_HOME":      "/xdg/state",
				"HOME":                "/home/tester",
				"PATCHBAY_LISTEN":     "127.0.0.1:9999",
				"PATCHBAY_REGISTRY":   "/etc/patchbay/routes.json",
				"PATCHBAY_LEDGER_DIR": "/var/lib/patchbay",
				"PATCHBAY_USAGE_DB":   "/var/lib/patchbay/usage.sqlite",
			},
			want: config{
				listen:    "127.0.0.1:9999",
				registry:  "/etc/patchbay/routes.json",
				ledgerDir: "/var/lib/patchbay",
				usageDB:   "/var/lib/patchbay/usage.sqlite",
			},
		},
		{
			name: "an empty override falls back to the default",
			env: map[string]string{
				"HOME":            "/home/tester",
				"PATCHBAY_LISTEN": "",
			},
			want: config{
				listen:    "127.0.0.1:4100",
				registry:  "/home/tester/.config/patchbay/routes.json",
				ledgerDir: "/home/tester/.local/state/patchbay/ledger",
				usageDB:   "/home/tester/.local/state/patchbay/usage.sqlite",
			},
		},
		{
			name: "an override needs no home at all",
			env: map[string]string{
				"PATCHBAY_REGISTRY":   "/etc/patchbay/routes.json",
				"PATCHBAY_LEDGER_DIR": "/var/lib/patchbay",
				"PATCHBAY_USAGE_DB":   "/var/lib/patchbay/usage.sqlite",
			},
			want: config{
				listen:    "127.0.0.1:4100",
				registry:  "/etc/patchbay/routes.json",
				ledgerDir: "/var/lib/patchbay",
				usageDB:   "/var/lib/patchbay/usage.sqlite",
			},
		},
	}

	for _, tc := range tests {
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

func TestLoadRateCardsFileParsesExactJSONRates(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "rates.json")
	if err := os.WriteFile(path, []byte(`[
		{
			"model":"openrouter/model",
			"effective_from":"2026-05-01T12:00:00Z",
			"source":"provider price sheet",
			"rates_usd_per_million":{
				"input":"3.00","output":"15.00","cache_read":"0.30",
				"cache_creation":"3.75","cache_creation_5m":"3.75","cache_creation_1h":"6.00"
			}
		}
	]`), 0o600); err != nil {
		t.Fatalf("write rate cards: %v", err)
	}
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
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("write rate cards: %v", err)
		}
		if _, err := loadRateCardsFile(path); err == nil {
			t.Fatal("loadRateCardsFile() returned nil error")
		}
	}
}

func TestServeGatewayFailsClosedOnNullRateCardsFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rateCards := filepath.Join(dir, "rates.json")
	if err := os.WriteFile(rateCards, []byte("null"), 0o600); err != nil {
		t.Fatalf("write null rate cards: %v", err)
	}
	err := serveGateway(t.Context(), envMap(map[string]string{
		"PATCHBAY_LISTEN":     "127.0.0.1:0",
		"PATCHBAY_REGISTRY":   filepath.Join(dir, "routes.json"),
		"PATCHBAY_LEDGER_DIR": filepath.Join(dir, "ledger"),
		"PATCHBAY_USAGE_DB":   filepath.Join(dir, "usage.sqlite"),
		"PATCHBAY_RATE_CARDS": rateCards,
	}), quietLogger(), nil)
	if err == nil || !strings.Contains(err.Error(), "load rate cards") {
		t.Fatalf("serveGateway() error = %v; want fatal null rate-card error", err)
	}
}

func TestServeGatewayFailsClosedOnInvalidRateCardsFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	err := serveGateway(t.Context(), envMap(map[string]string{
		"PATCHBAY_LISTEN":     "127.0.0.1:0",
		"PATCHBAY_REGISTRY":   filepath.Join(dir, "routes.json"),
		"PATCHBAY_LEDGER_DIR": filepath.Join(dir, "ledger"),
		"PATCHBAY_USAGE_DB":   filepath.Join(dir, "usage.sqlite"),
		"PATCHBAY_RATE_CARDS": filepath.Join(dir, "missing-rates.json"),
	}), quietLogger(), nil)
	if err == nil || !strings.Contains(err.Error(), "load rate cards") {
		t.Fatalf("serveGateway() error = %v; want fatal rate-card load error", err)
	}
}

func TestResolveConfigWithNowhereToLook(t *testing.T) {
	t.Parallel()

	// Neither an override, nor an XDG directory, nor a home: there is no honest
	// path to fall back to, and inventing one would write somebody's records
	// somewhere they never asked for.
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "nothing set at all", env: map[string]string{}},
		{name: "registry override only", env: map[string]string{"PATCHBAY_REGISTRY": "/etc/patchbay/routes.json"}},
		{name: "ledger override only", env: map[string]string{"PATCHBAY_LEDGER_DIR": "/var/lib/patchbay"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := resolveConfig(envMap(tc.env)); err == nil {
				t.Fatal("resolveConfig() error = nil; want a fatal misconfiguration")
			}
		})
	}
}

func TestRunFailsOnAnUnusableListenAddress(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	tests := []struct {
		name   string
		listen string
	}{
		{name: "port out of range", listen: "127.0.0.1:99999"},
		{name: "not an address at all", listen: "not-an-address"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := serveGateway(t.Context(), envMap(map[string]string{
				"PATCHBAY_LISTEN":     tc.listen,
				"PATCHBAY_REGISTRY":   filepath.Join(dir, "routes.json"),
				"PATCHBAY_LEDGER_DIR": filepath.Join(dir, "ledger"),
				"PATCHBAY_USAGE_DB":   filepath.Join(dir, "usage.sqlite"),
			}), quietLogger(), nil)

			if err == nil {
				t.Fatalf("serveGateway() with listen %q error = nil; want a bind failure", tc.listen)
			}

			if !strings.Contains(err.Error(), tc.listen) {
				t.Fatalf("serveGateway() error = %v; want it to name the address it could not bind", err)
			}
		})
	}
}

func TestRunFailsOnAnUnresolvableConfig(t *testing.T) {
	t.Parallel()

	if err := serveGateway(t.Context(), envMap(map[string]string{}), quietLogger(), nil); err == nil {
		t.Fatal("serveGateway() error = nil; want the misconfiguration reported")
	}
}

func TestRunServesUntilTheContextIsCancelled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ledgerDir := filepath.Join(dir, "state", "ledger")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- serveGateway(ctx, envMap(map[string]string{
			"PATCHBAY_LISTEN":     "127.0.0.1:0",
			"PATCHBAY_REGISTRY":   filepath.Join(dir, "routes.json"),
			"PATCHBAY_LEDGER_DIR": ledgerDir,
			"PATCHBAY_USAGE_DB":   filepath.Join(dir, "state", "usage.sqlite"),
		}), quietLogger(), nil)
	}()

	cancel()

	if err := <-done; err != nil {
		t.Fatalf("serveGateway() error = %v; want a clean shutdown", err)
	}

	info, err := os.Stat(ledgerDir)
	if err != nil {
		t.Fatalf("stat ledger directory: %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("ledger directory mode = %o; want 700", perm)
	}
}

func TestRunSurvivesAnUnusableLedger(t *testing.T) {
	t.Parallel()

	// A ledger that cannot be opened costs the records and nothing else. The
	// gateway's whole point is that the default route keeps working.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")

	if err := os.WriteFile(blocker, []byte("in the way"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- serveGateway(ctx, envMap(map[string]string{
			"PATCHBAY_LISTEN":     "127.0.0.1:0",
			"PATCHBAY_REGISTRY":   filepath.Join(dir, "routes.json"),
			"PATCHBAY_LEDGER_DIR": filepath.Join(blocker, "ledger"),
			"PATCHBAY_USAGE_DB":   filepath.Join(dir, "usage.sqlite"),
		}), quietLogger(), nil)
	}()

	cancel()

	if err := <-done; err != nil {
		t.Fatalf("serveGateway() error = %v; want the gateway to serve without a ledger", err)
	}
}

func TestRunFailsWhenHostnameCannotBeResolved(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	err := serveGatewayWithHostname(t.Context(), envMap(map[string]string{
		"PATCHBAY_LISTEN":     "127.0.0.1:0",
		"PATCHBAY_REGISTRY":   filepath.Join(dir, "routes.json"),
		"PATCHBAY_LEDGER_DIR": filepath.Join(dir, "ledger"),
		"PATCHBAY_USAGE_DB":   filepath.Join(dir, "usage.sqlite"),
	}), quietLogger(), nil, func() (string, error) {
		return "", errors.New("hostname unavailable")
	})
	if err == nil {
		t.Fatal("serveGatewayWithHostname() error = nil; want hostname failure to prevent startup")
	}
	if !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("serveGatewayWithHostname() error = %v; want it to name hostname resolution", err)
	}
}

func TestRunFailsOnAnUnusableUsageDatabase(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("in the way"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}

	err := serveGateway(t.Context(), envMap(map[string]string{
		"PATCHBAY_LISTEN":     "127.0.0.1:0",
		"PATCHBAY_REGISTRY":   filepath.Join(dir, "routes.json"),
		"PATCHBAY_LEDGER_DIR": filepath.Join(dir, "ledger"),
		"PATCHBAY_USAGE_DB":   filepath.Join(blocker, "usage.sqlite"),
	}), quietLogger(), nil)
	if err == nil {
		t.Fatal("serveGateway() error = nil; want an unusable usage database to prevent startup")
	}
}
