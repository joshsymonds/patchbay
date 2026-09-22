# Evaluation coverage for `services.patchbay`: the option types it declares, and
# the outputs it actually generates once it is switched on.
#
# Two things are being caught here.
#
# The types: the registry is the authority on what a Seat may hold, and it
# refuses a negative token ceiling outright. An option type that accepts one only
# moves the failure — the generated registry lands in the store, the gateway
# refuses to load it, and the first thing anyone hears about it is a unit that
# will not start.
#
# The wiring: everything the module writes sits under `lib.mkIf cfg.enable`, so a
# disabled evaluation forces none of it and a check that only reads option
# defaults could pass against a module that generates nothing at all. The
# enabled evaluation below forces that branch and reads back what it produced —
# the registry document, the two units, the CLIProxyAPI config, and the
# assertions that guard them.
#
# The applications themselves are stubbed. The module needs a store path and a
# main program from each; building the real gateway to read a unit file would
# make this check an application build.
#
# Wired into the flake as `checks.<system>.hm-module-options` and run by
# `just check`.
self: {
  lib,
  pkgs,
}: let
  # The little of the home-manager option surface this module writes into.
  #
  # `xdg.configFile` and `systemd.user.services` are `attrsOf` rather than plain
  # `attrs` because the module puts a `lib.mkIf` on individual entries of both.
  # Only an `attrsOf` discharges those per-entry: under a plain `attrs` the
  # conditional would survive into the value as an undischarged `mkIf`, and a
  # file the module means to omit would read as present.
  hostOptions = { ... }: {
    options = {
      xdg.configHome = lib.mkOption {
        type = lib.types.str;
        default = "/home/test/.config";
      };
      xdg.configFile = lib.mkOption {
        type = lib.types.attrsOf lib.types.attrs;
        default = { };
      };
      home = lib.mkOption {
        type = lib.types.attrs;
        default = { };
      };
      systemd.user.services = lib.mkOption {
        type = lib.types.attrsOf lib.types.attrs;
        default = { };
      };
      assertions = lib.mkOption {
        type = lib.types.listOf lib.types.attrs;
        default = [ ];
      };
    };
  };

  # evaluate returns the whole evaluated configuration for one settings attrset.
  # Nothing here is forced until a case reads it, which is what keeps the
  # disabled cases below from reaching the package defaults — those point back
  # into this same flake.
  evaluate = settings:
    (lib.evalModules {
      modules = [
        (import ./hm-module.nix self)
        hostOptions
        {
          _module.args.pkgs = pkgs;
          services.patchbay = settings;
        }
      ];
    }).config;

  maxInputTokens = settings: (evaluate settings).services.patchbay.codexUpstream.maxInputTokens;

  accepts = value: (builtins.tryEval (maxInputTokens { codexUpstream.maxInputTokens = value; })).success;

  # Stand-ins for the two applications. writeShellScriptBin names its own main
  # program, which is all `lib.getExe` asks of a package.
  stubPatchbay = pkgs.writeShellScriptBin "patchbay" "exit 0";
  stubProxy = pkgs.writeShellScriptBin "cli-proxy-api" "exit 0";
  stubOnPath = pkgs.writeShellScriptBin "aws" "exit 0";

  # The Seat IDs `codexUpstream.bindings` generates, written out rather than
  # recomputed from the module's own formula. A test that recomputes it agrees
  # with whatever the formula happens to be; these two literals are what make the
  # IDs stable across a change to it.
  solSeat = "codex-8b7acfbf63aa881539d60395e8509581c7de8efed6a5d3a95d2ca95d961a605f";
  lunaSeat = "codex-f702343384b30dddbaf2f8379abf3cf4bd38041c38799890369ee26e9b08215e";

  # The ID a formula keeping only the first 16 digest characters would generate
  # for the same selector. Nothing may collide with it, because nothing
  # generates it.
  truncatedSolSeat = "codex-8b7acfbf63aa8815";

  # The listener key file the module writes, rebuilt from the same name and the
  # same contents so the unit's value is pinned to an exact store path rather
  # than to "something under /nix/store".
  chatgptKeyFile = pkgs.writeText "patchbay-chatgpt-key" "patchbay-local";

  registryHome = "/home/test/.config/patchbay";

  baseRegistry = {
    default_context = "personal";
    seats.anthropic = {
      upstream = "https://api.anthropic.com";
      auth_mode = "forward";
    };
    contexts = {
      # "chatgpt/luna" is a selector codexUpstream also generates a Seat for.
      # The hand-written binding is documented to win: the reviewed document
      # beats the generated convenience.
      personal = {
        default_seat = "anthropic";
        models."chatgpt/luna" = "anthropic";
      };
      # Not named in codexUpstream.contexts, so no generated binding may reach
      # it.
      work = {
        default_seat = "anthropic";
        models = { };
      };
    };
  };

  enabledSettings = {
    enable = true;
    package = stubPatchbay;
    # The activation entry reaches for home-manager's own lib.hm.dag, which this
    # evaluation does not carry. Nothing here inspects activation, so the
    # generator is switched off rather than stubbed.
    generateCallerKey = false;
    listen = "127.0.0.1:4199";
    ledgerDir = "/home/test/.local/state/patchbay/ledger";
    path = [ stubOnPath ];
    environment.PATCHBAY_OPENROUTER_KEY_FILE = "/run/agenix/openrouter-key";
    registry = baseRegistry;
    codexUpstream = {
      enable = true;
      package = stubProxy;
      port = 8317;
      listenerKey = "patchbay-local";
      bindings = {
        "chatgpt/sol" = "gpt-6-sol";
        "chatgpt/luna" = "gpt-6-luna";
      };
      contexts = [ "personal" ];
    };
  };

  enabled = evaluate enabledSettings;

  # The same host with the ChatGPT upstream switched off, bindings and all still
  # declared: everything it generates has to disappear with it.
  withoutCodex = evaluate (enabledSettings
    // {
      codexUpstream = enabledSettings.codexUpstream // { enable = false; };
    });

  # A host whose own registry already holds a Seat under one hand-written ID.
  # The module refuses a collision with a generated Seat ID rather than
  # silently overwriting one with the other, so which way the assertion goes is
  # also an exact-equality probe on the IDs the bindings really generate.
  collidingWith = seatID:
    evaluate (enabledSettings
      // {
        registry = baseRegistry
          // {
            seats = baseRegistry.seats // {
              ${seatID} = {
                upstream = "https://api.anthropic.com";
                auth_mode = "forward";
              };
            };
          };
      });

  colliding = collidingWith solSeat;

  # The same host with one generated binding the registry will not take.
  #
  # Both sides of a binding are held to the same prohibition, because both reach
  # the registry as parts of an inject Seat: the value becomes the Seat's model
  # rewrite, and the key becomes the public selector bound to it. Patchbay
  # refuses either naming Claude — case-insensitively, anywhere in the string —
  # so evaluation has to refuse them too, or the first anyone hears about it is
  # a gateway that will not load its own generated registry.
  oneBinding = selector: target:
    evaluate (enabledSettings
      // {
        codexUpstream = enabledSettings.codexUpstream // {
          bindings = { ${selector} = target; };
        };
      });

  claudeTarget = target: oneBinding "chatgpt/sol" target;
  claudeSelector = selector: oneBinding selector "gpt-6-sol";

  # The selector half is about the document that comes out, not the option that
  # went in. A hand-written binding of the same selector wins, so a generated
  # selector overridden in every Context it would have been published in never
  # binds the inject Seat and is nothing for the assertion to refuse.
  #
  # claudeSelectorPublishedIn evaluates that: one Claude-named generated
  # binding, the Contexts it is published in, and the hand-written models each
  # Context declares of its own.
  bridgeSelector = "chatgpt/claude-bridge";

  claudeSelectorPublishedIn = contexts: overrides:
    evaluate (enabledSettings
      // {
        registry = baseRegistry
          // {
            contexts =
              lib.mapAttrs (
                name: context:
                  context // { models = (context.models or { }) // (overrides.${name} or { }); }
              ) baseRegistry.contexts;
          };
        codexUpstream = enabledSettings.codexUpstream // {
          bindings = { ${bridgeSelector} = "gpt-6-sol"; };
          inherit contexts;
        };
      });

  handWritten = { ${bridgeSelector} = "anthropic"; };

  # One assertion out of config.assertions, found by a stable fragment of its
  # message rather than by position in the list. A fragment that matches nothing
  # yields null, which no case accepts.
  assertionFor = conf: fragment: let
    match = lib.findFirst (entry: lib.hasInfix fragment entry.message) null conf.assertions;
  in
    if match == null
    then null
    else match.assertion;

  platformFragment = "supports Linux only";
  targetFragment = "targets a model naming Claude";
  selectorFragment = "publishes a selector naming Claude";
  emptySelectorFragment = "declares an empty generated selector";

  patchbayUnit = enabled.systemd.user.services.patchbay;
  patchbayEnvironment = patchbayUnit.Service.Environment;
  withoutCodexEnvironment =
    withoutCodex.systemd.user.services.patchbay.Service.Environment;

  cases = [
    {
      name = "default is the subscription's enforced window";
      ok = maxInputTokens { } == 372000;
    }
    {
      name = "a positive ceiling is kept verbatim";
      ok = maxInputTokens { codexUpstream.maxInputTokens = 262144; } == 262144;
    }
    {
      name = "zero — no ceiling — is accepted";
      ok = accepts 0;
    }
    {
      name = "a negative ceiling is rejected";
      ok = !(accepts (-1));
    }
    {
      name = "a large negative ceiling is rejected";
      ok = !(accepts (-372000));
    }

    # The registry the module generates and links into place. Its contents are
    # read from the realized file by the build script below; what is checked here
    # is that the link exists and points at a generated store path at all.
    {
      name = "an enabled host links a generated registry at the gateway's own path";
      ok =
        (enabled.xdg.configFile ? "patchbay/routes.json")
        && lib.isDerivation enabled.xdg.configFile."patchbay/routes.json".source;
    }

    # The gateway unit.
    {
      name = "the gateway unit runs the configured package's serve subcommand";
      ok = patchbayUnit.Service.ExecStart == "${stubPatchbay}/bin/patchbay serve";
    }
    {
      name = "the gateway unit is wanted by the user session";
      ok = patchbayUnit.Install.WantedBy == [ "default.target" ];
    }
    {
      name = "the gateway unit carries the listener, the registry and the caller key";
      ok = lib.all (entry: lib.elem entry patchbayEnvironment) [
        "PATCHBAY_LISTEN=127.0.0.1:4199"
        "PATCHBAY_REGISTRY=${registryHome}/routes.json"
        "PATCHBAY_CALLER_KEY_FILE=${registryHome}/caller.key"
      ];
    }
    {
      name = "the gateway unit carries the optional ledger, PATH and extra environment";
      ok = lib.all (entry: lib.elem entry patchbayEnvironment) [
        "PATCHBAY_LEDGER_DIR=/home/test/.local/state/patchbay/ledger"
        "PATH=${lib.makeBinPath [ stubOnPath ]}"
        "PATCHBAY_OPENROUTER_KEY_FILE=/run/agenix/openrouter-key"
      ];
    }
    {
      name = "the gateway unit names the generated ChatGPT key file";
      ok = lib.elem "PATCHBAY_CHATGPT_KEY_FILE=${chatgptKeyFile}" patchbayEnvironment;
    }
    {
      name = "the ChatGPT key file is named nowhere with the upstream off";
      ok = !lib.any (lib.hasPrefix "PATCHBAY_CHATGPT_KEY_FILE=") withoutCodexEnvironment;
    }

    # The CLIProxyAPI half, which exists only when it is opted into.
    {
      name = "the CLIProxyAPI unit runs the configured package against the generated config";
      ok =
        enabled.systemd.user.services.cli-proxy-api.Service.ExecStart
        == "${stubProxy}/bin/cli-proxy-api --config /home/test/.config/cliproxyapi/config.yaml";
    }
    {
      name = "the CLIProxyAPI config is generated when the upstream is enabled";
      ok =
        (enabled.xdg.configFile ? "cliproxyapi/config.yaml")
        && lib.isDerivation enabled.xdg.configFile."cliproxyapi/config.yaml".source;
    }
    {
      name = "no CLIProxyAPI config or unit exists with the upstream off";
      ok =
        !(withoutCodex.xdg.configFile ? "cliproxyapi/config.yaml")
        && !(withoutCodex.systemd.user.services ? cli-proxy-api);
    }

    # The generated Seat IDs themselves. An ID is the whole SHA-256 digest of
    # its selector, not a prefix of one: a prefix is just as deterministic, but
    # two selectors sharing one collapse into a single Seat, and the second one
    # generated under that ID silently replaces the first. Both halves of the
    # shape are pinned here, so shortening the digest again fails on this file
    # rather than in a registry where two selectors quietly became one Seat.
    {
      name = "a generated Seat ID is codex- plus the whole sha256 digest of its selector";
      ok =
        solSeat == "codex-${builtins.hashString "sha256" "chatgpt/sol"}"
        && lunaSeat == "codex-${builtins.hashString "sha256" "chatgpt/luna"}"
        && builtins.stringLength solSeat == 70
        && builtins.stringLength lunaSeat == 70;
    }
    {
      name = "a truncated prefix of the digest is no longer a generated Seat ID";
      ok = assertionFor (collidingWith truncatedSolSeat) "generates a Seat ID already present" == true;
    }

    # The assertions that guard the generated half.
    {
      name = "the platform assertion tracks the host platform";
      ok = assertionFor enabled platformFragment == pkgs.stdenv.hostPlatform.isLinux;
    }
    {
      name = "a valid enabled configuration raises no configuration assertion";
      ok = lib.all (entry: entry.assertion) (
        lib.filter (entry: !lib.hasInfix platformFragment entry.message) enabled.assertions
      );
    }
    {
      name = "a user Seat colliding with a generated Seat ID fails its assertion";
      ok = assertionFor colliding "generates a Seat ID already present" == false;
    }
    {
      name = "a Context the registry does define passes the scoping assertion";
      ok = assertionFor enabled "names a context that" == true;
    }
    {
      name = "a lowercase claude target fails its assertion";
      ok = assertionFor (claudeTarget "claude-opus-5") targetFragment == false;
    }
    {
      name = "a mixed-case Claude target fails its assertion";
      ok = assertionFor (claudeTarget "Claude-Opus-5") targetFragment == false;
    }
    {
      name = "a target with claude embedded in it fails its assertion";
      ok = assertionFor (claudeTarget "gpt-6-CLAUDE-bridge") targetFragment == false;
    }
    {
      name = "the configured GPT targets pass the target assertion";
      ok = assertionFor enabled targetFragment == true;
    }
    {
      name = "a lowercase claude selector fails its assertion";
      ok = assertionFor (claudeSelector "claude/opus") selectorFragment == false;
    }
    {
      name = "a mixed-case Claude selector fails its assertion";
      ok = assertionFor (claudeSelector "chatgpt/Claude") selectorFragment == false;
    }
    {
      name = "a selector with claude embedded in it fails its assertion";
      ok = assertionFor (claudeSelector "team-CLAUDE-fast") selectorFragment == false;
    }
    {
      name = "the configured GPT selectors pass the selector assertion";
      ok = assertionFor enabled selectorFragment == true;
    }
    # The other thing a generated selector may not be. An empty one is not a
    # name anything can ask for, and patchbay refuses it as a Context's models
    # key outright — so it is refused here, on the declaration, rather than by a
    # gateway that will not load its own generated registry.
    {
      name = "an empty generated selector fails its assertion";
      ok = assertionFor (oneBinding "" "gpt-6-sol") emptySelectorFragment == false;
    }
    {
      name = "an ordinary generated selector passes the empty-selector assertion";
      ok = assertionFor (oneBinding "chatgpt/sol" "gpt-6-sol") emptySelectorFragment == true;
    }
    {
      name = "the configured selectors pass the empty-selector assertion";
      ok = assertionFor enabled emptySelectorFragment == true;
    }
    {
      name = "a claude target leaves the selector assertion alone";
      ok = assertionFor (claudeTarget "claude-opus-5") selectorFragment == true;
    }
    {
      name = "a claude selector leaves the target assertion alone";
      ok = assertionFor (claudeSelector "claude/opus") targetFragment == true;
    }

    # The selector rule reads the merged document, so a hand-written override
    # decides whether there is a generated inject binding left to refuse.
    {
      name = "a claude selector published in the one selected Context fails";
      ok = assertionFor (claudeSelectorPublishedIn [ "personal" ] { }) selectorFragment == false;
    }
    {
      name = "a claude selector overridden in the one selected Context passes";
      ok =
        assertionFor
        (claudeSelectorPublishedIn [ "personal" ] { personal = handWritten; })
        selectorFragment
        == true;
    }
    {
      name = "a claude selector overridden in every selected Context passes";
      ok =
        assertionFor
        (claudeSelectorPublishedIn [ "personal" "work" ] {
          personal = handWritten;
          work = handWritten;
        })
        selectorFragment
        == true;
    }
    {
      name = "a claude selector overridden in only some selected Contexts still fails";
      ok =
        assertionFor
        (claudeSelectorPublishedIn [ "personal" "work" ] { personal = handWritten; })
        selectorFragment
        == false;
    }
    {
      name = "an overridden claude selector still generates its Seat, and passes the target assertion";
      ok =
        assertionFor
        (claudeSelectorPublishedIn [ "personal" ] { personal = handWritten; })
        targetFragment
        == true;
    }
  ];

  failed = builtins.filter (case: !case.ok) cases;
