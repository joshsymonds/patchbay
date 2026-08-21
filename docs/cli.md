# CLI

```text
usage: patchbay <command> [arguments]

commands:
  serve                        run the gateway until it is signaled to stop
  contexts                     list every context the running gateway serves
  seats                        list global reusable Seats
  bindings --ctx <name>        list one Context's public model bindings
  resolve [--subagent] <context> <model>
                               show the complete non-secret resolution
  project [dir]                show which context a directory's Claude Code settings select
  check [--registry <path>]    validate a registry file without asking the gateway
```

A bare command, unknown subcommand, or invalid argument list prints usage to
stderr and exits 2. `-h`, `--help`, and `help` print top-level usage and exit 0.
The legacy `routes` command does not exist.

| Exit | Meaning |
|---|---|
| `0` | Command completed. Notices may still appear on stderr. |
| `1` | Operational failure. |
| `2` | Invalid command line. |

Go's flag parser accepts one or two leading dashes. Flags must precede positional
arguments. Command-specific help prints to stderr and exits 2.

## String values

Model selectors and Seat rewrite targets are opaque exact strings: whatever bytes
an operator wrote, including tabs, newlines, and values that look like structure.
Written raw into a table or a `key: value` line those bytes would become
structure, and no token could be reserved to mean "absent" without colliding with
a selector genuinely spelled that way.

So every string value the CLI prints is a quoted Go/JSON-style literal, and every
absent one is an unquoted `null`:

| Printed | Meaning |
|---|---|
| `"moonshotai/kimi-k2"` | that exact string |
| `"-"` | a model named `-` |
| `"null"` | a model named `null` |
| `"with\tnewline"` | one value, escaped, still one cell |
| `null` | no value at all |
| `262144`, `false` | a number or a boolean, never quoted |

Quoting escapes tabs and newlines, so one binding is always one row and one field
is always one line. Table headers (`MODEL`, `SEAT`) and patchbay's own fixed
notes are unquoted, because they are this command's words rather than data.

Operational stderr stays prose rather than a quoted literal, but the ASCII
control bytes in it are escaped (`\n`, `\r`, `\t`, and `\xNN` for the rest), so
one diagnostic is always one line.

`contexts` is the one table left unquoted. Every cell in it is a registry ID or a
count, and an ID must match `^[a-z0-9][a-z0-9-]*$` — a grammar with no tab, no
newline and no ambiguity to escape.

`contexts`, `seats`, `bindings`, `resolve`, and `project` query the running
gateway at `$PATCHBAY_LISTEN`, defaulting to `127.0.0.1:4100`. They read the
caller key from `$PATCHBAY_CALLER_KEY_FILE` before dialing and fetch
`/_patchbay/registry`. `check` works without a running gateway.

Examples below use [examples/routes.json](../examples/routes.json).

## `serve`

```console
$ patchbay serve
{"time":"2026-08-20T16:11:32-07:00","level":"INFO","msg":"patchbay listening","version":"0.0.0-dev","listen":"127.0.0.1:4100","registry":"/home/you/.config/patchbay/routes.json","ledger_dir":"/home/you/.local/state/patchbay/ledger"}
```

The gateway validates the initial registry synchronously before creating the
listener. Missing, unreadable, malformed, or invalid configuration exits 1 and no
socket is bound.

After startup, the gateway runs until `SIGINT` or `SIGTERM`. It logs JSON to
stderr and writes nothing to stdout. HTTP shutdown and ledger drain each get up
to five seconds. Listener, server, or shutdown failures exit 1. Trailing
arguments after `serve` are currently ignored.

## `check`

```text
patchbay check [--registry <path>]
```

The registry path is selected from:

1. `--registry <path>`;
2. `$PATCHBAY_REGISTRY`;
3. `$XDG_CONFIG_HOME/patchbay/routes.json`;
4. `$HOME/.config/patchbay/routes.json`.

A missing file is an error. Success prints global Seat, Context, and public
binding counts:

```console
$ patchbay check --registry examples/routes.json
OK "examples/routes.json": 6 seats, 2 contexts (personal, work), 4 bindings
```

Validation is structural and referential only. It never reads credential files
or model maps, exports AWS credentials, validates AWS sessions, or contacts an
upstream. File and validation failures exit 1. Positional arguments and unknown
flags exit 2.

## `contexts`

