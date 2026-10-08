#!/bin/sh
# opencode-local.sh - the --opencode installer flow, for testing against the
# hub you run from this repo (the "Switchboard: hub" task), with everything
# local: this repository's plugin file and client instead of the Pages copy
# and the published PyPI package.
#
# It mirrors what install.sh --opencode does:
#   1. builds the installer-owned dummy OpenCode config dir around THIS repo's
#      plugin (editor/opencode/plugin/switchboard-hub.js) - your own opencode
#      config is never touched;
#   2. starts the repo's client from here (it tunnels ./mcp.json plus the
#      built-in harness to ws://127.0.0.1:8097, token from ./.env);
#   3. runs opencode with OPENCODE_CONFIG_DIR and MCP_SWITCHBOARD_HUB_URL
#      exported (console/API at http://127.0.0.1:8099); say goodbye and the
#      client is shut down with it.
#
# The hub task must be running first; nothing here goes to the network.

set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
confdir="${TMPDIR:-/tmp}/mcp-switchboard-opencode-local"

if ! curl -sf -m 2 http://127.0.0.1:8099/health >/dev/null 2>&1; then
    echo "[opencode-local] no hub answering on http://127.0.0.1:8099 - start the 'Switchboard: hub' task first" >&2
    exit 1
fi

mkdir -p "$confdir/plugins"
cp "$root/editor/opencode/plugin/switchboard-hub.js" "$confdir/plugins/switchboard-hub.js"

cd "$root"
uv run mcp-switchboard-client --hub-url ws://127.0.0.1:8097 &
client=$!
cleanup() {
    kill "$client" 2>/dev/null || true
}
trap cleanup INT TERM EXIT
# Give it a moment: a bad token or ./mcp.json shows up here as an early exit,
# and opencode without a tunnel is just a plain local session.
sleep 2
if ! kill -0 "$client" 2>/dev/null; then
    echo "[opencode-local] the client exited immediately (token in .env? is the hub task's tunnel port free?)" >&2
    exit 1
fi
echo "[opencode-local] client up (pid $client); opencode will bridge to http://127.0.0.1:8099"

# Not exec'd on purpose: the EXIT trap must run to take the client down.
# Real env vars beat this one from .env, so the hub URL export below wins for
# both the plugin and opencode's own view of the world.
OPENCODE_CONFIG_DIR="$confdir" MCP_SWITCHBOARD_HUB_URL=http://127.0.0.1:8099 opencode
status=$?
exit $status
