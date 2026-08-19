# CLI

```
usage: patchbay <command> [arguments]

commands:
  serve                        run the gateway until it is signaled to stop
  contexts                     list every context the running gateway serves
  routes [--ctx <name>]        list one context's aliases (default: personal)
  resolve <context> <model>    show which route one request would ride
  project [dir]                show which context a directory's Claude Code settings select
  check [--registry <path>]    validate a registry file without asking the gateway

  -h, --help                   print this text

The subcommands other than serve and check ask the running gateway, at
$PATCHBAY_LISTEN (default 127.0.0.1:4100), presenting the caller key
named by $PATCHBAY_CALLER_KEY_FILE.
```

A bare `patchbay` with no subcommand prints that usage on stderr and exits 2.
Serving is deliberately not the default: a bare invocation is far more likely to
be a mistyped subcommand than a request to bind the port, and starting a second
gateway on a typo is worse than printing usage.

Exit codes: `0` success, `1` something went wrong, `2` the command line was not
one patchbay understands.

`contexts`, `routes`, `resolve` and `project` all ask the running gateway over
`GET /_patchbay/registry`, presenting the caller key. They describe the gateway
as it is answering requests right now, not as a file on disk says it should be.
Tables are tab-separated before alignment, so every row stays greppable.

The examples below come from a gateway running with the registry in
[examples/routes.json](../examples/routes.json).

## `patchbay serve`

Runs the gateway until `SIGINT` or `SIGTERM`. It takes no flags — everything
comes from the environment ([configuration.md](configuration.md)). Logs are
JSON on stderr, which is what a unit file captures.

```console
$ patchbay serve
{"time":"2026-08-19T00:22:25.173140815-07:00","level":"INFO","msg":"patchbay listening","version":"0.0.0-dev","listen":"127.0.0.1:4199","registry":"/home/you/.config/patchbay/routes.json","ledger_dir":"/home/you/.local/state/patchbay/ledger"}
^C
{"time":"2026-08-19T00:23:03.286658757-07:00","level":"INFO","msg":"patchbay shutting down"}
```

In-flight requests get 5 s to finish, then the ledger's pending writes get
another 5 s before the process exits.

## `patchbay check`

The one subcommand that needs no running gateway — which is the point, since
after a bad edit there may be nothing left to ask. It resolves the registry path
exactly the way `serve` does, so it can never validate a different file from the
one that will be served.

```console
$ patchbay check --registry examples/routes.json
OK examples/routes.json: 2 contexts (personal, work), 4 routes
```

With no `--registry`, it checks `$PATCHBAY_REGISTRY` or the default path.
Failures name the file, the context or alias, and the rule that was broken:

```console
$ patchbay check --registry broken.json
patchbay: registry broken.json: context "personal": alias "bad": inject routes require a non-empty api_key_env_file

$ patchbay check --registry broken2.json
patchbay: registry broken2.json: context "personal": alias "claude-fast": inject routes must not reference "claude"; found it in the alias

$ patchbay check --registry nope.json
patchbay: registry file: stat nope.json: no such file or directory
```

Each of those exits 1.

## `patchbay contexts`

Every context the gateway serves, with the default route each falls back to and
how many aliases each defines.

```console
$ patchbay contexts
CONTEXT   DEFAULT_ROUTE                                          ROUTES
personal  built-in anthropic forward                             2
work      sigv4 https://bedrock-runtime.us-west-2.amazonaws.com  2
```

`built-in anthropic forward` means the context declared no `default_route` of
its own: unclaimed models go to Anthropic on the caller's credentials.

## `patchbay routes`

One context's aliases. Defaults to `personal`.

```console
$ patchbay routes
ALIAS               AUTH    UPSTREAM                   MODEL               MAX_INPUT_TOKENS  CREDENTIAL_SOURCE
openrouter/glm-4.6  inject  https://openrouter.ai/api  z-ai/glm-4.6        200000            PATCHBAY_OPENROUTER_KEY_FILE
openrouter/kimi-k2  inject  https://openrouter.ai/api  moonshotai/kimi-k2  262144            PATCHBAY_OPENROUTER_KEY_FILE

$ patchbay routes --ctx work
ALIAS             AUTH     UPSTREAM                                         MODEL            MAX_INPUT_TOKENS  CREDENTIAL_SOURCE
anthropic-direct  forward  https://api.anthropic.com                        -                -                 caller
bedrock/opus      sigv4    https://bedrock-runtime.us-west-2.amazonaws.com  claude-opus-4-5  200000            PATCHBAY_BEDROCK_MODEL_MAP_FILE
```

