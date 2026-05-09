"""Chat-turn orchestration.

`run_chat_turn` executes the agreed flow for one user message:

    1. Ensure the session has an MCP `conversation_id`
       (lazily create on the first turn via `conversation_create`).
    2. **PARALLEL** retrieval — `asyncio.gather`:
         - `memory_search` (Mem0 long-term)
         - `knowledgebase_search` (per-datapack RAG; skipped if no datapack_id)
    3. Load the last ≤N prior messages from the session store.
    4. LLM call with: system + memory_context + kb_context + history + user.
    5. Persistence:
         - `conversation_add_message` is awaited (fast, ~0.5s; guarantees
           the new conversation row is in Langfuse before the right-rail
           panel refetches).
         - `memory_add` is **fire-and-forget**: scheduled as a background
           task and NOT awaited. Mem0's internal LLM-based fact extractor
           takes ~30-60s per turn — blocking the user's reply on it
           multiplies user-perceived latency. Failures still log via the
           helper's own try/except; they just don't reach the request.
    6. Append `(user, assistant)` to the session store.

Search and persistence calls are best-effort: a failure logs a warning but
does not break the chat. The two MCP calls we cannot tolerate failing are
`conversation_create` (we need its UUID for trace nesting) and the LLM
call itself.
"""

from __future__ import annotations

import asyncio
import json
import logging
from typing import Any, Awaitable, Dict, List, NamedTuple, Optional, Set

from langchain_core.language_models import BaseChatModel
from langchain_core.messages import AIMessage, BaseMessage, HumanMessage, SystemMessage
from langchain_core.tools import BaseTool

from .config import AppConfig
from .session_store import InMemorySessionStore

logger = logging.getLogger(__name__)


# Strong references to fire-and-forget background tasks (currently used for
# the slow `memory_add` Mem0-extraction call). Without keeping a reference,
# Python's GC can collect a pending task and silently cancel it. We add a
# done-callback so the set self-cleans as tasks complete.
_BACKGROUND_TASKS: Set[asyncio.Task] = set()


def _spawn_background(coro: Awaitable[Any]) -> asyncio.Task:
    """Schedule `coro` to run on the event loop without blocking the caller.

    The task survives the originating request — failures inside the coroutine
    log via that coroutine's own try/except (best-effort) rather than
    reaching the user.
    """
    task = asyncio.create_task(coro)
    _BACKGROUND_TASKS.add(task)
    task.add_done_callback(_BACKGROUND_TASKS.discard)
    return task


SYSTEM_TEMPLATE = """You are the Meko playground assistant.

Use the relevant memories and knowledge sources below to ground your answer
when they apply, and ignore them when they don't. Be concise and helpful.

Relevant memories (from `memory_search`):
{memory_context}

Relevant knowledge (from `knowledgebase_search`):
{kb_context}
"""


class ChatTurnResult(NamedTuple):
    """Outcome of a single chat turn.

    `message_id` is the Langfuse message id minted by MCP
    `conversation_add_message`; the UI uses it to deep-link into the
    trace drawer. May be empty when the persistence call missed (e.g. tool
    unavailable, transient MCP error) — best-effort by design.
    """

    assistant_text: str
    message_id: str


