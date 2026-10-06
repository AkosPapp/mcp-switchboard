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
#                        Installs the sst-dev.opencode extension and the hub
#                        bridge plugin (see --opencode). Best-effort: a missing
#                        or unwritable editor never stops the client from
#                        starting. Implies --opencode.
#
#   --opencode           end this command IN an OpenCode session. Makes sure
#                        opencode exists (the official per-user installer,
#                        ~/.opencode/bin, no sudo, runs only when none is
#                        found - an existing copy, very likely a Nix-managed
#                        one, is left completely alone), installs the hub
#                        bridge plugin (editor/opencode/plugin in this repo,
#                        served from the Pages site): every opencode session
#                        gets a bridge chat in the hub console - transcript
#                        mirrored in both directions, permission prompts and
#                        questions pushed to the phone, and switchboard_*
#                        tools for talking to the hub's agents. The plugin
#                        dials the hub API at an origin derived from
#                        --hub-url (loopback -> http://127.0.0.1:8099,
#                        otherwise the same origin; MCP_SWITCHBOARD_MCP_URL
#                        overrides). Starts the client TUNNEL in the
#                        background (log/pid under
#                        ~/.cache/mcp-switchboard-installer/), then execs
#                        opencode in this terminal. Exiting opencode leaves
#                        the client running; `kill $(cat .../client.pid)`
#                        stops it.
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

# The hub-bridge plugin opencode loads from the config dir; it is served from
# the same Pages site as this script (see .github/workflows/pages.yml).
PLUGIN_FILE=""
PLUGIN_URL="${MCP_SWITCHBOARD_PLUGIN_URL:-https://akospapp.github.io/mcp-switchboard/opencode/plugin/switchboard-hub.js}"

# The endpoint written into opencode.json when nothing better is known.
DEFAULT_MCP_URL="http://127.0.0.1:8099/mcp"

# Peeked (never consumed) from --hub-url by main's argument loop, so the
# config merge can work out the endpoint from the hub the user already named.
HUB_URL_PEEKED=""

# resolved_mcp_url: what endpoint opencode should dial, best guess last.
#   1. MCP_SWITCHBOARD_MCP_URL - explicit wins, always.
#   2. A loopback tunnel URL means a hub right here: use its private port
#      directly (8097 is the tunnel listener, which does not serve /mcp).
#   3. Any other host - a proxy/gateway in front of the hub - reuse that
#      origin, keeping its path prefix: /mcp hangs off the same front door.
#   4. No --hub-url at all: the local default.
resolved_mcp_url() {
    [ -n "${MCP_SWITCHBOARD_MCP_URL:-}" ] && { printf '%s\n' "$MCP_SWITCHBOARD_MCP_URL"; return; }
    if [ -z "$HUB_URL_PEEKED" ]; then
        printf '%s\n' "$DEFAULT_MCP_URL"
        return
    fi
    _u=$HUB_URL_PEEKED
    _scheme=${_u%%://*}
    _rest=${_u#*://}
    case "$_scheme" in
        ws) _http=http ;;
        wss) _http=https ;;
        *) _http=$_scheme ;;
    esac
    _hostport=${_rest%%/*}
    case "$_hostport" in
        127.0.0.1:* | 127.0.0.1 | localhost:* | localhost | "[::1]:*" | "[::1]")
            printf 'http://127.0.0.1:8099/mcp\n'
            return
            ;;
    esac
    case "$_rest" in
        */*)
            _path=${_rest#*/}
            _path=${_path%/}
            if [ -n "$_path" ]; then
                printf '%s\n' "$_http://$_hostport/$_path/mcp"
            else
                printf '%s\n' "$_http://$_hostport/mcp"
            fi
            ;;
        *)
            printf '%s\n' "$_http://$_rest/mcp"
            ;;
    esac
}

# Set by main's argument loop; `set -u` means they have to exist even when no
# flags were given.
EDITOR_TARGET=""
OPENCODE_WANTED=0

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