`CREDENTIAL_SOURCE` is always an environment **variable name** — or `caller`,
meaning the route spends whatever the caller itself presented. It is never a
path and never a secret. `-` marks a field the route does not set.

A context the gateway does not serve is named as such, along with what it does
serve:

```console
$ patchbay routes --ctx nope
patchbay: no context named "nope" is served; the gateway serves: personal, work
```

## `patchbay resolve`

What one request would actually do, given a context and the model name the
request would carry.

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

A model no alias claims falls to the context's default route, and the two
contexts answer the same model very differently:

```console
$ patchbay resolve personal claude-sonnet-4-5
context: personal
request_model: claude-sonnet-4-5
route: default route
auth: forward
upstream: https://api.anthropic.com
model: claude-sonnet-4-5
credential: the caller's own credentials

$ patchbay resolve work claude-sonnet-4-5
context: work
request_model: claude-sonnet-4-5
route: default route
auth: sigv4
upstream: https://bedrock-runtime.us-west-2.amazonaws.com
model: claude-sonnet-4-5
credential: aws credentials the gateway exports for this route's profile
model_map: PATCHBAY_BEDROCK_MODEL_MAP_FILE=/home/you/.config/patchbay/bedrock-models.json (the gateway maps the model on every request)
mapping: "claude-sonnet-4-5" is mapped
```

For a `sigv4` route, `resolve` reads the model map itself — from wherever the
command is run, which is why it says so — and reports only whether the model has
an entry. The Bedrock id it maps to is never printed: keeping that out of sight
is what the map file exists for. A model with no entry is called out with the
answer the gateway would give:

```
mapping: "claude-haiku-4-5" is not mapped — the gateway would refuse this request with 400
```

## `patchbay project`

Which context a directory ends up in, and what that context will do. It reads
the Claude Code settings chain in precedence order, most local first, then
prints the winning base URL, the context it selects, and that context's routes.

```console
$ cd ~/work/some-project && patchbay project
settings /home/you/work/some-project/.claude/settings.local.json: not present
settings /home/you/work/some-project/.claude/settings.json: ANTHROPIC_BASE_URL=http://127.0.0.1:4100/ctx/work
settings /home/you/.claude/settings.json: ANTHROPIC_BASE_URL=http://127.0.0.1:4100
base_url: http://127.0.0.1:4100/ctx/work
context: work
ALIAS             AUTH     UPSTREAM                                         MODEL            MAX_INPUT_TOKENS  CREDENTIAL_SOURCE
anthropic-direct  forward  https://api.anthropic.com                        -                -                 caller
bedrock/opus      sigv4    https://bedrock-runtime.us-west-2.amazonaws.com  claude-opus-4-5  200000            PATCHBAY_BEDROCK_MODEL_MAP_FILE
```

Every file in the chain is listed even when it has nothing to say, because the
useful answer is often which file is doing it. A file that is missing,
unreadable or not JSON is reported and skipped rather than ending the command.

Take a directory argument to ask about somewhere else. A base URL pointing at
some other gateway is worth saying out loud — the routes printed below it are
*this* gateway's — but it is a notice, not a failure:

```console
$ patchbay project ~/scratch
settings /home/you/scratch/.claude/settings.local.json: not present
settings /home/you/scratch/.claude/settings.json: not present
settings /home/you/.claude/settings.json: ANTHROPIC_BASE_URL=http://127.0.0.1:4100
base_url: http://127.0.0.1:4100
patchbay: ANTHROPIC_BASE_URL points at 127.0.0.1:4100, which is not the gateway this command asks (127.0.0.1:4199)
context: personal
ALIAS               AUTH    UPSTREAM                   MODEL               MAX_INPUT_TOKENS  CREDENTIAL_SOURCE
openrouter/glm-4.6  inject  https://openrouter.ai/api  z-ai/glm-4.6        200000            PATCHBAY_OPENROUTER_KEY_FILE
openrouter/kimi-k2  inject  https://openrouter.ai/api  moonshotai/kimi-k2  262144            PATCHBAY_OPENROUTER_KEY_FILE
```

