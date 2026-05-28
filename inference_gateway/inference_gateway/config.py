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
# DEFAULT_MCP_ENDPOINT = "http://localhost:8000/mcp"
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
    # "http://localhost:5173",
    # "http://localhost:3010",
)

# Per the SessionStart hook from the meko MCP server: always use "agent".
DEFAULT_AGENT_ID = "meko"

# Sliding-window size for in-memory session history.
# Override via `--max-history` flag or `INFERENCE_MAX_HISTORY` env var.
#
# Holds entries, NOT turns — each turn writes two (user + assistant), so 6
# entries = 3 full turns of immediate co-reference context. Past ~3 turns
# the marginal coherence benefit drops off; longer-term context is carried
# by `memory_search` + `knowledgebase_search` instead of this window.
# Bumped DOWN from 15 → 6 to trim ~450 input tokens per turn without
# losing the cases that need history (positional follow-ups like "the
# second one", "make that shorter", "why?").
DEFAULT_MAX_HISTORY_MESSAGES = 6

# Top-K passed to MCP `memory_search` and `knowledgebase_search` per turn.
# Bumped from 5 → 10 to give the relevance thresholds (MEMORY_MIN_SCORE,
# KB_MAX_DISTANCE in chain.py) more material to keep the genuinely useful
# hits from — Meko's KB embedding model has under-spread distances, so a
# tight window risks the right chunk landing just outside it. After
# threshold filtering you typically still end up with ~3–5 final hits,
# and the prompt cost of the extra raw rows is negligible.
DEFAULT_MEMORY_TOP_K = 50

# Max output tokens we ask the LLM to budget for. Bedrock's `Converse`
# API and OpenAI's chat completions both honor this — when the model
# would exceed it, generation stops with `stopReason == "max_tokens"`,
# which presents to the user as a mid-sentence cutoff.
#
# 8,192 is roughly Claude Sonnet 4.5's default for Bedrock, but we set
# it explicitly so the gateway's behavior doesn't change silently when
# AWS bumps a default. Tune up (Sonnet supports much higher with
# extended thinking enabled) if you see legitimate long-form answers
# getting cut off; tune down if you want to bound the per-turn cost.
DEFAULT_LLM_MAX_TOKENS = 8192


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
    llm_max_tokens: int = DEFAULT_LLM_MAX_TOKENS