in
  if failed != [ ]
  then
    throw "services.patchbay module cases failed: ${
      lib.concatMapStringsSep ", " (case: case.name) failed
    }"
  else
    # The generated documents themselves. They are derivations, so they are read
    # here — realized as build inputs — rather than with builtins.readFile on
    # something that has not been built.
    pkgs.runCommand "patchbay-hm-module-options-test" {
      nativeBuildInputs = [ pkgs.jq pkgs.yq-go ];
      registryJSON = enabled.xdg.configFile."patchbay/routes.json".source;
      registryWithoutCodex = withoutCodex.xdg.configFile."patchbay/routes.json".source;
      proxyYAML = enabled.xdg.configFile."cliproxyapi/config.yaml".source;
      solSeatID = solSeat;
      lunaSeatID = lunaSeat;
      caseCount = toString (builtins.length cases);
    } ''
      set -euo pipefail

      # must <description> <file> <jq arguments...>
      must() {
        local what="$1" file="$2"
        shift 2
        if ! jq -e "$@" "$file" > /dev/null; then
          echo "services.patchbay generated document is wrong: $what" >&2
          cat "$file" >&2
          exit 1
        fi
      }

      must "the declared document survives generation" "$registryJSON" \
        '.default_context == "personal"
         and .seats.anthropic.auth_mode == "forward"
         and .seats.anthropic.upstream == "https://api.anthropic.com"
         and .contexts.personal.default_seat == "anthropic"'

      must "each binding generates a global inject Seat with the upstream's own fields" \
        "$registryJSON" --arg sol "$solSeatID" --arg luna "$lunaSeatID" \
        '(.seats[$sol] | .auth_mode == "inject"
          and .billing == "subscription"
          and .upstream == "http://127.0.0.1:8317"
          and .api_key_env_file == "PATCHBAY_CHATGPT_KEY_FILE"
          and .model == "gpt-6-sol"
          and .max_input_tokens == 372000)
         and (.seats[$luna] | .model == "gpt-6-luna"
           and .auth_mode == "inject"
           and .billing == "subscription")'

      must "a generated binding is published in the selected Context" \
        "$registryJSON" --arg sol "$solSeatID" \
        '.contexts.personal.models["chatgpt/sol"] == $sol'

      must "a hand-written binding wins over the generated one of the same selector" \
        "$registryJSON" \
        '.contexts.personal.models["chatgpt/luna"] == "anthropic"'

      must "an unselected Context publishes no generated binding" "$registryJSON" \
        '.contexts.work.models == {}'

      must "nothing is generated with the upstream off" \
        "$registryWithoutCodex" --arg sol "$solSeatID" --arg luna "$lunaSeatID" \
        '(.seats | has($sol) or has($luna) | not)
         and (.contexts.personal.models == {"chatgpt/luna": "anthropic"})'

      yq -o=json "." "$proxyYAML" > proxy.json

      must "the CLIProxyAPI config binds the loopback port patchbay's Seats target" \
        proxy.json '.host == "127.0.0.1" and .port == 8317'

      must "the CLIProxyAPI config admits the listener key patchbay injects" \
        proxy.json '.["api-keys"] == ["patchbay-local"]'

      must "the CLIProxyAPI config names the OAuth state directory" \
        proxy.json '.["auth-dir"] == "~/.cli-proxy-api"'

      must "the CLIProxyAPI management API and its control panel stay off" \
        proxy.json \
        '.["remote-management"] | .["allow-remote"] == false
         and .["secret-key"] == ""
         and .["disable-control-panel"] == true'

      echo "services.patchbay: $caseCount evaluation cases and the generated registry, unit and CLIProxyAPI documents all checked" > "$out"
    ''
