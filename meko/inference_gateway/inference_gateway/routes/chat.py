"""HTTP routes: POST /chat, GET /healthz.

The route is intentionally thin — it owns request parsing, error mapping,
and pulling shared state (LLM, session store, config) off `app.state`.
The actual chat orchestration lives in `chain.run_chat_turn`.
"""

from __future__ import annotations

import json
import logging
import uuid
from typing import Any, Dict, Literal, Optional

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
    persona: Optional[str] = Field(
        default=None,
        description=(
            "Named persona preset selecting this turn's system-prompt framing "
            "(e.g. 'review_agent'). Omitted or unrecognized -> the default "
            "WorkbenchLM playground-assistant persona, unchanged from today."
        ),
    )
    enable_knowledge_base: Optional[bool] = Field(
        default=None,
        description=(
            "Per-turn override for whether knowledgebase_search runs. None "
            "(default) reproduces today's behavior unchanged. False force-skips "
            "it for this turn even when it would otherwise run. True is accepted "
            "but is a no-op (cannot force a KB search that wouldn't otherwise "
            "happen, e.g. without datapack_id)."
        ),
    )
    enable_memory_search: Optional[bool] = Field(
        default=None,
        description="Same contract as enable_knowledge_base, for memory_search.",
    )
    focus: Optional[str] = Field(
        default=None,
        max_length=300,
        description=(
            "Optional bounded free-text note on what this turn's review should "
            "pay extra attention to. Rendered into the review_agent persona's "
            "system prompt as an explicitly delimited, advisory-only block -- "
            "never a raw prompt/instruction override (see chain.py's "
            "_render_focus_block). Silently ignored by personas whose template "
            "has no focus placeholder."
        ),
    )
    tone: Optional[Literal["concise", "detailed", "encouraging"]] = Field(
        default=None,
        description=(
            "Closed-set selection for how review feedback is phrased. Maps to a "
            "canned instruction line server-side (see chain.py's TONE_FRAGMENTS) "
            "-- not free text, so unlike `focus` there is no injection surface "
            "here at all. Omitted -> today's default phrasing, unchanged."
        ),
    )
    strictness: Optional[Literal["lenient", "balanced", "strict"]] = Field(
        default=None,
        description=(
            "Closed-set selection for how much to flag. Maps to a canned "
            "instruction line server-side (see chain.py's STRICTNESS_FRAGMENTS). "
            "Omitted -> today's default behavior, unchanged."
        ),
    )


class PendingActionOut(BaseModel):
    """A confirm-tier tool call the model proposed this turn (agentic chat,
    MEKO-407 Phase 2). NOT executed — the UI renders a confirmation card;
    POST /chat/confirm-action executes it with `approve=true` or discards it
    with `approve=false`."""

    action_id: str
    tool_name: str
    args: Dict[str, Any] = {}


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
    # Agentic chat: a proposed action awaiting user confirmation, or null.
    pending_action: Optional[PendingActionOut] = None


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
            user_message=req.message,
            datapack_id=req.datapack_id,
            resume_conversation_id=req.conversation_id,
            bypass_conversation_cache=req.bypass_conversation_cache,
            persona=req.persona,
            enable_knowledge_base=req.enable_knowledge_base,
            enable_memory_search=req.enable_memory_search,
            focus=req.focus,
            tone=req.tone,
            strictness=req.strictness,
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
        pending_action=(
            PendingActionOut(**result.pending_action) if result.pending_action else None
        ),
    )


class ConfirmActionRequest(BaseModel):
    """Body for POST /chat/confirm-action — resolve a pending agentic action."""

    session_id: str = Field(..., min_length=1)
    action_id: str = Field(..., min_length=1)
    user_token: str = Field(..., min_length=1, description="Bearer token forwarded to MCP.")
    approve: bool = Field(default=True, description="False discards the action without executing.")


class ConfirmActionResponse(BaseModel):
    # "executed" | "cancelled" | "error"
    status: str
    detail: str = ""
    # Parsed tool result on success (shape is tool-specific).
    result: Optional[Any] = None