# ensure_opencode: --opencode (implied by --editor=code). Detect first, install
# second. The detect step is the whole point and must never be dropped: an
# existing opencode is likely managed declaratively (on this project's author's
# machine it comes from the NixOS system closure), and per-user PATH entries
# sit ahead of system ones - installing over that would silently shadow the
# managed copy, and `opencode upgrade` would then mutate the wrong binary while
# nixos-rebuild quietly disagreed about which one was current. So: report an
# existing copy, touch nothing of it; run the official per-user installer
# (~/.opencode/bin, no sudo, edits the user's shell rc for PATH) only where the
# binary is absent entirely.
ensure_opencode() {
    if has_cmd opencode; then
        log "opencode found: $(command -v opencode)"
        return 0
    fi
    if ! has_cmd curl; then
        log "WARNING: opencode is not installed and curl is required for its installer; install it by hand (https://opencode.ai/docs)"
        return 0
    fi
    # The installer is a bash script; on hosts where sh is not bash, piping it
    # straight into sh would break on bash-isms, so prefer bash when present.
    if has_cmd bash; then
        # shellcheck disable=SC2209  # deliberately the name of a shell to run
        _shell=bash
    else
        # shellcheck disable=SC2209
        _shell=sh
    fi
    log "opencode not found; installing the official per-user build into ~/.opencode/bin (this never replaces an existing copy)"
    if curl -fsSL https://opencode.ai/install | "$_shell" >/dev/null 2>&1; then
        log "installed opencode"
        case ":$PATH:" in
            *":$HOME/.opencode/bin:"*) ;;
            *) log "note: add \$HOME/.opencode/bin to PATH (the installer edits your shell config for future shells)" ;;
        esac
    else
        log "WARNING: the opencode installer failed; install it by hand (https://opencode.ai/docs)"
    fi
    has_cmd opencode || has_cmd "$HOME/.opencode/bin/opencode" ||
        log "note: Nix users can get a managed copy instead with 'nix profile install nixpkgs#opencode'"
}

# install_opencode_plugin: fetch the bridge plugin and merge its entry into
# the global opencode.json without touching any other key (that file carries
# the user's providers and agents; clobbering it is exactly what this script
# promises not to do). Merging also REMOVES the legacy mcp "switchboard" entry:
# opencode gets its local file/shell power from its own built-in tools now,
# and hub comms from the plugin - a second harness over /mcp was duplication.
install_opencode_plugin() {
    _hub_base=$1
    _dir=$EDITOR_CONFIG_DIR
    _file="$_dir/opencode.json"
    PLUGIN_FILE="$_dir/plugin/switchboard-hub.js"

    mkdir -p "$_dir/plugin" 2>/dev/null || {
        log "WARNING: cannot create $_dir/plugin; skipping the hub bridge"
        return 0
    }
    if fetch "$PLUGIN_URL" "$PLUGIN_FILE"; then
        log "installed the hub bridge plugin: $PLUGIN_FILE"
    else
        log "WARNING: could not fetch $PLUGIN_URL; skipping the hub bridge"
        return 0
    fi

    [ -f "$_file" ] || printf '{}' >"$_file" 2>/dev/null || {
        log "WARNING: cannot write $_file; skipping the plugin config"
        return 0
    }
    _out=$(mktemp) || return 0
    OPENCODE_PLUGIN_FILE="$PLUGIN_FILE" \
    OPENCODE_HUB_BASE="$_hub_base" \
    OPENCODE_TOKEN="${MCP_SWITCHBOARD_PRIVATE_TOKEN:-}" \
        _merge_plugin_json "$_file" "$_out" || {
        rm -f "$_out"
        log "WARNING: could not merge $_file (it may not be valid JSON); leaving it untouched"
        log "  add this by hand: \"plugin\": [[\"$PLUGIN_FILE\", {\"hub\": \"$_hub_base\"}]]"
        return 0
    }
    if cat "$_out" >"$_file" 2>/dev/null; then
        log "opencode bridges to $_hub_base (chat + tools + questions: $_file)"
    else
        log "WARNING: could not write $_file"
    fi
    rm -f "$_out"
}

