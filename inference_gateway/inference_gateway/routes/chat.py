"""HTTP routes: POST /chat, GET /healthz.

The route is intentionally thin — it owns request parsing, error mapping,
and pulling shared state (LLM, session store, config) off `app.state`.
The actual chat orchestration lives in `chain.run_chat_turn`.
"""

from __future__ import annotations

import logging
import uuid
from typing import Optional

from fastapi import APIRouter, HTTPException, Request
from pydantic import BaseModel, Field

from ..chain import run_chat_turn
from ..config import AppConfig
from ..mcp_client import build_mcp_tools
from ..session_store import InMemorySessionStore

logger = logging.getLogger(__name__)

router = APIRouter()


class ChatRequest(BaseModel):
    """Body for POST /chat."""

    message: str = Field(..., min_length=1, description="User prompt.")
    user_token: str = Field(..., min_length=1, description="Bearer token forwarded to MCP.")
    user_id: str = Field(..., min_length=1, description="Authenticated account id (echoed for telemetry).")
    session_id: Optional[str] = Field(
        default=None,
        description="Client-supplied session id. Generated if absent.",
    )
    datapack_id: Optional[str] = Field(
        default=None,
        description="Optional datapack scope. When null, MCP uses the caller's default datapack.",
    )


class ChatResponse(BaseModel):
    """Body of a successful POST /chat response."""

    message: str
    session_id: str
    conversation_id: str
    # Langfuse message_id for this assistant turn — empty when persistence
    # didn't return one (best-effort). The UI uses it to deep-link into the
    # trace drawer for this specific message.
    message_id: str = ""


@router.get("/healthz")
async def healthz() -> dict:
    """Liveness probe. Doesn't check MCP/LLM upstream health."""
    return {"status": "ok"}


@router.post("/chat", response_model=ChatResponse)
async def chat(req: ChatRequest, request: Request) -> ChatResponse:
    cfg: AppConfig = request.app.state.config
    llm = request.app.state.llm
    session_store: InMemorySessionStore = request.app.state.session_store

    session_id = req.session_id or uuid.uuid4().hex

    try:
        tools = await build_mcp_tools(cfg.mcp_endpoint, req.user_token)
    except Exception as exc:  # noqa: BLE001
        logger.error("MCP tool discovery failed: %s", exc)
        raise HTTPException(
            status_code=502,
            detail=f"MCP tool discovery failed: {exc}",
        ) from exc

    try:
        result = await run_chat_turn(
            cfg=cfg,
            llm=llm,
            tools_by_name=tools,
            session_store=session_store,
            session_id=session_id,
            user_message=req.message,
            datapack_id=req.datapack_id,
        )
    except RuntimeError as exc:
        # Raised by chain when a required MCP tool is missing or returns
        # something we can't parse — that's an upstream/contract issue.
        logger.error("chat turn failed: %s", exc)
        raise HTTPException(status_code=502, detail=str(exc)) from exc
    except Exception as exc:  # noqa: BLE001
        logger.exception("chat turn failed unexpectedly")
        raise HTTPException(status_code=500, detail=f"chat turn failed: {exc}") from exc

    conversation_id = await session_store.get_conversation_id(session_id) or ""

    return ChatResponse(
        message=result.assistant_text,
        session_id=session_id,
        conversation_id=conversation_id,
        message_id=result.message_id,
    )
