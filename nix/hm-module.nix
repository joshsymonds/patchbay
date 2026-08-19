# The supported home-manager deployment of patchbay: the gateway as a systemd
# user service, the registry generated from Nix, and — opted in per host — the
# Codex-subscription upstream (CLIProxyAPI) run and routed at in one flag.
#
# The shape of the deployment is deliberate:
#
#   * the registry lands in the store, world-readable, because it holds no
#     secrets — only the NAMES of environment variables;
#   * those variables point at key files that live OUTSIDE the store, 0600,
#     read by the gateway on every request;
#   * nothing needs restarting when the registry changes: patchbay notices the
#     new store path and reloads on the next request.
#
# Imported from the flake as `homeModules.patchbay`; documented in docs/nix.md.
self: {
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.services.patchbay;

  jsonFormat = pkgs.formats.json { };
  yamlFormat = pkgs.formats.yaml { };

  flakePkgs = self.packages.${pkgs.stdenv.hostPlatform.system};

  registryPath = "${config.xdg.configHome}/patchbay/routes.json";

  # The CLIProxyAPI listener key travels store → unit env → injected header,
  # so patchbay's usual key-file indirection carries it like any other route
  # key. It lives world-readable in the store because it only gates a loopback
  # listener — see the option's description.
  chatgptKeyVar = "PATCHBAY_CHATGPT_KEY_FILE";
  chatgptKeyFile = pkgs.writeText "patchbay-chatgpt-key" cfg.codexUpstream.listenerKey;

  codexRoute = model: {
    auth = "inject";
    base_url = "http://127.0.0.1:${toString cfg.codexUpstream.port}";
    api_key_env_file = chatgptKeyVar;
    inherit model;
    max_input_tokens = cfg.codexUpstream.maxInputTokens;
  };

  codexRoutes = lib.mapAttrs (_: codexRoute) cfg.codexUpstream.routes;

  codexContextNames =
    if cfg.codexUpstream.contexts == null
    then lib.attrNames (cfg.registry.contexts or { })
    else cfg.codexUpstream.contexts;

  # The declared registry, with the chatgpt routes merged into the chosen
  # contexts. A hand-written route wins over a generated one of the same name:
  # the reviewed document beats the convenience.
  registry =
    if cfg.codexUpstream.enable && codexRoutes != { }
    then
      cfg.registry
      // {
        contexts =
          lib.mapAttrs (
            name: context:
              if lib.elem name codexContextNames
              then context // { routes = codexRoutes // (context.routes or { }); }
              else context
          ) (cfg.registry.contexts or { });
      }
    else cfg.registry;

  registryFile = jsonFormat.generate "patchbay-routes.json" registry;

  proxyConfig = yamlFormat.generate "cliproxyapi-config.yaml" {
    host = "127.0.0.1";
    port = cfg.codexUpstream.port;
    auth-dir = cfg.codexUpstream.authDir;
    api-keys = [ cfg.codexUpstream.listenerKey ];
    # Empty secret-key disables the management API and its control panel
    # (which otherwise auto-downloads a web UI from GitHub at runtime).
    remote-management = {
      allow-remote = false;
      secret-key = "";
      disable-control-panel = true;
    };
  };

  unitEnvironment =
    {
      PATCHBAY_LISTEN = cfg.listen;
      # Pinned to the same path the xdg.configFile below produces, so the unit
      # and an interactive `patchbay check` read one file by construction.
      PATCHBAY_REGISTRY = registryPath;
      PATCHBAY_CALLER_KEY_FILE = cfg.callerKeyFile;
    }
    // lib.optionalAttrs (cfg.ledgerDir != null) {
      PATCHBAY_LEDGER_DIR = cfg.ledgerDir;
    }
    // lib.optionalAttrs (cfg.path != [ ]) {
      PATH = lib.makeBinPath cfg.path;
    }
    // lib.optionalAttrs cfg.codexUpstream.enable {
      ${chatgptKeyVar} = "${chatgptKeyFile}";
    }
    // cfg.environment;
