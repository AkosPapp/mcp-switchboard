# The hub, built from hub/ with buildGoModule.
#
# No npm anywhere in this derivation: the console's build output is committed at
# hub/web/dist and embedded by hub/web/embed.go (spec.md U1), so a Nix build
# needs one toolchain rather than two, and an offline build works. CI rebuilds
# the console and fails if that committed copy is stale, which is what keeps the
# arrangement honest.
{
  lib,
  buildGoModule,
  version ? "0.3.0",
}:

buildGoModule (finalAttrs: {
  pname = "mcp-switchboard-hub";
  inherit version;

  # Only what the build reads. Narrowed with a fileset rather than by copying
  # hub/ wholesale, so an edit to a test fixture or to node_modules does not
  # invalidate the derivation.
  src = lib.fileset.toSource {
    root = ../hub;
    fileset = lib.fileset.unions [
      ../hub/go.mod
      ../hub/go.sum
      ../hub/cmd
      ../hub/internal
      ../hub/web/embed.go
      ../hub/web/dist
    ];
  };

  vendorHash = "sha256-1QB5V7cMvuYULcGA82lEcmSBJu1AcVrSjDYzU1j39Ic=";

  # A static binary, which is what lets the container be built FROM a minimal
  # base and the Nix closure stay free of a libc dependency. The pure-Go SQLite
  # driver is the reason this is possible at all (spec.md E1).
  env.CGO_ENABLED = 0;

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${finalAttrs.version}"
  ];

  # The unit tests need no network and no fixtures outside the source, so they
  # run as part of the build. -race is left to CI: it needs cgo, which would
  # defeat the static build above.
  doCheck = true;

  meta = {
    description = "Aggregating MCP gateway for outbound tunnels, with a web console and call log";
    homepage = "https://github.com/AkosPapp/mcp-switchboard";
    license = lib.licenses.mit;
    mainProgram = "mcp-switchboard-hub";
    platforms = lib.platforms.unix;
  };
})
