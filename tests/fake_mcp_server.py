"""A tiny real stdio MCP server, used by the end-to-end test.

Deliberately a real MCP server rather than a stub: the point of the end-to-end
test is that nothing in the path is faked, so the hub has to do a genuine
initialize/tools-list/tools-call against a separate process over the tunnel.
"""

import sys

import anyio
import mcp_types as types
from mcp.server.lowlevel import Server
from mcp.server.stdio import stdio_server


async def on_list_tools(ctx, params) -> types.ListToolsResult:
    return types.ListToolsResult(
        tools=[
            types.Tool(
                name="echo",
                description="Echo a message back.",
                input_schema={
                    "type": "object",
                    "properties": {"message": {"type": "string"}},
                    "required": ["message"],
                },
            ),
            types.Tool(
                name="boom",
                description="Always fails, so error handling can be tested.",
                input_schema={"type": "object", "properties": {}},
            ),
        ]
    )


async def on_call_tool(ctx, params: types.CallToolRequestParams) -> types.CallToolResult:
    if params.name == "boom":
        return types.CallToolResult(
            content=[types.TextContent(type="text", text="boom, as requested")], is_error=True
        )
    message = (params.arguments or {}).get("message", "")
    return types.CallToolResult(content=[types.TextContent(type="text", text=f"echo: {message}")])


async def main() -> None:
    name = sys.argv[1] if len(sys.argv) > 1 else "fake"
    server = Server(name, version="1.0.0", on_list_tools=on_list_tools, on_call_tool=on_call_tool)
    async with stdio_server() as (read_stream, write_stream):
        await server.run(read_stream, write_stream, server.create_initialization_options())


if __name__ == "__main__":
    anyio.run(main)
