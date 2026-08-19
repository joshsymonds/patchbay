# Nix

The flake is the supported deployment. It exports:

| Output | What it is |
|---|---|
| `packages.<system>.patchbay` | The gateway binary (also `default`). |
| `packages.<system>.cliproxyapi` | [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), packaged from upstream release binaries — the optional Codex-subscription upstream. |
| `homeModules.patchbay` | The home-manager module below (also `default`, and mirrored as `homeManagerModules`). |

The module runs everything as systemd user services, so it is Linux-only; on
other platforms use the packages directly and arrange the processes yourself.

## The module

```nix
{
  inputs.patchbay.url = "github:joshsymonds/patchbay";

  # …in your home-manager configuration:
  imports = [ inputs.patchbay.homeModules.patchbay ];

  services.patchbay = {
    enable = true;

    registry.contexts.personal.routes."openrouter/kimi-k2" = {
      auth = "inject";
      base_url = "https://openrouter.ai/api";
      model = "moonshotai/kimi-k2";
      api_key_env_file = "PATCHBAY_OPENROUTER_KEY_FILE";
      strip_cache_control = true;
    };

    environment.PATCHBAY_OPENROUTER_KEY_FILE = "/run/agenix/openrouter-key";
  };
}
```

That is a complete deployment. `home-manager switch` starts the gateway on
`127.0.0.1:4100`, generates the registry into the store and links it at
`~/.config/patchbay/routes.json` (patchbay hot-reloads when the store path
moves, so a rebuild takes effect on the next request with nothing to restart),
and generates a caller key at `~/.config/patchbay/caller.key` if none exists.
Point `ANTHROPIC_BASE_URL` at the gateway as the README describes and you are
done.

The division of labor is the same one the rest of the documentation assumes:
the registry is world-readable and holds only the *names* of environment
variables; `services.patchbay.environment` gives those variables values, each
the path of a key file that lives outside the store — placed by agenix,
sops-nix, or your own hands.

### Options

| Option | Default | Meaning |
|---|---|---|
| `enable` | `false` | Run the gateway as a systemd user service. |
| `package` | the flake's `patchbay` | The gateway binary. |
| `listen` | `"127.0.0.1:4100"` | Bind address, and what the CLI subcommands dial. Keep it loopback. |
| `registry` | one empty `personal` context | The registry document as Nix attrs, generated to JSON. The default is a pure Anthropic passthrough. Schema: [configuration.md](configuration.md). |
| `callerKeyFile` | `~/.config/patchbay/caller.key` | Path (a string, never a store path) of the caller key file. |
| `generateCallerKey` | `true` | Create `callerKeyFile` on activation when missing: 32 random bytes, base64, 0600. Disable if a secrets tool places it. |
| `environment` | `{ }` | Extra unit environment — the key-file and model-map variables the registry names. |
| `path` | `[ ]` | Packages on the unit's PATH. A registry with `sigv4` routes needs `[ pkgs.awscli2 ]` here; nothing else needs a PATH at all. |
| `ledgerDir` | patchbay's default | Where JSONL ledger records land. |

The module also exports `PATCHBAY_LISTEN` and `PATCHBAY_CALLER_KEY_FILE` as
session variables, so the CLI subcommands (`patchbay routes`, `resolve`,
`tail`, …) work in an interactive shell with no further ceremony.

What it deliberately does not manage: the key files themselves, and
`.claude/settings.json` — which project selects which context is the
project's own business ([README, Contexts](../README.md#contexts)).

## `codexUpstream`: ChatGPT subscription routes in one flag

Read [chatgpt-subscription.md](chatgpt-subscription.md) first. It opens with
the part that matters: fronting a ChatGPT subscription this way is at best a
gray area of OpenAI's Terms of Use, and enabling this option is accepting
that risk for the account you log in. The option exists to make the setup
seamless, not the decision.

```nix
services.patchbay.codexUpstream = {
  enable = true;
  routes = {
    "chatgpt/sol" = "gpt-5.6-sol";
    "chatgpt/luna" = "gpt-5.6-luna";
  };
};
```

With that, one `home-manager switch`:

- runs the flake's `cliproxyapi` as a second user service on loopback `:8317`,
  with a generated config (management API and control panel disabled);
- merges an `inject` route per entry of `routes` into every context of your
  registry (narrow it with `codexUpstream.contexts = [ "personal" ]`), keyed
  through the usual file indirection — the service, the routes, and the
  listener key can never drift apart, because one module defines all three;
- leaves a hand-written route of the same name alone: the reviewed registry
  wins over the generated convenience.

One step is yours, once per machine, because it mints the credential:

```console
$ cli-proxy-api --config ~/.config/cliproxyapi/config.yaml --codex-login
```

(`--codex-device-login` on headless hosts; the module puts `cli-proxy-api` on
your PATH.) The OAuth state lands in `~/.cli-proxy-api`, and only machines
holding it can answer — which is why `enable` belongs only on those hosts.
Model ids drift as OpenAI ships; list the current ones as
[chatgpt-subscription.md](chatgpt-subscription.md#the-proxy) shows and update
`routes` when one starts refusing.

Remaining `codexUpstream` options, all with working defaults: `package`,
`port` (8317), `maxInputTokens` (372000, the subscription's enforced window),
`listenerKey` (`"patchbay-local"` — a label, not a secret: it gates only the
loopback listener, and the OAuth state is the real credential), `authDir`
(`~/.cli-proxy-api`), and `contexts` (null: all of them).
