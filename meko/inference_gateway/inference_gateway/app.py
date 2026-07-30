"""FastAPI app factory.

Built once per process: stamps `cfg`, the LLM client, and the session store
onto `app.state` so route handlers can pull them off `request.app.state`
without reaching for module-level globals.
"""

from __future__ import annotations

import logging

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware

from .config import AppConfig
from .llm import build_llm, build_summary_llm
from .routes.chat import router as chat_router
from .session_store import InMemorySessionStore

logger = logging.getLogger(__name__)


def create_app(cfg: AppConfig) -> FastAPI:
    app = FastAPI(
        title="Meko Inference Gateway",
        description="LangChain + MCP chat backend for the Meko Playground.",
        version="0.1.0",
    )

    app.add_middleware(
        CORSMiddleware,
        allow_origins=list(cfg.cors_origins),
        allow_credentials=False,  # we don't use cookies; tokens go via JSON body
        allow_methods=["GET", "POST", "OPTIONS"],
        allow_headers=["*"],
    )

    app.state.config = cfg
    app.state.llm = build_llm(cfg)
    app.state.summary_llm = build_summary_llm(cfg)
    app.state.session_store = InMemorySessionStore(
        max_messages_per_session=cfg.max_history,
    )

    app.include_router(chat_router)

    logger.info(
        "inference gateway ready (provider=%s, model=%s, summary_model=%s, mcp=%s, cors=%s)",
        cfg.llm_provider,
        cfg.llm_model,
        cfg.conv_summary_model or cfg.llm_model,
        cfg.mcp_endpoint,
        list(cfg.cors_origins),
    )
    return app
