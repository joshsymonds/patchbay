# ChatGPT subscription routes

Patchbay can publish aliases such as `chatgpt/sol` through
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). CLIProxyAPI translates
the Anthropic Messages API to the Codex backend and holds the ChatGPT OAuth state;
patchbay treats it as an `inject` upstream.

Patchbay does not implement OpenAI or Codex OAuth.

## Account risk

This is not an OpenAI-supported patchbay integration. It sends a ChatGPT Codex
OAuth credential through a third-party proxy. OpenAI may change or block the
backend or act under its [Terms of Use](https://openai.com/policies/terms-of-use/).
Enabling this route accepts that risk for the account used to log in.

This does not affect direct Anthropic forwarding or normal API-key routes.

## Deployment choices

- Nix: enable `services.patchbay.codexUpstream`; see
  [nix.md](nix.md#chatgpt-subscription-routes).
- Docker Compose: enable the `codex` profile; see
  [docker.md](docker.md#optional-chatgpt-subscription-profile).
- Other systems: follow the manual setup below.

All three use a private CLIProxyAPI listener, a listener key shared with patchbay,
persistent OAuth state, and an `inject` route.

## Manual setup

### 1. Configure CLIProxyAPI

```yaml
# ~/.config/cliproxyapi/config.yaml
host: 127.0.0.1
port: 8317
auth-dir: "~/.cli-proxy-api"
api-keys:
  - "patchbay-local"
remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true
```

An empty management secret disables the management API. Keep the listener on
loopback. `patchbay-local` authenticates local HTTP requests; it is separate from
the OAuth credential in `auth-dir`.

### 2. Log in

```console
$ cli-proxy-api --config "$HOME/.config/cliproxyapi/config.yaml" --codex-login
```

On a headless host:

```console
$ cli-proxy-api --config "$HOME/.config/cliproxyapi/config.yaml" --codex-device-login
```

OAuth state is written under `~/.cli-proxy-api` for this configuration.

### 3. Run the service

For systemd user services:

```ini
[Unit]
Description=CLIProxyAPI
After=network-online.target

[Service]
ExecStart=/path/to/cli-proxy-api --config %h/.config/cliproxyapi/config.yaml
Restart=on-failure

[Install]
WantedBy=default.target
```

```console
$ systemctl --user daemon-reload
$ systemctl --user enable --now cli-proxy-api
$ journalctl --user -u cli-proxy-api -f
```

### 4. Choose a model ID

```console
$ curl -fsS -H 'Authorization: Bearer patchbay-local' \
    http://127.0.0.1:8317/v1/models | jq -r '.data[].id'
```

Model IDs can change. Use one returned by this endpoint.

### 5. Add the patchbay route

Create the listener-key file:

```console
$ install -m600 /dev/null ~/.config/patchbay/chatgpt.key
$ printf '%s\n' 'patchbay-local' > ~/.config/patchbay/chatgpt.key
$ export PATCHBAY_CHATGPT_KEY_FILE="$HOME/.config/patchbay/chatgpt.key"
```

Add the alias to a registry context:

```json
"chatgpt/sol": {
  "auth": "inject",
  "base_url": "http://127.0.0.1:8317",
  "model": "gpt-5.6-sol",
  "api_key_env_file": "PATCHBAY_CHATGPT_KEY_FILE",
  "max_input_tokens": 372000
}
```

Replace the model with an ID returned by `/v1/models`.

Loopback HTTP does not need `insecure: true`. A Docker service name is not
loopback, so the Compose route uses `insecure: true` for cleartext transport on
the private network.

`max_input_tokens` is model metadata; patchbay does not enforce it. The examples
use 372000. Change it if the proxy or provider reports a different usable window.

The client must still present the patchbay caller key. patchbay removes the
client's Anthropic credentials and injects the CLIProxyAPI listener key. Publish
the alias only on machines that run CLIProxyAPI and hold its OAuth state.

## Validate

```console
$ patchbay check
$ patchbay routes --ctx personal
$ patchbay resolve personal chatgpt/sol
$ claude --model chatgpt/sol -p 'say hi'
```

`resolve` requires both context and model arguments. The ledger records this like
any other inject route. Credential headers are not part of the ledger schema;
body capture, if enabled, can still contain sensitive application data.