async def run_chat_turn(
    *,
    cfg: AppConfig,
    llm: BaseChatModel,
    tools_by_name: Dict[str, BaseTool],
    session_store: InMemorySessionStore,
    session_id: str,
    user_message: str,
    datapack_id: Optional[str],
) -> ChatTurnResult:
    """Run one chat turn end-to-end. Returns the assistant text + Langfuse message_id."""
    # 1. Ensure we have an MCP conversation_id for this session.
    conversation_id = await session_store.get_conversation_id(session_id)
    if conversation_id is None:
        conversation_id = await _create_conversation(
            tools_by_name=tools_by_name,
            agent_id=cfg.agent_id,
            datapack_id=datapack_id,
            title=_truncate(user_message, 80),
        )
        await session_store.set_conversation_id(session_id, conversation_id)
        logger.info(
            "created MCP conversation %s for session %s", conversation_id, session_id,
        )

    # 2. Parallel retrieval: memory_search + knowledgebase_search.
    # Both are best-effort; either failing returns "" via its helper.
    # `_search_knowledge` no-ops when datapack_id is missing (kb_search
    # requires datapack_id; memory_search doesn't).
    memory_context, kb_context = await asyncio.gather(
        _search_memories(
            tools_by_name=tools_by_name,
            agent_id=cfg.agent_id,
            query=user_message,
            conversation_id=conversation_id,
            datapack_id=datapack_id,
            top_k=cfg.memory_top_k,
        ),
        _search_knowledge(
            tools_by_name=tools_by_name,
            agent_id=cfg.agent_id,
            query=user_message,
            conversation_id=conversation_id,
            datapack_id=datapack_id,
            top_k=cfg.memory_top_k,
        ),
    )

    # 3. Load history (≤max_history messages, oldest first).
    history = await session_store.get_messages(session_id)

    # 4. LLM call.
    messages: List[BaseMessage] = [
        SystemMessage(
            content=SYSTEM_TEMPLATE.format(
                memory_context=memory_context or "(none yet)",
                kb_context=kb_context or "(none yet)",
            )
        )
    ]
    messages.extend(_to_lc_message(role, content) for role, content in history)
    messages.append(HumanMessage(content=user_message))

    result = await llm.ainvoke(messages)
    assistant_text = _coerce_text(result.content)

    # 5. Persistence — split because `memory_add` is dramatically slower.
    #
    #   - conversation_add_message: awaited (fast; ~0.5s). We want the row
    #     in Langfuse before we return so the right-rail "Recent
    #     conversations" panel sees it on the post-turn refetch. We also
    #     capture the returned message_id so the UI can deep-link into the
    #     trace drawer for this turn.
    #   - memory_add: fire-and-forget. Mem0's internal LLM-based fact
    #     extractor takes ~30-60s, which would otherwise dominate the
    #     user-perceived latency. The background task survives this
    #     request; failures inside `_add_memory` log warnings but never
    #     surface as errors to the user.
    message_id = await _add_message(
        tools_by_name=tools_by_name,
        conversation_id=conversation_id,
        agent_id=cfg.agent_id,
        user_message=user_message,
        assistant_message=assistant_text,
        datapack_id=datapack_id,
    )
    _spawn_background(
        _add_memory(
            tools_by_name=tools_by_name,
            conversation_id=conversation_id,
            agent_id=cfg.agent_id,
            user_message=user_message,
            assistant_message=assistant_text,
            datapack_id=datapack_id,
        )
    )

    # 6. Update local session state.
    await session_store.append(session_id, "user", user_message)
    await session_store.append(session_id, "assistant", assistant_text)

    return ChatTurnResult(assistant_text=assistant_text, message_id=message_id)


# --------------------------------------------------------------------------- #
# MCP tool wrappers
# --------------------------------------------------------------------------- #


async def _create_conversation(
    *,
    tools_by_name: Dict[str, BaseTool],
    agent_id: str,
    datapack_id: Optional[str],
    title: str,
) -> str:
    """Mint a new MCP conversation_id. Required — failure raises."""
    tool = _require_tool(tools_by_name, "conversation_create")
    args: Dict[str, Any] = {
        "scope": "write",
        "agent_id": agent_id,
        "title": title,
    }
    if datapack_id:
        args["datapack_id"] = datapack_id
    raw = await tool.ainvoke(args)
    payload = _parse_json(raw)
    # The MCP server may surface the new UUID under several keys depending on
    # version. Try the common ones; fall back to a clear error.
    if isinstance(payload, dict):
        for key in ("conversation_id", "id", "uuid", "session_id"):
            value = payload.get(key)
            if isinstance(value, str) and value:
                return value
    raise RuntimeError(f"conversation_create did not return a UUID: {payload!r}")


async def _search_memories(
    *,
    tools_by_name: Dict[str, BaseTool],
    agent_id: str,
    query: str,
    conversation_id: str,
    datapack_id: Optional[str],
    top_k: int,
) -> str:
    """Run `memory_search` and format its results as a bulleted context block."""
    tool = tools_by_name.get("memory_search")
    if tool is None:
        logger.warning("memory_search tool unavailable; skipping memory grounding")
        return ""
    args: Dict[str, Any] = {
        "scope": "read",
        "query": query,
        "conversation_id": conversation_id,
        "agent_id": agent_id,
        "limit": top_k,
    }
    if datapack_id:
        args["datapack_id"] = datapack_id
    try:
        raw = await tool.ainvoke(args)
    except Exception as exc:  # noqa: BLE001 — MCP failure must not break chat
        logger.warning("memory_search failed: %s", exc)
        return ""
    return _format_memory_context(_parse_json(raw))


