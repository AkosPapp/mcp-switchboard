#!/bin/sh
# install.sh - bootstrap installer for mcp-switchboard-client
#
# Usage:
#   curl -fsSL https://<static-url>/install.sh | sh -s -- --hub-url wss://... --token ...
#
# Ensures npx and uvx are available (via nix, if present, otherwise via
# native per-user installs with no sudo and no system package state
# changes), then hands off entirely to the real tool via `uvx`, forwarding
# every argument this script received, unmodified.
#
# One flag belongs to this script and is consumed here rather than forwarded:
#
#   --editor=code|none   set up the OpenCode TUI inside VS Code (default none).
#                        Installs the sst-dev.opencode extension and points
#                        OpenCode at this hub's /mcp endpoint. Best-effort: a
#                        missing or unwritable editor never stops the client
#                        from starting. MCP_SWITCHBOARD_MCP_URL overrides the
#                        endpoint written into the config.
#
# Note the deliberate omission: --editor does NOT install opencode itself. See
# setup_editor for why.
#
# POSIX sh only - no bashisms. Safe to re-run. Fails loudly rather than
# falling through to a broken `uvx` invocation.

set -eu

PACKAGE_NAME="mcp-switchboard-client"
CACHE_DIR="${MCP_SWITCHBOARD_INSTALLER_CACHE:-$HOME/.cache/mcp-switchboard-installer}"
NODE_VERSION="${NODE_VERSION:-22.11.0}"

log() {
    printf '[install.sh] %s\n' "$*" >&2
}

die() {
    printf '[install.sh] ERROR: %s\n' "$*" >&2
    exit 1
}

has_cmd() {
    command -v "$1" >/dev/null 2>&1
}

# fetch <url> <output-path>: tries curl, falls back to wget, fails loudly if
# neither is available. Every download in this script goes through this.
fetch() {
    _url="$1"
    _out="$2"
    if has_cmd curl; then
        curl -fsSL "$_url" -o "$_out"
    elif has_cmd wget; then
        wget -q "$_url" -O "$_out"
    else
        die "neither curl nor wget is available; cannot download $_url"
    fi
}

# fetch_stdout <url>: same as fetch(), but streams to stdout (for piping
# installers like uv's straight into sh).
fetch_stdout() {
    _url="$1"
    if has_cmd curl; then
        curl -fsSL "$_url"
    elif has_cmd wget; then
        wget -q -O - "$_url"
    else
        die "neither curl nor wget is available; cannot download $_url"
    fi
}

detect_os() {
    _uname_s="$(uname -s)"
    case "$_uname_s" in
        Linux) echo "linux" ;;
        Darwin) echo "darwin" ;;
        *) die "unsupported OS: $_uname_s" ;;
    esac
}

detect_arch() {
    _uname_m="$(uname -m)"
    case "$_uname_m" in
        x86_64 | amd64) echo "x64" ;;
        aarch64 | arm64) echo "arm64" ;;
        *) die "unsupported architecture: $_uname_m" ;;
    esac
}

# install_uv: official installer, no sudo, installs to ~/.local/bin (or
# $CARGO_HOME/$UV_INSTALL_DIR as configured by that installer itself).
install_uv_natively() {
    log "installing uv (uvx) via the official installer..."
    _uv_installer_tmp="$(mktemp)"
    if has_cmd curl; then
        curl -fsSL https://astral.sh/uv/install.sh -o "$_uv_installer_tmp"
    elif has_cmd wget; then
        wget -q https://astral.sh/uv/install.sh -O "$_uv_installer_tmp"
    else
        die "neither curl nor wget is available; cannot install uv"
    fi
    sh "$_uv_installer_tmp"
    rm -f "$_uv_installer_tmp"

    # The installer places uv/uvx in ~/.local/bin (or $UV_INSTALL_DIR) by
    # default; make sure this run can see it without requiring a new shell.
    for _candidate in "$HOME/.local/bin" "$HOME/.cargo/bin"; do
        case ":$PATH:" in
            *":$_candidate:"*) ;;
            *) PATH="$_candidate:$PATH" ;;
        esac
    done
    export PATH

    has_cmd uvx || die "uv installer ran but 'uvx' is still not on PATH"
}

