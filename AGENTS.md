# Agent notes

Guidance for coding agents (and humans) working in this repo.

## Pre-commit checks

Run these before committing changes under `hub/`:

- **`hub/go.mod` / `hub/go.sum` changed?** → `nix/hub.nix` pins a `vendorHash` for
  `buildGoModule`'s vendoring step. It goes stale the moment `go.mod` or `go.sum`
  changes (including a plain `go mod tidy` that only reclassifies a dependency
  from `// indirect` to direct) and `nix build .#hub` / `nix flake check` fail
  with a Go "inconsistent vendoring" or Nix "hash mismatch" error. To fix:

  ```sh
  # in nix/hub.nix, temporarily:
  vendorHash = lib.fakeHash;
  nix build .#hub   # fails, prints the real hash under "got:"
  # paste that hash back into vendorHash
  ```

  Do this whenever a commit touches `hub/go.mod` or `hub/go.sum`, then confirm
  with `nix flake check`.

- **`hub/web/package-lock.json` changed?** → `nix/console.nix` pins an
  `npmDepsHash` for `buildNpmPackage`'s dependency fetch, the same kind of pin
  as `vendorHash` above and just as stale-prone. Fix the same way:

  ```sh
  # in nix/console.nix, temporarily:
  npmDepsHash = lib.fakeHash;
  nix build .#console   # fails, prints the real hash under "got:"
  # paste that hash back into npmDepsHash
  ```

- **`hub/web/dist/` is not committed.** `go build`/`go vet`/`go test` against
  `hub/` need it built first (`cd hub/web && npm ci && npm run build`) or
  `embed.go`'s `go:embed all:dist` fails outright with "no matching files".
  `nix build .#hub` builds it itself via `nix/console.nix` and needs no npm
  step; a bare `go build` does.
- `cd hub && gofmt -l .` must print nothing — CI runs this separately from
  `go vet`/`go test` and fails the build on any unformatted file. Fix with
  `gofmt -w <file>`.
- `cd hub && go build ./... && go vet ./... && go test ./...`
- `nix flake check` (needs Nix; covers the Go build, the console build, the
  NixOS module, and the VM test)

## Repo layout notes

- `hub/vendor/` is not used by anything — the Nix build vendors its own copy from
  `go.mod`/`go.sum` (see above) and does not read a committed vendor directory.
  Don't add one.