async def _search_knowledge(
    *,
    tools_by_name: Dict[str, BaseTool],
    agent_id: str,
    query: str,
    conversation_id: str,
    datapack_id: Optional[str],
    top_k: int,
) -> str:
    """Run `knowledgebase_search` and format hits as a bulleted context block.

    `knowledgebase_search` requires `datapack_id`; we no-op when missing
    (e.g. user is on the Welcome / Playground top page) instead of
    raising — KB grounding is "nice to have," not load-bearing.
    """
    if not datapack_id:
        return ""
    tool = tools_by_name.get("knowledgebase_search")
    if tool is None:
        logger.warning("knowledgebase_search tool unavailable; skipping KB grounding")
        return ""
    args: Dict[str, Any] = {
        "scope": "read",
        "query": query,
        "conversation_id": conversation_id,
        "datapack_id": datapack_id,
        "agent_id": agent_id,
        "limit": top_k,
    }
    try:
        raw = await tool.ainvoke(args)
    except Exception as exc:  # noqa: BLE001
        logger.warning("knowledgebase_search failed: %s", exc)
        return ""
    return _format_knowledge_context(_parse_json(raw))


async def _add_message(
    *,
    tools_by_name: Dict[str, BaseTool],
    conversation_id: str,
    agent_id: str,
    user_message: str,
    assistant_message: str,
    datapack_id: Optional[str],
) -> str:
    """Append the turn to the conversation. Returns the Langfuse `message_id`
    minted by MCP (used by the UI to deep-link into the trace drawer).

    Best-effort: a missing tool or a tool failure logs and returns "" — the
    chat turn still succeeds; the user just doesn't get a trace deep-link
    for that one message.
    """
    tool = tools_by_name.get("conversation_add_message")
    if tool is None:
        logger.warning(
            "conversation_add_message tool unavailable; turn won't be persisted",
        )
        return ""
    args: Dict[str, Any] = {
        "scope": "write",
        "conversation_id": conversation_id,
        "agent_id": agent_id,
        "input": user_message,
        "output": assistant_message,
    }
    if datapack_id:
        args["datapack_id"] = datapack_id
    try:
        raw = await tool.ainvoke(args)
    except Exception as exc:  # noqa: BLE001
        logger.warning("conversation_add_message failed: %s", exc)
        return ""
    payload = _parse_json(raw)
    if isinstance(payload, dict):
        mid = payload.get("message_id")
        if isinstance(mid, str) and mid:
            return mid
    return ""


async def _add_memory(
    *,
    tools_by_name: Dict[str, BaseTool],
    conversation_id: str,
    agent_id: str,
    user_message: str,
    assistant_message: str,
    datapack_id: Optional[str],
) -> None:
    """Hand Mem0 the full turn so its extractor can mine facts.

    We pass the full {user, assistant} pair as the `messages` JSON
    parameter (per the MCP `memory_add` docstring, this overrides the
    `text` field). Mem0 runs its own LLM extraction internally — we don't
    pre-distill facts on the gateway. Best-effort — failure logs only.
    """
    tool = tools_by_name.get("memory_add")
    if tool is None:
        logger.warning("memory_add tool unavailable; skipping fact extraction")
        return
    messages_json = json.dumps(
        [
            {"role": "user", "content": user_message},
            {"role": "assistant", "content": assistant_message},
        ]
    )
    args: Dict[str, Any] = {
        "scope": "write",
        # `text` is required by the tool signature; `messages` overrides it
        # but the validation `if not (text or messages)` still wants one
        # truthy value. Pass the user message so the validation also
        # passes if the server rejects empty `text`.
        "text": user_message,
        "messages": messages_json,
        "conversation_id": conversation_id,
        "agent_id": agent_id,
    }
    if datapack_id:
        args["datapack_id"] = datapack_id
    try:
        await tool.ainvoke(args)
    except Exception as exc:  # noqa: BLE001
        logger.warning("memory_add failed: %s", exc)


# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #


