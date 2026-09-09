# Configuration

`patchbay serve` reads environment variables and one strict JSON registry. It
accepts no flags. `patchbay check` uses the same registry path rules plus an
optional `--registry <path>` override.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `PATCHBAY_LISTEN` | `127.0.0.1:4100` | Address used by the gateway and daemon-backed CLI commands. |
| `PATCHBAY_REGISTRY` | XDG config path, then `~/.config/patchbay/routes.json` | Registry path. `check --registry` takes precedence. |
| `PATCHBAY_LEDGER_DIR` | XDG state path, then `~/.local/state/patchbay/ledger` | Ledger directory. Missing directories are created with mode 0700. |
| `PATCHBAY_USAGE_DB` | XDG state path, then `~/.local/state/patchbay/usage.sqlite` | Accounting database. Unlike the ledger, `serve` fails if it cannot be opened. |
| `PATCHBAY_RATE_CARDS` | none | JSON file of immutable effective-dated per-million-token rates. `serve` fails if it cannot be read, parsed, or reconciled with a card already recorded. |
| `PATCHBAY_CALLER_KEY_FILE` | none | File containing the caller key. |
| Variable named by `api_key_env_file` | none | Path to an `inject` Seat's key file. |
| Variable named by `model_map_env_file` | none | Path to a `sigv4` Seat's JSON model map. |
| `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` | unset | Standard Go proxy settings for upstream requests. |

Registry and ledger paths resolve independently: the `PATCHBAY_*` override, then
the matching XDG base, then `HOME`. `serve` fails if either path cannot be
resolved. `check` needs only the registry path.

The Home Manager module exports the listen, registry, and caller-key variables.
Additional key and map variables go in `services.patchbay.environment`. SigV4
also needs the AWS CLI on the service `PATH`. See [nix.md](nix.md).

## Strict registry

```json
{
  "default_context": "personal",
  "seats": {
    "anthropic": {
      "upstream": "https://api.anthropic.com",
      "auth_mode": "forward"
    },
    "openrouter-kimi-k2": {
      "upstream": "https://openrouter.ai/api",
      "model": "moonshotai/kimi-k2",
      "auth_mode": "inject",
      "api_key_env_file": "PATCHBAY_OPENROUTER_KEY_FILE",
      "billing": "metered"
    },
    "work-bedrock": {
      "auth_mode": "sigv4",
      "aws_profile": "work-bedrock",
      "aws_region": "us-west-2",
      "model_map_env_file": "PATCHBAY_BEDROCK_MODEL_MAP_FILE"
    }
  },
  "contexts": {
    "personal": {
      "default_seat": "anthropic",
      "models": {
        "openrouter/kimi-k2": "openrouter-kimi-k2"
      },
      "subagents": {
        "default_seat": "openrouter-kimi-k2",
        "models": {
          "claude-opus-5": "openrouter-kimi-k2"
        }
      }
    },
    "work": {
      "default_seat": "work-bedrock",
      "models": {}
    }
  }
}
```

The root fields are exactly these three, and all are required:

- `default_context`
- `seats`
- `contexts`

A Context has exactly:

- required `default_seat`;
- required `models`;
- optional `subagents`.

A present `subagents` object has exactly:

- required `default_seat`;
- optional `models`, defaulting to an empty exact-binding map.

Every binding value is a Seat ID string. Inline Seat objects are invalid.
`default_context`, Context IDs, Seat IDs, and all Seat references must resolve.
Context and Seat IDs match `^[a-z0-9][a-z0-9-]*$`.

Model selector keys are exact opaque non-empty strings. They are not normalized,
trimmed, decoded, or restricted to the ID grammar.

The registry rejects:

- missing required fields;
- null root, `seats`, `contexts`, Context, `models`, `subagents`, or
  `subagents.models` values;
- null or non-object `extra_body`, or an exact top-level `model` member within it;
- unknown fields;
- incompatible types;
- malformed JSON or trailing data;
- duplicate JSON member names in every object at every depth — the root,
  `seats`, each Seat, `contexts`, each Context, each binding map, `subagents`,
  and every object nested anywhere inside an `extra_body`, including objects
  carried in arrays;
- unresolved Context or Seat references;
- inline Seats in binding maps;
- legacy `routes`, `default_route`, `base_url`, or `auth` spellings.

A repeated member name is rejected rather than resolved last-wins, so a reviewed
value can never be silently shadowed by a second one further down the file.

There is no compatibility parser.

## Seats

A Seat is global and reusable. Contexts and marked-subagent policy reference it
by ID.

