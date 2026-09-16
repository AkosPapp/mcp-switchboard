{
  description = "mcp-switchboard - aggregate NATed MCP servers behind one hub";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";

    pyproject-nix = {
      url = "github:pyproject-nix/pyproject.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    uv2nix = {
      url = "github:pyproject-nix/uv2nix";
      inputs.pyproject-nix.follows = "pyproject-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    pyproject-build-systems = {
      url = "github:pyproject-nix/build-system-pkgs";
      inputs.pyproject-nix.follows = "pyproject-nix";
      inputs.uv2nix.follows = "uv2nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      pyproject-nix,
      uv2nix,
      pyproject-build-systems,
    }:
    let
      # The whole Python side is derived from the root uv.lock. See
      # nix/pythonset.nix for what that involves.
      mkSwitchboard =
        pkgs:
        import ./nix/pythonset.nix {
          inherit
            pkgs
            pyproject-nix
            uv2nix
            pyproject-build-systems
            ;
          workspaceRoot = ./.;
        };
    in
    {
      # Usable from someone else's nixpkgs: adds mcp-switchboard-hub and
      # mcp-switchboard-client as top-level packages, rebuilt against *their*
      # pkgs rather than handing over our own store paths. This is what makes
      # nixosModules.default work with a bare `nixpkgs.overlays = [ ... ]`.
      overlays.default =
        final: _prev:
        let
          switchboard = mkSwitchboard final;
        in
        {
          mcp-switchboard-hub = switchboard.hub;
          mcp-switchboard-client = switchboard.client;
        };

      nixosModules.default = import ./nix/module.nix;
      nixosModules.mcp-switchboard = self.nixosModules.default;
    }
    // flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        inherit (pkgs) lib;

        switchboard = mkSwitchboard pkgs;
        inherit (switchboard) workspace pythonSet python;

        # Dev shell: the full workspace closure - both members plus every extra,
        # which is where pytest and pytest-asyncio come from - with hub/ and
        # client/ installed *editable*, so the venv points at the working tree
        # instead of a store copy.
        editableOverlay = workspace.mkEditablePyprojectOverlay {
          root = "$REPO_ROOT";
        };

        editablePythonSet = pythonSet.overrideScope (
          lib.composeManyExtensions [
            editableOverlay
            (
              final: prev:
              let
                # An editable install only needs the metadata and the package
                # dir; narrowing the source keeps the shell from rebuilding
                # every time an unrelated file in the repo changes.
                trim =
                  name: module:
                  prev.${name}.overrideAttrs (old: {
                    src = lib.fileset.toSource {
                      root = old.src;
                      fileset = lib.fileset.unions [
                        (old.src + "/pyproject.toml")
                        (old.src + "/src/${module}")
                        # hatchling reads `readme = "README.md"` at build time
                        # and hard-errors if the file is absent, so it has to be
                        # in the narrowed source - but only if it exists yet.
                        (lib.fileset.maybeMissing (old.src + "/README.md"))
                      ];
                    };
                    nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ final.resolveBuildSystem { editables = [ ]; };
                  });
              in
              {
                mcp-switchboard-hub = trim "mcp-switchboard-hub" "mcp_switchboard_hub";
                mcp-switchboard-client = trim "mcp-switchboard-client" "mcp_switchboard_client";
              }
            )
          ]
        );

        devEnv = editablePythonSet.mkVirtualEnv "mcp-switchboard-dev-env" workspace.deps.all;

        # The fake stdio MCP server used by the VM test is a real MCP server, so
        # it needs the SDK - but only the SDK, not the hub.
        mcpPython = pythonSet.mkVirtualEnv "mcp-sdk-env" { mcp = [ ]; };
      in
      {
        packages = {
          inherit (switchboard) hub client;
          default = switchboard.hub;
        };

        apps = {
          hub = {
            type = "app";
            program = "${switchboard.hub}/bin/mcp-switchboard-hub";
            meta.description = "Run the mcp-switchboard hub";
          };
          client = {
            type = "app";
            program = "${switchboard.client}/bin/mcp-switchboard-client";
            meta.description = "Run the mcp-switchboard tunnel client";
          };
          default = self.apps.${system}.hub;
        };

        checks = {
          inherit (switchboard) hub client;

          # Cheap guard against the two packages silently swapping identities,
          # and against the client growing an `mcp` dependency (it is meant to
          # speak no MCP at all - see docs/PROTOCOL.md).
          package-shape = pkgs.runCommand "mcp-switchboard-package-shape" { } ''
            test -x ${switchboard.hub}/bin/mcp-switchboard-hub
            test -x ${switchboard.client}/bin/mcp-switchboard-client

            # NB: stdenv sets `shopt -s nullglob`, so a non-matching glob
            # vanishes rather than being passed through literally - hence the
            # explicit loop instead of testing a glob directly.
            for sitePackages in ${switchboard.client}/lib/python*/site-packages; do
              if [ -e "$sitePackages/mcp" ]; then
                echo "mcp-switchboard-client must not depend on the mcp package" >&2
                exit 1
              fi
            done

            touch $out
          '';
        }
        // lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          # Two VMs, a real tunnel and a real tool call. Needs KVM; see
          # nix/vm-test.nix.
          vm = import ./nix/vm-test.nix {
            inherit pkgs mcpPython;
            module = self.nixosModules.default;
            hubPackage = switchboard.hub;
            clientPackage = switchboard.client;
            fakeMcpServerSource = ./tests/fake_mcp_server.py;
          };
        };

        devShells.default = pkgs.mkShell {
          packages = [
            devEnv
            # ruff is a standalone binary, not a Python import, so there is no
            # reason to route it through the lock file.
            pkgs.ruff
            pkgs.uv
            pkgs.nixfmt
          ];

          env = {
            # Never let uv try to manage or download an interpreter: the venv is
            # already built by Nix.
            UV_NO_SYNC = "1";
            UV_PYTHON = python.interpreter;
            UV_PYTHON_DOWNLOADS = "never";
          };

          shellHook = ''
            unset PYTHONPATH
            export REPO_ROOT=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
          '';
        };

        formatter = pkgs.nixfmt;
      }
    );
}
