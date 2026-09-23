# The web console (hub/web), built with Vite via Nix's own npm support
# (buildNpmPackage) rather than trusting a committed dist/ directory. hub.nix
# feeds this derivation's output to the Go build, which embeds it.
{
  lib,
  buildNpmPackage,
  version ? "0.3.0",
}:

buildNpmPackage {
  pname = "mcp-switchboard-console";
  inherit version;

  # Only what the Vite build reads: source, config and the npm lockfile, not
  # e2e/ (Playwright, needs a real browser and a running hub), test-results/,
  # or the tsbuildinfo caches.
  src = lib.fileset.toSource {
    root = ../hub/web;
    fileset = lib.fileset.unions [
      ../hub/web/package.json
      ../hub/web/package-lock.json
      ../hub/web/index.html
      ../hub/web/vite.config.ts
      ../hub/web/tsconfig.json
      ../hub/web/tsconfig.app.json
      ../hub/web/tsconfig.node.json
      ../hub/web/postcss.config.js
      ../hub/web/tailwind.config.js
      ../hub/web/public
      ../hub/web/src
    ];
  };

  # `npm ci` is run against this instead of hitting the network; bump it
  # whenever package-lock.json changes (see AGENTS.md's pre-commit checks -
  # the same "recompute with lib.fakeHash, rebuild, paste the real hash in"
  # dance as hub.nix's vendorHash).
  npmDepsHash = "sha256-Sm9/SXSwo0vViIQOVA1lmEUnxcUkt5QczvDjfElbuKg=";

  npmBuildScript = "build";

  installPhase = ''
    runHook preInstall
    mkdir -p "$out"
    cp -r dist/. "$out"/
    runHook postInstall
  '';

  meta = {
    description = "mcp-switchboard's web console, built for the hub to embed";
    license = lib.licenses.mit;
  };
}
