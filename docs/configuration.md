# Configuration

patchbay is configured by two things: a handful of environment variables, and
one JSON registry file. There are no flags on `patchbay serve` — deployment is
meant to be declarative, so the unit file is generated rather than typed.

## Environment variables

| Variable | Read by | Default | Meaning |
|---|---|---|---|
| `PATCHBAY_LISTEN` | `serve`, and every CLI subcommand that asks the daemon | `127.0.0.1:4100` | The address the gateway binds, and the address the CLI dials. Empty is treated as unset. |
| `PATCHBAY_REGISTRY` | `serve`, `check` | `$XDG_CONFIG_HOME/patchbay/routes.json`, else `$HOME/.config/patchbay/routes.json` | The registry file. `check --registry <path>` overrides it for that one invocation. |
| `PATCHBAY_LEDGER_DIR` | `serve` | `$XDG_STATE_HOME/patchbay/ledger`, else `$HOME/.local/state/patchbay/ledger` | Where JSONL records are written. Created 0700 if missing. |
| `PATCHBAY_CALLER_KEY_FILE` | `serve`, and every CLI subcommand that asks the daemon | none | Path of the file holding the caller key. Required by `inject` routes, `sigv4` routes and `/_patchbay/registry`; without it those refuse with 500 and the CLI fails before dialing. |
| `XDG_CONFIG_HOME` | `serve`, `check` | unset | Base for the default registry path. |
| `XDG_STATE_HOME` | `serve` | unset | Base for the default ledger directory. |
| `HOME` | `serve`, `check`, `project` | unset | Fallback base for both defaults, and where `project` looks for the user-level `~/.claude/settings.json`. |
| *(named by a route's `api_key_env_file`)* | `serve` | none | Its value is the path of a file holding that route's API key. You choose the variable name. |
| *(named by a route's `model_map_env_file`)* | `serve` | none | Its value is the path of the JSON model map a `sigv4` route translates through. You choose the variable name. (`patchbay resolve` reads the map *file*, at the path the gateway reports — never the variable.) |
| `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` | `serve` | unset | Honored by Go's standard transport for upstream calls, as in any Go program. |

If neither the XDG variable nor `HOME` is set, `serve` refuses to start: there
is nowhere honest to put a registry or a ledger, and guessing would be worse
than failing.

`AWS_PROFILE` is not read from patchbay's own environment. It is *set* on the
`aws configure export-credentials` child process, to the `aws_profile` the route
names; the rest of the gateway's environment (`AWS_CONFIG_FILE`,
`AWS_SHARED_CREDENTIALS_FILE`, `AWS_REGION`, …) is inherited by that child, so
the CLI resolves the profile exactly as it would in your shell. See
[auth-modes.md](auth-modes.md).

## The registry file

The registry is a JSON document of **contexts**. Each context has an optional
`default_route` and an optional map of `routes` keyed by the model name that
selects them.

```json
{
  "contexts": {
    "personal": {
      "routes": {
        "openrouter/kimi-k2": {
          "auth": "inject",
          "base_url": "https://openrouter.ai/api",
          "model": "moonshotai/kimi-k2",
          "api_key_env_file": "PATCHBAY_OPENROUTER_KEY_FILE"
        }
      }
    },
    "work": {
      "default_route": {
        "auth": "sigv4",
        "aws_profile": "work-bedrock",
        "aws_region": "us-west-2",
        "model_map_env_file": "PATCHBAY_BEDROCK_MODEL_MAP_FILE"
      },
      "routes": {}
    }
  }
}
```

Parsing is strict. The top level accepts exactly one key, `contexts`; a context
object accepts exactly `default_route` and `routes`; a route object accepts
exactly the fields below. Anything else — an unknown key, a misspelled key,
trailing data after the document — rejects the **whole file** by name, and so
does any single validation failure. There is no partial application: a document
with one bad route is a document you did not review.

### Context names

A context name rides in a URL path, so it may contain only lowercase letters,
digits and dashes, and may not be empty. `personal` is special only in that a
request with no `/ctx/<name>` prefix resolves there; you do not have to define
it, and if you do not, the personal context is the built-in forward to Anthropic
with no aliases.

Alias keys inside `routes` are unconstrained and are matched whole as opaque
strings. `openrouter/sol` is a convention, not a syntax — the slash means
nothing to patchbay. An alias may deliberately shadow a real Anthropic model id;
that is how you pin `claude-sonnet-4-5` to a different upstream inside one
context.

### Route fields

| Field | Type | Meaning |
|---|---|---|
| `auth` | string | Required. `forward`, `inject` or `sigv4`. See [auth-modes.md](auth-modes.md). |
| `base_url` | string | The upstream API root. The inbound path and raw query are appended verbatim, so `https://openrouter.ai/api` + `/v1/messages` is what gets requested. Absolute `https` URL, or `http` for `127.0.0.1`/`localhost` — any other `http` host requires `insecure`. |
| `model` | string | Rewrite target: the model actually asked of the upstream. Omitted or empty means identity — forward whatever model the caller named. |
| `api_key_env_file` | string | The NAME of an environment variable whose VALUE is the path of a file holding the API key. Never the key, and never a path. |
| `aws_profile` | string | `sigv4` only. The AWS profile whose credentials sign the request. |
| `aws_region` | string | `sigv4` only. The region signed for, and the region of the `bedrock-runtime` endpoint derived from it. |
| `model_map_env_file` | string | `sigv4` only. The NAME of an environment variable whose VALUE is the path of a JSON object mapping requested model ids to Bedrock model ids. |
| `extra_body` | object | Merged over the request document's top-level keys at forward time; the route's values win over the caller's. Must be a JSON object. |
| `max_input_tokens` | integer ≥ 0 | Advertised as `max_input_tokens` on `/v1/models`. **Not enforced** — it is a published ceiling, not an applied one. Zero omits it. |
| `max_output_tokens` | integer ≥ 0 | Clamps the request's `max_tokens` down to this when the caller asks for more. Also advertised as `max_tokens` in the listing. Zero means no cap. |
| `insecure` | bool | Permits a plain-`http` `base_url` on a non-loopback host, accepting that the route travels an unencrypted link — see [Local models](#local-models). Setting it anywhere it changes nothing (an `https` route, a loopback `http` route, a `sigv4` route) fails validation, so a set flag always means what it says. On an `inject` route the injected API key travels in cleartext over that link. |
| `strip_cache_control` | bool | Removes every `cache_control` key from the request document, at any depth. Prompt caching is Anthropic's; a foreign upstream may reject the marker. |
| `capture_bodies` | bool | Records the forwarded request body and the response body in this route's ledger records. Off by default. |

A route that sets `model`, `extra_body`, `strip_cache_control` or
`max_output_tokens` has its request body decoded, rewritten and re-encoded. A
route that sets none of the four is on the identity path: the caller's exact
bytes are what travel upstream.

### Required and forbidden fields, by auth mode

These rules are enforced on `default_route` and on every alias alike, and a
violation fails the whole document.

| | `forward` | `inject` | `sigv4` |
|---|---|---|---|
| `base_url` | **required**, and must be exactly `https://api.anthropic.com` | required, `https` (or `http` for loopback, or with `insecure`) | **forbidden** — the endpoint is derived from `aws_region` |
| `api_key_env_file` | **forbidden** | **required**, non-empty | **forbidden** |
| `insecure` | forbidden — the Anthropic endpoint is `https` | allowed only on a non-loopback `http` `base_url` | **forbidden** — the Bedrock endpoint is always `https` |
| `aws_profile` | forbidden | forbidden | **required**, non-blank, and only lowercase letters, digits, dashes, dots and underscores |
| `aws_region` | forbidden | forbidden | **required**, non-blank, and only lowercase letters, digits and dashes |
| `model_map_env_file` | forbidden | forbidden | **required**, non-blank |
| `claude` in the alias or in `model` | allowed | **forbidden** (case-insensitive) | allowed |

Everything else applies to all three modes: `max_input_tokens` and
`max_output_tokens` must not be negative, and `extra_body` must be a JSON
object.

The `forward` restriction is the exfiltration rule: that mode passes the
caller's own subscription token upstream, so Anthropic is the only place it may
go. The `claude` restriction on `inject` is the misbilling rule: a route paid
for with a foreign key must not present itself as a Claude model, in the ledger
or anywhere else.

### Local models

A route can point at a model server on your own network — anything that speaks
the Anthropic Messages API, such as a recent Ollama. Nothing about the upstream
has to be a cloud:

```json
"local/qwen": {
  "base_url": "http://gpubox.lan:11434",
  "auth": "inject",
  "api_key_env_file": "PATCHBAY_LOCAL_KEY_FILE",
  "insecure": true
}
```

patchbay appends `/v1/messages` verbatim, so the server must answer the
Messages API at that path — an OpenAI-style `/v1/chat/completions` server needs
a translating proxy in front.

`insecure` is the operator accepting, in the reviewed document itself, that
this route travels an unencrypted link: anyone on that network path can read
the conversation and the injected API key. For a keyless local server the key
file can hold a placeholder, but the flag is still the honest description of
the link. The alternatives that avoid it are an SSH tunnel (`base_url` becomes
loopback) or TLS in front of the model server — for example `tailscale serve`,
which yields a valid `https` endpoint with no certificate management.

Validate before you deploy:

```console
$ patchbay check --registry examples/routes.json
OK examples/routes.json: 2 contexts (personal, work), 4 routes
```

## Hot reload

The registry file is re-read whenever it has changed, checked on every request
that consults it. "Changed" means the file's existence, size, modification time
or resolved symlink target differs from the last load — the symlink target
matters because a Nix-managed file is a symlink into `/nix/store`, where two
generations can share a normalized timestamp.

Nothing needs restarting after a rebuild. Both outcomes are cached, success and
failure alike, so a broken file is not re-parsed and re-logged on every single
request.

## Failure semantics

patchbay degrades on everything except the socket it was asked to bind.

| Failure | Effect |
|---|---|
| Registry file missing | No foreign aliases. Bare paths ride the built-in forward to Anthropic. `registry_loaded` is `false`. Not an error and not logged as one. |
| Registry file broken (parse or validation) | Same as missing, plus an error log naming the file, the context or alias, and the rule it broke. Re-logged on `/v1/models` requests, since a rebuild can break it long after startup. |
| Request names a context (`/ctx/<name>/…`) that cannot be produced | **404**, in Anthropic's `not_found_error` shape, naming the context. Never falls back to another context or to the built-in route. This includes `/ctx`, `/ctx/`, and `/ctx//x`, which name no context at all. |
| Request names no context | Always resolves. The built-in default route — plain forward to Anthropic, no rewrite, no capture — is a fixed value that is never read from the registry, so no registry failure can take it away. |
| Ledger directory cannot be created or opened | The gateway serves without recording, and logs it once at startup. |
| Hostname unavailable | Records are rejected by ledger validation and logged as dropped; serving is unaffected. |
| Listen address cannot be bound | Fatal. This is the one thing that stops the gateway. |

The asymmetry is deliberate: an unprefixed path is what every Anthropic client
sends, so it must keep working with no configuration at all; a prefixed path is
an explicit request for one particular world, and serving it from somewhere else
would be worse than refusing.

## Endpoints

| Path | Answered by |
|---|---|
| `GET`/`HEAD` `/_patchbay/registry` | patchbay, gated on the caller key. Returns the routing document the CLI reads. Another method → 405 with `Allow`; any other path under `/_patchbay/` → 404. |
| `GET /v1/models` | patchbay: the selected context's aliases, then the models the caller's own credentials can see from Anthropic, minus any id an alias already claims. A context whose default route does not forward to Anthropic gets a listing synthesized entirely from the registry — for a `sigv4` default route that is the KEYS of its model map, never the Bedrock ids they map to. Never fails — an unreachable upstream costs the upstream half and logs a warning. |
| `GET /v1/models/{id}` | patchbay when `{id}` is an alias in the selected context, or a model the context's non-forwarding default route serves. Otherwise: relayed to Anthropic with the caller's credentials and returned verbatim when the context still forwards there, and 404 `not_found_error` when it does not — a context that never reaches Anthropic is never described by it. |
| everything else | The proxy pipeline, including `/v1/messages`, `/v1/messages/count_tokens`, these two paths under any other method, and every endpoint patchbay has never heard of. |

Neither model endpoint writes a ledger record: nothing was proxied.

Both model endpoints are gated on the caller key when the path NAMES a context
(`/ctx/<name>/v1/models`), with the same 403 and 500 `/_patchbay/registry` gives:
asking what another world of this machine serves is what that key protects. The
bare path is never gated — it is what Claude Code sends knowing nothing about
patchbay, and it describes only the context that caller was already riding.

## Limits

| Limit | Value |
|---|---|
| Request body held in memory | 64 MiB; over it, 413 |
| Response capture held for the ledger | 64 MiB; over it the capture is dropped whole and the record carries no summary |
| Upstream model-listing body | 4 MiB |
| Introspection document the CLI will decode | 4 MiB |
| One Bedrock event-stream message | 16 MiB |
| Request header read timeout | 10 s |
| Upstream response *header* timeout | 10 min (there is no overall request timeout: a long thinking turn may stream for many minutes) |
| Graceful shutdown, and ledger drain after it | 5 s each |
