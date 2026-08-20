# patchbay

patchbay is a per-host gateway for the Anthropic Messages API. Claude Code sends
requests to patchbay through `ANTHROPIC_BASE_URL`; patchbay selects a route from
the request model and the context encoded in the URL.

An unmatched model uses the context's default route. The built-in default
forwards to Anthropic with the caller's credentials. Aliases can instead inject
a provider key or translate and sign requests for Amazon Bedrock.

Contexts give projects separate route sets. A work checkout can use Bedrock while
unprefixed requests continue to use a personal Claude subscription.

The Go module uses only the standard library. `sigv4` routes also require the AWS
CLI because patchbay runs `aws configure export-credentials` for the configured
profile.

## Routing

```text
Claude Code                    patchbay                       upstream

ANTHROPIC_BASE_URL ----------> select context
  http://127.0.0.1:4100        bare path: personal
  .../ctx/work                  /ctx/work: work
                                      |
                               resolve model
                         +------------+------------+
                         |            |            |
                      default       inject       sigv4
                         |            |            |
                      caller      provider key    AWS
                       auth          file       credentials
                         |            |            |
                         +------------+------------+
                                      |
                         relay response and append JSONL ledger
```

For each request, patchbay:

1. Selects `personal` for a bare path or the named `/ctx/<context>` context.
2. Resolves an exact alias, then falls back to the context's default route.
3. Applies the route's model and body rewrites.
4. Applies `forward`, `inject`, or `sigv4` authentication.
5. Relays the response and writes one ledger record asynchronously.

An unavailable explicit context returns 404. A missing or invalid registry still
leaves the bare personal path with the built-in Anthropic route.

`GET /_patchbay/registry` and explicit-context model endpoints require the caller
key. Model endpoints on the bare personal path do not. These local endpoints do
not write ledger records.

## Quickstart

Build with Go 1.26:

```console
$ go build ./cmd/patchbay
```

Create a caller key. This key authorizes routes that spend credentials configured
on the gateway:

```console
$ mkdir -p ~/.config/patchbay
$ head -c 32 /dev/urandom | base64 > ~/.config/patchbay/caller.key
$ chmod 600 ~/.config/patchbay/caller.key
```

Create `~/.config/patchbay/routes.json`:

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

`api_key_env_file` is an environment variable name. The variable contains the
path to the provider-key file; the registry contains neither the path nor the
secret. Do not put secrets in `extra_body` or other registry fields.

Write the provider key, validate the registry, and start patchbay:

```console
$ install -m600 /dev/null ~/.config/patchbay/openrouter.key
$ $EDITOR ~/.config/patchbay/openrouter.key

$ ./patchbay check
OK /home/you/.config/patchbay/routes.json: 1 context (personal), 1 route

$ export PATCHBAY_CALLER_KEY_FILE=~/.config/patchbay/caller.key
$ export PATCHBAY_OPENROUTER_KEY_FILE=~/.config/patchbay/openrouter.key
$ ./patchbay serve
```

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
context, commit only `ANTHROPIC_BASE_URL` and provide the header from user or local
settings.

Leave `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` unset when the personal
default should use the Claude Code subscription login. Claude Code sends its
stored OAuth credential to patchbay, and patchbay forwards it only on `forward`
routes.

With this configuration, normal Claude model names use Anthropic while
`/model openrouter/glm-4.6` uses the OpenRouter route. Third-party gateways may
accept non-Claude aliases, but [Anthropic does not support](https://code.claude.com/docs/en/llm-gateway)
non-Claude models behind them.

See [examples/routes.json](examples/routes.json) for all three auth modes.

## Contexts

The URL path selects the context:

| Base URL | Context |
|---|---|
| `http://127.0.0.1:4100` | `personal` |
| `http://127.0.0.1:4100/ctx/work` | `work` |

The prefix is removed before the request is sent upstream. Both base URLs send
`/v1/messages` to the selected route.

A project can select a context in `.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:4100/ctx/work"
  }
}
```

Contexts fail closed. If `work` is unavailable, `/ctx/work` returns 404 rather
than falling back to `personal`.

`patchbay project [dir]` shows which Claude Code settings file wins, which context
it selects, and which routes that context serves.

## Caller key

`X-Patchbay-Key` must match the trimmed contents of
`$PATCHBAY_CALLER_KEY_FILE` for:

- `inject` and `sigv4` routes
- `GET` and `HEAD /_patchbay/registry`
- explicit-context `GET /v1/models` requests

patchbay compares SHA-256 digests in constant time and removes the header before
contacting an upstream. A missing or incorrect value returns 403. An unreadable or
unconfigured key file returns 500.

A `forward` route does not require this key because it uses only credentials sent
by the caller. The caller key is not a general authentication layer for the
gateway.

## CLI

```text
patchbay serve                        run the gateway
patchbay contexts                     list contexts
patchbay routes [--ctx <name>]        list aliases
patchbay resolve <context> <model>    show route selection
patchbay project [dir]                inspect Claude Code settings
patchbay check [--registry <path>]    validate a registry file
```

The inspection commands query the running gateway at `$PATCHBAY_LISTEN` and use
`$PATCHBAY_CALLER_KEY_FILE`. See [docs/cli.md](docs/cli.md) for output and exit
codes.

## Ledger

Proxy requests produce JSONL records under
`$XDG_STATE_HOME/patchbay/ledger`, or `~/.local/state/patchbay/ledger` when
`XDG_STATE_HOME` is unset:

```text
patchbay-YYYY-MM-DD.jsonl
```

Records include route selection, status, token counts, timings, response shape,
and stop reason. Routes with `capture_bodies: true` also store request and
response bodies. Header fields are not part of the schema, but captured bodies
may contain sensitive application data.

Ledger writes happen after the client response. A capture or append failure drops
the record and logs a warning without changing the response. See
[docs/ledger.md](docs/ledger.md).

## Security

Keep the default listener on loopback. Claude Code sends its subscription
credential to patchbay over HTTP, and the caller key does not gate `forward`
routes.

Provider keys and Bedrock model maps use file indirection and are read per
request. AWS credentials are exported through the AWS CLI and cached until near
their reported expiration, or for 15 minutes when no expiration is reported.

patchbay also:

- never follows redirects;
- restricts `forward` routes to `https://api.anthropic.com`;
- removes caller credentials before `inject` and `sigv4` requests;
- accepts plain HTTP only for loopback or an `inject` route with `insecure: true`;
- removes hop-by-hop headers in both directions;
- returns Anthropic-shaped gateway errors without credential values.

The container image does not include the AWS CLI and cannot serve `sigv4` routes
without modification.

## Documentation

- [Configuration](docs/configuration.md) -- environment, registry fields, reload,
  endpoints, and limits
- [Auth modes](docs/auth-modes.md) -- `forward`, `inject`, and `sigv4`
- [CLI](docs/cli.md) -- commands, output, and introspection
- [Ledger](docs/ledger.md) -- JSONL schema and reading records
- [Nix](docs/nix.md) -- flake outputs and Home Manager module
- [Docker](docs/docker.md) -- image and Compose setup
- [ChatGPT subscription routes](docs/chatgpt-subscription.md) -- optional
  CLIProxyAPI integration and account risk

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

MIT -- see [LICENSE](LICENSE).
