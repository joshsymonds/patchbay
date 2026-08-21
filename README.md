# patchbay

A per-host loopback gateway that speaks the Anthropic Messages API, so one
Claude Code install can talk to several models — and several people's money —
without ever handing your subscription token to anybody but Anthropic.

Point `ANTHROPIC_BASE_URL` at patchbay and set no auth environment variables at
all. Claude Code keeps its own subscription OAuth login, and every Claude
request is forwarded to Anthropic with the caller's credentials untouched, byte
for byte. Ask for a model name that patchbay's registry claims — `openrouter/glm-4.6`,
`bedrock/opus`, anything you name — and that request is sent to the upstream the
route names, with a provider key injected or an AWS SigV4 signature applied
server-side. The key never reaches the client. Which set of routes applies is
chosen per project by a path prefix on the base URL, so a work checkout can bill
a company Bedrock account while everything else stays on your personal
subscription.

It is a single static Go binary with a pure-Go SQLite dependency.

## How it works

```
Claude Code                 patchbay (127.0.0.1:4100)                upstream
-----------                 -------------------------                --------

ANTHROPIC_BASE_URL ───► /v1/messages ─┐
  (no /ctx prefix)                    │  context: personal
                                      │
ANTHROPIC_BASE_URL ───► /ctx/work/... ┤  context: work (prefix stripped)
  (per project)                       │
                                      ▼
                            look up "model" in the
                            selected context's routes
                                      │
        ┌─────────────────────────────┼─────────────────────────────┐
        ▼                             ▼                             ▼
   no alias matched              auth: inject                  auth: sigv4
   → default route          caller key required            caller key required
        │                    drop caller creds              drop caller creds
   auth: forward             inject key from file           sign with AWS creds
   caller's own creds        (env name → path → key)        exported from a profile
        │                             │                             │
        ▼                             ▼                             ▼
  api.anthropic.com            openrouter.ai/…            bedrock-runtime.<region>
        │                             │                             │
        └─────────────────────────────┴─────────────────────────────┘
                                      │
                       stream back verbatim, flush by flush
                       + one JSONL ledger record per request
```

Request flow, in order:

```
1.  /_patchbay/…            answered by patchbay itself (caller key required)
2.  /ctx/<name>/…           context selected, prefix stripped; unknown name → 404
3.  GET /v1/models[/{id}]   answered locally: aliases merged with the account's models
4.  everything else         resolve the body's "model" in the selected context
                            → matched alias  → that route
                            → a foreign route's rewrite target
                                             → 404, naming the alias to ask for;
                                               never the default route
                            → no match       → the context's default route
                                               (built-in: forward to Anthropic)
5.  rewrite the body only if the route asks for it, otherwise forward the
    caller's exact bytes
6.  apply the route's credentials, send, stream the answer back unchanged
7.  append one ledger record, off the request path
```

## Quickstart

Build it — Go downloads the pinned pure-Go SQLite module on first build:

```console
$ go build ./cmd/patchbay
```

Give patchbay a caller key. It is a local shared secret that gates the routes
which spend *your* provider keys:

```console
$ mkdir -p ~/.config/patchbay
$ head -c 32 /dev/urandom | base64 > ~/.config/patchbay/caller.key
$ chmod 600 ~/.config/patchbay/caller.key
```

Write a registry at `~/.config/patchbay/routes.json`. The minimum useful one is
a single foreign alias in the personal context:

```json
{
  "contexts": {
    "personal": {
      "routes": {
        "openrouter/glm-4.6": {
          "auth": "inject",
          "base_url": "https://openrouter.ai/api",
          "model": "z-ai/glm-4.6",
          "api_key_env_file": "PATCHBAY_OPENROUTER_KEY_FILE",
          "strip_cache_control": true
        }
      }
    }
  }
}
```

Nothing in that file is a secret: `api_key_env_file` is the *name* of an
environment variable, whose *value* is the path of a file holding the key.

