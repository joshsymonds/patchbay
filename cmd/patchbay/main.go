// Command patchbay is the local Anthropic Messages API gateway: one process
// per host, listening on loopback, that Claude Code points ANTHROPIC_BASE_URL
// at.
//
// It is configured entirely by environment, with no flags, because deployment
// is declarative: the unit file is generated, not typed. Everything it needs
// has a default that works on a machine where nothing has been set up, and
// nothing it needs is fatal except the socket it was asked to listen on.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/joshsymonds/patchbay/internal/buildinfo"
	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/server"
)

// Environment variables patchbay reads. An unset or empty value takes the
// default: empty is not a listen address, a registry path or a ledger
// directory, and honoring it would bind the wrong socket or write to the
// wrong place.
const (
	envListen    = "PATCHBAY_LISTEN"
	envRegistry  = "PATCHBAY_REGISTRY"
	envLedgerDir = "PATCHBAY_LEDGER_DIR"
	envConfigXDG = "XDG_CONFIG_HOME"
	envStateXDG  = "XDG_STATE_HOME"
	envHome      = "HOME"
)

// defaultListen is loopback, deliberately. The gateway forwards the caller's
// own Anthropic credentials, so anything that can reach it can spend them.
const defaultListen = "127.0.0.1:4100"

// Where the registry and the ledger live when the environment does not say.
const (
	registryFile   = "routes.json"
	appDir         = "patchbay"
	ledgerLeafDir  = "ledger"
	configHomeTail = ".config"
	stateHomeTail  = ".local/state"
)

// ledgerDirMode is the ledger directory's permission: the records name every
// model a machine used and when, which is nobody else's business.
const ledgerDirMode = 0o700

// readHeaderTimeout bounds how long a caller may take to send its request
// headers. It is the only deadline the server sets: a write or idle deadline
// would kill a long streamed turn, which is indistinguishable from an upstream
// failure to whoever is waiting on it.
const readHeaderTimeout = 10 * time.Second

// shutdownTimeout bounds how long in-flight requests have to finish once a
// signal arrives.
const shutdownTimeout = 5 * time.Second

// lookupEnvFunc reads one environment variable, reporting whether it was set.
// os.LookupEnv satisfies it; a test supplies its own.
type lookupEnvFunc func(name string) (string, bool)

// config is patchbay's resolved runtime configuration.
type config struct {
	listen    string
	registry  string
	ledgerDir string
}

func main() {
	os.Exit(start())
}

// start runs the gateway and reports the process exit code, so main itself
// holds no deferred work that os.Exit would skip.
func start() int {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A nil client leaves each package to build its own default, which is what
	// production wants; only a test substitutes one.
	if err := run(ctx, os.LookupEnv, logger, nil); err != nil {
		logger.ErrorContext(ctx, "patchbay stopped", slog.Any("error", err))

		return 1
	}

	return 0
}

// run starts the gateway and serves until ctx is canceled. It returns an error
// only for a failure that leaves nothing to serve: a configuration that cannot
// be resolved, or a socket that cannot be bound. Everything else degrades — a
// registry that will not parse costs the aliases, a ledger that will not open
// costs the records — because a gateway that refuses to start is worse for the
// caller than one that starts diminished.
//
// client is the upstream HTTP client both halves of the gateway use. A nil
// client means each package builds its own default, which is what production
// passes; a test passes one that never leaves the machine.
func run(ctx context.Context, lookupEnv lookupEnvFunc, logger *slog.Logger, client *http.Client) error {
	cfg, err := resolveConfig(lookupEnv)
	if err != nil {
		return err
	}

	var listen net.ListenConfig

	listener, listenErr := listen.Listen(ctx, "tcp", cfg.listen)
	if listenErr != nil {
		return fmt.Errorf("listen on %s: %w", cfg.listen, listenErr)
	}

	logger.InfoContext(ctx, "patchbay listening",
		slog.String("version", buildinfo.Version()),
		slog.String("listen", listener.Addr().String()),
		slog.String("registry", cfg.registry),
		slog.String("ledger_dir", cfg.ledgerDir))

	reg := registry.New(cfg.registry)
	if regErr := reg.Err(); regErr != nil {
		logger.ErrorContext(ctx, "registry file is broken; foreign aliases are unavailable",
			slog.String("registry", cfg.registry), slog.Any("error", regErr))
	}

	// A nil *ledger.Ledger must never be handed over as a typed nil: the
	// pipeline skips a Recorder that is nil, and panics on one that only looks
	// like it.
	var recorder proxy.Recorder

	if led := openLedger(ctx, cfg.ledgerDir, logger); led != nil {
		recorder = led

		defer closeLedger(ctx, led, logger)
	}

	pipeline := proxy.New(proxy.Config{
		Registry: reg,
		Recorder: recorder,
		Client:   client,
		Logger:   logger,
		Host:     hostname(ctx, logger),
	})

	// Deferred after the ledger's own close so it runs before it: records are
	// written off the request path, and closing the ledger under one loses it.
	defer drainRecords(pipeline)

	handler := server.Handler(server.Config{
		Proxy:    pipeline,
		Registry: reg,
		Client:   client,
		Logger:   logger,
	})

	return serve(ctx, listener, handler, logger)
}