def _format_memory_context(payload: object) -> str:
    """Pull a list of memory texts out of whatever `memory_search` returned.

    The shape varies by MCP server version (`{results: [...]}`, `{memories: [...]}`,
    or a bare list). We coerce all of them to a bulleted text block.
    """
    items: list[Any] = []
    if isinstance(payload, dict):
        for key in ("results", "memories", "data"):
            candidate = payload.get(key)
            if isinstance(candidate, list):
                items = candidate
                break
    elif isinstance(payload, list):
        items = payload

    lines: list[str] = []
    for item in items[:20]:  # hard cap regardless of caller's top_k
        if isinstance(item, dict):
            text = (
                item.get("memory")
                or item.get("data")
                or item.get("text")
                or item.get("content")
                or ""
            )
            if text:
                lines.append(f"- {text}")
        elif isinstance(item, str):
            lines.append(f"- {item}")
    return "\n".join(lines)


def _format_knowledge_context(payload: object) -> str:
    """Format `knowledgebase_search` hits as a bulleted block.

    Each KB hit may carry a source filename + a chunk text. Annotate the
    bullet with the source when available so the LLM can attribute back.
    Tolerates `{results: [...]}`, `{matches: [...]}`, `{documents: [...]}`,
    or a bare list — same fallback strategy as `_format_memory_context`.
    """
    items: list[Any] = []
    if isinstance(payload, dict):
        for key in ("results", "matches", "documents", "data"):
            candidate = payload.get(key)
            if isinstance(candidate, list):
                items = candidate
                break
    elif isinstance(payload, list):
        items = payload

    lines: list[str] = []
    for item in items[:20]:
        if isinstance(item, dict):
            text = (
                item.get("text")
                or item.get("content")
                or item.get("snippet")
                or item.get("chunk")
                or ""
            )
            source = (
                item.get("document_name")
                or item.get("source")
                or item.get("source_uri")
                or ""
            )
            if text:
                if source:
                    # Trim long source paths so the bullet stays readable.
                    short = source.split("/")[-1] if "/" in source else source
                    lines.append(f"- [{short}] {text}")
                else:
                    lines.append(f"- {text}")
        elif isinstance(item, str):
            lines.append(f"- {item}")
    return "\n".join(lines)


def _to_lc_message(role: str, content: str) -> BaseMessage:
    if role == "user":
        return HumanMessage(content=content)
    if role == "assistant":
        return AIMessage(content=content)
    return SystemMessage(content=content)


def _truncate(s: str, n: int) -> str:
    s = (s or "").strip()
    return s if len(s) <= n else s[: n - 1].rstrip() + "…"


def _parse_json(raw: object) -> object:
    """Coerce an MCP tool response into a parsed Python value.

    Handles three observed shapes:

      1. plain JSON string  ──► `json.loads`
      2. already a dict/list (some adapters pre-decode) ──► passthrough
      3. LangChain MCP content-block wrapping:
            [{"type": "text", "text": "<json>", "id": "lc_..."}]
         ──► unwrap and recurse on the inner `text`. This is what
         `langchain-mcp-adapters` returns by default for text-typed tool
         results.
    """
    # Unwrap LangChain MCP content-block format first — only when we see the
    # exact shape, so we don't accidentally swallow a real list payload.
    if (
        isinstance(raw, list)
        and len(raw) >= 1
        and isinstance(raw[0], dict)
        and raw[0].get("type") == "text"
        and isinstance(raw[0].get("text"), str)
    ):
        return _parse_json(raw[0]["text"])

    if isinstance(raw, (dict, list)):
        return raw
    if isinstance(raw, str):
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            return raw
    return raw


def _coerce_text(content: Any) -> str:
    """LangChain message content can be a string or a list of parts; flatten."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        # Common pattern: list of {"type": "text", "text": "..."} blocks.
        parts: list[str] = []
        for chunk in content:
            if isinstance(chunk, dict):
                text = chunk.get("text") or chunk.get("content")
                if isinstance(text, str):
                    parts.append(text)
            elif isinstance(chunk, str):
                parts.append(chunk)
        return "".join(parts)
    return str(content)


def _require_tool(tools_by_name: Dict[str, BaseTool], name: str) -> BaseTool:
    tool = tools_by_name.get(name)
    if tool is None:
        raise RuntimeError(f"required MCP tool {name!r} not available")
    return tool
