{ pkgs, ... }:

# Shared base devenv module. Imported by devenv.nix.
# Provides the Go 1.26 toolchain and shared development tools.
#
# Intentionally minimal: no services (postgres, redis), no GCP emulators,
# no language runtimes beyond Go. Add those per-task if ever needed.

{
  packages = [
    pkgs.go_1_26
    pkgs.gopls
    pkgs.delve
    pkgs.just
    pkgs.coreutils
    pkgs.golangci-lint
  ];

  # HOME-rooted Go caches, namespaced to this repo. A common path avoids
  # redundant downloads and rebuilds when working across multiple checkouts.
  env.GOMODCACHE = "${builtins.getEnv "HOME"}/.cache/patchbay/mod";
  env.GOCACHE = "${builtins.getEnv "HOME"}/.cache/patchbay/build";

  # golangci-lint's cache, unlike Go's, keys analysis results by package import
  # path — identical across checkouts — so its default per-user cache replays
  # one checkout's results (and file paths) inside another, failing the gate
  # with phantom findings whenever checkouts diverge or one is removed. Keep it
  # per-checkout instead; $PWD at devenv eval time is the checkout root.
  env.GOLANGCI_LINT_CACHE = "${builtins.getEnv "PWD"}/.golangci-cache";

  enterShell = ''
    mkdir -p "$GOMODCACHE" "$GOCACHE" "$GOLANGCI_LINT_CACHE"
  '';
}
