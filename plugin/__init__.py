"""Coding Hermes fleet scheduler plugin for Hermes Agent."""

def register(ctx):
    """Plugin entry point — called once when Hermes loads."""
    ctx.log("coding-hermes plugin loaded")
    mcp = ctx.mcp.get("coding-hermes")
    if mcp is None:
        ctx.log("WARNING: coding-hermes MCP server not configured — /fleet commands disabled")
        return

    try:
        mcp.ping()
        ctx.log("coding-hermes MCP connected — fleet commands ready")
    except Exception as e:
        ctx.log(f"WARNING: coding-hermes MCP unreachable: {e}")
