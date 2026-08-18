{ ... }:

# Idempotently wire .githooks/ as the active hooks directory whenever a
# devenv shell is entered. This runs for every checkout so hooks are
# self-installing — no manual `git config` step required.
#
# devenv concatenates enterShell blocks across all imported modules, so
# this composes cleanly with the block in devenv-base.nix.

{
  enterShell = ''
    if git rev-parse --is-inside-work-tree > /dev/null 2>&1; then
      git config core.hooksPath .githooks
    fi
  '';
}