# install_node_natively: fetch the official Node.js LTS tarball directly
# from nodejs.org into a cache dir and extract it, giving us a bundled npx
# for this run only. No system package manager, no sudo.
install_node_natively() {
    _os="$(detect_os)"
    _arch="$(detect_arch)"
    _node_dist="node-v${NODE_VERSION}-${_os}-${_arch}"
    _node_home="$CACHE_DIR/$_node_dist"

    if [ -x "$_node_home/bin/npx" ]; then
        log "using cached Node.js $NODE_VERSION from $_node_home"
    else
        log "downloading Node.js $NODE_VERSION for ${_os}-${_arch}..."
        mkdir -p "$CACHE_DIR"
        _tarball="$CACHE_DIR/${_node_dist}.tar.gz"
        fetch "https://nodejs.org/dist/v${NODE_VERSION}/${_node_dist}.tar.gz" "$_tarball"
        tar -xzf "$_tarball" -C "$CACHE_DIR"
        rm -f "$_tarball"
    fi

    case ":$PATH:" in
        *":$_node_home/bin:"*) ;;
        *) PATH="$_node_home/bin:$PATH" ;;
    esac
    export PATH

    has_cmd npx || die "Node.js was fetched but 'npx' is still not on PATH"
}

# ensure_ca_bundle: point TLS clients at a system CA bundle if the environment
# does not already. Some Pythons (the portable CPython builds uvx downloads,
# especially on NixOS, whose trust store is not at the usual path) cannot find
# one and fail every wss:// connection with CERTIFICATE_VERIFY_FAILED. An
# existing SSL_CERT_FILE / SSL_CERT_DIR is always respected, and if nothing is
# found the client's bundled certifi certificates are still the fallback.
ensure_ca_bundle() {
    if [ -n "${SSL_CERT_FILE:-}" ] && [ -r "$SSL_CERT_FILE" ]; then
        return 0
    fi
    if [ -n "${SSL_CERT_DIR:-}" ] && [ -d "$SSL_CERT_DIR" ]; then
        return 0
    fi
    for _bundle in \
        "${NIX_SSL_CERT_FILE:-}" \
        /etc/ssl/certs/ca-certificates.crt \
        /run/current-system/sw/etc/ssl/certs/ca-bundle.crt \
        /etc/pki/tls/certs/ca-bundle.crt \
        /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem \
        /etc/ssl/ca-bundle.pem \
        /etc/ssl/cert.pem \
        /usr/local/etc/openssl/cert.pem \
        /opt/homebrew/etc/ca-certificates/cert.pem
    do
        if [ -n "$_bundle" ] && [ -r "$_bundle" ]; then
            SSL_CERT_FILE="$_bundle"
            export SSL_CERT_FILE
            log "using CA bundle $_bundle"
            return 0
        fi
    done
}

# ---------------------------------------------------------------------------
# editor integration (--editor)
# ---------------------------------------------------------------------------

# The official OpenCode extension. Its whole payload is a terminal launcher:
# openTerminal / openNewTerminal / addFilepathToTerminal, plus ctrl+Escape and
# the selection-and-tab sharing the TUI picks up from the active editor. It is
# 5 KiB and contributes no view of its own, so it is not a hub console - it is
# the piece that makes "run opencode in VS Code" behave like an integration.
OPENCODE_EXTENSION="sst-dev.opencode"

# Where the config merge below writes. Global rather than project-level
# because a hub URL describes this machine, not one repository.
EDITOR_CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/opencode"

DEFAULT_MCP_URL="http://127.0.0.1:8099/mcp"

# Set by main's argument loop; `set -u` means it has to exist even when no
# --editor flag was given.
EDITOR_TARGET=""

# find_editor_cli: the first VS Code flavoured CLI that can actually install an
# extension. Cursor is not in that list on purpose: its `cursor` command here
# is the agent CLI, which refuses to report a version and offers no
# --install-extension, so probing it would install nothing and say nothing.
find_editor_cli() {
    for _candidate in code codium; do
        if has_cmd "$_candidate" &&
            "$_candidate" --help 2>&1 | grep -qi -- "--install-extension"; then
            echo "$_candidate"
            return 0
        fi
    done
    return 1
}

