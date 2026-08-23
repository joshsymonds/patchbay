// Command patchbay is the local Anthropic Messages API gateway: one process
// per host, listening on loopback, that Claude Code points ANTHROPIC_BASE_URL
// at — and the command-line tool for asking that process what it will do with
// a request.
//
// The gateway itself is configured entirely by environment, with no flags,
// because deployment is declarative: the unit file is generated, not typed.
// Runtime paths have deterministic defaults, while the strict registry must
// already exist and validate before the process binds its listener. So must the
// hostname, the usage database and the socket: accounting is the durable
// evidence that upstream contact is allowed, so a machine that cannot account
// does not serve. A ledger failure costs only the records.
//
// The subcommands are the other half. "patchbay serve" is the daemon; every
// other subcommand answers a question an operator would otherwise answer by
// reading JSON — which Context a directory selects, which Seat a model uses,
// whether a registry file is valid. Serving is deliberately not the default: a
// bare invocation is far more likely to be a mistyped subcommand than a request
// to bind the port, and starting a second gateway on a typo is worse than
// printing usage.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/joshsymonds/patchbay/internal/accounting"
	"github.com/joshsymonds/patchbay/internal/buildinfo"
	"github.com/joshsymonds/patchbay/internal/ledger"
	"github.com/joshsymonds/patchbay/internal/proxy"
	"github.com/joshsymonds/patchbay/internal/registry"
	"github.com/joshsymonds/patchbay/internal/server"
	"github.com/joshsymonds/patchbay/internal/usage"
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
	cmdSeats    = "seats"
	cmdBindings = "bindings"
	cmdResolve  = "resolve"
	cmdProject  = "project"
	cmdCheck    = "check"
	cmdUsage    = "usage"

	usageToolingArgumentCount = 2
)

// usageText is the whole command line, in the order an operator meets it.
const usageText = `usage: patchbay <command> [arguments]

commands:
  serve                        run the gateway until it is signaled to stop
  contexts                     list every context the running gateway serves
  seats                        list global reusable Seats
  bindings --ctx <name>        list one Context's public model bindings
  resolve [--subagent] <context> <model>
                               show the complete non-secret resolution
  project [dir]                show which context a directory's Claude Code settings select
  check [--registry <path>]    validate a registry file without asking the gateway
  usage snapshot <dest>        write a consistent SQLite usage database snapshot
  usage export <dest>          write a deterministic JSONL usage database export
  usage import <ledger-dir>    import legacy JSONL ledger accounting

  -h, --help                   print this text

The contexts, seats, bindings, resolve, and project subcommands ask the running gateway,
at $PATCHBAY_LISTEN (default ` + defaultListen + `), presenting the caller key
named by $` + proxy.CallerKeyEnvFile + `. Usage snapshot and export operate
directly on $PATCHBAY_USAGE_DB and need neither a running gateway nor a caller key.
`

