"""Runtime config for the gateway.

Defaults live here so `argparse` and `app.py` agree on a single source of truth.
Frozen dataclass so configuration is immutable after startup.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Optional


# Provider defaults — reused by argparse and tests.
DEFAULT_OPENAI_MODEL = "gpt-4o-mini"
DEFAULT_BEDROCK_MODEL = "us.amazon.nova-2-lite-v1:0"

# Small, cheap model used to summarize conversation-history chunks on a cache
# hit (separate from the main chat model). Nova Lite is the cost/quality sweet
# spot on Bedrock; override with --conv-summary-model / INFERENCE_CONV_SUMMARY_MODEL.
DEFAULT_CONV_SUMMARY_BEDROCK_MODEL = "mistral.ministral-3-3b-instruct"

# MCP / network defaults.
DEFAULT_MCP_ENDPOINT = "https://mcp.mekodata.ai/mcp"
DEFAULT_PORT = 8088
DEFAULT_AWS_REGION = "us-east-1"

# CORS — empty: no cross-origin callers unless configured. The Meko UI calls the
# gateway same-origin at /inference/ (its nginx in-cluster, its Express server in
# dev), so it needs no entry here. This used to list the hosted origins, which
# meant every deployment that left INFERENCE_CORS_ORIGINS unset — every BYOC data
# plane — trusted app.mekodev.com and cloud.mekodata.ai. Hosted sets it explicitly.
# Override with `--cors-origin ...` flags or `INFERENCE_CORS_ORIGINS=` env
# (comma-separated).
DEFAULT_CORS_ORIGINS: tuple[str, ...] = ()

# Per the SessionStart hook from the meko MCP server: always use "agent".
DEFAULT_AGENT_ID = "WorkbenchLM"

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
DEFAULT_MEMORY_TOP_K = 5

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

# --- Agentic chat (MEKO-407, Phase 0) ---------------------------------------
#
# When MEKO_AGENTIC_CHAT is enabled, the main LLM call gets a curated set of
# READ-ONLY Meko MCP tools bound to it, so intents that preset retrieval can't
# satisfy ("list all my memories", "describe datapack X") are answered by the
# model calling the right tool instead of a semantic search. Plain Q&A is
# unaffected: if the model makes no tool call, the turn costs exactly what it
# does today. Retrieval (contextual prominence search / KB+memory) still runs
# first, unchanged.
#
# Auto-executed tools. Reads, plus ONE write: `memory_add` (Phase 1), which
# the guidance restricts to explicit save intent ("remember this") — the MCP
# server's extraction already captures conversationally-stated facts, so an
# eager memory_add would only write duplicates. No artifact tools (WorkbenchLM
# has no artifacts UX, and artifact_get's large-file path writes to the
# gateway pod's filesystem). Retrieval tools the gateway already ran
# (contextual_prominence_search) are deliberately absent so the model
# doesn't re-run them.
DEFAULT_AGENT_TOOLS: tuple[str, ...] = (
    "memory_get_all",
    "memory_get_by_id",
    "memory_search",
    "memory_add",
    "knowledgebase_search",
    "conversation_get",
    "conversation_list",
    "datapack_list",
    "datapack_describe",
)

# Confirm-tier tools (Phase 2): the model may PROPOSE these, but the gateway
# never executes them on the model's say-so — the turn returns a
# `pending_action` the user must approve in the UI (POST /chat/confirm-action).
#
# Every call is pinned to the datapack the WorkbenchLM chat is scoped to
# (datapack_id is gateway-forced on all agent tool calls), and the Meko API
# underneath enforces the caller's grant (e.g. memory_promote requires
# owner/maintainer and 403s otherwise). Deliberately absent:
# datapack_create (a WorkbenchLM chat is already scoped to one datapack —
# creating new ones doesn't fit this surface), memory_update (deferred),
# and the highest-blast-radius tools (memory_delete_all, datapack_delete).
DEFAULT_AGENT_CONFIRM_TOOLS: tuple[str, ...] = (
    "memory_delete_by_id",
    "memory_promote",
    "datapack_update",
)

# AMP (Postgres databases) tool sets, appended to the two tiers above ONLY
# when SHOW_PG_AMP is true — the same env var that gates AMP in meko-ui,
# api-server, and the MCP server's own tool registration, so the gate
# composes at both layers (a flag-off MCP deployment doesn't list these
# tools at all; a flag-off gateway won't offer them even if listed).
AMP_AGENT_READ_TOOLS: tuple[str, ...] = (
    "amp_cluster_list", "amp_cluster_get",
    "amp_branch_list", "amp_branch_get",
    "amp_compute_list", "amp_compute_get",
)
AMP_AGENT_CONFIRM_TOOLS: tuple[str, ...] = (
    "amp_cluster_create", "amp_cluster_delete",
    "amp_branch_create", "amp_branch_delete",
    "amp_compute_create", "amp_compute_delete",
)

# Max tool-execution rounds per turn. Each round is one extra LLM call; the
# cap bounds worst-case latency/cost. After the cap, the model is forced to
# answer with whatever it has.
DEFAULT_MAX_TOOL_ROUNDS = 3


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
    # Small model for summarizing conversation-history hits. None → reuse the
    # main llm_model (still works, just not as cheap).
    conv_summary_model: Optional[str] = None
    # When True, retrieval runs the MCP server's unified `contextual_prominence_search`
    # tool (one call covering conversation cache + memory + knowledgebase) instead of
    # the separate `knowledgebase_search` + `memory_search` calls. Shares the literal
    # env var name (MEKO_ENABLE_CONTEXTUAL_PROMINENCE_SEARCH) with the MCP server's own
    # flag so one setting toggles both sides of the integration together. Default off.
    contextual_prominence_search_enabled: bool = False
    # Agentic chat (MEKO-407): bind agent_tools (auto-executed) and
    # agent_confirm_tools (propose-only; user must approve via
    # /chat/confirm-action) onto the main LLM call so the model can satisfy
    # tool-shaped intents itself. Gated by MEKO_AGENTIC_CHAT; default off.
    # Retrieval is unchanged either way. main.py appends the AMP tool sets
    # to both tiers when SHOW_PG_AMP is true.
    agentic_chat_enabled: bool = False
    agent_tools: tuple[str, ...] = DEFAULT_AGENT_TOOLS
    agent_confirm_tools: tuple[str, ...] = DEFAULT_AGENT_CONFIRM_TOOLS
    max_tool_rounds: int = DEFAULT_MAX_TOOL_ROUNDS