// drainRecords gives the ledger writes still in flight a bounded chance to
// finish. A Recorder that never returns costs its records, not the shutdown.
func drainRecords(pipeline *proxy.Proxy) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	pipeline.Drain(ctx)
}

// resolveConfig reads the environment into a configuration. An explicit
// setting always wins; otherwise the XDG directories decide, falling back to
// the home directory's conventional paths. With none of the three there is
// nowhere honest to put a registry or a ledger, so that is a fatal
// misconfiguration rather than a guess.
func resolveConfig(lookupEnv lookupEnvFunc) (config, error) {
	cfg := config{
		listen:    valueOr(lookupEnv, envListen, defaultListen),
		registry:  value(lookupEnv, envRegistry),
		ledgerDir: value(lookupEnv, envLedgerDir),
	}

	if cfg.registry == "" {
		base, err := baseDir(lookupEnv, envConfigXDG, configHomeTail)
		if err != nil {
			return config{}, fmt.Errorf("resolve the registry path: %w", err)
		}

		cfg.registry = filepath.Join(base, appDir, registryFile)
	}

	if cfg.ledgerDir == "" {
		base, err := baseDir(lookupEnv, envStateXDG, stateHomeTail)
		if err != nil {
			return config{}, fmt.Errorf("resolve the ledger directory: %w", err)
		}

		cfg.ledgerDir = filepath.Join(base, appDir, ledgerLeafDir)
	}

	return cfg, nil
}

// baseDir returns the XDG directory named by xdgName, or the conventional path
// under the home directory when it is unset.
func baseDir(lookupEnv lookupEnvFunc, xdgName, homeTail string) (string, error) {
	if xdg := value(lookupEnv, xdgName); xdg != "" {
		return xdg, nil
	}

	home := value(lookupEnv, envHome)
	if home == "" {
		return "", fmt.Errorf("neither %s nor %s is set", xdgName, envHome)
	}

	return filepath.Join(home, filepath.FromSlash(homeTail)), nil
}

// value returns an environment variable's value, treating unset and empty
// alike.
func value(lookupEnv lookupEnvFunc, name string) string {
	found, ok := lookupEnv(name)
	if !ok {
		return ""
	}

	return found
}

// valueOr returns an environment variable's value, or fallback when it is
// unset or empty.
func valueOr(lookupEnv lookupEnvFunc, name, fallback string) string {
	if found := value(lookupEnv, name); found != "" {
		return found
	}

	return fallback
}

// openLedger prepares the ledger directory and opens it. A failure is reported
// and returns no ledger: the gateway serves without records rather than not at
// all, and the built-in default route never fails closed.
func openLedger(ctx context.Context, dir string, logger *slog.Logger) *ledger.Ledger {
	if err := os.MkdirAll(dir, ledgerDirMode); err != nil {
		logger.ErrorContext(ctx, "ledger unavailable; requests will not be recorded",
			slog.String("ledger_dir", dir), slog.Any("error", err))

		return nil
	}

	led, err := ledger.Open(dir)
	if err != nil {
		logger.ErrorContext(ctx, "ledger unavailable; requests will not be recorded",
			slog.String("ledger_dir", dir), slog.Any("error", err))

		return nil
	}

	return led
}

// closeLedger releases the ledger's directory handle at shutdown.
func closeLedger(ctx context.Context, led *ledger.Ledger, logger *slog.Logger) {
	if err := led.Close(); err != nil {
		logger.ErrorContext(ctx, "closing the ledger failed", slog.Any("error", err))
	}
}

// hostname is the machine name stamped on every ledger record. A machine that
// cannot name itself costs the records — the ledger rejects one with no host —
// and nothing else, which is the same bargain every other ledger failure makes.
func hostname(ctx context.Context, logger *slog.Logger) string {
	host, err := os.Hostname()
	if err != nil {
		logger.ErrorContext(ctx, "hostname unavailable; records will be rejected", slog.Any("error", err))

		return ""
	}

	return host
}

// serve runs the HTTP server on listener until ctx is canceled, then gives
// in-flight requests a bounded chance to finish. The shutdown deadline is its
// own context: the one that just ended is in no position to bound anything.
func serve(ctx context.Context, listener net.Listener, handler http.Handler, logger *slog.Logger) error {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}

	failed := make(chan error, 1)

	go func() { failed <- srv.Serve(listener) }()

	select {
	case err := <-failed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.InfoContext(ctx, "patchbay shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down: %w", err)
	}

	return nil
}