// Environment variables patchbay reads. An unset or empty value takes the
// default: empty is not a listen address, a registry path or a ledger
// directory, and honoring it would bind the wrong socket or write to the
// wrong place.
const (
	envListen    = "PATCHBAY_LISTEN"
	envRegistry  = "PATCHBAY_REGISTRY"
	envLedgerDir = "PATCHBAY_LEDGER_DIR"
	envUsageDB   = "PATCHBAY_USAGE_DB"
	envRateCards = "PATCHBAY_RATE_CARDS"
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
	usageLeafFile  = "usage.sqlite"
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

// The ASCII control bytes an operational diagnostic escapes: everything below
// the first printable byte, and DEL above the printable range.
const (
	firstPrintableByte = 0x20
	deleteByte         = 0x7f
)

// lookupEnvFunc reads one environment variable, reporting whether it was set.
// os.LookupEnv satisfies it; a test supplies its own.
type lookupEnvFunc func(name string) (string, bool)

// config is patchbay's resolved runtime configuration.
type config struct {
	listen    string
	registry  string
	ledgerDir string
	usageDB   string
	rateCards string
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
	case cmdSeats:
		return invocation.seats(ctx, rest)
	case cmdBindings:
		return invocation.bindings(ctx, rest)
	case cmdResolve:
		return invocation.resolve(ctx, rest)
	case cmdProject:
		return invocation.project(ctx, rest)
	case cmdCheck:
		return invocation.check(rest)
	case cmdUsage:
		return invocation.usageTooling(ctx, rest)
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

// usageTooling writes a consistent snapshot or deterministic export of the
// durable usage database without starting a daemon or changing the source.
func (c cli) usageTooling(ctx context.Context, args []string) int {
	if len(args) != usageToolingArgumentCount {
		c.usage(c.stderr)

		return exitUsage
	}
	operation, dest := args[0], args[1]
	if operation != "snapshot" && operation != "export" && operation != "import" {
		c.fail(fmt.Errorf("unknown usage subcommand %q", operation))
		c.usage(c.stderr)

		return exitUsage
	}
	cfg, err := resolveConfig(c.lookupEnv)
	if err != nil {
		c.fail(err)

		return exitFailure
	}
	if operation == "import" {
		if importErr := c.usageImport(ctx, cfg.usageDB, dest); importErr != nil {
			c.fail(importErr)
			return exitFailure
		}
		return exitOK
	}
	var operationErr error
	if operation == "snapshot" {
		operationErr = usage.Snapshot(ctx, cfg.usageDB, dest)
	} else {
		operationErr = usage.Export(ctx, cfg.usageDB, dest)
	}
	if operationErr != nil {
		c.fail(operationErr)

		return exitFailure
	}
	fmt.Fprintf(c.stdout, "OK usage %s: %s\n", operation, dest)

	return exitOK
}

func (c cli) usageImport(ctx context.Context, usageDB, ledgerDir string) (result error) {
	files, err := ledgerImportFiles(ledgerDir)
	if err != nil {
		return err
	}
	store, err := usage.Open(usageDB)
	if err != nil {
		return fmt.Errorf("open usage database %s: %w", usageDB, err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close usage database %s: %w", usageDB, closeErr))
		}
	}()
	boundary, boundaryErr := store.ImportBoundary(ctx)
	if boundaryErr != nil {
		return fmt.Errorf("read import boundary: %w", boundaryErr)
	}
	for _, path := range files {
		importResult, importErr := store.ImportLedgerFile(ctx, path, filepath.Base(path), boundary)
		if importErr != nil {
			return fmt.Errorf("import ledger %s: %w", path, importErr)
		}
		fmt.Fprintf(c.stdout, "OK usage import %s: imported=%d skipped-boundary=%d skipped-existing=%d\n",
			filepath.Base(path), importResult.Imported, importResult.SkippedBoundary, importResult.SkippedExisting)
	}
	return nil
}

func ledgerImportFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read ledger directory %q: %w", dir, err)
	}
	var files []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && isLedgerImportFile(entry.Name()) {
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("ledger directory %q contains no ledger files", dir)
	}
	sort.Strings(files)
	return files, nil
}

func isLedgerImportFile(name string) bool {
	const suffix = ".jsonl"
	for _, prefix := range []string{"patchbay-", "patchbay-v2-"} {
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		date := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
		if len(date) != len("2006-01-02") {
			continue
		}
		if _, err := time.Parse("2006-01-02", date); err == nil {
			return true
		}
	}
	return false
}

// fail reports an operational failure on stderr, marked as patchbay's own.
func (c cli) fail(err error) {
	fmt.Fprintf(c.stderr, "patchbay: %s\n", escapeControls(err.Error()))
}

// noticef reports something an operator should see but that did not stop the
// command. It goes to stderr so stdout stays a clean table.
func (c cli) noticef(format string, args ...any) {
	fmt.Fprintf(c.stderr, "patchbay: %s\n", escapeControls(fmt.Sprintf(format, args...)))
}

