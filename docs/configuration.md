# Configuration

`patchbay serve` reads environment variables and one JSON registry. It accepts no
flags. `patchbay check` uses the same registry path rules plus an optional
`--registry <path>` override.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `PATCHBAY_LISTEN` | `127.0.0.1:4100` | Address used by the gateway and daemon-backed CLI commands. |
| `PATCHBAY_REGISTRY` | XDG config path, then `~/.config/patchbay/routes.json` | Registry path. `check --registry` takes precedence. |
| `PATCHBAY_LEDGER_DIR` | XDG state path, then `~/.local/state/patchbay/ledger` | Ledger directory. Missing directories are created with mode 0700. |
| `PATCHBAY_CALLER_KEY_FILE` | none | File containing the caller key. |
| Variable named by `api_key_env_file` | none | Path to an `inject` route's key file. |
| Variable named by `model_map_env_file` | none | Path to a `sigv4` route's JSON model map. |
| `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` | unset | Standard Go proxy settings for upstream requests. |

Registry and ledger paths resolve independently: the `PATCHBAY_*` override, then
the matching XDG base, then `HOME`. `serve` fails if either path cannot be
resolved. `check` needs only the registry path.

The Home Manager module exports the listen, registry, and caller-key variables.
Additional key and map variables go in `services.patchbay.environment`. SigV4
also needs the AWS CLI on the service `PATH`. See [nix.md](nix.md).

## Registry

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
      }
    }
  }
}
```

Only these object fields are accepted:

- root: `contexts`
- context: `default_route`, `routes`
- route: the fields listed below

Omitted `contexts` or `routes` are empty. An omitted `default_route` uses the
built-in Anthropic `forward` route. A null document, context, `contexts`, or
`routes` value is also treated as empty. Unknown fields, incompatible types,
malformed JSON, trailing data, or one invalid route reject the entire registry.
`extra_body: null` is invalid.

### Contexts and route selection

Context names contain lowercase ASCII letters, digits, and dashes. Bare paths
select `personal`; `/ctx/<name>` selects only that context. If an explicit context
is unavailable, patchbay returns 404 rather than falling back.

Aliases are non-empty opaque strings matched exactly. Route selection is:

1. exact alias;
2. refusal of a protected foreign rewrite target;
3. the context's default route.

A foreign rewrite target is a model served by an alias from an upstream different
from the context default. Requesting that target directly returns 404 and names
an alias to use. Targets served from the default upstream still fall through.

### Route fields

| Field | Meaning |
|---|---|
| `auth` | Required: `forward`, `inject`, or `sigv4`. |
| `base_url` | API root for `forward` and `inject`. HTTPS is required except for loopback HTTP or an `inject` route with `insecure: true`. |
| `model` | Upstream model rewrite. Empty preserves the requested model. |
| `api_key_env_file` | Environment variable whose value is the provider-key file path. |
| `aws_profile` | AWS profile for a `sigv4` route. |
| `aws_region` | AWS region used for signing and the Bedrock endpoint. |
| `model_map_env_file` | Environment variable whose value is the Bedrock model-map path. |
| `extra_body` | Object merged over top-level request fields. Route values win. |
| `max_input_tokens` | Non-negative metadata published by model endpoints. It is not enforced. |
| `max_output_tokens` | Non-negative ceiling applied to numeric request `max_tokens` values. |
| `insecure` | Allows non-loopback HTTP on an `inject` route. |
| `strip_cache_control` | Recursively removes object fields named `cache_control`. |
| `capture_bodies` | Stores rewritten request and captured response bodies in the ledger. |

For `forward` and `inject`, the original request bytes are preserved unless the
route sets `model`, `extra_body`, `strip_cache_control`, or
`max_output_tokens`. When a rewrite is needed, the body must be a JSON object and
is re-encoded after those operations. SigV4 always performs an additional
Bedrock translation. See [auth-modes.md](auth-modes.md).

### Auth-mode validation

| Rule | `forward` | `inject` | `sigv4` |
|---|---|---|---|
| `base_url` | Exactly `https://api.anthropic.com` | Required | Forbidden; derived from region |
| `api_key_env_file` | Forbidden | Required | Forbidden |
| `insecure` | Forbidden | Non-loopback HTTP only | Forbidden |
| AWS fields | Forbidden | Forbidden | Profile, region, and model-map variable required |
| `claude` in alias or rewrite | Allowed | Forbidden, case-insensitive | Allowed |

All token ceilings must be non-negative. `extra_body` must be an object.

### Local models

An `inject` route can target a local Anthropic-compatible server:

```json
"local/qwen": {
  "auth": "inject",
  "base_url": "http://gpubox.lan:11434",
  "api_key_env_file": "PATCHBAY_LOCAL_KEY_FILE",
  "insecure": true
}
```

The server must implement `/v1/messages` and, if used,
`/v1/messages/count_tokens`. A keyless server still needs a non-empty placeholder
key file because `inject` requires one. Prefer TLS or a loopback tunnel to
`insecure: true`.

## Reload and failure behavior

patchbay reloads the registry when its existence, size, modification time, or
resolved symlink target changes. This detects both ordinary file replacement and
new Home Manager generations. A request uses one registry snapshot.

| Failure | Result |
|---|---|
| Registry missing or invalid | Bare requests use the built-in personal Anthropic route; configured contexts and aliases are unavailable. |
| Explicit context unavailable | 404, with no fallback. |
| Foreign rewrite target requested directly | 404 naming an alias. |
| Ledger unavailable | Requests continue without records. |
| Listen address unavailable | Startup fails. |

## Local endpoints

| Path | Behavior |
|---|---|
| `GET`, `HEAD /_patchbay/registry` | Caller-key-gated routing document used by the CLI. |
| `GET /v1/models` | Lists aliases and models available through the selected default route. |
| `GET /v1/models/{id}` | Describes an alias or default-route model. |
| Everything else | Enters the proxy pipeline. |

Explicit-context model endpoints require the caller key. Bare personal model
endpoints do not. Model and introspection endpoints do not write ledger records.
If an Anthropic model listing fails, patchbay logs the failure and returns the
local entries it has.

## Limits

| Limit | Value |
|---|---|
| Request body | 64 MiB |
| Ledger response capture | 64 MiB |
| Model endpoint upstream body | 4 MiB |
| CLI introspection document | 4 MiB |
| Bedrock event-stream message | 16 MiB |
| Request-header, upstream dial, and TLS handshake timeouts | 10 s |
| Proxy upstream response-header timeout | 10 min |
| Model endpoint fetch timeout | 5 s |
| AWS credential export timeout | 10 s |
| HTTP shutdown and ledger drain | 5 s each |

There is no overall proxy request timeout after upstream response headers arrive.
