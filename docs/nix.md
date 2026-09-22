# Nix

The flake exports:

| Output | Description |
|---|---|
| `packages.<system>.patchbay` | Gateway binary and default package. |
| `packages.<system>.cliproxyapi` | Optional CLIProxyAPI release binary. |
| `homeModules.patchbay` | Home Manager module, also exported as `homeModules.default` and under `homeManagerModules`. |

Packages are available for `x86_64-linux`, `aarch64-linux`, and
`aarch64-darwin`. The module manages systemd user services and supports Linux
only.

## Home Manager

Add the input and import the module:

```nix
{
  inputs.patchbay.url = "github:joshsymonds/patchbay";
}
```

```nix
{
  inputs,
  ...
}: {
  imports = [ inputs.patchbay.homeModules.patchbay ];

  services.patchbay = {
    enable = true;

    registry = {
      default_context = "personal";
      seats = {
        anthropic = {
          upstream = "https://api.anthropic.com";
          auth_mode = "forward";
        };
        openrouter-kimi-k2 = {
          upstream = "https://openrouter.ai/api";
          model = "moonshotai/kimi-k2";
          auth_mode = "inject";
          billing = "metered";
          api_key_env_file = "PATCHBAY_OPENROUTER_KEY_FILE";
        };
      };
      contexts.personal = {
        default_seat = "anthropic";
        models."openrouter/kimi-k2" = "openrouter-kimi-k2";
      };
    };

    environment.PATCHBAY_OPENROUTER_KEY_FILE = "/run/agenix/openrouter-key";
  };
}
```

`api_key_env_file` names an environment variable, and that variable contains the
path to a key file outside the Nix store. Use agenix, sops-nix, or another secret
manager for provider keys.

The generated strict registry is stored in the Nix store and linked at
`~/.config/patchbay/routes.json`. It contains global Seats, Context bindings, and
secret references, never secret contents. Patchbay reloads it when the Home
Manager symlink target changes.

The module's default registry is valid by itself:

```nix
{
  default_context = "personal";
  seats.anthropic = {
    upstream = "https://api.anthropic.com";
    auth_mode = "forward";
  };
  contexts.personal = {
    default_seat = "anthropic";
    models = { };
  };
}
```

Apply and inspect:

```console
$ home-manager switch
$ . "$HOME/.nix-profile/etc/profile.d/hm-session-vars.sh"
$ systemctl --user status patchbay
$ journalctl --user -u patchbay -f
$ patchbay check
$ patchbay contexts
$ patchbay seats
$ patchbay bindings --ctx personal
```

The activation creates `~/.config/patchbay/caller.key` with mode 0600 when it is
missing, unless `generateCallerKey` is disabled. The module exports
`PATCHBAY_LISTEN`, `PATCHBAY_REGISTRY`, and `PATCHBAY_CALLER_KEY_FILE` to Home
Manager sessions.

Configure Claude Code separately:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:4100",
    "ANTHROPIC_CUSTOM_HEADERS": "X-Patchbay-Key: <contents of ~/.config/patchbay/caller.key>"
  }
}
```

Leave `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` unset to keep using the
Claude Code subscription login. Keep the listener on loopback.

Ledger v2 records are JSONL under `~/.local/state/patchbay/ledger` unless
`ledgerDir` overrides the path:

```console
$ tail -f "$HOME/.local/state/patchbay/ledger/patchbay-v2-$(date -u +%F).jsonl"
```

## Module options

| Option | Default | Description |
|---|---|---|
| `enable` | `false` | Install and run patchbay as a systemd user service. |
| `package` | flake package | Gateway package. |
| `listen` | `127.0.0.1:4100` | Service and CLI address. |
| `registry` | strict Anthropic Seat plus `personal` Context | Registry rendered to JSON. |
| `callerKeyFile` | `~/.config/patchbay/caller.key` | String path to the caller key. |
| `generateCallerKey` | `true` | Create the caller key when missing. |
| `environment` | `{ }` | Extra unit environment, normally key and model-map file paths. |
| `path` | `[ ]` | Packages added to the unit `PATH`. SigV4 needs `pkgs.awscli2`. |
| `ledgerDir` | `null` | Override the default ledger directory. |

The module does not manage provider-key files or Claude Code settings.

## ChatGPT subscription Seats

Read [chatgpt-subscription.md](chatgpt-subscription.md) before enabling this
feature. `codexUpstream` runs CLIProxyAPI on loopback, creates global `inject`
Seats, and publishes bindings in selected Contexts:

```nix
services.patchbay.codexUpstream = {
  enable = true;
  bindings = {
    "chatgpt/sol" = "gpt-6-sol";
    "chatgpt/luna" = "gpt-6-luna";
  };
  contexts = [ "personal" ];
};
```

With `contexts = null`, bindings are added to every registry Context. A
hand-written Context binding with the same selector wins. Naming an unknown
Context fails the Home Manager assertion.

A generated selector must be non-empty. It is published as a public model name,
and patchbay refuses an empty selector key in a Context's bindings, so an empty
one fails a Home Manager assertion whichever Contexts are selected.

A generated binding may not name Claude — in any case, anywhere in the string —
and each side is checked where it lands. Every entry generates an `inject` Seat,
so a target containing `claude` always fails a Home Manager assertion; patchbay
admits no inject model naming Claude. A selector containing `claude` fails only
where it is actually published, because a hand-written Context binding of the
same selector wins: a generated selector overridden in every Context it would
reach binds no inject Seat, and that assertion leaves it alone. Both refusals
happen at evaluation instead of at gateway startup. Hand-written registry Seats
and Context bindings are validated by patchbay itself.

Each entry gets a stable generated Seat ID: `codex-` followed by the full SHA-256
digest of the selector. The whole digest keeps SHA-256's full collision resistance.
The earlier 16-character prefix discarded three quarters of it for no benefit,
leaving 64 bits in which two selectors could collapse onto a single Seat — an
unlikely failure, but a silent one, and an avoidable one. The module rejects a
generated ID that collides with a hand-written global Seat ID. Generated Seats
use:

- `auth_mode = "inject"`;
- loopback CLIProxyAPI `upstream`;
- `PATCHBAY_CHATGPT_KEY_FILE` indirection;
- the configured upstream model;
- `maxInputTokens` metadata.

Apply, log in, and verify:

```console
$ home-manager switch
$ cli-proxy-api --config "$HOME/.config/cliproxyapi/config.yaml" --codex-login
$ systemctl --user status cli-proxy-api patchbay
$ patchbay seats
$ patchbay bindings --ctx personal
$ patchbay resolve personal chatgpt/sol
```

Use `--codex-device-login` on a headless host. OAuth state is stored under
`~/.cli-proxy-api` by default.

List currently available model IDs before choosing Seat targets:

```console
$ curl -fsS -H 'Authorization: Bearer patchbay-local' \
    http://127.0.0.1:8317/v1/models | jq -r '.data[].id'
```

`codexUpstream` also provides:

| Option | Default |
|---|---|
| `package` | flake `cliproxyapi` package |
| `port` | `8317` |
| `bindings` | `{ }` |
| `contexts` | `null`, meaning all Contexts |
| `maxInputTokens` | `372000`, advertised but not enforced. Unsigned: the registry refuses a negative ceiling, so evaluation refuses one too |
| `listenerKey` | `patchbay-local` |
| `authDir` | `~/.cli-proxy-api` |

The listener key is stored in the Nix store because it protects only a loopback
listener. The OAuth files in `authDir` are the subscription credential.
