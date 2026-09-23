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

main() {
    ensure_ca_bundle

    # Releases are rolling dev builds published from every commit to main (no
    # tags), so pre-release resolution has to be allowed explicitly or uvx
    # will refuse to consider any version at all.
    if has_cmd npx && has_cmd uvx; then
        log "npx and uvx already on PATH, nothing to bootstrap"
        exec uvx --prerelease allow "$PACKAGE_NAME" "$@"
    fi

    if has_cmd nix; then
        log "npx and/or uvx missing; using 'nix shell' to provide them for this run only"
        exec nix shell nixpkgs#nodejs nixpkgs#uv --command uvx --prerelease allow "$PACKAGE_NAME" "$@"
    fi

    log "nix not available; installing missing tools natively (no sudo, per-user only)"

    if ! has_cmd uvx; then
        install_uv_natively
    fi

    if ! has_cmd npx; then
        install_node_natively
    fi

    has_cmd npx || die "npx still not available after installation attempts"
    has_cmd uvx || die "uvx still not available after installation attempts"

    exec uvx --prerelease allow "$PACKAGE_NAME" "$@"
}

main "$@"
