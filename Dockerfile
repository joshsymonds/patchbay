# patchbay as a container. The binary is static (stdlib-only, CGO off), so the
# runtime image is distroless: no shell, no package manager, CA certificates
# and a nonroot user included. See docs/docker.md for how to run it — in
# particular, publish the port to host loopback ONLY: anything that can reach
# this socket can spend the caller's credentials.
#
# Note the one thing this image cannot do: `sigv4` routes exec the aws CLI,
# which is not here. Registries using them need a native install (or the Nix
# module) — see docs/docker.md.

FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod ./
COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /patchbay ./cmd/patchbay \
    # Pre-create the ledger directory with nonroot ownership: a named volume
    # mounted there inherits it on first use, so the nonroot gateway can write.
    && mkdir -p /var/lib/patchbay/ledger \
    && chown -R 65532:65532 /var/lib/patchbay

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /patchbay /patchbay
COPY --from=build --chown=65532:65532 /var/lib/patchbay /var/lib/patchbay

# The gateway must bind a non-loopback address to be reachable from outside
# the container; the loopback-only promise is restored by publishing the port
# to 127.0.0.1 on the host (see examples/docker/compose.yaml).
ENV PATCHBAY_LISTEN=0.0.0.0:4100 \
    PATCHBAY_REGISTRY=/etc/patchbay/routes.json \
    PATCHBAY_LEDGER_DIR=/var/lib/patchbay/ledger

EXPOSE 4100

ENTRYPOINT ["/patchbay"]
CMD ["serve"]
