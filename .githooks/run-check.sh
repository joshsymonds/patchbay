#!/usr/bin/env bash
# Quality-gate runner for the pre-push hook. The gate — and the exact verbatim
# failure message — lives in one place so it cannot drift from CI, which runs
# the same `just check` as the remote backstop. Commits are ungated on purpose
# (see the comment in pre-push).
set -euo pipefail

# Run from the worktree root so relative paths resolve regardless of where the
# git command was invoked.
cd "$(git rev-parse --show-toplevel)"

# Put `just` on PATH even when invoked by a bare git command outside an active
# devenv/direnv shell: cached snapshot first, live devenv fallback.
set -a
source .devenv-shell-env.sh 2>/dev/null || true
set +a
if ! command -v just >/dev/null 2>&1; then
    eval "$(devenv print-dev-env)"
fi

if ! just check; then
    echo "It failed — you must fix it, it doesn't matter if you caused it or not, do not skip. Fix flakes and lints and any erroneous tests as if you caused them yourself, idiomatically and correctly." >&2
    exit 1
fi
