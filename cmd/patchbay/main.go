// Command patchbay is the local Anthropic Messages API gateway: one process
// per host, listening on loopback, that Claude Code points ANTHROPIC_BASE_URL
// at — and the command-line tool for asking that process what it will do with
// a request.
//
// The gateway itself is configured entirely by environment, with no flags,
// because deployment is declarative: the unit file is generated, not typed.
// Everything it needs has a default that works on a machine where nothing has
// been set up, and nothing it needs is fatal except the socket it was asked to
// listen on.
//
// The subcommands are the other half. "patchbay serve" is the daemon; every
// other subcommand answers a question an operator would otherwise answer by
// reading JSON — which context a directory selects, which route a model rides,
// whether a registry file is valid. Serving is deliberately not the default: a
// bare invocation is far more likely to be a mistyped subcommand than a request
// to bind the port, and starting a second gateway on a typo is worse than
// printing usage.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// The process exit codes: nothing went wrong, something did, or the command
// line itself was not one patchbay understands.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// The subcommands patchbay answers to.
const (
	cmdServe    = "serve"
	cmdContexts = "contexts"
	cmdRoutes   = "routes"
	cmdResolve  = "resolve"
	cmdProject  = "project"
	cmdCheck    = "check"
)

// usageText is the whole command line, in the order an operator meets it.
const usageText = `usage: patchbay <command> [arguments]

commands:
  serve                        run the gateway until it is signaled to stop
  contexts                     list every context the running gateway serves
  routes [--ctx <name>]        list one context's aliases (default: personal)
  resolve <context> <model>    show which route one request would ride
  project [dir]                show which context a directory's Claude Code settings select
  check [--registry <path>]    validate a registry file without asking the gateway

  -h, --help                   print this text

The subcommands other than serve and check ask the running gateway, at
$PATCHBAY_LISTEN (default ` + defaultListen + `), presenting the caller key
named by $` + proxy.CallerKeyEnvFile + `.
`

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

// cli is everything one invocation needs from the process it runs in. A test
// supplies its own of each, so no subcommand reaches for a stream, an
// environment or a network of its own.
type cli struct {
	stdout    io.Writer
	stderr    io.Writer
	lookupEnv lookupEnvFunc

	// client is the HTTP client both the gateway's upstream calls and the CLI's
	// own calls to the daemon use. A nil client means each package builds its
	// own default, which is what production passes.
	client *http.Client

	// logger receives the gateway's structured log. A nil logger gets JSON on
	// stderr, which is what the unit file captures.
	logger *slog.Logger
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one command line and reports the process exit code, so main
// itself holds no deferred work that os.Exit would skip.
func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return dispatch(ctx, args, cli{
		stdout:    stdout,
		stderr:    stderr,
		lookupEnv: os.LookupEnv,
		client:    nil,
		logger:    nil,
	})
}

// dispatch runs the subcommand args names. A command line naming none — or
// naming one patchbay has never heard of — is a usage error rather than a
// gateway: the bare invocation used to serve, and a typo that silently binds
// the port is a worse answer than printing what the commands are.
func dispatch(ctx context.Context, args []string, invocation cli) int {
	if len(args) == 0 {
		invocation.usage(invocation.stderr)

		return exitUsage
	}

	rest := args[1:]

	switch args[0] {
	case cmdServe:
		return invocation.serve(ctx)
	case cmdContexts:
		return invocation.contexts(ctx, rest)
	case cmdRoutes:
		return invocation.routes(ctx, rest)
	case cmdResolve:
		return invocation.resolve(ctx, rest)
	case cmdProject:
		return invocation.project(ctx, rest)
	case cmdCheck:
		return invocation.check(rest)
	case "-h", "--help", "help":
		invocation.usage(invocation.stdout)

		return exitOK
	default:
		invocation.fail(fmt.Errorf("unknown subcommand %q", args[0]))
		invocation.usage(invocation.stderr)

		return exitUsage
	}
}

// usage prints the whole command line to w.
func (c cli) usage(w io.Writer) {
	fmt.Fprint(w, usageText)
}

// fail reports an operational failure on stderr, marked as patchbay's own.
func (c cli) fail(err error) {
	fmt.Fprintf(c.stderr, "patchbay: %v\n", err)
}

// noticef reports something an operator should see but that did not stop the
// command. It goes to stderr so stdout stays a clean table.
func (c cli) noticef(format string, args ...any) {
	fmt.Fprintf(c.stderr, "patchbay: "+format+"\n", args...)
}

// serve runs the gateway until the process is signaled to stop.
func (c cli) serve(ctx context.Context) int {
	logger := c.logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(c.stderr, nil))
	}

	if err := serveGateway(ctx, c.lookupEnv, logger, c.client); err != nil {
		logger.ErrorContext(ctx, "patchbay stopped", slog.Any("error", err))

		return exitFailure
	}

	return exitOK
}

// serveGateway starts the gateway and serves until ctx is canceled. It returns an error
// only for a failure that leaves nothing to serve: a configuration that cannot
// be resolved, or a socket that cannot be bound. Everything else degrades — a
// registry that will not parse costs the aliases, a ledger that will not open
// costs the records — because a gateway that refuses to start is worse for the
// caller than one that starts diminished.
//
// client is the upstream HTTP client both halves of the gateway use. A nil
// client means each package builds its own default, which is what production
// passes; a test passes one that never leaves the machine.
func serveGateway(ctx context.Context, lookupEnv lookupEnvFunc, logger *slog.Logger, client *http.Client) error {
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
		Listen:   listener.Addr().String(),
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
		found, err := resolveRegistryPath(lookupEnv)
		if err != nil {
			return config{}, err
		}

		cfg.registry = found
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

// resolveRegistryPath returns the registry file the gateway reads: the explicit
// setting, or the conventional path under the config directory. The CLI's check
// subcommand resolves it the same way, so it can never validate a different file
// from the one that will be served.
func resolveRegistryPath(lookupEnv lookupEnvFunc) (string, error) {
	if explicit := value(lookupEnv, envRegistry); explicit != "" {
		return explicit, nil
	}

	base, err := baseDir(lookupEnv, envConfigXDG, configHomeTail)
	if err != nil {
		return "", fmt.Errorf("resolve the registry path: %w", err)
	}

	return filepath.Join(base, appDir, registryFile), nil
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