// escapeControls renders every ASCII control byte in text as a readable escape
// and leaves every other byte exactly as it was.
//
// A diagnostic is prose, so it is not quoted the way a table cell or a
// "key: value" field is — quoting patchbay's own sentence would make its words
// look like something a file said. But the values inside that sentence are
// still byte strings nobody vetted: a registry path, a listen address, the text
// an upstream error carries. A newline in one written raw stops being part of
// the message and becomes the end of it, and whatever followed reads as a
// second diagnostic patchbay never issued. Escaping just the control bytes
// keeps one diagnostic on one physical line while the prose around them stays
// prose.
//
// The work is byte-wise rather than rune-wise because a path is a byte string.
// A byte no rune starts is not an error to be replaced with U+FFFD here; it is
// part of the filename, and an operator looking for that file needs to see it.
func escapeControls(text string) string {
	var escaped strings.Builder

	escaped.Grow(len(text))

	for index := range len(text) {
		switch char := text[index]; {
		case char == '\n':
			escaped.WriteString(`\n`)
		case char == '\r':
			escaped.WriteString(`\r`)
		case char == '\t':
			escaped.WriteString(`\t`)
		case char < firstPrintableByte || char == deleteByte:
			fmt.Fprintf(&escaped, `\x%02x`, char)
		default:
			escaped.WriteByte(char)
		}
	}

	return escaped.String()
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

// serveGateway validates the registry, opens the accounting store, binds the
// listener, and serves until ctx is canceled. It returns an error only for a
// failure that leaves nothing to serve: a configuration, registry or hostname
// that cannot be resolved, a usage database or rate-card file that cannot be
// loaded, or a socket that cannot be bound. Runtime registry reload failures
// install a fail-closed sentinel; ledger failures drop records.
//
// client is the upstream HTTP client both halves of the gateway use. A nil
// client means each package builds its own default, which is what production
// passes; a test passes one that never leaves the machine.
func serveGateway(
	ctx context.Context,
	lookupEnv lookupEnvFunc,
	logger *slog.Logger,
	client *http.Client,
) error {
	return serveGatewayWithHostname(ctx, lookupEnv, logger, client, os.Hostname)
}

// serveGatewayWithHostname is serveGateway with the machine's own name supplied
// rather than asked for, so the one startup failure that cannot be provoked by
// arranging files on disk is still reachable from a test.
func serveGatewayWithHostname(
	ctx context.Context,
	lookupEnv lookupEnvFunc,
	logger *slog.Logger,
	client *http.Client,
	lookupHostname func() (string, error),
) error {
	cfg, err := resolveConfig(lookupEnv)
	if err != nil {
		return err
	}

	reg, regErr := registry.New(cfg.registry)
	if regErr != nil {
		return fmt.Errorf("load registry: %w", regErr)
	}

	host, hostnameErr := lookupHostname()
	if hostnameErr != nil {
		return fmt.Errorf("resolve hostname: %w", hostnameErr)
	}
	if host == "" {
		return errors.New("resolve hostname: hostname is empty")
	}

	usageStore, usageErr := usage.Open(cfg.usageDB)
	if usageErr != nil {
		return fmt.Errorf("open usage database %s: %w", cfg.usageDB, usageErr)
	}
	if cfg.rateCards != "" {
		cards, loadErr := loadRateCardsFile(cfg.rateCards)
		if loadErr != nil {
			return closeUsageAfterRateCardError(usageStore, cfg.rateCards, loadErr)
		}
		storeLoadErr := usageStore.LoadRateCards(ctx, cards)
		if storeLoadErr != nil {
			return closeUsageAfterRateCardError(usageStore, cfg.rateCards, storeLoadErr)
		}
	}
	// Registered before drainRecords below, so LIFO defer order closes the usage
	// store only after Drain returns. Rows still pending at a bounded drain timeout
	// are the durable, auditable crash-equivalent signal for later inspection.
	defer closeUsage(ctx, usageStore, logger)

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

	// A nil *ledger.Ledger must never be handed over as a typed nil: the
	// pipeline skips a Recorder that is nil, and panics on one that only looks
	// like it.
	var recorder proxy.Recorder

	if led := openLedger(ctx, cfg.ledgerDir, logger); led != nil {
		recorder = led

		defer closeLedger(ctx, led, logger)
	}

	pipeline, proxyErr := proxy.New(proxy.Config{
		Registry:   reg,
		Recorder:   recorder,
		Accountant: usageStore,
		Client:     client,
		Logger:     logger,
		Host:       host,
	})
	if proxyErr != nil {
		return fmt.Errorf("construct proxy: %w", proxyErr)
	}

	// Deferred after the ledger's own close so it runs before it: records are
	// written off the request path, and closing the ledger under one loses it.
	defer drainRecords(pipeline)

	handler := server.Handler(server.Config{
		Proxy:               pipeline,
		Registry:            reg,
		AdmissionMiddleware: pipeline.AdmissionMiddleware(),
		Usage:               usageStore,
		Client:              client,
		Logger:              logger,
		Listen:              listener.Addr().String(),
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

type rateCardFileEntry struct {
	Model              string `json:"model"`
	EffectiveFrom      string `json:"effective_from"`
	Source             string `json:"source"`
	RatesUSDPerMillion struct {
		Input           string `json:"input"`
		Output          string `json:"output"`
		CacheRead       string `json:"cache_read"`
		CacheCreation   string `json:"cache_creation"`
		CacheCreation5m string `json:"cache_creation_5m"`
		CacheCreation1h string `json:"cache_creation_1h"`
	} `json:"rates_usd_per_million"`
}

func loadRateCardsFile(path string) ([]usage.RateCard, error) {
	contents, readErr := os.ReadFile(filepath.Clean(path))
	if readErr != nil {
		return nil, fmt.Errorf("read rate cards: %w", readErr)
	}
	var entries []rateCardFileEntry
	if unmarshalErr := json.Unmarshal(contents, &entries); unmarshalErr != nil {
		return nil, fmt.Errorf("parse JSON: %w", unmarshalErr)
	}
	if entries == nil {
		return nil, errors.New("rate cards must be a JSON array")
	}
	cards := make([]usage.RateCard, 0, len(entries))
	for index, entry := range entries {
		effectiveFrom, parseErr := time.Parse(time.RFC3339, entry.EffectiveFrom)
		if parseErr != nil {
			return nil, fmt.Errorf("card %d effective_from: %w", index, parseErr)
		}
		rates := entry.RatesUSDPerMillion
		input, err := parseRate(index, "input", rates.Input)
		if err != nil {
			return nil, err
		}
		output, err := parseRate(index, "output", rates.Output)
		if err != nil {
			return nil, err
		}
		cacheRead, err := parseRate(index, "cache_read", rates.CacheRead)
		if err != nil {
			return nil, err
		}
		cacheCreation, err := parseRate(index, "cache_creation", rates.CacheCreation)
		if err != nil {
			return nil, err
		}
		cacheCreation5m, err := parseRate(index, "cache_creation_5m", rates.CacheCreation5m)
		if err != nil {
			return nil, err
		}
		cacheCreation1h, err := parseRate(index, "cache_creation_1h", rates.CacheCreation1h)
		if err != nil {
			return nil, err
		}
		cards = append(cards, usage.RateCard{
			Model:                            entry.Model,
			EffectiveFrom:                    effectiveFrom,
			InputNanoUSDPerMillion:           input,
			OutputNanoUSDPerMillion:          output,
			CacheReadNanoUSDPerMillion:       cacheRead,
			CacheCreationNanoUSDPerMillion:   cacheCreation,
			CacheCreation5mNanoUSDPerMillion: cacheCreation5m,
			CacheCreation1hNanoUSDPerMillion: cacheCreation1h,
			Source:                           entry.Source,
		})
	}
	return cards, nil
}

func parseRate(index int, name, value string) (accounting.NanoUSD, error) {
	if value == "" {
		return 0, fmt.Errorf("card %d rate %q is required", index, name)
	}
	rate, err := accounting.ParseUSD(value)
	if err != nil {
		return 0, fmt.Errorf("card %d rate %q: %w", index, name, err)
	}
	return rate, nil
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
		usageDB:   value(lookupEnv, envUsageDB),
		rateCards: value(lookupEnv, envRateCards),
	}

	if cfg.registry == "" {
		found, err := resolveRegistryPath(lookupEnv)
		if err != nil {
			return config{}, err
		}

		cfg.registry = found
	}

	if cfg.ledgerDir == "" || cfg.usageDB == "" {
		base, err := baseDir(lookupEnv, envStateXDG, stateHomeTail)
		if err != nil {
			return config{}, fmt.Errorf("resolve the state directory: %w", err)
		}

		if cfg.ledgerDir == "" {
			cfg.ledgerDir = filepath.Join(base, appDir, ledgerLeafDir)
		}
		if cfg.usageDB == "" {
			cfg.usageDB = filepath.Join(base, appDir, usageLeafFile)
		}
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
// and returns no ledger: the gateway serves without records while registry and
// Context failures remain fail closed.
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

// closeUsageAfterRateCardError reports a rate-card failure with the store
// already released. The store was opened moments earlier and nothing has been
// deferred yet, so this is the one path that has to close it by hand.
func closeUsageAfterRateCardError(store *usage.Store, path string, cause error) error {
	cause = fmt.Errorf("load rate cards %s: %w", path, cause)
	if err := store.Close(); err != nil {
		return errors.Join(cause, fmt.Errorf("close usage database after rate-card load failure: %w", err))
	}
	return cause
}

// closeUsage releases the usage store after record goroutines have drained.
func closeUsage(ctx context.Context, store *usage.Store, logger *slog.Logger) {
	if err := store.Close(); err != nil {
		logger.ErrorContext(ctx, "closing the usage database failed", slog.Any("error", err))
	}
}

// serve runs the HTTP server on listener until ctx is canceled, then gives
// in-flight requests a bounded chance to finish. The shutdown deadline is its
// own context: the one that just ended is in no position to bound anything.
func serve(
	ctx context.Context,
	listener net.Listener,
	handler http.Handler,
	logger *slog.Logger,
) error {
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
