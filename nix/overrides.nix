# Fixups layered on top of the uv2nix-generated package set.
#
# Keep this as close to empty as possible: every entry here is a place where
# uv.lock stopped being enough. Anything that merely needs a PEP 517 backend is
# pyproject-build-systems' job, not this file's.
#
# It is currently empty, and that is the whole point of the uv2nix switch: with
# sourcePreference = "wheel", the mcp 2.x chain that nixpkgs cannot provide
# (mcp 2.2.0, mcp-types 2.2.0, httpx2 2.13.0, httpcore2 2.13.0) arrives as
# py3-none-any wheels needing no build backend at all, and the two workspace
# members build with the hatchling that pyproject-build-systems supplies.
#
# When something does need patching, the shape is:
#
#   final: prev: {
#     some-package = prev.some-package.overrideAttrs (old: {
#       nativeBuildInputs =
#         (old.nativeBuildInputs or [ ]) ++ final.resolveBuildSystem { setuptools = [ ]; };
#     });
#   }
#
# and for a package whose wheel misbehaves, override sourcePreference for just
# that one by giving it the sdist from the lock instead.
_final: _prev: { }