@router.post("/chat/confirm-action", response_model=ConfirmActionResponse)
async def confirm_action(req: ConfirmActionRequest, request: Request) -> ConfirmActionResponse:
    """Execute (or discard) the ONE pending confirm-tier action for a session.

    The action's arguments were frozen — identity already gateway-enforced —
    when the model proposed it; the model has no further say here. A stale
    or unknown action_id is a 404 (e.g. a newer proposal replaced it)."""
    cfg: AppConfig = request.app.state.config
    session_store: InMemorySessionStore = request.app.state.session_store

    action = await session_store.pop_pending_action(req.session_id, req.action_id)
    if action is None:
        raise HTTPException(
            status_code=404,
            detail="No pending action with that id — it may have expired or been replaced.",
        )

    if not req.approve:
        logger.info("confirm-action: user cancelled %s (%s)", action["tool_name"], req.action_id)
        return ConfirmActionResponse(status="cancelled")

    try:
        tools = await build_mcp_tools(cfg.mcp_endpoint, req.user_token)
    except Exception as exc:  # noqa: BLE001
        logger.error("MCP tool discovery failed: %s", exc)
        raise HTTPException(status_code=502, detail=f"MCP tool discovery failed: {exc}") from exc

    tool = tools.get(action["tool_name"])
    if tool is None:
        raise HTTPException(
            status_code=502, detail=f"{action['tool_name']} tool unavailable",
        )

    try:
        raw = await tool.ainvoke(action["args"])
    except Exception as exc:  # noqa: BLE001
        logger.exception("confirm-action: %s failed", action["tool_name"])
        return ConfirmActionResponse(status="error", detail=str(exc))

    payload: Any
    if isinstance(raw, str):
        try:
            payload = json.loads(raw)
        except json.JSONDecodeError:
            payload = raw
    else:
        payload = raw
    if isinstance(payload, dict) and payload.get("error"):
        return ConfirmActionResponse(
            status="error",
            detail=str(payload.get("detail") or payload["error"]),
            result=payload,
        )

    # Best-effort: record the confirmed action as a turn so the conversation
    # (and downstream memory extraction) reflects what actually happened.
    addmsg = tools.get("conversation_add_message")
    conversation_id = action["args"].get("conversation_id") or ""
    if addmsg is not None and conversation_id:
        try:
            await addmsg.ainvoke({
                "conversation_id": conversation_id,
                "agent_id": cfg.agent_id,
                "input": f"[User approved action: {action['tool_name']}]",
                "output": json.dumps(payload, default=str)[:2000],
            })
        except Exception:  # noqa: BLE001
            logger.warning("confirm-action: recording turn failed", exc_info=True)

    logger.info("confirm-action: executed %s (%s)", action["tool_name"], req.action_id)
    return ConfirmActionResponse(status="executed", result=payload)


class IndexConversationRequest(BaseModel):
    """Body for POST /index-conversation — backfill the cache for one past conversation."""

    conversation_id: str = Field(..., min_length=1)
    user_token: str = Field(..., min_length=1)
    datapack_id: Optional[str] = None


class IndexConversationResponse(BaseModel):
    indexed: int = 0
    can_index: bool = True
    reason: str = ""
    # True when the account's conversation_search_embedding quota ran out
    # mid-backfill: `indexed` turns were embedded, the remainder were not.
    quota_exceeded: bool = False


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
    err = payload.get("error")
    if err == "already_indexed":
        # Conversation-level guard on the MCP side: re-indexing would only
        # re-burn embedding quota. Not a server fault — surface as a conflict.
        raise HTTPException(
            status_code=409,
            detail=str(payload.get("detail") or "This conversation is already embedded."),
        )
    if err == "free_tier_limit_reached":
        raise HTTPException(
            status_code=429,
            detail=str(payload.get("detail") or "Conversation embedding limit reached for your plan."),
        )
    if err:
        raise HTTPException(status_code=502, detail=str(payload.get("detail") or err))
    return IndexConversationResponse(
        indexed=int(payload.get("indexed") or 0),
        can_index=bool(payload.get("can_index", True)),
        reason=str(payload.get("reason") or ""),
        quota_exceeded=bool(payload.get("quota_exceeded", False)),
    )