```console
$ patchbay contexts
CONTEXT   DEFAULT_SEAT  BINDINGS  SUBAGENT_BINDINGS
personal  anthropic     2         2
work      work-bedrock  2         0
```

Contexts are sorted by ID. `BINDINGS` counts public `models` entries.
`SUBAGENT_BINDINGS` is zero when no marked-subagent policy exists; otherwise it
counts exact model bindings plus the subagent default.

## `seats`

```console
$ patchbay seats
SEAT                  AUTH_MODE  UPSTREAM                                           MODEL                 MAX_INPUT_TOKENS  CONFIG_SOURCE
"anthropic"           "forward"  "https://api.anthropic.com"                        null                  null              "caller"
"openrouter-glm-4-6"  "inject"   "https://openrouter.ai/api"                        "z-ai/glm-4.6"        200000            "PATCHBAY_OPENROUTER_KEY_FILE"
"openrouter-kimi-k2"  "inject"   "https://openrouter.ai/api"                        "moonshotai/kimi-k2"  262144            "PATCHBAY_OPENROUTER_KEY_FILE"
"sol-subscription"    "inject"   "http://127.0.0.1:8317"                            "gpt-5.6-sol"         372000            "PATCHBAY_CHATGPT_KEY_FILE"
"work-bedrock"        "sigv4"    "https://bedrock-runtime.us-west-2.amazonaws.com"  null                  null              "aws_profile:work-bedrock"
"work-bedrock-opus"   "sigv4"    "https://bedrock-runtime.us-west-2.amazonaws.com"  "claude-opus-4-5"     200000            "aws_profile:work-bedrock"
```

Seats are global and sorted by ID. `CONFIG_SOURCE` is the non-secret configured
source descriptor: `caller`, an API-key environment-variable name, or
`aws_profile:<name>`. The table never resolves file paths, exports credentials,
reads model maps, or prints secrets.

## `bindings`

```text
patchbay bindings --ctx <name>
```

`--ctx` is required.

```console
$ patchbay bindings --ctx personal
MODEL                 SEAT
"openrouter/glm-4.6"  "openrouter-glm-4-6"
"openrouter/kimi-k2"  "openrouter-kimi-k2"
```

Selectors are sorted and displayed exactly. This table includes only public
`context.models` bindings. Marked-subagent policy is not published as model IDs.
An unknown Context exits 1.

## `resolve`

```text
patchbay resolve [--subagent] <context> <model>
```

Exactly two positional arguments are required. The Context is explicit; this
command does not substitute `default_context`.

```console
$ patchbay resolve personal openrouter/kimi-k2
context: "personal"
request_model: "openrouter/kimi-k2"
observation: "none"
selection: "model"
selection_model: "openrouter/kimi-k2"
seat: "openrouter-kimi-k2"
auth_mode: "inject"
upstream: "https://openrouter.ai/api"
model: "moonshotai/kimi-k2"
config_source: "PATCHBAY_OPENROUTER_KEY_FILE"
api_key_env_file: "PATCHBAY_OPENROUTER_KEY_FILE"
aws_profile: null
aws_region: null
model_map_env_file: null
max_input_tokens: 262144
max_output_tokens: 32000
insecure: false
strip_cache_control: true
capture_bodies: false
```

`observation` identifies the input observation class explicitly: `"none"` for
an ordinary resolution and `"subagent"` when `--subagent` supplies the marked
observation. `resolve` uses the same resolution implementation as the gateway, including
public binding precedence and protected foreign rewrite-target refusal. It does
not read provider-key or model-map files, resolve file paths, export AWS
credentials, or contact upstreams. SigV4 output shows only the model-map
environment-variable name, never a mapped Bedrock identifier.

`--subagent` supplies a valid synthetic observation with agent ID
`patchbay-cli`. The requested model naturally chooses an exact marked-subagent
binding or the marked-subagent default. The `personal` Context of
[examples/routes.json](../examples/routes.json) binds `claude-opus-5` to the
`sol-subscription` Seat under `subagents`:

