# Illustrative home-manager module for patchbay: it generates the registry and
# runs the gateway as a systemd user service. It is an example, not a supported
# interface — copy it into your own configuration and edit it, rather than
# importing it.
#
# The shape of the deployment is the point:
#
#   * the registry is generated from Nix and lands in the store, world-readable,
#     because it holds no secrets — only the NAMES of environment variables;
#   * those variables point at key files that live OUTSIDE the store, 0600, and
#     are read by the gateway on every request;
#   * nothing needs restarting when the registry changes: patchbay notices the
#     new store path and reloads on the next request.
#
# Usage sketch, with patchbay's flake as an input named `patchbay`:
#
#   home-manager.users.you.imports = [ ./home-manager.nix ];
#   # and, in that module's arguments, pass the package:
#   #   patchbayPackage = inputs.patchbay.packages.${pkgs.system}.patchbay;

{ config, lib, pkgs, ... }:

let
  # Where the gateway listens. Loopback only: the forward path spends the
  # caller's own Anthropic credentials, so anything that can reach this socket
  # can spend them.
  listen = "127.0.0.1:4100";

  # Secrets, kept out of the Nix store. Create them by hand, or have agenix /
  # sops-nix place them here; patchbay only ever needs the paths.
  secretsDir = "${config.home.homeDirectory}/.config/patchbay/secrets";
  callerKeyFile = "${secretsDir}/caller.key";
  openrouterKeyFile = "${secretsDir}/openrouter.key";

  # The Bedrock model map is its own file for the same reason the API keys are:
  # no account-identifying inference-profile id belongs in a config you might
  # share. Substitute your own account's ids.
  bedrockModelMapFile = pkgs.writeText "patchbay-bedrock-models.json" (builtins.toJSON {
    "claude-opus-4-5" = "us.anthropic.claude-opus-4-5-20251101-v1:0";
    "claude-sonnet-4-5" = "us.anthropic.claude-sonnet-4-5-20250929-v1:0";
  });

  # Environment variable names the registry refers to. Declaring them once keeps
  # the registry and the unit file from drifting apart.
  openrouterKeyVar = "PATCHBAY_OPENROUTER_KEY_FILE";
  bedrockModelMapVar = "PATCHBAY_BEDROCK_MODEL_MAP_FILE";

  registry = {
    contexts = {
      # No default_route: unclaimed models ride the built-in forward to
      # Anthropic on the caller's own subscription credentials.
      personal.routes = {
        "openrouter/kimi-k2" = {
          auth = "inject";
          base_url = "https://openrouter.ai/api";
          model = "moonshotai/kimi-k2";
          api_key_env_file = openrouterKeyVar;
          strip_cache_control = true;
          max_input_tokens = 262144;
          max_output_tokens = 32000;
        };
      };

      # A project that points ANTHROPIC_BASE_URL at .../ctx/work bills every
      # model to Bedrock on the work account, including plain claude-* names.
      work = {
        default_route = {
          auth = "sigv4";
          aws_profile = "work-bedrock";
          aws_region = "us-west-2";
          model_map_env_file = bedrockModelMapVar;
        };
        routes = { };
      };
    };
  };

  registryFile = pkgs.writeText "patchbay-routes.json" (builtins.toJSON registry);

  # Replace with the package from patchbay's own flake:
  #   inputs.patchbay.packages.${pkgs.system}.patchbay
  patchbayPackage = pkgs.buildGoModule {
    pname = "patchbay";
    version = "dev";
    src = ../.;
    vendorHash = null; # stdlib only: there is nothing to vendor
    subPackages = [ "cmd/patchbay" ];
    doCheck = false;
  };

  patchbay = lib.getExe' patchbayPackage "patchbay";
in
{
  home.packages = [ patchbayPackage ];

  # So the CLI subcommands work in an interactive shell without repeating
  # themselves: they dial $PATCHBAY_LISTEN and present $PATCHBAY_CALLER_KEY_FILE.
  home.sessionVariables = {
    PATCHBAY_LISTEN = listen;
    PATCHBAY_REGISTRY = "${registryFile}";
    PATCHBAY_CALLER_KEY_FILE = callerKeyFile;
  };

  systemd.user.services.patchbay = {
    Unit = {
      Description = "patchbay — local Anthropic Messages API gateway";
      After = [ "network.target" ];
    };

    Service = {
      # `aws configure export-credentials` is exec'd for sigv4 routes, so the
      # AWS CLI has to be on the unit's PATH.
      Environment = [
        "PATCHBAY_LISTEN=${listen}"
        "PATCHBAY_REGISTRY=${registryFile}"
        "PATCHBAY_LEDGER_DIR=%S/patchbay/ledger"
        "PATCHBAY_CALLER_KEY_FILE=${callerKeyFile}"
        "${openrouterKeyVar}=${openrouterKeyFile}"
        "${bedrockModelMapVar}=${bedrockModelMapFile}"
        "PATH=${lib.makeBinPath [ pkgs.awscli2 ]}"
      ];

      ExecStart = "${patchbay} serve";
      Restart = "on-failure";
      RestartSec = 2;

      # The ledger records name every model this machine used and when, so its
      # directory is the unit's own, 0700.
      StateDirectory = "patchbay";
      StateDirectoryMode = "0700";
      PrivateTmp = true;
      NoNewPrivileges = true;
      # Filesystem sandboxing (ProtectHome, ProtectSystem) is worth adding, but
      # note that sigv4 routes shell out to the AWS CLI, which needs ~/.aws
      # readable and — for an SSO session it has to refresh — its token cache
      # writable.
    };

    Install.WantedBy = [ "default.target" ];
  };

  # Point Claude Code at the gateway for every project. A single project
  # overrides this with its own .claude/settings.json — that file is how a
  # checkout selects a context, e.g.
  #   "ANTHROPIC_BASE_URL": "http://127.0.0.1:4100/ctx/work"
  #
  # ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN stay unset: Claude Code keeps its
  # own subscription login, and patchbay forwards it to Anthropic untouched.
  # X-Patchbay-Key is what unlocks the inject and sigv4 routes; it is read from
  # the key file at build time here, so use this only if your generated
  # settings file is acceptable to you as 0600 outside the store — otherwise
  # write .claude/settings.json by hand.
  #
  # home.file.".claude/settings.json".text = builtins.toJSON {
  #   env = {
  #     ANTHROPIC_BASE_URL = "http://${listen}";
  #     ANTHROPIC_CUSTOM_HEADERS = "X-Patchbay-Key: <caller key>";
  #   };
  # };
}
