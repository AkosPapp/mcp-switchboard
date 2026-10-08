# Sandbox proposal (not implemented)

**Status: a proposal, implemented by nobody.** Nothing in this repo enforces any of it. What the
harness does today is documented honestly instead — see the Security sections of
`servers/harness/README.md` and the module docstring of
`servers/harness/src/mcp_switchboard_server_harness/server.py`.

## The problem

The harness's file tools confine paths to *root + scratch + temp*, but `bash`
and the `process_*` tools execute arbitrary code as the launching user, with that
user's full read access (`~/.git-credentials`, ssh keys, anything). Path confinement is a guard
against mistakes and surprises, not a security boundary, and we deliberately keep the file-tool
set exactly as wide as what the shell can already write, so the two execution paths never
disagree about what is writable.

A real boundary needs OS-level mechanisms. Two viable shapes:

## Option A — wrap the harness process (preferred shape)

Launch the harness inside a **bubblewrap** (`bwrap`) sandbox from the client (or a systemd unit
on the hub host):

- `--ro-bind / /` for the repo root plus `--bind` for scratch and `$TMPDIR` only;
- a fresh mount namespace, PID namespace and IPC namespace;
- drop to a dedicated uid/gid (a `switchboard-harness` user) that owns nothing readable it
  shouldn't — notably not `~/.git-credentials`;
- network optionally cut (`--unshare-net`), trading away installs/tests that need it;
- the hub tunnels stdio over the existing socket: the sandbox is invisible to the protocol.

Costs: bwrap must be installed (NixOS module changes in `nix/` to wire it in, unprivileged userns
enabled); debugging "why did the tool fail" gets harder; anything the project legitimately needs
outside the root (checkout caches, `~/.cargo`, `~/.npm`, ccache) must be explicitly exposed,
which re-opens exactly the holes the sandbox is for.

## Option B — honesty (what ships today)

Keep the file-tool convention, document exactly what it does and does not protect, and rely on
trust at the hub edge: expose the hub's private endpoints only to consumers you trust, and keep
`MCP_SWITCHBOARD_PRIVATE_TOKEN` set. This is the shipped behaviour; this document exists so the
alternative is on the record rather than silently assumed.

## If you implement Option A

1. Decide per-deployment whether the sandboxed user keeps network access.
2. Make the exposed set explicit config (`--sandbox-bind RO|RW path`), never implicit.
3. Fail loudly at harness start if bwrap is unavailable rather than silently running unconfined.
4. Add a hub-side capability flag to the harness's `hello` so the console can show whether a
   given connection is sandboxed — today no connection is, and the UI should not imply otherwise.