`base_url` is a root, not an endpoint: the inbound path is appended to it
verbatim, so `https://openrouter.ai/api` + `/v1/messages` is what gets
requested. Check your provider's own Anthropic-compatible endpoint and set the
root that produces it. The upstream does not have to be a cloud — a model
server on your own network works too ([Local
models](docs/configuration.md#local-models)), as does a translating proxy in
front of a chat subscription
([ChatGPT subscription routes](docs/chatgpt-subscription.md)).

Put the provider key in the file that variable will name, then check the
registry and run the gateway:

```console
$ install -m600 /dev/null ~/.config/patchbay/openrouter.key
$ $EDITOR ~/.config/patchbay/openrouter.key      # the OpenRouter key, one line

$ ./patchbay check
OK /home/you/.config/patchbay/routes.json: 1 context (personal), 1 route

$ export PATCHBAY_CALLER_KEY_FILE=~/.config/patchbay/caller.key
$ export PATCHBAY_OPENROUTER_KEY_FILE=~/.config/patchbay/openrouter.key
$ ./patchbay serve
```

Point Claude Code at it — in `~/.claude/settings.json` for every project, or
`.claude/settings.json` for one:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:4100",
    "ANTHROPIC_CUSTOM_HEADERS": "X-Patchbay-Key: <contents of caller.key>"
  }
}
```

Leave `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` unset. Claude Code keeps
using its subscription login, patchbay forwards that login to Anthropic
untouched, and `/model openrouter/glm-4.6` rides the OpenRouter key instead.

A full example registry using all three auth modes is in
[examples/routes.json](examples/routes.json). On Nix, the flake exports a
home-manager module that runs all of this as a service — see
[docs/nix.md](docs/nix.md). Without Nix, there is a container image and a
compose example — see [docs/docker.md](docs/docker.md).

## Contexts

A context is a named world: its own aliases, and its own default route for every
model those aliases do not claim. A request selects one with a path prefix on
the base URL:

| Base URL | Context |
|---|---|
| `http://127.0.0.1:4100` | `personal` (implicit — what any Anthropic client sends) |
| `http://127.0.0.1:4100/ctx/work` | `work` |