| Field | Meaning |
|---|---|
| `upstream` | API root for `forward` and `inject`. HTTPS is required except for loopback HTTP or an `inject` Seat with `insecure: true`. |
| `model` | Upstream model rewrite. Empty preserves the requested model. |
| `auth_mode` | `forward`, `inject`, or `sigv4`. |
| `billing` | `subscription`, `metered`, or `self_hosted`. Required by `inject` and forbidden elsewhere: a `forward` Seat's class follows the caller's credential and a `sigv4` Seat is metered by the account that signed it. |
| `api_key_env_file` | Environment variable whose value is the provider-key file path. Required by `inject` and matched against `^[A-Za-z_][A-Za-z0-9_]*$` — the same grammar the ledger validates `config_source` with. |
| `aws_profile` | AWS profile for a `sigv4` Seat. |
| `aws_region` | AWS region used for signing and the Bedrock endpoint. |
| `model_map_env_file` | Environment variable whose value is the Bedrock model-map path. |
| `extra_body` | Object merged over top-level request fields. The exact top-level member `model` is reserved; `Seat.model` is the sole model authority. Other Seat values win. |
| `max_input_tokens` | Non-negative metadata published by model endpoints. It is not enforced. |
| `max_output_tokens` | Non-negative ceiling applied to numeric request `max_tokens` values. |
| `insecure` | Allows non-loopback HTTP on an `inject` Seat. |
| `strip_cache_control` | Recursively removes object fields named `cache_control`. |
| `capture_bodies` | Stores rewritten request and captured response bodies in ledger v2. |
| `speed` | `fast` requests the upstream's fast tier on every request: Anthropic fast mode on a Claude upstream, the Codex priority tier through CLIProxyAPI, which maps this field. Written after `extra_body`, so the Seat value wins over the caller's and over `extra_body`. Not allowed on `sigv4`. |

For `forward` and `inject`, the original request bytes are preserved unless the
Seat sets `model`, `extra_body`, `strip_cache_control`, `max_output_tokens`, or
`speed`.
When a rewrite is needed, the body must be a JSON object and is re-encoded after
those operations. SigV4 always performs an additional Bedrock translation. See
[auth-modes.md](auth-modes.md).

### Auth-mode validation

| Rule | `forward` | `inject` | `sigv4` |
|---|---|---|---|
| `upstream` | Exactly `https://api.anthropic.com` | Required | Forbidden; derived from region |
| `api_key_env_file` | Forbidden | Required | Forbidden |
| `billing` | Forbidden | Required | Forbidden |
| `insecure` | Forbidden | Non-loopback HTTP only | Forbidden |
| AWS fields | Forbidden | Forbidden | Profile, region, and model-map variable required |
| `claude` in Seat rewrite | Allowed | Forbidden, case-insensitive | Allowed |

All token ceilings must be non-negative. `extra_body` must be an object when
present. Its exact top-level `model` member is reserved so `Seat.model` remains
the sole model authority; member names are case-sensitive, and nested `model`
members remain allowed. Other contents are arbitrary: an operator merges
whatever the provider accepts, so no schema constrains their member names,
values, or depth. Every object inside `extra_body`, at any depth and including
objects carried in arrays, must use unique member names.

A public `context.models` selector containing `claude`, case-insensitively,
cannot bind to an inject Seat. Marked-subagent model selectors are observed input
model names and are exempt from that selector rule. Every inject Seat referenced
by a marked-subagent exact or default binding must configure a non-empty,
non-Claude rewrite model.

Registry validation is structural and referential only. It does not read
credential files or model maps, export AWS credentials, validate AWS sessions,
probe upstreams, or perform network requests.

## Context and Seat selection

Bare paths select `default_context`; `/ctx/<name>` explicitly selects that Context.
While the registry is valid, an unknown explicit Context returns 404 rather than
falling back.

Selection precedence is:

1. exact public `context.models` binding;
2. valid marked subagent plus exact `context.subagents.models` binding;
3. valid marked subagent plus `context.subagents.default_seat`;
4. refusal of a protected foreign rewrite target;
5. `context.default_seat`.

### Marked-subagent observation

Only `x-claude-code-agent-id` creates a positive observation, and only when:

- exactly one value is present;
- length is 1–256 bytes;
- every byte is printable ASCII `0x21`–`0x7e`.

The value is used verbatim without trimming, decoding, or prefix assumptions.
Empty, duplicate, oversized, whitespace-containing, control-containing, or
non-ASCII values make the request unmarked.

`x-claude-code-parent-agent-id` never marks traffic by itself. When the agent ID
is valid, a parent value is recorded only if it independently passes the same
validation. Invalid parent values are omitted and never affect selection.

Both headers continue upstream unchanged under ordinary header forwarding. They
are observations only, never authentication, identity, authorization, permission,
or a security boundary. Parent-only and unmarked auxiliary traffic uses ordinary
selection. Patchbay performs no prompt matching or semantic agent inference.

### Foreign rewrite-target refusal

A protected target is computed only from public `context.models` bindings:

