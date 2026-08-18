#!/usr/bin/env bash
#
# Single-nixpkgs-pin gate: devenv.lock and flake.lock must resolve nixpkgs to
# the SAME rev. devenv.yaml and flake.nix pin one literal rev; this check
# catches the locks drifting apart (someone updated one side by hand, or ran
# `devenv update` / `nix flake update` directly). The only sanctioned way to
# move the pin is `just bump-nix`, which rewrites both pin files and refreshes
# both locks together.
#
# Runs OUTSIDE the devenv shell (it gates entry into everything else), so it
# depends only on plain shell + jq — both present on this machine and on
# ubuntu GitHub runners.

set -euo pipefail

cd "$(dirname "$0")/.."

fail=""
for lock in devenv.lock flake.lock; do
    if [ ! -r "$lock" ]; then
        echo "check-nix-pin: $lock is missing or unreadable" >&2
        fail=1
    fi
done
[ -z "$fail" ] || exit 1

devenv_rev="$(jq -er '.nodes.nixpkgs.locked.rev' devenv.lock)" || {
    echo "check-nix-pin: devenv.lock has no .nodes.nixpkgs.locked.rev" >&2
    exit 1
}
flake_rev="$(jq -er '.nodes.nixpkgs.locked.rev' flake.lock)" || {
    echo "check-nix-pin: flake.lock has no .nodes.nixpkgs.locked.rev" >&2
    exit 1
}

if [ "$devenv_rev" != "$flake_rev" ]; then
    echo "check-nix-pin: nixpkgs locks have diverged:" >&2
    echo "  devenv.lock  nixpkgs rev: $devenv_rev" >&2
    echo "  flake.lock   nixpkgs rev: $flake_rev" >&2
    echo "Fix with \`just bump-nix\` — the single sanctioned way to move the pin." >&2
    exit 1
fi

echo "check-nix-pin OK: both locks at nixpkgs $devenv_rev"