The prefix is stripped before anything else looks at the path, so the upstream
sees `/v1/messages` either way. Since Claude Code reads `.claude/settings.json`
from the project directory, the context is a per-project choice:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:4100/ctx/work"
  }
}
```

Every request from that checkout resolves inside `work` — including plain
`claude-*` model names, which the `work` context can route to Bedrock on the
company account while the same model in any other directory stays on your
subscription. `patchbay project` prints which context a directory ends up in
and what that context will do.

Contexts fail closed. A named context the current registry cannot produce — an
unknown name, a broken file, no file at all — is refused with 404 rather than
served by some other context. Only the bare, unprefixed path has a built-in
fallback.

## The caller key

`inject` and `sigv4` routes, the `/_patchbay/registry` introspection endpoint,
and the `/_patchbay/usage/summary` accounting endpoint require the header
`X-Patchbay-Key` to match the contents of the file
named by `$PATCHBAY_CALLER_KEY_FILE`. Both sides are hashed and compared in
constant time.

The reason is asymmetry. A `forward` route spends the caller's own credentials:
a process that reaches patchbay without any credential of its own gets nothing
from it. An `inject` or `sigv4` route spends *yours* — a household OpenRouter
key, an AWS account — and any local process could otherwise ask for it just by
naming the alias. So those two modes refuse to build the upstream request until
the caller proves it knows the key, and the check runs before any key is read,
any credential exported, or any signature made. Missing or mismatched is 403
(never 401, which Claude Code would read as a prompt to log in again); a caller
key the gateway itself cannot read is 500.

## CLI

```
patchbay serve                        run the gateway until signaled
patchbay contexts                     list every context the running gateway serves
patchbay routes [--ctx <name>]        list one context's aliases
patchbay resolve <context> <model>    show which route one request would ride
patchbay project [dir]                show which context a directory selects
patchbay check [--registry <path>]    validate a registry file, no daemon needed
patchbay usage snapshot <dest>        write a consistent SQLite usage database snapshot
patchbay usage export <dest>          write a deterministic JSONL usage database export
```

`contexts`, `routes`, `resolve`, and `project` ask the running gateway over its
introspection endpoint and need the caller key. `usage snapshot` and `usage export`
operate directly on the usage database and need neither a running gateway nor a
caller key. Details and worked output in [docs/cli.md](docs/cli.md).

## Ledger

Every proxied request appends one JSON line to
`$XDG_STATE_HOME/patchbay/ledger/patchbay-<UTC date>.jsonl`: when, which host,
which context, what class of credential the client presented, which alias and
upstream and model, the status, token counts, timings, the shape of the content
blocks that came back, and the stop reason. It is written off the request path
and fsynced; a ledger that fails costs the record and nothing else. The schema
has no field for headers of any kind, so no credential can land in it. See
[docs/ledger.md](docs/ledger.md).

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

## Security model

patchbay binds `127.0.0.1:4100` by default, and that default matters: the
gateway forwards the caller's own Anthropic credentials, so anything that can
reach the socket can spend them. Do not move it to a routable address.

The caller key is a gate on patchbay's *own* spending — inject routes, Bedrock
signing, and the introspection document that enumerates every upstream this
machine can spend money at. It is not a general authentication layer: the
forward path is deliberately ungated, because a caller there pays with a
credential it already had. Anyone who can read the key file can use the foreign
routes, so it lives outside the store, 0600, owned by you.

Secrets reach patchbay by double indirection only: the registry names an
environment variable, the variable's value is a path, and the file at that path
holds the key. The registry file itself holds no secret and can be world-
readable, checked in, or built by Nix into `/nix/store`. Key files are read per
request, so rotating one takes effect immediately, and nothing is cached in
memory afterwards. Bedrock model ids live in their own map file for the same
reason — no account-identifying ARN belongs in a config you might share.

A few more guarantees worth knowing:

- Redirects are never followed, on any path. A 3xx is relayed to the caller as
  the upstream's own answer rather than replayed, with credentials, at whatever
  host it named.
- A `forward` route may target only `https://api.anthropic.com`; validation
  rejects any other upstream, so the caller's subscription token cannot be
  aimed elsewhere.
- An `inject` route may not carry `claude` in its alias or its rewrite target,
  so a foreign key can never pay for something that reads in the ledger as a
  Claude model.
- `inject` relays only `/v1/messages` and `/v1/messages/count_tokens`; `sigv4`
  relays only `/v1/messages`. A stray path on either is refused, not spent.
- Errors patchbay originates are prefixed `patchbay:` inside an Anthropic-shaped
  error envelope, and never echo a header value.

## Documentation

- [docs/configuration.md](docs/configuration.md) — environment variables, the
  registry v2 schema field by field, hot reload, failure semantics
- [docs/auth-modes.md](docs/auth-modes.md) — `forward`, `inject`, `sigv4`, and
  the full Bedrock story
- [docs/chatgpt-subscription.md](docs/chatgpt-subscription.md) — fronting a
  ChatGPT subscription through CLIProxyAPI, and why that is a Terms-of-Use
  decision before it is a configuration one
- [docs/nix.md](docs/nix.md) — the flake outputs and the home-manager module,
  including the one-flag `codexUpstream` deployment
- [docs/docker.md](docs/docker.md) — the GHCR image and the compose example,
  for hosts without Nix
- [docs/ledger.md](docs/ledger.md) — the JSONL record schema
- [docs/cli.md](docs/cli.md) — every subcommand with real output

## Building and testing

```console
$ go build ./cmd/patchbay      # the binary
$ just check                   # nixpkgs pin check, golangci-lint, race + coverage
$ nix build .#patchbay         # packaged build
```

## License

MIT — see [LICENSE](LICENSE).
