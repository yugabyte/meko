"""MCP client builder.

Per-request because the bearer token differs per user — there is no useful
notion of a "shared" MCP client across users. The MCP transport is HTTP, so
connection setup is cheap (no long-lived TCP, no auth handshake we own).
"""

from __future__ import annotations

import logging
from typing import Dict

from langchain_core.tools import BaseTool
from langchain_mcp_adapters.client import MultiServerMCPClient

logger = logging.getLogger(__name__)


async def build_mcp_tools(endpoint: str, user_token: str) -> Dict[str, BaseTool]:
    """Connect to the meko MCP server with the user's bearer token and discover tools.

    Returns a dict keyed by tool name (e.g. `memory_search`,
    `conversation_create`). Raises if the server is unreachable or
    authentication fails.
    """
    client = MultiServerMCPClient(
        connections={
            "meko": {
                "url": endpoint,
                "transport": "streamable_http",
                "headers": {"Authorization": f"Bearer {user_token}"},
            }
        }
    )
    tools = await client.get_tools()
    by_name = {tool.name: tool for tool in tools}
    logger.info("MCP tool discovery: %d tools (%s)", len(by_name), ", ".join(sorted(by_name)))
    return by_name
