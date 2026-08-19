# Docker

For hosts without Nix. The image is the same static stdlib-only binary on a
distroless base — no shell, no package manager — published multi-arch
(amd64, arm64) to GHCR:

```
ghcr.io/joshsymonds/patchbay:latest
```

One caveat before anything else: **`sigv4` routes do not work in this
container.** They exec `aws configure export-credentials`, and the aws CLI is
deliberately not in a distroless image. A registry with Bedrock routes needs
a native install or the [Nix module](nix.md); every `forward` and `inject`
route works here unchanged.

## Compose

[examples/docker/](../examples/docker/) is a working deployment: copy the
directory, edit `routes.json` to your registry, and create the files it
names:

```console
$ head -c 32 /dev/urandom | base64 > caller.key
$ $EDITOR openrouter.key                  # your provider key, one line
$ echo patchbay-local > chatgpt.key       # only used by the codex profile
$ docker compose up -d
```

Every bind-mounted file must exist before `up` — Docker silently turns a
missing bind source into an empty directory, which then shadows the file and
produces confusing 500s instead of a clean startup failure.

The port mapping is the deployment's security posture, so it is worth reading
rather than skimming:

```yaml
ports:
  - "127.0.0.1:4100:4100"
```

Inside the container patchbay binds `0.0.0.0` (it has to, to be reachable
across the container boundary at all); the loopback-only promise the README
makes is restored by publishing to the host's `127.0.0.1`. Writing
`"4100:4100"` instead would offer your Anthropic credentials to the LAN.

Claude Code on the host then points at the gateway exactly as the README
describes: `ANTHROPIC_BASE_URL=http://127.0.0.1:4100`. The `patchbay` CLI
subcommands (`routes`, `resolve`, `tail`, …) also work from the host
unchanged — they dial the daemon over HTTP, so a native CLI binary next to a
containerized gateway is a fine combination. The ledger lives in the `ledger`
named volume; `patchbay tail` reads it through the daemon, so you rarely need
the volume itself.

## The codex profile

The compose file carries an optional second service: CLIProxyAPI (upstream's
own published image), translating Anthropic Messages to the ChatGPT Codex
OAuth backend. Read [chatgpt-subscription.md](chatgpt-subscription.md) first
— it opens with the Terms-of-Use caveat, and enabling this profile is
accepting that risk for the account you log in.

```console
$ docker compose --profile codex up -d
```

The differences from the bare-metal recipe are mechanical, and the example
files already carry them: the route's `base_url` is the service name
(`http://cli-proxy-api:8317`), which is non-loopback, so the route honestly
declares `"insecure": true` — cleartext over the compose network, readable by
anything attached to it. The proxy binds `0.0.0.0` in its container but
publishes no port, so only that network reaches it.

Log in once — device login, because the container cannot open your browser:

```console
$ docker compose --profile codex run --rm cli-proxy-api \
    ./CLIProxyAPI --config /CLIProxyAPI/config.yaml --codex-device-login
```

It prints a URL and a code to enter from any browser. The OAuth state lands
in the `codex-auth` volume and the proxy refreshes it from then on. List the
subscription's current model ids from inside the network (the listener is
not published to the host):

```console
$ docker compose --profile codex exec cli-proxy-api sh -c \
    'wget -qO- --header="Authorization: Bearer patchbay-local" http://127.0.0.1:8317/v1/models'
```

## Building locally

The compose file builds from source when the image is absent, or explicitly:

```console
$ docker build -t patchbay .
```

The published image is built by [.github/workflows/docker.yml](../.github/workflows/docker.yml)
on every push to `main` (as `latest`) and on `v*` tags (as their version).