install_editor_extension() {
    _cli=$1
    # --list-extensions prints lowercase ids; match exactly, case-insensitively,
    # so a longer id that merely starts with ours does not read as installed.
    if "$_cli" --list-extensions 2>/dev/null | grep -qix -- "$OPENCODE_EXTENSION"; then
        log "$OPENCODE_EXTENSION already installed in $_cli"
        return 0
    fi
    log "installing $OPENCODE_EXTENSION into $_cli..."
    if "$_cli" --install-extension "$OPENCODE_EXTENSION" >/dev/null 2>&1; then
        log "installed $OPENCODE_EXTENSION - reload the window (Ctrl+Shift+P -> Reload Window) to activate it"
    else
        # The usual cause is an editor whose extensions directory is managed
        # elsewhere (a Nix vscode-with-extensions derivation), where a CLI
        # install cannot write. Not fatal: opencode also self-installs this
        # extension the first time it runs in the integrated terminal.
        log "WARNING: $_cli could not install $OPENCODE_EXTENSION; install it from the Extensions view, or just run 'opencode' in the integrated terminal"
    fi
}

# merge_opencode_config: add/refresh the hub's mcp entry in the global
# opencode.json without touching any other key. Merging rather than writing the
# file is the whole point - that file carries the user's providers, agents and
# plugins, and overwriting it would be exactly the host damage this script
# promises not to do. Round-tripping JSON reflows formatting and key order.
merge_opencode_config() {
    _url=$1
    _dir=$EDITOR_CONFIG_DIR
    _file="$_dir/opencode.json"

    mkdir -p "$_dir" 2>/dev/null || {
        log "WARNING: cannot create $_dir; skipping the MCP wiring"
        return 0
    }
    [ -f "$_file" ] || printf '{}' >"$_file" 2>/dev/null || {
        log "WARNING: cannot write $_file; skipping the MCP wiring"
        return 0
    }

    _out=$(mktemp) || return 0
    # The token is only forwarded when the caller already exported it; the
    # private listener is unauthenticated by default, and writing a secret into
    # a config file is not something to do unasked.
    OPENCODE_MCP_URL="$_url" \
    OPENCODE_MCP_TOKEN="${MCP_SWITCHBOARD_PRIVATE_TOKEN:-}" \
        _merge_json "$_file" "$_out" || {
        rm -f "$_out"
        log "WARNING: could not merge $_file (it may not be valid JSON); leaving it untouched"
        log "  add this by hand under \"mcp\": {\"switchboard\": {\"type\": \"remote\", \"url\": \"$_url\", \"enabled\": true}}"
        return 0
    }
    if cat "$_out" >"$_file" 2>/dev/null; then
        log "wired opencode -> $_url ($_file)"
    else
        log "WARNING: could not write $_file"
    fi
    rm -f "$_out"
}

