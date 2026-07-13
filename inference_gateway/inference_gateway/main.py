"""CLI entry + uvicorn boot.

Every flag has an env-var fallback so deployments can configure via either:
  python -m inference_gateway --llm-provider bedrock --port 8088
  INFERENCE_LLM_PROVIDER=bedrock INFERENCE_PORT=8088 python -m inference_gateway

CLI flags win over env when both are set.
"""

from __future__ import annotations

import argparse
import logging
import os
from typing import Sequence

import uvicorn

from .app import create_app
from .config import (
    DEFAULT_AGENT_ID,
    DEFAULT_AWS_REGION,
    DEFAULT_BEDROCK_MODEL,
    DEFAULT_CONV_SUMMARY_BEDROCK_MODEL,
    DEFAULT_CORS_ORIGINS,
    DEFAULT_MAX_HISTORY_MESSAGES,
    DEFAULT_MCP_ENDPOINT,
    DEFAULT_LLM_MAX_TOKENS,
    DEFAULT_MEMORY_TOP_K,
    DEFAULT_OPENAI_MODEL,
    DEFAULT_PORT,
    AppConfig,
)


def parse_args(argv: Sequence[str] | None = None) -> tuple[AppConfig, str]:
    """Build an `AppConfig` from CLI argv + env. Returns config + log level."""
    p = argparse.ArgumentParser(prog="inference-gateway")
    p.add_argument(
        "--llm-provider",
        default=os.getenv("INFERENCE_LLM_PROVIDER", "openai"),
        choices=["openai", "bedrock"],
    )
    p.add_argument(
        "--llm-model",
        default=os.getenv("INFERENCE_LLM_MODEL"),
        help="Provider-specific model id. Defaults to gpt-4o-mini (openai) "
             "or us.amazon.nova-2-lite-v1:0 (bedrock).",
    )
    p.add_argument(
        "--conv-summary-model",
        default=os.getenv("INFERENCE_CONV_SUMMARY_MODEL"),
        help="Small/cheap model used to summarize conversation-history hits on a "
             "cache hit. Defaults to us.amazon.nova-lite-v1:0 (bedrock) or "
             "gpt-4o-mini (openai). Set to the main model to disable the SLM split.",
    )
    p.add_argument(
        "--aws-region",
        default=os.getenv("AWS_REGION", DEFAULT_AWS_REGION),
        help="Bedrock only.",
    )
    p.add_argument(
        "--mcp-endpoint",
        default=os.getenv("INFERENCE_MCP_ENDPOINT", DEFAULT_MCP_ENDPOINT),
    )
    p.add_argument(
        "--port",
        type=int,
        default=int(os.getenv("INFERENCE_PORT", str(DEFAULT_PORT))),
    )
    p.add_argument(
        "--cors-origin",
        action="append",
        dest="cors_origins",
        default=None,
        help="CORS allow-origin, repeatable. Falls back to "
             "INFERENCE_CORS_ORIGINS (comma-separated) or built-in defaults.",
    )
    p.add_argument(
        "--agent-id",
        default=os.getenv("INFERENCE_AGENT_ID", DEFAULT_AGENT_ID),
    )
    p.add_argument(
        "--max-history",
        type=int,
        default=int(os.getenv("INFERENCE_MAX_HISTORY", str(DEFAULT_MAX_HISTORY_MESSAGES))),
        help="Sliding-window size for in-memory session history (messages).",
    )
    p.add_argument(
        "--memory-top-k",
        type=int,
        default=int(os.getenv("INFERENCE_MEMORY_TOP_K", str(DEFAULT_MEMORY_TOP_K))),
        help="Top-K passed to MCP memory_search per turn.",
    )
    p.add_argument(
        "--llm-max-tokens",
        type=int,
        default=int(os.getenv("INFERENCE_LLM_MAX_TOKENS", str(DEFAULT_LLM_MAX_TOKENS))),
        help=(
            "Max output tokens budget passed to the LLM. Bedrock / OpenAI "
            "honor this; exceeding it produces a mid-sentence cutoff."
        ),
    )
    p.add_argument(
        "--log-level",
        default=os.getenv("INFERENCE_LOG_LEVEL", "INFO"),
    )
    p.add_argument(
        "--enable-contextual-prominence-search",
        action=argparse.BooleanOptionalAction,
        default=os.getenv("MEKO_ENABLE_CONTEXTUAL_PROMINENCE_SEARCH", "").strip().lower()
        in ("1", "true", "yes"),
        help="Route retrieval through the MCP server's unified contextual_prominence_search "
             "tool instead of separate knowledgebase_search + memory_search calls. Shares "
             "the MEKO_ENABLE_CONTEXTUAL_PROMINENCE_SEARCH env var with the MCP server.",
    )
    args = p.parse_args(argv)

    # Pick a sensible default model per provider when the user didn't set one.
    if not args.llm_model:
        args.llm_model = (
            DEFAULT_OPENAI_MODEL if args.llm_provider == "openai" else DEFAULT_BEDROCK_MODEL
        )

    # Default the conversation-summary model to a cheap one per provider.
    if not args.conv_summary_model:
        args.conv_summary_model = (
            DEFAULT_OPENAI_MODEL if args.llm_provider == "openai"
            else DEFAULT_CONV_SUMMARY_BEDROCK_MODEL
        )

    if args.cors_origins is None:
        env_origins = os.getenv("INFERENCE_CORS_ORIGINS", "").strip()
        if env_origins:
            origins: tuple[str, ...] = tuple(
                o.strip() for o in env_origins.split(",") if o.strip()
            )
        else:
            origins = DEFAULT_CORS_ORIGINS
    else:
        origins = tuple(args.cors_origins)

    cfg = AppConfig(
        llm_provider=args.llm_provider,
        llm_model=args.llm_model,
        mcp_endpoint=args.mcp_endpoint,
        aws_region=args.aws_region,
        port=args.port,
        cors_origins=origins,
        agent_id=args.agent_id,
        max_history=args.max_history,
        memory_top_k=args.memory_top_k,
        llm_max_tokens=args.llm_max_tokens,
        conv_summary_model=args.conv_summary_model,
        contextual_prominence_search_enabled=args.enable_contextual_prominence_search,
    )
    return cfg, args.log_level


def cli_entry(argv: Sequence[str] | None = None) -> None:
    cfg, log_level = parse_args(argv)
    logging.basicConfig(
        level=getattr(logging, log_level.upper(), logging.INFO),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    app = create_app(cfg)
    # log_config=None keeps uvicorn from clobbering our handler; we lose its
    # default access-log line but that's fine in dev.
    uvicorn.run(app, host="0.0.0.0", port=cfg.port, log_config=None)


if __name__ == "__main__":
    cli_entry()
