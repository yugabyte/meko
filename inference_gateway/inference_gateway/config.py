"""Runtime config for the gateway.

Defaults live here so `argparse` and `app.py` agree on a single source of truth.
Frozen dataclass so configuration is immutable after startup.
"""

from __future__ import annotations

from dataclasses import dataclass


# Provider defaults — reused by argparse and tests.
DEFAULT_OPENAI_MODEL = "gpt-4o-mini"
DEFAULT_BEDROCK_MODEL = "us.amazon.nova-2-lite-v1:0"

# MCP / network defaults.
DEFAULT_MCP_ENDPOINT = "https://mcp.mekodata.ai/mcp"
# DEFAULT_MCP_ENDPOINT = "https://mcp.mekodev.com/mcp"
DEFAULT_PORT = 8088
DEFAULT_AWS_REGION = "us-east-1"

# CORS — production origins plus the common localhost dev ports. Override
# entirely with `--cors-origin ...` flags or `INFERENCE_CORS_ORIGINS=` env
# (comma-separated). Drop the localhost entries once we ship.
#  - 5173 → Vite default (npm run dev:client)
#  - 3010 → Meko's Express dev server (npm run dev — proxies /api/v1)
DEFAULT_CORS_ORIGINS: tuple[str, ...] = (
    "https://app.mekodev.com",
    "https://cloud.mekodata.ai",
)

# "http://localhost:5173",
# "http://localhost:3010",

# Per the SessionStart hook from the meko MCP server: always use "agent".
DEFAULT_AGENT_ID = "meko"

# Sliding-window size for in-memory session history.
# Override via `--max-history` flag or `INFERENCE_MAX_HISTORY` env var.
DEFAULT_MAX_HISTORY_MESSAGES = 15

# Top-K passed to MCP `memory_search` per turn.
DEFAULT_MEMORY_TOP_K = 5


@dataclass(frozen=True)
class AppConfig:
    """Immutable gateway configuration. Built once at startup."""

    llm_provider: str            # "openai" | "bedrock"
    llm_model: str
    mcp_endpoint: str
    aws_region: str
    port: int
    cors_origins: tuple[str, ...]
    agent_id: str
    max_history: int
    memory_top_k: int
