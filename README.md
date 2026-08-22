# patchbay

patchbay is a per-host gateway for the Anthropic Messages API. Claude Code sends
requests through `ANTHROPIC_BASE_URL`; patchbay selects a Context from the URL,
resolves the requested model to a global Seat, and forwards the request.

A Seat is a reusable upstream, authentication, rewrite, limits, and capture
configuration. A Context publishes exact model-selector-to-Seat-ID bindings and
names a default Seat. An optional marked-subagent policy can select separate
Seats when Claude Code sends a valid direct agent marker.

It is a single static CGO-free binary. Its one module dependency is a pure-Go
SQLite driver, which backs the accounting database. `sigv4` Seats also require
the AWS CLI because patchbay runs `aws configure export-credentials` for the
configured profile.

## Resolution

```text
Claude Code                    patchbay                       upstream

ANTHROPIC_BASE_URL ----------> select Context
  http://127.0.0.1:4100        bare path: default_context
  .../ctx/work                  /ctx/work: work
                                      |
                               resolve model
                         +------------+------------+
                         |            |            |
                     public model   marked       default
                       binding     subagent        Seat
                         |          policy           |
                         +------------+--------------+
                                      |
                               select global Seat
                         +------------+------------+
                         |            |            |
                      forward       inject       sigv4
                         |            |            |
                       caller      provider key    AWS
                        auth          file       credentials
                         +------------+------------+
                                      |
                         relay response and append ledger v2
```

Selection precedence is exact and shared by the gateway and CLI:

1. exact `context.models` binding;
2. valid marked subagent plus exact `context.subagents.models` binding;
3. valid marked subagent plus `context.subagents.default_seat`;
4. refusal of a protected foreign rewrite target;
5. `context.default_seat`.

Steps 2 and 3 are both recorded as a `subagent` selection; step 3 names no model,
which is the only thing distinguishing the unnamed marked fallback from an exact
marked hit. Step 5 is recorded as a `default` selection, also naming no model.

Patchbay observes `x-claude-code-agent-id` only when exactly one value is present,
it is 1–256 bytes, and every byte is printable ASCII `0x21`–`0x7e`. The value is
used verbatim. `x-claude-code-parent-agent-id` uses the same validation but never
marks traffic by itself. Both headers continue upstream unchanged. They are
observations, not authentication, identity, authorization, or a security
boundary.

Patchbay has no auxiliary-call classifier, prompt matching, or semantic agent
inference. Unmarked traffic follows ordinary bindings. Any request carrying a
valid direct agent marker follows the configured marked-subagent policy.

## Quickstart

Build with Go 1.26, which downloads the pinned pure-Go SQLite module on the
first build:

```console
$ go build ./cmd/patchbay
```

Create a caller key. This local shared secret gates Seats that spend credentials
configured on the gateway:

```console
$ mkdir -p ~/.config/patchbay
$ head -c 32 /dev/urandom | base64 > ~/.config/patchbay/caller.key
$ chmod 600 ~/.config/patchbay/caller.key
```

Create `~/.config/patchbay/routes.json`:

```json
{
  "default_context": "personal",
  "seats": {
    "anthropic": {
      "upstream": "https://api.anthropic.com",
      "auth_mode": "forward"
    },
    "openrouter-glm-4-6": {
      "upstream": "https://openrouter.ai/api",
      "model": "z-ai/glm-4.6",
      "auth_mode": "inject",
      "api_key_env_file": "PATCHBAY_OPENROUTER_KEY_FILE",
      "billing": "metered",
      "strip_cache_control": true
    }
  },
  "contexts": {
    "personal": {
      "default_seat": "anthropic",
      "models": {
        "openrouter/glm-4.6": "openrouter-glm-4-6"
      }
    }
  }
}
```

`api_key_env_file` is an environment variable name. The variable contains the
path to the provider-key file; the registry contains neither the path nor the
secret. Do not put secrets in `extra_body` or other registry fields.

