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

- `cd hub && go build ./... && go vet ./... && go test ./...`
- `nix flake check` (needs Nix; covers the Go build, the NixOS module, and the VM test)

## Repo layout notes

- `hub/vendor/` is not used by anything — the Nix build vendors its own copy from
  `go.mod`/`go.sum` (see above) and does not read a committed vendor directory.
  Don't add one.