1. consider bindings whose Seat has a non-empty model rewrite;
2. protect the target when that Seat's effective upstream differs from the
   Context default Seat's effective upstream;
3. remove protection when any public binding serves the same target through the
   default upstream;
4. name the lexically first public selector in the error.

Exact public and marked-subagent selection happen before this guard.
`subagents.models`, `subagents.default_seat`, and unused Seats never contribute
protected targets. Requesting a protected target directly returns 404 and names
the public selector to use.

## Local models

An `inject` Seat can target a local Anthropic-compatible server:

```json
"local-qwen": {
  "upstream": "http://gpubox.lan:11434",
  "model": "qwen3",
  "auth_mode": "inject",
  "api_key_env_file": "PATCHBAY_LOCAL_KEY_FILE",
  "billing": "self_hosted",
  "insecure": true
}
```

The server must implement `/v1/messages` and, if used,
`/v1/messages/count_tokens`. A keyless server still needs a non-empty placeholder
key file because `inject` requires one. Prefer TLS or a loopback tunnel to
`insecure: true`.

## Reload and failure behavior

Initial registry validation occurs synchronously before the listener binds.
Initial missing, unreadable, malformed, or invalid configuration is fatal.

Patchbay reloads when registry existence, size, modification time, or resolved
symlink target changes. This detects ordinary replacement and new Home Manager
generations. A request uses one immutable resolution snapshot.

| Failure | Result |
|---|---|
| Hot registry missing or invalid | Installs an invalid sentinel; every new proxy and model request returns 503 `api_error`. |
| Unknown explicit Context while valid | 404, with no fallback. |
| Protected foreign rewrite target requested directly | 404 naming a public selector. |
| Valid replacement after hot failure | Restores routing atomically on the next request. |
| Ledger unavailable | Requests continue; affected records are dropped and warned. |
| Listen address unavailable | Startup fails. |

Stale valid snapshots are never used for new requests after a hot failure.
Already-resolved requests finish on their attached immutable resolution. Registry
invalidity takes precedence over unknown-Context handling.

## Local endpoints

| Path | Behavior |
|---|---|
| `GET`, `HEAD /_patchbay/registry` | Caller-key-gated non-secret registry state used by the CLI. |
| `GET /v1/models` | Lists public selectors and models available through the selected default Seat. |
| `GET /v1/models/{id}` | Describes a public selector or default-Seat model. |
| Everything else | Enters the proxy pipeline and is ledger-eligible. |

Explicit-Context model endpoints require the caller key. Bare model endpoints do
not. Model and introspection endpoints do not write ledger records.

Public models expose selectors only, never Seat IDs or marked-subagent policy. A
forward default merges selectors with Anthropic's listing. An inject default adds
its non-empty rewrite model. A SigV4 default adds non-empty model-map keys.
Mapped Bedrock identifiers remain private. Public selectors shadow duplicate
upstream/default IDs on every page, and Anthropic pagination semantics are
preserved.

The binding entries are emitted once, on the unqualified initial page only,
ahead of the upstream's first page. A page requested with either cursor —
`after_id` paging forward or `before_id` paging back — carries the upstream's
own entries alone. Shadowing still applies on every page, cursor-qualified or
not, because a selector this Context serves is what that name means here
whichever upstream page the colliding ID arrives on.

A cursor-qualified page, in either direction, relays the upstream's own
`first_id` and `last_id` unchanged, whatever `has_more` says — including when
the entry a cursor names was shadowed away locally, since a substituted cursor
would page from an ID the upstream never listed. Only the unqualified page
begins and ends with the merged listing's own IDs, and there `last_id` becomes
the upstream's whenever `has_more` is true, so the next page is one Anthropic
can be asked for.

Unprefixed caller-key-gated introspection remains HTTP 200 while the registry is
invalid and reports:

```json
{
  "default_context": null,
  "seats": {},
  "contexts": {},
  "registry_valid": false,
  "validation_error": "sanitized message"
}
```

Valid introspection includes global non-secret Seat summaries, public Context
bindings, and optional marked-subagent policy. It omits `extra_body` contents,
resolved key and model-map paths, credential values, AWS credentials, and mapped
Bedrock identifiers.

## Limits

| Limit | Value |
|---|---|
| Request body | 64 MiB |
| Ledger response capture | 64 MiB |
| Model endpoint upstream body | 4 MiB |
| CLI introspection document | 4 MiB |
| Agent and parent observation | 256 bytes each |
| Bedrock event-stream message | 16 MiB |
| Request-header, upstream dial, and TLS handshake timeouts | 10 s |
| Proxy upstream response-header timeout | 10 min |
| Model endpoint fetch timeout | 5 s |
| AWS credential export timeout | 10 s |
| HTTP shutdown and ledger drain | 5 s each |

There is no overall proxy request timeout after upstream response headers arrive.