A settings file that selects a context the gateway does not serve is a warning
and exit 1 — that project's Claude Code would be getting 404s.

## When something is wrong

The failures you will actually hit, and what they mean:

```console
$ patchbay contexts
patchbay: the caller key is unavailable: environment variable PATCHBAY_CALLER_KEY_FILE is not set
```

This process cannot read a caller key at all — a misconfiguration here, before
any request is made.

```console
$ patchbay contexts
patchbay: the gateway at 127.0.0.1:4199 refused the caller key: the file named by PATCHBAY_CALLER_KEY_FILE does not match the one it reads
```

Both sides have a key file; they are not the same key. In practice this means
the daemon was started with a different `PATCHBAY_CALLER_KEY_FILE` than your
shell has.

```console
$ patchbay contexts
patchbay: no gateway answered at 127.0.0.1:4198 — is patchbay running? Get "http://127.0.0.1:4198/_patchbay/registry": dial tcp 127.0.0.1:4198: connect: connection refused
```

Nothing is listening there. Check `PATCHBAY_LISTEN` on both sides.

## The endpoint behind the CLI

`GET /_patchbay/registry` with the `X-Patchbay-Key` header returns the document
the CLI renders. It is the same resolution the proxy forwards by, so it cannot
drift from what the gateway actually does — and it names the environment
variables and resolved paths a route uses, never the contents of any file.

```console
$ curl -s -H "X-Patchbay-Key: $(cat ~/.config/patchbay/caller.key)" \
    http://127.0.0.1:4100/_patchbay/registry | jq .
{
  "contexts": {
    "personal": {
      "default_route": {
        "auth": "forward",
        "upstream": "https://api.anthropic.com"
      },
      "routes": {
        "openrouter/glm-4.6": {
          "auth": "inject",
          "upstream": "https://openrouter.ai/api",
          "model": "z-ai/glm-4.6",
          "max_input_tokens": 200000,
          "api_key_env_file": "PATCHBAY_OPENROUTER_KEY_FILE",
          "api_key_file": "/home/you/.config/patchbay/openrouter.key"
        },
        "openrouter/kimi-k2": {
          "auth": "inject",
          "upstream": "https://openrouter.ai/api",
          "model": "moonshotai/kimi-k2",
          "max_input_tokens": 262144,
          "api_key_env_file": "PATCHBAY_OPENROUTER_KEY_FILE",
          "api_key_file": "/home/you/.config/patchbay/openrouter.key"
        }
      }
    },
    "work": {
      "default_route": {
        "auth": "sigv4",
        "upstream": "https://bedrock-runtime.us-west-2.amazonaws.com",
        "model_map_env_file": "PATCHBAY_BEDROCK_MODEL_MAP_FILE",
        "model_map_file": "/home/you/.config/patchbay/bedrock-models.json"
      },
      "routes": {
        "anthropic-direct": {
          "auth": "forward",
          "upstream": "https://api.anthropic.com"
        },
        "bedrock/opus": {
          "auth": "sigv4",
          "upstream": "https://bedrock-runtime.us-west-2.amazonaws.com",
          "model": "claude-opus-4-5",
          "max_input_tokens": 200000,
          "model_map_env_file": "PATCHBAY_BEDROCK_MODEL_MAP_FILE",
          "model_map_file": "/home/you/.config/patchbay/bedrock-models.json"
        }
      }
    }
  },
  "registry_path": "/home/you/.config/patchbay/routes.json",
  "registry_loaded": true,
  "listen": "127.0.0.1:4100"
}
```

`registry_loaded` is false when the registry file is missing or broken; the
contexts shown are then just the built-in personal one.

Without the key, or with the wrong one:

```console
$ curl -s -i http://127.0.0.1:4100/_patchbay/registry
HTTP/1.1 403 Forbidden
Content-Type: application/json

{"type":"error","error":{"type":"api_error","message":"patchbay: caller key missing or mismatched"}}
```
