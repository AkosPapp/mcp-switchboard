# Tunnel protocol v1

The wire format between `mcp-switchboard-client` (on a machine with local MCP servers) and
`mcp-switchboard-hub`. This file is the single source of truth; `protocol.py` in each package is a
literal transcription of it and the two copies are kept byte-identical by a test.

## Shape

One **outbound** WebSocket per client process, carrying every MCP server that client runs. All frames
are JSON text frames, each an object with a `type` field.

The client is a **dumb pipe**: it speaks no MCP. It spawns local stdio MCP servers and shuttles their
stdin/stdout across the tunnel tagged with a server name. Every MCP concern — `initialize`,
`tools/list`, id correlation — lives in the hub, which runs one MCP client session per server channel.

## Connecting

The client opens `wss://<hub>/tunnel/v1` with:

```
Authorization: Bearer <tunnel token>
```

An invalid or missing token is rejected during the handshake with HTTP 401. The token is always
required: this listener is the one intended to face the public internet.

## Frames

### `hello` — client → hub, first frame

```json
{
  "type": "hello",
  "protocol": 1,
  "client": {
    "name": "mcp-switchboard-client",
    "version": "0.2.0",
    "instance": "3f8a…",
    "label": "legion5"
  },
  "servers": [
    {"name": "git", "command": "uvx mcp-server-git --repository ."}
  ]
}
```

`instance` is a per-process UUID. `label` identifies the machine, defaults to its hostname, and is what
the hub uses to tag tools by host. `command` is descriptive only — the hub never executes it.

Server names must not contain `__`, since the hub joins names as `{label}__{server}__{tool}` and splits
on that separator. The hub rejects the connection with an `error` frame if they do.

### `hello_ack` — hub → client

```json
{"type": "hello_ack", "connectionId": "c1a2…", "hub": {"name": "mcp-switchboard-hub", "version": "0.1.0"}}
```

### `mcp` — both directions

The data plane. `payload` is a raw MCP JSON-RPC message, passed through untouched.

```json
{"type": "mcp", "server": "git", "payload": {"jsonrpc": "2.0", "id": 1, "method": "tools/list"}}
```

Hub → client frames are written to that server's stdin; client → hub frames are one line read from its
stdout. Because each server has its own channel with its own MCP session, JSON-RPC ids never collide
and the hub needs no id rewriting.

### `server_state` — client → hub

```json
{"type": "server_state", "server": "git", "state": "running", "exitCode": null, "error": null}
```

States: `starting`, `running`, `exited`, `failed`. Sent on every transition so the console can show a
crashed server with its error instead of silently listing no tools.

### `restart` — hub → client

```json
{"type": "restart", "server": "git"}
```

Stop and respawn that server, then report the resulting `server_state`. Triggered from the console.

### `error` — hub → client

```json
{"type": "error", "message": "server names must not contain '__'", "server": null}
```

Sent before closing when the connection is unusable (bad protocol version, illegal server names).

## Liveness

WebSocket ping/pong only, native to the `websockets` library, with a 20s interval and 10s timeout.

**There is deliberately no application-level heartbeat frame.** The protocol this replaces had both
sides answer `heartbeat` with `heartbeat`, which is an infinite loop — measured at ~80 messages/second
per session against a live gateway. Any future heartbeat frame must be asymmetric (`ping` answered by
`pong`, never by another `ping`).

## Reconnection

The client reconnects with exponential backoff (1s doubling to a 60s cap). On a successful reconnect it
**restarts every local MCP server**, because the hub establishes a fresh MCP session and a server
process that has already completed `initialize` would reject a second one.

## Versioning

`protocol` in `hello` is an integer. The hub rejects a version it does not implement with an `error`
frame and closes; it does not attempt to negotiate down.
