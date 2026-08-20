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

    registry.contexts.personal.routes."openrouter/kimi-k2" = {
      auth = "inject";
      base_url = "https://openrouter.ai/api";
      model = "moonshotai/kimi-k2";
      api_key_env_file = "PATCHBAY_OPENROUTER_KEY_FILE";
    };

    environment.PATCHBAY_OPENROUTER_KEY_FILE = "/run/agenix/openrouter-key";
  };
}
```

`api_key_env_file` names an environment variable, and that variable contains the
path to a key file outside the Nix store. Use agenix, sops-nix, or another secret
manager for provider keys.

The generated registry is stored in the Nix store and linked at
`~/.config/patchbay/routes.json`. It contains configuration and secret references,
not secret contents. Patchbay reloads it when the Home Manager symlink target
changes.

Apply and inspect:

```console
$ home-manager switch
$ . "$HOME/.nix-profile/etc/profile.d/hm-session-vars.sh"
$ systemctl --user status patchbay
$ journalctl --user -u patchbay -f
$ patchbay check
$ patchbay contexts
$ patchbay routes --ctx personal
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

Ledger records are JSONL under `~/.local/state/patchbay/ledger` unless
`ledgerDir` overrides the path:

```console
$ tail -f "$HOME/.local/state/patchbay/ledger/patchbay-$(date -u +%F).jsonl"
```

## Module options

| Option | Default | Description |
|---|---|---|
| `enable` | `false` | Install and run patchbay as a systemd user service. |
| `package` | flake package | Gateway package. |
| `listen` | `127.0.0.1:4100` | Service and CLI address. |
| `registry` | empty `personal` context | Registry rendered to JSON. |
| `callerKeyFile` | `~/.config/patchbay/caller.key` | String path to the caller key. |
| `generateCallerKey` | `true` | Create the caller key when missing. |
| `environment` | `{ }` | Extra unit environment, normally key and model-map file paths. |
| `path` | `[ ]` | Packages added to the unit `PATH`. SigV4 needs `pkgs.awscli2`. |
| `ledgerDir` | `null` | Override the default ledger directory. |

The module does not manage provider-key files or Claude Code settings.

## ChatGPT subscription routes

Read [chatgpt-subscription.md](chatgpt-subscription.md) before enabling this
feature. `codexUpstream` runs CLIProxyAPI on loopback and adds `inject` aliases:

```nix
services.patchbay.codexUpstream = {
  enable = true;
  routes = {
    "chatgpt/sol" = "gpt-5.6-sol";
    "chatgpt/luna" = "gpt-5.6-luna";
  };
  contexts = [ "personal" ];
};
```

With `contexts = null`, routes are added to every registry context. A hand-written
route with the same alias wins. Naming an unknown context fails the Home Manager
assertion.

Apply, log in, and verify:

```console
$ home-manager switch
$ cli-proxy-api --config "$HOME/.config/cliproxyapi/config.yaml" --codex-login
$ systemctl --user status cli-proxy-api patchbay
$ patchbay resolve personal chatgpt/sol
```

Use `--codex-device-login` on a headless host. OAuth state is stored under
`~/.cli-proxy-api` by default.

List currently available model IDs before choosing route targets:

```console
$ curl -fsS -H 'Authorization: Bearer patchbay-local' \
    http://127.0.0.1:8317/v1/models | jq -r '.data[].id'
```

`codexUpstream` also provides:

| Option | Default |
|---|---|
| `package` | flake `cliproxyapi` package |
| `port` | `8317` |
| `routes` | `{ }` |
| `contexts` | `null`, meaning all contexts |
| `maxInputTokens` | `372000`, advertised but not enforced |
| `listenerKey` | `patchbay-local` |
| `authDir` | `~/.cli-proxy-api` |

The listener key is stored in the Nix store because it protects only a loopback
listener. The OAuth files in `authDir` are the subscription credential.