# _merge_json: pick whichever interpreter is on PATH. Both exist in this
# script's normal environment (node is what it bootstraps), but the nix-shell
# path keeps node inside the nix shell rather than on our PATH, so python3 is
# the fallback and printing the snippet is the last resort.
_merge_json() {
    _in=$1
    _out=$2
    if has_cmd node; then
        node -e '
            const fs = require("fs");
            const cfg = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
            if (cfg === null || typeof cfg !== "object" || Array.isArray(cfg)) throw new Error("not an object");
            cfg.mcp = (cfg.mcp && typeof cfg.mcp === "object") ? cfg.mcp : {};
            const entry = { type: "remote", url: process.env.OPENCODE_MCP_URL, enabled: true };
            if (process.env.OPENCODE_MCP_TOKEN) entry.headers = { Authorization: "Bearer " + process.env.OPENCODE_MCP_TOKEN };
            cfg.mcp.switchboard = entry;
            fs.writeFileSync(process.argv[2], JSON.stringify(cfg, null, 2) + "\n");
        ' "$_in" "$_out" 2>/dev/null
    elif has_cmd python3; then
        python3 - "$_in" "$_out" <<'PY' 2>/dev/null
import json, os, sys
try:
    with open(sys.argv[1]) as f:
        cfg = json.load(f)
except Exception:
    sys.exit(1)
if not isinstance(cfg, dict):
    sys.exit(1)
mcp = cfg.get("mcp")
if not isinstance(mcp, dict):
    mcp = {}
entry = {"type": "remote", "url": os.environ["OPENCODE_MCP_URL"], "enabled": True}
token = os.environ.get("OPENCODE_MCP_TOKEN") or ""
if token:
    entry["headers"] = {"Authorization": f"Bearer {token}"}
mcp["switchboard"] = entry
cfg["mcp"] = mcp
with open(sys.argv[2], "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
PY
    else
        return 1
    fi
}

# setup_editor: --editor=code. Runs before the client launches, because it is
# one-time host setup; the client itself stays what it is - a tunnel that
# listens on nothing and touches only what mcp.json names.
#
# Why opencode is detected but never installed: it is very likely already
# managed declaratively (on this project's author's machine it comes from the
# NixOS system closure), and ~/.local/bin sits ahead of /run/current-system/sw
# bin, so a per-user install would silently shadow the managed copy - and
# `opencode upgrade` would then mutate the wrong binary while nixos-rebuild
# quietly disagreed about which one was current. Reporting beats installing.
setup_editor() {
    case "$EDITOR_TARGET" in
        "" | none) return 0 ;;
        code) ;;
        *) die "--editor must be 'code' or 'none' (got '$EDITOR_TARGET')" ;;
    esac

    if has_cmd opencode; then
        log "opencode found: $(command -v opencode)"
    else
        log "opencode not found - install it for this machine (with Nix: 'nix profile install nixpkgs#opencode'); not installing it from here, so a declaratively managed copy is never shadowed"
    fi

    _cli=$(find_editor_cli) || {
        log "WARNING: no VS Code CLI with --install-extension found; skipped the extension"
        _cli=""
    }
    if [ -n "$_cli" ]; then
        install_editor_extension "$_cli"
    fi

    merge_opencode_config "${MCP_SWITCHBOARD_MCP_URL:-$DEFAULT_MCP_URL}"

    log "in VS Code: Ctrl+Escape opens the OpenCode TUI and shares your current selection; use @File#L37-42 to pin a range"
    log "narrow the endpoint to this machine only if you prefer: ${MCP_SWITCHBOARD_MCP_URL:-$DEFAULT_MCP_URL}/host/$(hostname 2>/dev/null || echo '<label>')"
    # Plain expansion, deliberately: $(...) would *run* the CLI, and `code`
    # with no arguments opens an editor window instead of printing a revert hint.
    _revert_cli=${_cli:-code}
    log "revert: $_revert_cli --uninstall-extension $OPENCODE_EXTENSION, and delete the switchboard entry from $EDITOR_CONFIG_DIR/opencode.json"
}

main() {
    ensure_ca_bundle

    # Consume this script's own --editor flag; every other argument is kept,
    # in order, for the client CLI at the end, whose parser rejects anything it
    # does not know. POSIX sh has no arrays, so surviving arguments are rotated
    # onto the tail of the positional list while the head is examined - and it
    # has to happen here rather than in a helper, because a function receives
    # its own "$@" and the caller's list would come back unfiltered.
    _remaining=$#
    while [ "$_remaining" -gt 0 ]; do
        _arg=$1
        shift
        _remaining=$((_remaining - 1))
        case "$_arg" in
            --editor)
                EDITOR_TARGET=${1:-}
                [ -n "$EDITOR_TARGET" ] || die "--editor requires a value (code, none)"
                shift
                _remaining=$((_remaining - 1))
                ;;
            --editor=*)
                EDITOR_TARGET=${_arg#--editor=}
                ;;
            *)
                set -- "$@" "$_arg"
                ;;
        esac
    done

    # Releases are rolling dev builds published from every commit to main (no
    # tags), so pre-release resolution has to be allowed explicitly or uvx
    # will refuse to consider any version at all.
    _via_nix=0
    if has_cmd npx && has_cmd uvx; then
        log "npx and uvx already on PATH, nothing to bootstrap"
    elif has_cmd nix; then
        log "npx and/or uvx missing; using 'nix shell' to provide them for this run only"
        _via_nix=1
    else
        log "nix not available; installing missing tools natively (no sudo, per-user only)"

        if ! has_cmd uvx; then
            install_uv_natively
        fi

        if ! has_cmd npx; then
            install_node_natively
        fi

        has_cmd npx || die "npx still not available after installation attempts"
        has_cmd uvx || die "npx still not available after installation attempts"
    fi

    # After the bootstrap, so node exists for the config merge where possible,
    # and before the exec, so it happens even on a first-ever run.
    setup_editor

    if [ "$_via_nix" -eq 1 ]; then
        exec nix shell nixpkgs#nodejs nixpkgs#uv --command uvx --prerelease allow "$PACKAGE_NAME" "$@"
    fi
    exec uvx --prerelease allow "$PACKAGE_NAME" "$@"
}

main "$@"
