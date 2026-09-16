# Builds the uv2nix-derived Python package set for this workspace.
#
# Everything here comes out of the root uv.lock: uv resolves hub/ and client/
# together, uv2nix turns that lock into a Nix overlay, and pyproject-build-systems
# supplies the PEP 517 backends (hatchling, setuptools, ...) that uv does not
# lock. There are no hand-written derivations - in particular mcp 2.2.0,
# mcp-types 2.2.0, httpx2 2.13.0 and httpcore2 2.13.0, none of which exist at
# those versions in nixpkgs, are picked up straight from the lock.
#
# Factored out of flake.nix so that both `packages.*` and `overlays.default` can
# build it, the latter against the *consumer's* nixpkgs rather than ours.
{
  pkgs,
  lib ? pkgs.lib,
  pyproject-nix,
  uv2nix,
  pyproject-build-systems,
  workspaceRoot,
  # Both packages require >= 3.10, so one interpreter covers both.
  python ? pkgs.python313,
}:

let
  workspace = uv2nix.lib.workspace.loadWorkspace { inherit workspaceRoot; };

  # "wheel" keeps pydantic-core, httpx2 and friends as prebuilt wheels instead
  # of demanding a Rust toolchain or the uv-dynamic-versioning/hatch-fancy-pypi-readme
  # build backends that several of these sdists require. Flip an individual
  # package to sdist in ./overrides.nix if a wheel ever misbehaves.
  uvOverlay = workspace.mkPyprojectOverlay {
    sourcePreference = "wheel";
  };

  baseSet = pkgs.callPackage pyproject-nix.build.packages {
    inherit python;
  };

  pythonSet = baseSet.overrideScope (
    lib.composeManyExtensions [
      pyproject-build-systems.overlays.default
      uvOverlay
      (import ./overrides.nix)
    ]
  );
in
{
  inherit workspace pythonSet python;

  hub = pythonSet.mkVirtualEnv "mcp-switchboard-hub-env" {
    mcp-switchboard-hub = [ ];
  };

  client = pythonSet.mkVirtualEnv "mcp-switchboard-client-env" {
    mcp-switchboard-client = [ ];
  };
}
