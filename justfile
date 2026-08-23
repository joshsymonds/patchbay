#!/usr/bin/env -S just --justfile
#
# Quality gate for the patchbay module. Single Go module at the repo root: the
# shared devenv env snapshot is cached in .devenv-shell-env.sh and sourced by
# every recipe that runs Go tooling.

set shell := ["bash", "-uc"]

_envfile := ".devenv-shell-env.sh"

# Wall-clock budget (seconds) for the test suite — a hang guard, not a
# performance target. Override on the CLI: just TEST_BUDGET=60 check
TEST_BUDGET := "120"

# List available recipes.
default:
    @just --list

# Run the full quality gate. The nixpkgs pin check runs first: it gates
# everything on devenv.lock and flake.lock agreeing on one nixpkgs rev. Then
# lint, then race+coverage, then the home-manager module's option types and the
# wiring an enabled `services.patchbay` generates.
check: && lint cover nix-module
    ./scripts/check-nix-pin.sh

# Evaluate the home-manager module (nix/hm-module-test.nix): the option types it
# declares, plus the registry, CLIProxyAPI config and systemd user units an
# enabled `services.patchbay` generates. The flake only sees git-tracked files,
# so `git add` anything new under nix/ before running this.
nix-module:
    nix build --no-link ".#checks.$(nix eval --impure --raw --expr builtins.currentSystem).hm-module-options"

# Source the cached devenv env, regenerating if missing or stale. The snapshot
# is invalidated by any env-defining file: the nixpkgs input (devenv.yaml), the
# lock, or any module (devenv.nix and its imports devenv-base.nix/githooks.nix
# all contribute env vars).
_load-env:
    @if [ ! -f {{_envfile}} ] || [ -n "$(find devenv.yaml devenv.lock *.nix -newer {{_envfile}} 2>/dev/null)" ]; then \
        echo "Refreshing devenv env snapshot ({{_envfile}})..."; \
        devenv print-dev-env > {{_envfile}}; \
    fi

# Regenerate the devenv environment snapshot unconditionally.
refresh-env:
    devenv print-dev-env > {{_envfile}}
    @echo "Regenerated {{_envfile}}"

# Run golangci-lint for the host platform, then again for Darwin. Build-tagged
# files are invisible to a single-GOOS run: the purego ACL bindings behind
# //go:build darwin escaped the gate entirely until this second pass existed.
lint: _load-env
    set -a && source {{_envfile}} && set +a && golangci-lint run
    set -a && source {{_envfile}} && set +a && GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 golangci-lint run

# Run go test.
test: _load-env
    set -a && source {{_envfile}} && set +a && go test ./...

# Run go test with the race detector.
test-race: _load-env
    set -a && source {{_envfile}} && set +a && go test -race -count=1 ./...

# Run tests with race+coverage and enforce >=80% total coverage within the time
# budget. -coverpkg=./... attributes coverage across package boundaries. Entry-
# point glue (cmd/) is excluded from the denominator — it is exercised end-to-
# end, not by unit tests.
cover: _load-env
    #!/usr/bin/env bash
    set -euo pipefail
    set -a; source {{_envfile}}; set +a

    rc=0
    timeout "{{TEST_BUDGET}}" go test -race -covermode=atomic -coverpkg=./... -coverprofile=coverage.out ./... || rc=$?
    if [ "$rc" -eq 124 ]; then
        echo "tests exceeded {{TEST_BUDGET}}s budget" >&2
        exit 124
    elif [ "$rc" -ne 0 ]; then
        exit "$rc"
    fi

    grep -vE '/cmd/' coverage.out > coverage.cov.out

    pct=$(go tool cover -func=coverage.cov.out | awk '/^total:/{gsub(/%/, "", $NF); print $NF}')
    ok=$(awk -v p="$pct" 'BEGIN { print (p+0 >= 80.0) ? "yes" : "no" }')
    if [ "$ok" = "no" ]; then
        echo "coverage ${pct}% is below 80% threshold" >&2
        exit 1
    fi
    echo "coverage: ${pct}% (>= 80% OK)"

# Move the shared nixpkgs pin. Rewrites the rev literal in BOTH devenv.yaml and
# flake.nix, refreshes both locks with their own tools, and regenerates the env
# snapshot. This is the ONLY sanctioned way to move the pin — never hand-edit
# the locks (scripts/check-nix-pin.sh gates `just check` on them agreeing).
# REF is a 40-char rev used verbatim, or a branch name resolved via git
# ls-remote against github.com/NixOS/nixpkgs.
bump-nix $ref="nixpkgs-unstable":
    #!/usr/bin/env bash
    set -euo pipefail

    if [[ "$ref" =~ ^[0-9a-f]{40}$ ]]; then
        rev="$ref"
    else
        rev="$(git ls-remote https://github.com/NixOS/nixpkgs "refs/heads/$ref" | cut -f1)"
        if [ -z "$rev" ]; then
            echo "bump-nix: refs/heads/$ref did not resolve on github.com/NixOS/nixpkgs" >&2
            exit 1
        fi
    fi
    echo "Bumping nixpkgs pin to $rev..."

    # Rewrite each pin file's single URL literal. Refuse unless the anchored
    # pattern matches exactly once — zero or several means the file's shape
    # changed and the sed would silently do the wrong thing.
    rewrite() {
        local file="$1" pattern="$2" replacement="$3" count
        count="$(grep -cE "$pattern" "$file" || true)"
        if [ "$count" -ne 1 ]; then
            echo "bump-nix: expected exactly 1 line matching $pattern in $file, found $count" >&2
            exit 1
        fi
        sed -i -E "s|$pattern|$replacement|" "$file"
    }
    rewrite devenv.yaml \
        '^    url: github:NixOS/nixpkgs/[^ ]+$' \
        "    url: github:NixOS/nixpkgs/$rev"
    rewrite flake.nix \
        '^  inputs\.nixpkgs\.url = "github:NixOS/nixpkgs/[^"]+";$' \
        "  inputs.nixpkgs.url = \"github:NixOS/nixpkgs/$rev\";"

    devenv update nixpkgs
    nix flake update nixpkgs
    just refresh-env

    ./scripts/check-nix-pin.sh
