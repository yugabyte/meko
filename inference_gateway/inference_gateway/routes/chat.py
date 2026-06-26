"""HTTP routes: POST /chat, GET /healthz.

The route is intentionally thin — it owns request parsing, error mapping,
and pulling shared state (LLM, session store, config) off `app.state`.
The actual chat orchestration lives in `chain.run_chat_turn`.
"""

from __future__ import annotations

import json
import logging
import uuid
from typing import Any, Dict, Optional

from fastapi import APIRouter, HTTPException, Request
from pydantic import BaseModel, Field

from ..chain import run_chat_turn
from ..config import AppConfig
from ..mcp_client import build_mcp_tools
from ..session_store import InMemorySessionStore


class MemoryHitOut(BaseModel):
    """One structured memory hit for the UI's Agent Reasoning panel.

    `kind`: "mem" (vector hit) | "graph" (relation triple) | "promoted"
    (relation elevated to the tenant KB).
    """

    kind: str
    text: str
    score: Optional[float] = None
    char_count: int = 0


class KbHitOut(BaseModel):
    """One KB chunk hit. `score` is a normalized [0..1] similarity (1 -
    cosine_distance, clamped) so the UI can sort consistently with
    memory hits; `distance` is the raw value for debugging."""

    document_id: str
    chunk_text: str
    score: Optional[float] = None
    distance: Optional[float] = None
    char_count: int = 0


class ProcessingStepOut(BaseModel):
    """One pipeline step with timing — drives the per-bubble
    observability timeline. `status`: "ok" | "error" | "skipped".

    `query` is populated for retrieval steps (`memory_search`,
    `knowledgebase_search`) — the exact string the gateway sent to MCP.
    None for non-retrieval steps.

    `tool_args` is the rest of the args sent to MCP for this step —
    everything except `query`, which has its own dedicated field. The
    full payload (scope, conversation_id, agent_id, limit, datapack_id)
    is surfaced so the UI's "MCP args" sub-expand is an honest "what
    we sent" view. None when the step has no extra args worth showing."""

    name: str
    duration_ms: int
    status: str
    detail: str = ""
    query: Optional[str] = None
    tool_args: Optional[Dict[str, Any]] = None

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
    conversation_id: Optional[str] = Field(
        default=None,
        description=(
            "Resume an existing conversation. When set, the gateway adopts "
            "this conversation_id instead of minting a new one via MCP — "
            "every turn of a resumed chat appends to the same conversation "
            "row in Langfuse. Set by the UI when the user picked a previous "
            "chat from the dropdown; absent for fresh chats."
        ),
    )
    bypass_conversation_cache: bool = Field(
        default=False,
        description=(
            "When true, skip the conversation-history cache for this turn and "
            "answer via the full memory + KB + LLM path. Set by the UI's "
            "'Regenerate with full search' button after a cache-hit answer."
        ),
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
    # Bulleted blocks of retrieval context that fed the LLM's system prompt.
    # Kept for "show me the exact prompt text" UX / debugging; the
    # structured `memory_hits` / `kb_hits` below are what the UI's Agent
    # Reasoning panel renders. Empty when retrieval was skipped (trivial
    # query) or returned no hits.
    memory_context: str = ""
    kb_context: str = ""
    # Structured retrieval hits. `memory_hits` covers Mem0's three parallel
    # arrays (vector results, graph relations, promoted relations);
    # `kb_hits` are KB chunks with score + char count. The UI groups
    # KB chunks by `document_id` and shows scores per hit.
    memory_hits: list[MemoryHitOut] = []
    kb_hits: list[KbHitOut] = []
    # Per-turn observability timeline. Each entry is one pipeline stage
    # with a duration and status — the UI renders this as a short list
    # below the structured hits so the user can see what happened in
    # which order without opening the full trace drawer.
    processing_steps: list[ProcessingStepOut] = []


@router.get("/healthz")
async def healthz() -> dict:
    """Liveness probe. Doesn't check MCP/LLM upstream health."""
    return {"status": "ok"}


@router.post("/chat", response_model=ChatResponse)
async def chat(req: ChatRequest, request: Request) -> ChatResponse:
    cfg: AppConfig = request.app.state.config
    llm = request.app.state.llm
    summary_llm = request.app.state.summary_llm
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
            summary_llm=summary_llm,
            tools_by_name=tools,
            session_store=session_store,
            session_id=session_id,
            user_id=req.user_id,
            user_message=req.message,
            datapack_id=req.datapack_id,
            resume_conversation_id=req.conversation_id,
            bypass_conversation_cache=req.bypass_conversation_cache,
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
        memory_context=result.memory_context,
        kb_context=result.kb_context,
        # Convert NamedTuples → Pydantic models. `model_validate` accepts
        # the NamedTuple's positional / `_asdict` form via Pydantic 2's
        # default behavior on dataclass-like inputs.
        memory_hits=[MemoryHitOut(**h._asdict()) for h in result.memory_hits],
        kb_hits=[KbHitOut(**h._asdict()) for h in result.kb_hits],
        processing_steps=[ProcessingStepOut(**s._asdict()) for s in result.processing_steps],
    )


class IndexConversationRequest(BaseModel):
    """Body for POST /index-conversation — backfill the cache for one past conversation."""

    conversation_id: str = Field(..., min_length=1)
    user_token: str = Field(..., min_length=1)
    datapack_id: Optional[str] = None


class IndexConversationResponse(BaseModel):
    indexed: int = 0
    can_index: bool = True
    reason: str = ""


@router.post("/index-conversation", response_model=IndexConversationResponse)
async def index_conversation(req: IndexConversationRequest, request: Request) -> IndexConversationResponse:
    """Embed a past conversation's turns into the cache via the MCP
    conversation_index_history tool (the gateway is the MCP client)."""
    cfg: AppConfig = request.app.state.config
    try:
        tools = await build_mcp_tools(cfg.mcp_endpoint, req.user_token)
    except Exception as exc:  # noqa: BLE001
        logger.error("MCP tool discovery failed: %s", exc)
        raise HTTPException(status_code=502, detail=f"MCP tool discovery failed: {exc}") from exc

    tool = tools.get("conversation_index_history")
    if tool is None:
        raise HTTPException(status_code=502, detail="conversation_index_history tool unavailable")

    args: Dict[str, Any] = {
        "scope": "write",
        "conversation_id": req.conversation_id,
        "agent_id": cfg.agent_id,
    }
    if req.datapack_id:
        args["datapack_id"] = req.datapack_id
    try:
        raw = await tool.ainvoke(args)
    except Exception as exc:  # noqa: BLE001
        logger.exception("index-conversation failed")
        raise HTTPException(status_code=502, detail=f"index failed: {exc}") from exc

    payload = raw if isinstance(raw, dict) else (json.loads(raw) if isinstance(raw, str) and raw.strip() else {})
    if payload.get("error"):
        raise HTTPException(status_code=502, detail=str(payload.get("detail") or payload["error"]))
    return IndexConversationResponse(
        indexed=int(payload.get("indexed") or 0),
        can_index=bool(payload.get("can_index", True)),
        reason=str(payload.get("reason") or ""),
    )