`billing` declares how an `inject` Seat's traffic is accounted for —
`subscription`, `metered`, or `self_hosted`. Every `inject` Seat must declare
it, and only an `inject` Seat may: nothing downstream can look at a request and
tell what an operator key draws on. A `forward` Seat's class is derived from the
credential the caller presented and a `sigv4` Seat is metered by the AWS account
that signed it, so a declaration on either is refused rather than ignored.

Write the provider key, validate the registry, and start patchbay:

```console
$ install -m600 /dev/null ~/.config/patchbay/openrouter.key
$ $EDITOR ~/.config/patchbay/openrouter.key

$ export PATCHBAY_CALLER_KEY_FILE=~/.config/patchbay/caller.key
$ export PATCHBAY_OPENROUTER_KEY_FILE=~/.config/patchbay/openrouter.key

$ ./patchbay check
OK /home/you/.config/patchbay/routes.json: 2 seats, 1 context (personal), 1 binding

$ ./patchbay serve
```

Initial registry loading is strict and happens before the listener binds. A
missing, unreadable, malformed, or invalid registry makes `serve` fail.

Configure Claude Code in user settings or a project's local settings:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:4100",
    "ANTHROPIC_CUSTOM_HEADERS": "X-Patchbay-Key: <contents of caller.key>"
  }
}
```

Keep the caller key out of shared project settings. If the project should share a
Context, commit only `ANTHROPIC_BASE_URL` and provide the header from user or
local settings.

Leave `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` unset when the default Seat
should use the Claude Code subscription login. Claude Code sends its stored OAuth
credential to patchbay, and patchbay forwards it only on `forward` Seats.

With this configuration, ordinary Claude model names use the Context default,
while `/model openrouter/glm-4.6` uses the OpenRouter Seat. Third-party gateways
may accept non-Claude selectors, but [Anthropic does not support](https://code.claude.com/docs/en/llm-gateway)
non-Claude models behind them.

See [examples/routes.json](examples/routes.json) for all three auth modes.

## Contexts and Seats

The URL path selects a Context:

| Base URL | Context |
|---|---|
| `http://127.0.0.1:4100` | configured `default_context` |
| `http://127.0.0.1:4100/ctx/work` | `work` |

The prefix is removed before forwarding. Both forms send `/v1/messages` to the
selected Seat.

A project can select a Context in `.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:4100/ctx/work"
  }
}
```

Contexts fail closed. While the registry is valid, an unknown explicit Context
returns 404. A hot invalid or missing registry installs an invalid sentinel, and
all new proxy and model requests return 503 `api_error`; stale snapshots are not
used. Requests that already resolved finish with their immutable resolution. A
valid replacement restores service atomically on the next request.

`patchbay project [dir]` shows which Claude Code settings file wins, which Context
it selects, and that Context's public bindings.

## Caller key

`X-Patchbay-Key` must match the trimmed contents of
`$PATCHBAY_CALLER_KEY_FILE` for:

- `inject` and `sigv4` Seats, including marked-subagent selections;
- `GET` and `HEAD /_patchbay/registry`;
- `GET /_patchbay/usage/summary`;
- explicit-Context `GET /v1/models` requests.

The gate runs after Seat selection and before provider-key reads, model-map reads,
AWS credential export, or upstream access. Patchbay compares SHA-256 digests in
constant time and removes the header before contacting an upstream. A missing or
incorrect value returns 403. An unreadable or unconfigured caller-key file returns
500.

A `forward` Seat does not require the caller key because it uses only credentials
sent by the caller. The caller key is not general gateway authentication.

## CLI

```text
patchbay serve
patchbay contexts
patchbay seats
patchbay bindings --ctx <name>
patchbay resolve [--subagent] <context> <model>
patchbay project [dir]
patchbay check [--registry <path>]
patchbay usage snapshot <dest>
patchbay usage export <dest>
patchbay usage import <ledger-dir>
```

