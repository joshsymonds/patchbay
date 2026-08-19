{
  description = "patchbay: a single-binary local HTTP gateway (Anthropic Messages API-compatible proxy).";

  # Package outputs only — devenv.nix/devenv.yaml own the dev shell. This flake
  # and devenv.yaml pin the SAME literal nixpkgs rev — one toolchain everywhere,
  # enforced by scripts/check-nix-pin.sh (the first step of `just check`). Move
  # the pin only via `just bump-nix`, which rewrites both files and both locks.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/e8273b29fe1390ec8d4603f2477357555291432e";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (system:
        let
          pkgs = import nixpkgs { inherit system; };

          # go.mod declares `go 1.26`; pin the matching nixpkgs toolchain so the
          # build doesn't try (and fail, sandboxed) to fetch a newer toolchain.
          patchbay = pkgs.buildGoModule {
            pname = "patchbay";
            version = "dev";

            src = ./.;
            # Stdlib-only module with no external dependencies: there is nothing
            # to vendor, so a nil vendorHash is correct.
            vendorHash = null;

            go = pkgs.go_1_26;
            subPackages = [ "cmd/patchbay" ];

            ldflags = [ "-s" "-w" ];

            # `just check` is the real gate (lint + race + coverage, run locally
            # and in CI). The Nix build stays a pure artifact build; the test
            # suite is not re-run inside the sandbox.
            doCheck = false;

            env.CGO_ENABLED = 0;
          };
          # The optional Codex-subscription upstream the home-manager module can
          # run — see docs/chatgpt-subscription.md. Packaged from upstream
          # release binaries in ./nix/cliproxyapi.
          cliproxyapi = pkgs.callPackage ./nix/cliproxyapi { };
        in
        {
          inherit patchbay cliproxyapi;
          default = patchbay;
        });

      # The supported deployment: `services.patchbay` as a home-manager module,
      # running the gateway (and, opted in, the Codex-subscription upstream) as
      # systemd user services. Documented in docs/nix.md.
      homeModules = rec {
        patchbay = import ./nix/hm-module.nix self;
        default = patchbay;
      };
      # The name home-manager's own documentation has historically used.
      homeManagerModules = self.homeModules;
    };
}
