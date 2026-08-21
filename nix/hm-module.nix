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
  # so patchbay's usual key-file indirection carries it like any other Seat key.
  # It lives world-readable in the store because it only gates a loopback
  # listener — see the option's description.
  chatgptKeyVar = "PATCHBAY_CHATGPT_KEY_FILE";
  chatgptKeyFile = pkgs.writeText "patchbay-chatgpt-key" cfg.codexUpstream.listenerKey;

  # A generated Seat's ID: the selector's whole SHA-256 digest, deterministic
  # across evaluations and carrying the digest's full collision resistance.
  #
  # No hash is injective — 256 bits cannot separate every possible selector — so
  # what the whole digest offers is strength, not impossibility. Keeping only the
  # first 16 hex characters discarded three quarters of that strength on purpose,
  # leaving 64 bits. Two hand-declared selectors colliding in 64 bits is unlikely;
  # it is also a risk taken for nothing, and the failure it admits is a silent
  # one. An attrset holds one value per key, so two selectors landing on one ID
  # become one Seat: the second generated under it replaces the first, and the
  # registry that reaches the gateway simply has a binding missing. Nothing
  # outside the generated document reads these IDs, so the truncation bought
  # nothing to set against that.
  codexSeatID = selector: "codex-${builtins.hashString "sha256" selector}";

  # The registry's own inject rule, spelled once for the two sides of a binding
  # it applies to. Case-insensitive and anywhere in the string, exactly as
  # patchbay reads it.
  namesNoClaude = text: !lib.hasInfix "claude" (lib.toLower text);

  codexSeat = model: {
    auth_mode = "inject";
    upstream = "http://127.0.0.1:${toString cfg.codexUpstream.port}";
    api_key_env_file = chatgptKeyVar;
    inherit model;
    max_input_tokens = cfg.codexUpstream.maxInputTokens;
  };

  codexSeats = lib.mapAttrs' (
    selector: model: lib.nameValuePair (codexSeatID selector) (codexSeat model)
  ) cfg.codexUpstream.bindings;

  codexBindings = lib.mapAttrs (selector: _: codexSeatID selector) cfg.codexUpstream.bindings;

  codexContextNames =
    if cfg.codexUpstream.contexts == null
    then lib.attrNames (cfg.registry.contexts or { })
    else cfg.codexUpstream.contexts;

  # The declared registry, with generated ChatGPT Seats added globally and
  # bindings merged into the chosen Contexts. A hand-written binding wins over a
  # generated one of the same selector: the reviewed document beats convenience.
  registry =
    if cfg.codexUpstream.enable && codexBindings != { }
    then
      cfg.registry
      // {
        seats = codexSeats // (cfg.registry.seats or { });
        contexts =
          lib.mapAttrs (
            name: context:
              if lib.elem name codexContextNames
              then context // { models = codexBindings // (context.models or { }); }
              else context
          ) (cfg.registry.contexts or { });
      }
    else cfg.registry;

  # The generated selectors that survive into the document above still bound to
  # their own generated Seat.
  #
  # A declared binding is not the same thing as a published one. The merge lets
  # a hand-written binding of the same selector win, and publishes nothing at
  # all in a Context the upstream does not name or the registry does not define
  # — so a selector can be declared here and bind no inject Seat anywhere. The
  # question is asked of the merged Contexts rather than of the option, because
  # the merged document is what patchbay will actually read.
  publishedCodexSelectors = lib.filter (
    selector:
      lib.any (
        name: ((registry.contexts.${name} or { }).models or { }).${selector} or null == codexSeatID selector
      ) codexContextNames
  ) (lib.attrNames cfg.codexUpstream.bindings);

  registryFile = jsonFormat.generate "patchbay-registry.json" registry;

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
      default = {
        default_context = "personal";
        seats.anthropic = {
          upstream = "https://api.anthropic.com";
          auth_mode = "forward";
        };
        contexts.personal = {
          default_seat = "anthropic";
          models = { };
        };
      };
      example = lib.literalExpression ''
        {
          default_context = "personal";
          seats = {
            anthropic = {
              upstream = "https://api.anthropic.com";
              auth_mode = "forward";
            };
            openrouter-kimi-k2 = {
              upstream = "https://openrouter.ai/api";
              model = "moonshotai/kimi-k2";
              auth_mode = "inject";
              api_key_env_file = "PATCHBAY_OPENROUTER_KEY_FILE";
              strip_cache_control = true;
            };
          };
          contexts.personal = {
            default_seat = "anthropic";
            models."openrouter/kimi-k2" = "openrouter-kimi-k2";
          };
        }
      '';
      description = ''
        The strict registry document, generated to JSON in the store and linked
        at `~/.config/patchbay/routes.json`. It holds no secrets — only the
        names of environment variables whose values are key-file paths (declare
        those under {option}`services.patchbay.environment`). The default has
        one global Anthropic Seat and one empty `personal` Context using it.
      '';
    };

    callerKeyFile = lib.mkOption {
      type = lib.types.str;
      default = "${config.xdg.configHome}/patchbay/caller.key";
      defaultText = lib.literalExpression ''"''${config.xdg.configHome}/patchbay/caller.key"'';
      description = ''
        Path of the file holding the caller key — the local shared secret that
        gates Seats which spend your provider keys. A path, not a
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
        Packages on the gateway unit's PATH. `sigv4` Seats exec
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
        translating Anthropic Messages to the Codex OAuth backend, and adds
        global `inject` Seats plus Context bindings to the registry. Read
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

      bindings = lib.mkOption {
        type = lib.types.attrsOf lib.types.str;
        default = { };
        example = lib.literalExpression ''
          {
            "chatgpt/sol" = "gpt-5.6-sol";
            "chatgpt/luna" = "gpt-5.6-luna";
          }
        '';
        description = ''
          Public selector → upstream model id. Each entry creates one global
          `inject` Seat and publishes a binding in every Context selected by
          {option}`codexUpstream.contexts`. List current subscription ids with
          `curl -H 'Authorization: Bearer <listenerKey>' http://127.0.0.1:8317/v1/models`.

          Every selector must be non-empty: it is published as a public model
          name, and patchbay refuses an empty selector key in a Context's
          bindings.

          No target may contain `claude`, in any case: every entry generates an
          `inject` Seat, and patchbay admits no inject model naming Claude. A
          selector naming Claude is refused where it is actually published —
          a hand-written Context binding of the same selector wins, so one
          overridden in every Context it would reach binds no inject Seat and
          is left alone.
        '';
      };

      contexts = lib.mkOption {
        type = lib.types.nullOr (lib.types.listOf lib.types.str);
        default = null;
        example = lib.literalExpression ''[ "personal" ]'';
        description = ''
          Which Contexts of {option}`services.patchbay.registry` publish the
          ChatGPT bindings. Null means all of them.
        '';
      };

      maxInputTokens = lib.mkOption {
        # Unsigned, because the registry refuses a negative ceiling: a signed
        # option would only move that refusal from evaluation to a unit that
        # will not start.
        type = lib.types.ints.unsigned;
        default = 372000;
        description = ''
          `max_input_tokens` on every generated Seat: the subscription's
          enforced context window, not the model's headline one. Zero means no
          ceiling.
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
      {
        assertion = !cfg.codexUpstream.enable || lib.all (seatID: !lib.hasAttr seatID (cfg.registry.seats or { })) (lib.attrNames codexSeats);
        message = "services.patchbay.codexUpstream.bindings generates a Seat ID already present in services.patchbay.registry.seats.";
      }
      # An empty selector is not a name anything can ask for. Unlike the Claude
      # rule below it is refused on the declaration rather than where it lands:
      # patchbay rejects an empty models key in any Context, so an empty
      # selector is invalid operator input whatever the Context scoping goes on
      # to publish it in.
      {
        assertion = !cfg.codexUpstream.enable || !lib.hasAttr "" cfg.codexUpstream.bindings;
        message = "services.patchbay.codexUpstream.bindings declares an empty generated selector (\"\"); a generated selector is published as a public model name, and patchbay refuses an empty selector key in a context's models.";
      }
      # Both sides of a generated binding, held to the one prohibition the
      # registry puts on an inject Seat.
      #
      # Everything these bindings generate is an `inject` Seat, and inject
      # strips the caller's own Anthropic credentials: a Seat that rewrites to a
      # Claude model, or a selector that publishes one, is a request for
      # Anthropic's own models spent on somebody else's key. Patchbay refuses
      # both, so evaluation refuses them here — on the file that wrote them,
      # rather than on a gateway that will not load its own generated registry.
      #
      # The two sides are asked different questions, because they reach the
      # document differently. Every declared target becomes a generated Seat's
      # model, whether or not anything binds that Seat. A selector only reaches
      # an inject Seat where the merge actually published it, so that half is
      # asked of the merged Contexts: a selector hand-written policy overrode
      # everywhere binds nothing and is no business of this assertion.
      #
      # This is about generated bindings only. A hand-written Seat or Context
      # binding is patchbay's to validate, and it does.
      {
        assertion =
          !cfg.codexUpstream.enable
          || lib.all namesNoClaude (lib.attrValues cfg.codexUpstream.bindings);
        message = "services.patchbay.codexUpstream.bindings targets a model naming Claude; the generated Seats are inject Seats, and patchbay refuses an inject model containing \"claude\" in any case.";
      }
      {
        assertion = !cfg.codexUpstream.enable || lib.all namesNoClaude publishedCodexSelectors;
        message = "services.patchbay.codexUpstream.bindings publishes a selector naming Claude; a published generated selector binds a generated inject Seat, and patchbay refuses a selector containing \"claude\" on an inject binding in any case. Overriding it with a hand-written binding in every Context it is published in also resolves this.";
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