in {
  options.services.patchbay = {
    enable = lib.mkEnableOption "the patchbay Anthropic Messages API gateway";

    package = lib.mkOption {
      type = lib.types.package;
      default = flakePkgs.patchbay;
      defaultText = lib.literalExpression "patchbay.packages.\${system}.patchbay";
      description = "The patchbay package to run.";
    };

    listen = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:4100";
      description = ''
        Address the gateway binds, and the address the CLI subcommands dial.
        Keep it loopback: the forward path spends the caller's own Anthropic
        credentials, so anything that can reach this socket can spend them.
      '';
    };

    registry = lib.mkOption {
      type = jsonFormat.type;
      default = { contexts.personal.routes = { }; };
      example = lib.literalExpression ''
        {
          contexts.personal.routes."openrouter/kimi-k2" = {
            auth = "inject";
            base_url = "https://openrouter.ai/api";
            model = "moonshotai/kimi-k2";
            api_key_env_file = "PATCHBAY_OPENROUTER_KEY_FILE";
            strip_cache_control = true;
          };
        }
      '';
      description = ''
        The registry document, generated to JSON in the store and linked at
        `~/.config/patchbay/routes.json`. It holds no secrets — only the names
        of environment variables whose values are key-file paths (declare
        those under {option}`services.patchbay.environment`). The default is
        one empty `personal` context: a pure Anthropic passthrough on the
        caller's own credentials.
      '';
    };

    callerKeyFile = lib.mkOption {
      type = lib.types.str;
      default = "${config.xdg.configHome}/patchbay/caller.key";
      defaultText = lib.literalExpression ''"''${config.xdg.configHome}/patchbay/caller.key"'';
      description = ''
        Path of the file holding the caller key — the local shared secret that
        gates the routes which spend your provider keys. A path, not a
        {type}`path`: the file must never enter the store.
      '';
    };

    generateCallerKey = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Generate {option}`callerKeyFile` on activation when it does not exist
        yet: 32 random bytes, base64, mode 0600. Disable if agenix, sops-nix
        or your own hands place the file instead.
      '';
    };

    environment = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      example = lib.literalExpression ''
        { PATCHBAY_OPENROUTER_KEY_FILE = "/run/agenix/openrouter-key"; }
      '';
      description = ''
        Extra environment for the gateway unit — typically the variables the
        registry's `api_key_env_file` and `model_map_env_file` fields name,
        each valued with the path of a key or map file outside the store.
      '';
    };

    path = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [ ];
      example = lib.literalExpression "[ pkgs.awscli2 ]";
      description = ''
        Packages on the gateway unit's PATH. `sigv4` routes exec
        `aws configure export-credentials`, so a registry using them needs
        `awscli2` here; nothing else about patchbay needs a PATH at all.
      '';
    };

    ledgerDir = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = ''
        Where JSONL ledger records are written. Null uses patchbay's own
        default, `~/.local/state/patchbay/ledger`.
      '';
    };

    codexUpstream = {
      enable = lib.mkEnableOption ''
        the ChatGPT-subscription upstream: runs CLIProxyAPI on loopback,
        translating Anthropic Messages to the Codex OAuth backend, and merges
        `inject` routes at it into the registry. Read
        docs/chatgpt-subscription.md first — reaching a ChatGPT subscription
        this way is at best a gray area of OpenAI's Terms of Use, and enabling
        this option is accepting that risk for the logged-in account. Enable
        only on hosts that hold Codex OAuth state (run
        `cli-proxy-api --config ~/.config/cliproxyapi/config.yaml --codex-login`
        once per machine)
      '';

      package = lib.mkOption {
        type = lib.types.package;
        default = flakePkgs.cliproxyapi;
        defaultText = lib.literalExpression "patchbay.packages.\${system}.cliproxyapi";
        description = "The CLIProxyAPI package to run.";
      };

      port = lib.mkOption {
        type = lib.types.port;
        default = 8317;
        description = "Loopback port CLIProxyAPI listens on.";
      };

      routes = lib.mkOption {
        type = lib.types.attrsOf lib.types.str;
        default = { };
        example = lib.literalExpression ''
          {
            "chatgpt/sol" = "gpt-5.6-sol";
            "chatgpt/luna" = "gpt-5.6-luna";
          }
        '';
        description = ''
          Alias → upstream model id. Each entry becomes an `inject` route at
          the proxy in every context {option}`codexUpstream.contexts` selects.
          List the subscription's current ids with
          `curl -H 'Authorization: Bearer <listenerKey>' http://127.0.0.1:8317/v1/models`.
        '';
      };

      contexts = lib.mkOption {
        type = lib.types.nullOr (lib.types.listOf lib.types.str);
        default = null;
        example = lib.literalExpression ''[ "personal" ]'';
        description = ''
          Which contexts of {option}`services.patchbay.registry` publish the
          chatgpt routes. Null means all of them.
        '';
      };

      maxInputTokens = lib.mkOption {
        type = lib.types.int;
        default = 372000;
        description = ''
          `max_input_tokens` on every generated route: the subscription's
          enforced context window, not the model's headline one.
        '';
      };

      listenerKey = lib.mkOption {
        type = lib.types.str;
        default = "patchbay-local";
        description = ''
          The CLIProxyAPI listener key patchbay injects. It sits
          world-readable in the store and only gates the loopback listener —
          loopback is reachable by host-network containers on the same
          machine, so treat it as a label, not a secret. The actual credential
          is the Codex OAuth state in {option}`codexUpstream.authDir`, which
          exists only on hosts where the login was run; that per-host gating
          is the real containment.
        '';
      };

      authDir = lib.mkOption {
        type = lib.types.str;
        default = "~/.cli-proxy-api";
        description = ''
          Where CLIProxyAPI keeps its OAuth state (mutable, like `~/.codex`).
          The literal `~` is expanded by the proxy itself.
        '';
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = pkgs.stdenv.hostPlatform.isLinux;
        message = "services.patchbay manages systemd user services and supports Linux only; on other platforms use the packages directly.";
      }
      {
        assertion =
          cfg.codexUpstream.contexts == null
          || lib.all (name: lib.hasAttr name (cfg.registry.contexts or { })) cfg.codexUpstream.contexts;
        message = "services.patchbay.codexUpstream.contexts names a context that services.patchbay.registry does not define.";
      }
    ];

    home.packages =
      [ cfg.package ]
      # cliproxyapi is also the one-time login CLI (`--codex-login`, or
      # `--codex-device-login` on headless hosts), so it belongs on PATH
      # wherever the upstream runs.
      ++ lib.optional cfg.codexUpstream.enable cfg.codexUpstream.package;

    # So the CLI subcommands work in an interactive shell without repeating
    # themselves: they dial $PATCHBAY_LISTEN and present the caller key.
    home.sessionVariables = {
      PATCHBAY_LISTEN = cfg.listen;
      PATCHBAY_CALLER_KEY_FILE = cfg.callerKeyFile;
    };

    home.activation.patchbayCallerKey = lib.mkIf cfg.generateCallerKey (
      lib.hm.dag.entryAfter [ "writeBoundary" ] ''
        if [ ! -e "${cfg.callerKeyFile}" ]; then
          run mkdir -p "$(dirname "${cfg.callerKeyFile}")"
          run sh -c '(umask 077; head -c 32 /dev/urandom | base64 > "${cfg.callerKeyFile}")'
        fi
      ''
    );

    xdg.configFile."patchbay/routes.json".source = registryFile;

    xdg.configFile."cliproxyapi/config.yaml" = lib.mkIf cfg.codexUpstream.enable {
      source = proxyConfig;
    };

    systemd.user.services.patchbay = {
      Unit = {
        Description = "patchbay — per-host Anthropic Messages API gateway";
        After = [ "network-online.target" ];
        Wants = [ "network-online.target" ];
      };
      Service = {
        ExecStart = "${lib.getExe cfg.package} serve";
        Restart = "on-failure";
        RestartSec = 5;
        Environment = lib.mapAttrsToList (name: value: "${name}=${value}") unitEnvironment;
      };
      Install.WantedBy = [ "default.target" ];
    };

    systemd.user.services.cli-proxy-api = lib.mkIf cfg.codexUpstream.enable {
      Unit = {
        Description = "CLIProxyAPI — Anthropic-compatible endpoint over the ChatGPT Codex subscription";
        After = [ "network-online.target" ];
        Wants = [ "network-online.target" ];
      };
      Service = {
        ExecStart = "${lib.getExe cfg.codexUpstream.package} --config ${config.xdg.configHome}/cliproxyapi/config.yaml";
        Restart = "on-failure";
        RestartSec = 5;
      };
      Install.WantedBy = [ "default.target" ];
    };
  };
}