# _merge_plugin_json: node first, python3 fallback; identical semantics.
_merge_plugin_json() {
    _in=$1
    _out=$2
    if has_cmd node; then
        node -e '
            const fs = require("fs");
            const cfg = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
            if (cfg === null || typeof cfg !== "object" || Array.isArray(cfg)) throw new Error("not an object");
            if (cfg.mcp && typeof cfg.mcp === "object") delete cfg.mcp.switchboard;
            const pth = process.env.OPENCODE_PLUGIN_FILE;
            const entry = [pth, { hub: process.env.OPENCODE_HUB_BASE, ...(process.env.OPENCODE_TOKEN ? { token: process.env.OPENCODE_TOKEN } : {}) }];
            let list = Array.isArray(cfg.plugin) ? cfg.plugin : [];
            list = list.filter((e) => {
                const first = Array.isArray(e) ? e[0] : e;
                return !(typeof first === "string" && first.endsWith("switchboard-hub.js"));
            });
            list.push(entry);
            cfg.plugin = list;
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
if isinstance(mcp, dict):
    mcp.pop("switchboard", None)
pth = os.environ["OPENCODE_PLUGIN_FILE"]
opts = {"hub": os.environ["OPENCODE_HUB_BASE"]}
token = os.environ.get("OPENCODE_TOKEN") or ""
if token:
    opts["token"] = token
lst = [e for e in (cfg.get("plugin") or []) if isinstance(e, (str, list))]
lst = [e for e in lst if not (isinstance(e, str) and e.endswith("switchboard-hub.js"))
       and not (isinstance(e, list) and e and isinstance(e[0], str) and e[0].endswith("switchboard-hub.js"))]
lst.append([pth, opts])
cfg["plugin"] = lst
with open(sys.argv[2], "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
PY
    else
        return 1
    fi
}

# setup_editor: --editor=code (and the opencode-only half of --opencode). Runs
# before the client launches, because it is one-time host setup; the client
# itself stays what it is - a tunnel that listens on nothing and touches only
# what mcp.json names.
setup_editor() {
    case "$EDITOR_TARGET" in
        "" | none)
            if [ "$OPENCODE_WANTED" -eq 1 ]; then
                # Standalone TUI hosts: no editor involved, but the same global
                # opencode.json merge still applies, so `opencode` in any
                # terminal reaches the hub's /mcp without VS Code.
                ensure_opencode
                _url=$(resolved_mcp_url)
                install_opencode_plugin "${_url%/mcp}"
            fi
            return 0
            ;;
        code) ;;
        *) die "--editor must be 'code' or 'none' (got '$EDITOR_TARGET')" ;;
    esac

    ensure_opencode

    _cli=$(find_editor_cli) || {
        log "WARNING: no VS Code CLI with --install-extension found; skipped the extension"
        _cli=""
    }
    if [ -n "$_cli" ]; then
        install_editor_extension "$_cli"
    fi

    _mcp=$(resolved_mcp_url)
    install_opencode_plugin "${_mcp%/mcp}"

    log "in VS Code: Ctrl+Escape opens the OpenCode TUI and shares your current selection; use @File#L37-42 to pin a range"
    # Plain expansion, deliberately: $(...) would *run* the CLI, and `code`
    # with no arguments opens an editor window instead of printing a revert hint.
    _revert_cli=${_cli:-code}
    log "revert: $_revert_cli --uninstall-extension $OPENCODE_EXTENSION, delete $PLUGIN_FILE, and drop the plugin entry from $EDITOR_CONFIG_DIR/opencode.json"
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
            --opencode)
                OPENCODE_WANTED=1
                ;;
            --hub-url)
                # Peek, do not consume: both the flag and its value are still
                # forwarded to the client. The value is this iteration's $1;
                # the flag goes back on the tail and the value follows when
                # the loop reaches it.
                HUB_URL_PEEKED=${1:-}
                set -- "$@" --hub-url
                ;;
            --hub-url=*)
                HUB_URL_PEEKED=${_arg#*=}
                set -- "$@" "$_arg"
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

    # The client's command line as positional arguments, so one exec point can
    # either replace this shell with it (plain run) or background it (--opencode).
    if [ "$_via_nix" -eq 1 ]; then
        set -- nix shell nixpkgs#nodejs nixpkgs#uv --command uvx --prerelease allow "$PACKAGE_NAME" "$@"
    else
        set -- uvx --prerelease allow "$PACKAGE_NAME" "$@"
    fi

    # --opencode ends the command IN an opencode session: the client goes to
    # the background (it is the tunnel that gives opencode's hub tools their
    # local reach; nohup keeps it past this terminal), and opencode takes over
    # the foreground terminal. The session is the point of the flag, so after
    # exiting opencode the client keeps running - stop it with the printed
    # kill, or just say so with a plain re-run of this script.
    if [ "$OPENCODE_WANTED" -eq 1 ]; then
        _oc=""
        if has_cmd opencode; then
            _oc=$(command -v opencode)
        elif [ -x "$HOME/.opencode/bin/opencode" ]; then
            # Just installed by ensure_opencode: ~/.opencode/bin is only on the
            # PATH of shells the installer EDITED, not of this one.
            _oc="$HOME/.opencode/bin/opencode"
        fi
        if [ -n "$_oc" ]; then
            _pidfile="$CACHE_DIR/client.pid"
            _log="$CACHE_DIR/client.log"
            _running=0
            if [ -f "$_pidfile" ] && read -r _p <"$_pidfile" 2>/dev/null && [ -n "${_p:-}" ] \
                && kill -0 "$_p" 2>/dev/null; then
                # /proc/cmdline check where it exists: a bare kill -0 would
                # believe a recycled pid.
                if [ -r "/proc/${_p}/cmdline" ]; then
                    grep -qa "$PACKAGE_NAME" "/proc/${_p}/cmdline" 2>/dev/null && _running=1
                    [ "$_running" -eq 1 ] || rm -f "$_pidfile"
                else
                    _running=1
                fi
            fi
            if [ "$_running" -eq 1 ]; then
                log "client already running in the background (pid $_p); leaving it alone"
            else
                mkdir -p "$CACHE_DIR"
                nohup "$@" < /dev/null >> "$_log" 2>&1 &
                echo "$!" >"$_pidfile"
                log "client started in the background (pid $!); log: $_log; stop: kill \$(cat $_pidfile)"
            fi
            exec "$_oc"
        fi
        log "WARNING: no opencode binary found; starting the client in the foreground instead"
    fi
    exec "$@"
}

main "$@"
