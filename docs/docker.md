# Docker

For hosts without Nix. The published image carries the same static CGO-free
binary a native install builds:

```text
ghcr.io/joshsymonds/patchbay:latest
```

It is a distroless Linux image for amd64 and arm64. It has no shell or package
manager and runs as UID/GID 65532.

The image cannot serve `sigv4` Seats because it does not include the AWS CLI. Use
it for `forward` and `inject` Seats. Use a native install or the
[Nix module](nix.md) for Bedrock.

## Compose setup

The maintained example is [examples/docker/](../examples/docker/):

```console
$ cp -R examples/docker ./patchbay-docker
$ cd patchbay-docker
```

Edit the strict `routes.json`, then create every bind-mounted file before
starting Compose:

```console
$ head -c 32 /dev/urandom | base64 > caller.key
$ $EDITOR openrouter.key
$ printf '%s\n' 'patchbay-local' > chatgpt.key
```

Remove the ChatGPT Seat and binding, key variable, and key mount if you will not
use the optional `codex` profile. A missing bind-mount source may be created as a
directory and hide the expected file.

On native Linux, make the files readable by container group 65532:

```console
$ sudo chown "$(id -u):65532" caller.key openrouter.key chatgpt.key
$ chmod 0640 caller.key openrouter.key chatgpt.key
$ chmod 0644 routes.json cliproxyapi.yaml
```

Docker Desktop uses its VM's file-sharing rules instead. Keep provider keys
unreadable to other local accounts.

Validate and start:

```console
$ docker compose run --rm --no-deps patchbay check
$ docker compose up -d patchbay
$ docker compose logs -f patchbay
```

Initial registry failure is fatal, so `patchbay` will not bind its listener until
the mounted JSON is strict and valid. So is an accounting database that cannot be
opened: the container will not start without a writable `/var/lib/patchbay`.

The image sets these runtime values:

| Purpose | Value |
|---|---|
| Listener | `0.0.0.0:4100` |
| Registry | `/etc/patchbay/routes.json` |
| Ledger | `/var/lib/patchbay/ledger` |
| Accounting database | `/var/lib/patchbay/usage.sqlite` |

The example mounts the whole `/var/lib/patchbay` state directory as the named
`state` volume, so the accounting database survives a container recreate. Mount
only the ledger under it and the spending record is lost with the container.
Back it up with `patchbay usage snapshot`, which is consistent while the daemon
is writing:

```console
$ docker compose exec patchbay /patchbay usage snapshot /var/lib/patchbay/usage.backup.sqlite
```

Compose publishes the gateway only on host loopback:

```yaml
ports:
  - "127.0.0.1:4100:4100"
```

Do not replace this with a public bind. Forward Seats can spend caller
credentials, and inject Seats can spend gateway credentials when the caller key
is known.

## Claude Code

Configure the host client:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:4100",
    "ANTHROPIC_CUSTOM_HEADERS": "X-Patchbay-Key: <contents of caller.key>"
  }
}
```

Leave `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` unset to keep using the
Claude Code subscription login through a `forward` default Seat.

## Inspection

A host `patchbay` binary can query the containerized gateway:

```console
$ export PATCHBAY_LISTEN=127.0.0.1:4100
$ export PATCHBAY_CALLER_KEY_FILE="$PWD/caller.key"
$ patchbay contexts
$ patchbay seats
$ patchbay bindings --ctx personal
$ patchbay resolve personal openrouter/kimi-k2
```

Without the binary, query introspection directly:

```console
$ curl -fsS \
    -H "X-Patchbay-Key: $(tr -d '\n' < caller.key)" \
    http://127.0.0.1:4100/_patchbay/registry | jq .
```

Patchbay has no ledger-tail command. Copy a snapshot and inspect v2 JSONL files:

```console
$ docker compose cp patchbay:/var/lib/patchbay/ledger ./patchbay-ledger
$ jq -c . ./patchbay-ledger/patchbay-v2-*.jsonl
```

Use `docker compose logs -f patchbay` for service logs.

## Optional ChatGPT subscription profile

Read [chatgpt-subscription.md](chatgpt-subscription.md) before enabling this
profile. It uses CLIProxyAPI and a ChatGPT Codex OAuth login through a third-party
proxy.

Run the one-time device login, then start both services:

```console
$ docker compose --profile codex run --rm cli-proxy-api \
    /CLIProxyAPI/CLIProxyAPI \
    --config /CLIProxyAPI/config.yaml \
    --codex-device-login

$ docker compose --profile codex up -d
$ docker compose logs -f cli-proxy-api patchbay
```

OAuth state persists in the `codex-auth` volume. CLIProxyAPI has no published host
port. Its Seat uses plain HTTP on the private Compose network and therefore sets
`insecure: true`; do not attach untrusted containers to that network.

The CLIProxyAPI image does not contain `curl`, `wget`, or `jq`. Query its models
through a temporary client in the same network namespace:

```console
$ docker run --rm \
    --network "container:$(docker compose ps -q cli-proxy-api)" \
    curlimages/curl:latest \
    -fsS -H 'Authorization: Bearer patchbay-local' \
    http://127.0.0.1:8317/v1/models | jq -r '.data[].id'
```

After changing `routes.json`:

```console
$ docker compose run --rm --no-deps patchbay check
$ docker compose up -d --force-recreate patchbay
$ PATCHBAY_CALLER_KEY_FILE="$PWD/caller.key" patchbay resolve personal chatgpt/sol
```

## Building locally

From the repository root:

```console
$ docker build -t ghcr.io/joshsymonds/patchbay:latest .
$ docker compose -f examples/docker/compose.yaml build patchbay
```

The Compose build context is `../..` relative to `examples/docker`. If you copy
the directory elsewhere, remove the `build` block, use the published image, or
point the context at a patchbay checkout.
