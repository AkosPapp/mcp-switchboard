"""mcp-switchboard-client: tunnels local stdio MCP servers to a switchboard hub.

A dumb pipe by design - it speaks no MCP and has no `mcp` dependency. It spawns
the local servers listed in mcp.json and shuttles their stdin/stdout over one
outbound WebSocket, tagged with the server name. Every MCP concern lives in the
hub. See docs/PROTOCOL.md.
"""

__version__ = "0.2.0"
