# The hub, built from hub/ with buildGoModule. The console (hub/web) is built
# separately by console.nix - via Nix's own npm support, not a committed
# dist/ - and copied into place below before the Go build reads it through
# hub/web/embed.go's `go:embed all:dist` (spec.md U1).
{
  lib,
  buildGoModule,
  console,
  version ? "0.3.0",
}:

buildGoModule (finalAttrs: {
  pname = "mcp-switchboard-hub";
  inherit version;

  # Only what the build reads. Narrowed with a fileset rather than by copying
  # hub/ wholesale, so an edit to a test fixture or to node_modules does not
  # invalidate the derivation. hub/web/dist is not part of this: it is not
  # committed, and postPatch below fills it in from `console` instead.
  src = lib.fileset.toSource {
    root = ../hub;
    fileset = lib.fileset.unions [
      ../hub/go.mod
      ../hub/go.sum
      ../hub/cmd
      ../hub/internal
      ../hub/web/embed.go
    ];
  };

  # `go:embed all:dist` (hub/web/embed.go) needs this directory to exist with
  # real content before configurePhase, or the build fails outright - which
  # includes doCheck's `go test` below, not just the final binary.
  postPatch = ''
    mkdir -p web/dist
    cp -r ${console}/. web/dist/
  '';

  vendorHash = "sha256-i3Cp9CZ6oa30Ds7L3Buf/6ViOfh+KdiSS7KyKWfqrkQ=";

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
