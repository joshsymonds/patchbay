# CLI

```text
usage: patchbay <command> [arguments]

commands:
  serve                        run the gateway until it is signaled to stop
  contexts                     list every context the running gateway serves
  routes [--ctx <name>]        list one context's aliases (default: personal)
  resolve <context> <model>    show which route one request would use
  project [dir]                show which context a directory's Claude Code settings select
  check [--registry <path>]    validate a registry file without asking the gateway
```

A bare command, unknown subcommand, or invalid argument list prints usage to
stderr and exits 2. `-h`, `--help`, and `help` print top-level usage and exit 0.

| Exit | Meaning |
|---|---|
| `0` | Command completed. Notices may still appear on stderr. |
| `1` | Operational failure. |
| `2` | Invalid command line. |

Go's flag parser accepts one or two leading dashes. Flags must precede positional
arguments. Command-specific help prints to stderr and exits 2.

`contexts`, `routes`, `resolve`, and `project` query the running gateway at
`$PATCHBAY_LISTEN`, defaulting to `127.0.0.1:4100`. They read the caller key from
`$PATCHBAY_CALLER_KEY_FILE` before dialing and fetch `/_patchbay/registry`.
`check` works without a running gateway.

Examples below use [examples/routes.json](../examples/routes.json).

## `serve`

```console
$ patchbay serve
{"time":"2026-08-20T16:11:32-07:00","level":"INFO","msg":"patchbay listening","version":"0.0.0-dev","listen":"127.0.0.1:4100","registry":"/home/you/.config/patchbay/routes.json","ledger_dir":"/home/you/.local/state/patchbay/ledger"}
```

The gateway runs until `SIGINT` or `SIGTERM`. It logs JSON to stderr and writes
nothing to stdout. HTTP shutdown and ledger drain each get up to five seconds.

A broken registry or unavailable ledger does not stop the gateway; those features
degrade as described in [configuration.md](configuration.md). Path resolution,
listener, server, or shutdown failures exit 1. Trailing arguments after `serve`
are currently ignored.

## `check`

```text
patchbay check [--registry <path>]
```

The registry path is selected from:

1. `--registry <path>`;
2. `$PATCHBAY_REGISTRY`;
3. `$XDG_CONFIG_HOME/patchbay/routes.json`;
4. `$HOME/.config/patchbay/routes.json`.

A missing file is an error. Success prints effective contexts and the total alias
count:

```console
$ patchbay check --registry examples/routes.json
OK examples/routes.json: 2 contexts (personal, work), 4 routes
```

File and validation failures exit 1. Positional arguments and unknown flags exit
2.

## `contexts`

```console
$ patchbay contexts
CONTEXT   DEFAULT_ROUTE                                          ROUTES
personal  built-in anthropic forward                             2
work      sigv4 https://bedrock-runtime.us-west-2.amazonaws.com  2
```

Contexts are sorted by name. `ROUTES` counts aliases and does not include the
default route. A forward to `https://api.anthropic.com` without a model rewrite
is displayed as `built-in anthropic forward`, whether implicit or explicitly
configured.

## `routes`

```console
$ patchbay routes --ctx personal
ALIAS               AUTH    UPSTREAM                   MODEL               MAX_INPUT_TOKENS  CREDENTIAL_SOURCE
openrouter/glm-4.6  inject  https://openrouter.ai/api  z-ai/glm-4.6        200000            PATCHBAY_OPENROUTER_KEY_FILE
openrouter/kimi-k2  inject  https://openrouter.ai/api  moonshotai/kimi-k2  262144            PATCHBAY_OPENROUTER_KEY_FILE
```

Aliases are sorted. The table omits the default route.
`CREDENTIAL_SOURCE` shows an environment variable, AWS profile, or `caller`;
it never contains a secret. `MAX_INPUT_TOKENS` is advertised metadata and is not
enforced by the proxy.

The default context is `personal`. An unknown context exits 1.

## `resolve`

```text
patchbay resolve <context> <model>
```

Exactly two positional arguments are required.

```console
$ patchbay resolve personal openrouter/kimi-k2
context: personal
request_model: openrouter/kimi-k2
route: alias openrouter/kimi-k2
auth: inject
upstream: https://openrouter.ai/api
model: moonshotai/kimi-k2
credential: PATCHBAY_OPENROUTER_KEY_FILE=/home/you/.config/patchbay/openrouter.key
```

An unmatched model displays the effective default route. SigV4 output also shows
the model-map variable and path, then checks whether the selected model is mapped.
It never prints the mapped Bedrock identifier. A missing or invalid map is shown
as `unknown`; that notice does not change the exit code.

`resolve` reports alias/default selection from introspection. The introspection
document does not include the protected foreign rewrite-target set, so the live
gateway may still refuse one of those direct model names with 404. See
[configuration.md](configuration.md#contexts-and-route-selection).

## `project`

```text
patchbay project [dir]
```

The command checks these Claude Code settings files in order:

1. `<dir>/.claude/settings.local.json`;
2. `<dir>/.claude/settings.json`;
3. `$HOME/.claude/settings.json`.

The first non-empty `env.ANTHROPIC_BASE_URL` wins. Missing, unreadable, or invalid
files are reported and skipped. The command prints the selected base URL,
context, and route table.

```console
$ patchbay project /home/you/work/project
settings /home/you/work/project/.claude/settings.local.json: not present
settings /home/you/work/project/.claude/settings.json: ANTHROPIC_BASE_URL=http://127.0.0.1:4100/ctx/work
settings /home/you/.claude/settings.json: ANTHROPIC_BASE_URL=http://127.0.0.1:4100
base_url: http://127.0.0.1:4100/ctx/work
context: work
```

Context selection uses the path Claude Code will request after appending
`/v1/messages`. The command always queries the gateway at `$PATCHBAY_LISTEN`, not
the host in `ANTHROPIC_BASE_URL`; a host mismatch produces a notice. An invalid
URL or unavailable selected context exits 1.

## Gateway-backed failures

| Failure | Result |
|---|---|
| Caller-key variable or file unavailable | Fails before dialing; exit 1. |
| Gateway returns 403 | Reports caller-key refusal; exit 1. |
| Registry endpoint returns 404 | Reports an older patchbay; exit 1. |
| Other non-200 response | Reports address and status; exit 1. |
| Connection failure | Reports that no gateway answered; exit 1. |
| Response exceeds 4 MiB or is invalid JSON | Rejects the response; exit 1. |
| Requested context absent | Reports the requested and available names; exit 1. |

Operational failures and notices are prefixed with `patchbay:` on stderr.

## Introspection endpoint

The CLI uses unprefixed `GET /_patchbay/registry` with `X-Patchbay-Key`. `HEAD` is
also accepted. Other methods return 405; other paths under `/_patchbay` return
404 and are never proxied.

The document contains effective contexts, default routes, aliases, upstreams,
model rewrites, advertised input ceilings, credential-source names, and resolved
key/map paths. It excludes secrets, AWS credentials, mapped Bedrock identifiers,
request bodies, and body-capture settings.

Query it directly with:

```console
$ curl -s -H "X-Patchbay-Key: $(cat ~/.config/patchbay/caller.key)" \
    http://127.0.0.1:4100/_patchbay/registry | jq .
```