```console
$ patchbay resolve --subagent personal claude-opus-5
context: "personal"
request_model: "claude-opus-5"
observation: "subagent"
selection: "subagent"
selection_model: "claude-opus-5"
seat: "sol-subscription"
auth_mode: "inject"
upstream: "http://127.0.0.1:8317"
model: "gpt-5.6-sol"
config_source: "PATCHBAY_CHATGPT_KEY_FILE"
api_key_env_file: "PATCHBAY_CHATGPT_KEY_FILE"
aws_profile: null
aws_region: null
model_map_env_file: null
max_input_tokens: 372000
max_output_tokens: null
insecure: false
strip_cache_control: false
capture_bodies: false
```

Marked traffic that matches no exact binding falls to the marked-subagent
default, which is still a subagent selection — it just names no model. The null
`selection_model` is the only thing separating the unnamed fallback from an
exact marked hit, and it is what ledger v2 stores:

```console
$ patchbay resolve --subagent personal some-unbound-model
context: "personal"
request_model: "some-unbound-model"
observation: "subagent"
selection: "subagent"
selection_model: null
seat: "sol-subscription"
...
```

An ordinary Context default — unmarked traffic naming no public binding —
reports `selection: "default"` with `selection_model: null`.

## `project`

```text
patchbay project [dir]
```

The command checks these Claude Code settings files in order:

1. `<dir>/.claude/settings.local.json`;
2. `<dir>/.claude/settings.json`;
3. `$HOME/.claude/settings.json`.

The first non-empty `env.ANTHROPIC_BASE_URL` wins. Missing, unreadable, or invalid
files are reported and skipped. The command fetches introspection first and uses
its configured `default_context` for a bare or absent base URL. It prints the
selected base URL, Context, and public bindings. When no file sets one,
`base_url` is `null` and the reason is a notice on stderr.

Settings paths and base URLs come out of files patchbay does not own, so they are
printed as quoted literals. The fixed notes — `not present`, `unreadable`,
`malformed JSON, skipped`, `no ANTHROPIC_BASE_URL` — are patchbay's own words and
stay unquoted.

```console
$ patchbay project /home/you/work/project
settings "/home/you/work/project/.claude/settings.local.json": not present
settings "/home/you/work/project/.claude/settings.json": ANTHROPIC_BASE_URL="http://127.0.0.1:4100/ctx/work"
settings "/home/you/.claude/settings.json": ANTHROPIC_BASE_URL="http://127.0.0.1:4100"
base_url: "http://127.0.0.1:4100/ctx/work"
context: "work"
MODEL               SEAT
"anthropic-direct"  "anthropic"
"bedrock/opus"      "work-bedrock-opus"
```

Context selection uses the path Claude Code will request after appending
`/v1/messages`. The command always queries the gateway at `$PATCHBAY_LISTEN`, not
the host in `ANTHROPIC_BASE_URL`; a host mismatch produces a notice. An invalid
URL, invalid registry, or unavailable selected Context exits 1.

## Gateway-backed failures

| Failure | Result |
|---|---|
| Caller-key variable or file unavailable | Fails before dialing; exit 1. |
| Gateway returns 403 | Reports caller-key refusal; exit 1. |
| Registry endpoint returns 404 | Reports an older patchbay; exit 1. |
| Introspection reports `registry_valid: false` | Reports the sanitized validation error; exit 1. |
| Other non-200 response | Reports address and status; exit 1. |
| Connection failure | Reports that no gateway answered; exit 1. |
| Response exceeds 4 MiB or is invalid JSON | Rejects the response; exit 1. |
| Requested Context absent | Reports the requested and available IDs; exit 1. |

Operational failures and notices are prefixed with `patchbay:` on stderr.

## Introspection endpoint

The CLI uses unprefixed `GET /_patchbay/registry` with `X-Patchbay-Key`. `HEAD` is
also accepted. Other methods return 405; other paths under `/_patchbay` return
404 and are never proxied.

The valid document contains:

- `default_context`;
- global non-secret Seat summaries;
- Context defaults and public bindings;
- optional marked-subagent policy;
- registry path, listen address, and `registry_valid: true`.

It excludes credential values, AWS credentials, mapped Bedrock identifiers,
`extra_body` contents, and resolved key or model-map file paths.

While the registry is invalid, unprefixed caller-key-gated introspection remains
200 and returns `default_context: null`, empty `seats` and `contexts`,
`registry_valid: false`, and a sanitized `validation_error`.

Query it directly with:

```console
$ curl -s -H "X-Patchbay-Key: $(cat ~/.config/patchbay/caller.key)" \
    http://127.0.0.1:4100/_patchbay/registry | jq .
```