Inspection commands query the running gateway at `$PATCHBAY_LISTEN` and use
`$PATCHBAY_CALLER_KEY_FILE`. `resolve --subagent` supplies a valid synthetic
marker and uses the same resolution algorithm as runtime traffic. The `usage`
commands operate directly on `$PATCHBAY_USAGE_DB` and need neither a running
gateway nor a caller key. See [docs/cli.md](docs/cli.md).

## Models and introspection

Public model endpoints expose Context selectors, never Seat IDs or marked-subagent
policy. A forward default merges public bindings with Anthropic's model listing.
An inject default contributes its configured rewrite model. A SigV4 default
contributes non-empty model-map keys, never mapped Bedrock identifiers. Public
bindings shadow duplicate upstream IDs.

Unprefixed caller-key-gated `GET /_patchbay/registry` remains available while the
registry is invalid. It returns `registry_valid: false`, a sanitized
`validation_error`, `default_context: null`, and empty Seat and Context summaries.
Model and introspection endpoints do not write ledger records.

## Ledger

Every proxy-bound request is ledger-eligible, including registry, Context, body,
rewrite-target, caller-key, and selected-Seat failures. Records are written under
`$XDG_STATE_HOME/patchbay/ledger`, or
`~/.local/state/patchbay/ledger` when `XDG_STATE_HOME` is unset:

```text
patchbay-v2-YYYY-MM-DD.jsonl
```

`PATCHBAY_LEDGER_DIR` overrides the ledger directory. `PATCHBAY_USAGE_DB`
overrides the durable accounting database, which defaults to
`$XDG_STATE_HOME/patchbay/usage.sqlite` (or
`$HOME/.local/state/patchbay/usage.sqlite`). Unlike the ledger, Patchbay will
not start if the accounting database cannot be opened.

`PATCHBAY_RATE_CARDS` optionally names a JSON file containing immutable,
effective-dated per-million-token rates. Each entry has `model`, RFC3339
`effective_from`, `source`, and string-valued `rates_usd_per_million` fields:
`input`, `output`, `cache_read`, `cache_creation`, `cache_creation_5m`, and
`cache_creation_1h`. Patchbay parses rates exactly as decimal USD, and will not
start if the file cannot be read or parsed or conflicts with a card already
recorded in the usage database.

Selected records include Context, selection reason, Seat, effective upstream,
upstream model, requested model, auth mode, inbound credential class, configured
credential source, and any validated subagent observation. Pre-selection failures
omit selection-dependent fields. `capture_bodies: true` stores rewritten request
and captured response bodies.

Only v2 files are scanned; there is no legacy decoder. Ledger writes happen after
the client response. A capture or append failure drops the record and logs a
warning without changing the response. See [docs/ledger.md](docs/ledger.md).

## Accounting

### Usage backup and export

`patchbay usage snapshot <dest>` makes an atomic SQLite snapshot that is
consistent even while the daemon is writing. `patchbay usage export <dest>`
writes the same point-in-time data as deterministic JSONL: a version header,
then `rate_cards` rows and `accounting_requests` rows in ID order. Both commands
read the database without migrating or changing it, refuse an existing
destination, and create a 0600 destination file.

The live `PATCHBAY_USAGE_DB` must remain on a local filesystem. Snapshot and
export destinations can be anywhere. To restore a snapshot, stop the daemon,
plain-copy the snapshot over the resolved `$PATCHBAY_USAGE_DB` path, then start
the daemon. `patchbay serve` opens the restored database and automatically
migrates an older-schema snapshot forward before serving.

### Legacy ledger import

`patchbay usage import <ledger-dir>` imports recognized `patchbay-YYYY-MM-DD.jsonl`
and `patchbay-v2-YYYY-MM-DD.jsonl` files in filename order. Each imported row
keeps its ledger basename and one-based line number as `import_source` and
`import_line`; live accounting rows leave both columns null. That provenance
makes a rerun safe and lets a resumed import skip only records already stored.
Idempotence assumes ledger files are immutable: changing an imported line, or
importing a different file with the same basename, retains the originally
imported accounting row for that basename and line number.

