# ChatGPT subscription routes

patchbay can publish aliases that answer with a ChatGPT subscription's models
— `chatgpt/sol` next to `openrouter/kimi-k2` in the same context. patchbay
itself knows nothing about OpenAI. The work is done by a translating proxy on
loopback that speaks the Anthropic Messages API on one side and the Codex
OAuth backend on the other, and the registry points an ordinary `inject` route
at it. This page is the recipe for that arrangement.

## Walk in eyes wide open

Reaching a ChatGPT subscription through a third-party proxy is, at best, a
gray area of OpenAI's Terms of Use. The proxy holds a real Codex OAuth login
and speaks the same backend the Codex CLI does, but OpenAI has not blessed the
arrangement, may change or close the backend at any time, and enforcement
against the logged-in account is a risk you accept by setting this up. Nothing
below reduces that risk; decide whether the account can bear it first.

None of this applies to the rest of patchbay. Skip this page and the gateway
is exactly what the README describes: your Anthropic subscription forwarded
untouched, foreign models paid per-token with API keys.

## On Nix, this is one flag

The flake's home-manager module wires everything below — the proxy service,
its config, the listener key, and the routes — from a single option:

```nix
services.patchbay.codexUpstream = {
  enable = true;
  routes."chatgpt/sol" = "gpt-5.6-sol";
};
```

See [nix.md](nix.md). Only the one-time `--codex-login` remains yours, because
it mints the credential. Under Docker, the compose example's `codex` profile
is the equivalent — see [docker.md](docker.md#the-codex-profile). The rest of
this page is the same arrangement by hand, and the reference for what either
one actually sets up.

## The proxy

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) is a Go proxy
that exposes CLI-agent subscriptions (ChatGPT Codex, Gemini, and others) as
OpenAI- and Anthropic-compatible endpoints. Install a release binary or
package it yourself, then give it a config:

```yaml
# ~/.config/cliproxyapi/config.yaml
host: 127.0.0.1
port: 8317
auth-dir: "~/.cli-proxy-api"
api-keys:
  - "patchbay-local"
# An empty secret-key disables the management API and its control panel,
# which otherwise auto-downloads a web UI from GitHub at runtime.
remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true
```

Log in once per machine:

```console
$ cli-proxy-api --config ~/.config/cliproxyapi/config.yaml --codex-login
```

On a headless host, `--codex-device-login` prints a URL and a code to enter
from any browser. Either way the OAuth state lands in `auth-dir`, and the
proxy refreshes it on its own from then on.

Then run it — in a terminal, or as a service. A minimal systemd user unit:

```ini
[Unit]
Description=CLIProxyAPI — Anthropic-compatible endpoint over the Codex subscription
After=network-online.target

[Service]
ExecStart=/path/to/cli-proxy-api --config %h/.config/cliproxyapi/config.yaml
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

Confirm it answers, and see which model ids the subscription offers:

```console
$ curl -s -H 'Authorization: Bearer patchbay-local' \
    http://127.0.0.1:8317/v1/models | jq -r '.data[].id'
```

## What is a secret here, and what is not

The `api-keys` entry is the listener key: it is what patchbay injects, and it
only gates the loopback listener. Loopback is reachable by anything on the
machine — including host-network containers — so treat the listener key as a
label, not a secret. The actual credential is the OAuth state in `auth-dir`,
which exists only on machines where you ran the login. That per-host gating is
the real containment, and it is why the key can sit in a world-readable file
without ceremony.

## The route

An `inject` route with a loopback `base_url`. Loopback `http` needs no
`insecure` flag — the bytes never leave the machine:

```json
"chatgpt/sol": {
  "auth": "inject",
  "base_url": "http://127.0.0.1:8317",
  "model": "gpt-5.6-sol",
  "api_key_env_file": "PATCHBAY_CHATGPT_KEY_FILE",
  "max_input_tokens": 372000
}
```

- `model` is one of the ids the `/v1/models` listing above reported. It will
  drift as OpenAI ships models; re-check the listing when a route starts
  refusing.
- `api_key_env_file` follows the usual indirection: the variable's value is a
  path, the file at that path holds the listener key. Point it at a file
  containing the same string as the proxy's `api-keys` entry.
- `max_input_tokens` should be the subscription's context window, not the
  model's headline one — the Codex backend enforces its own limit. 372000 is
  right for the current subscription tier; adjust if the proxy starts
  answering 400s about context length.
- `strip_cache_control` is not needed: the proxy understands Anthropic's
  prompt-cache markers and translates them.

All the usual `inject` rules apply ([auth-modes.md](auth-modes.md)): the
caller must present the patchbay caller key, the caller's own Anthropic
credentials are dropped before the request leaves, and only `/v1/messages`
and `/v1/messages/count_tokens` are relayed.

Publish the alias only on hosts that actually run the proxy. Anywhere else it
would point at a port nothing listens on, and a request would fail with a 502
instead of resolving somewhere sensible.

## Check it end to end

```console
$ patchbay resolve chatgpt/sol
$ claude --model chatgpt/sol -p 'say hi'
```

The ledger records the turn like any other inject route: alias, upstream
model, tokens, duration. Spend is the subscription's flat rate, so the
token counts are informational rather than billable.

The same shape covers any other subscription or translator the proxy family
supports — a Gemini login, an OpenAI-compatible server behind a translating
front — because to patchbay each one is only ever a loopback `base_url` that
speaks Messages.