Before the first file, Patchbay fixes one overlap boundary at the earliest live
SQLite `started_at`. Ledger records at or after that instant are skipped, so the
period where both ledger and SQLite accounting ran is never double-counted.
The command prints imported, boundary-skipped, and already-present counts for
each file. It can run while the daemon is serving.

Legacy ledgers do not contain complete pricing or normalization evidence. Imported
subscription rows are known zero-marginal cost; imported metered rows deliberately
remain unknown cost and are never rate-card priced.

### Usage summary

`GET /_patchbay/usage/summary?since=<RFC3339>&until=<RFC3339>[&context=<name>]`
returns the half-open `[since, until)` aggregate from the durable accounting
database. `since` and `until` are required; `context` limits the aggregate to
one context when supplied. The caller key is required.

The response has a stable wrapper with canonical UTC window bounds:

```json
{
  "since": "2026-08-20T12:00:00Z",
  "until": "2026-08-20T13:00:00Z",
  "context": "work",
  "summary": {
    "requests": {"messages": 12, "count_tokens": 3, "other": 1},
    "messages": {
      "finalized": 10,
      "pending": 2,
      "known_cost_nano_usd": 123456789,
      "unknown_cost_rows": 1,
      "input_tokens": 1000,
      "output_tokens": 200,
      "cache_read_tokens": 50,
      "cache_creation_total": 25,
      "by_billing_class": {
        "metered": {"requests": 8, "known_cost_nano_usd": 123456789, "unknown_cost_rows": 1}
      },
      "by_cost_basis": {
        "provider_reported": {"requests": 7, "known_cost_nano_usd": 123456789, "unknown_cost_rows": 0}
      }
    }
  }
}
```

All money is integer nano-USD. `unknown_cost_rows` counts finalized message rows
whose cost cannot be established; it is never estimated. Null billing classes
and cost bases are grouped as `"unknown"`. Cache creation totals use 5-minute
and 1-hour values when either is present, otherwise the base cache-creation
value, so one request is never counted twice.

## Security

Keep the listener on loopback. Claude Code sends its subscription credential to
patchbay over HTTP, and the caller key does not gate `forward` Seats.

Provider keys and Bedrock model maps use file indirection and are read per request.
AWS credentials are exported through the AWS CLI and cached until near their
reported expiration, or for 15 minutes when no expiration is reported.

Registry validation is structural and referential only. It does not read provider
keys or model maps, export AWS credentials, validate AWS sessions, probe upstreams,
or otherwise spend configured credentials.

Patchbay also:

- never follows redirects;
- restricts `forward` Seats to `https://api.anthropic.com`;
- removes caller credentials before `inject` and `sigv4` requests;
- accepts plain HTTP only for loopback or an `inject` Seat with `insecure: true`;
- removes hop-by-hop headers in both directions;
- returns Anthropic-shaped gateway errors without credential values.

The container image does not include the AWS CLI and cannot serve `sigv4` Seats
without modification.

## Documentation

- [Configuration](docs/configuration.md) — strict registry, Seats, bindings,
  marked subagents, reload, endpoints, and limits
- [Auth modes](docs/auth-modes.md) — `forward`, `inject`, and `sigv4`
- [CLI](docs/cli.md) — commands, output, and introspection
- [Ledger](docs/ledger.md) — v2 schema and reading records
- [Nix](docs/nix.md) — flake outputs and Home Manager module
- [Docker](docs/docker.md) — image and Compose setup
- [ChatGPT subscription](docs/chatgpt-subscription.md) — optional CLIProxyAPI
  integration and account risk

## Building and testing

```console
$ go build ./cmd/patchbay
$ go test ./...
$ just check
$ nix build .#patchbay
```

`just check` runs lint, race-enabled tests, the coverage gate, and the Nix pin
check.

## License

MIT — see [LICENSE](LICENSE).
