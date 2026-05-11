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
import re
import time
from typing import Any, Awaitable, Dict, List, NamedTuple, Optional, Set, Tuple

from langchain_core.language_models import BaseChatModel
from langchain_core.messages import AIMessage, BaseMessage, HumanMessage, SystemMessage
from langchain_core.tools import BaseTool

from .config import AppConfig
from .session_store import InMemorySessionStore

logger = logging.getLogger(__name__)


# --------------------------------------------------------------------------- #
# Retrieval relevance tuning
# --------------------------------------------------------------------------- #
#
# `memory_search` and `knowledgebase_search` both return top-K results sorted
# by vector similarity, with no built-in relevance floor — so for a vague
# query like "hi" they happily return five basically-unrelated hits and we
# render them as if they were grounding context. These two thresholds clip
# the long tail in the formatters: anything weaker gets dropped before the
# LLM (or the Agent Reasoning panel) sees it.
#
# Tune from observation:
#   - Bump `MEMORY_MIN_SCORE` up if you see Mem0 hits the agent shouldn't have used.
#   - Lower it if relevant memories disappear from the panel.
#   - Same logic mirror-flipped for `KB_MAX_DISTANCE`.

# Mem0 score is cosine similarity in [0, 1] — higher is better. 0.3 is
# conservative: drops near-orthogonal hits (the "hi" failure mode) while
# keeping moderately-related hits like "Name is Raj" → "what does Raj like"
# (score ~0.34) which are useful even though their score is modest.
MEMORY_MIN_SCORE = 0.3

# pgvector cosine distance is in [0, 2] — lower is better. Empirically
# Meko's KB returns distances clustered near 1.0 even for clearly-relevant
# chunks (we captured 1.0166 for a hit that was textually a perfect match),
# probably because the embedding model produces under-spread distances on
# short queries. So this threshold is intentionally permissive — it guards
# against truly orthogonal noise (~1.5+) while keeping the cluster-near-1.0
# real hits. Tune DOWN if you see KB junk leak through; the memory-score
# threshold does most of the relevance work today because Mem0 scores are
# better-spread than KB distances.
KB_MAX_DISTANCE = 1.2


# Greetings / acknowledgements that don't need any retrieval grounding —
# we'd rather skip the two MCP round-trips than ask the agent to ground
# "hi" against five random memories. Anything beyond this list still
# hits both searches normally.
_TRIVIAL_QUERY_RX = re.compile(
    r"^\s*(hi|hello|hey|hiya|howdy|thanks|thank you|ok|okay|cool|sounds good|bye|goodbye|yes|no|yep|nope)\b[.!?]*\s*$",
    re.I,
)
# Minimum length below which we also skip retrieval. Even non-greeting
# 1-2 char inputs ("ok", "k", "y") embed too weakly to produce useful
# similarity. 4 chars feels right — "what?" passes, "what" passes, "hi"
# doesn't. Tune if real questions start getting skipped.
_MIN_QUERY_LENGTH_FOR_RETRIEVAL = 4


# Defensive backstop on how many hits we'll iterate over when extracting
# structured rows from a search response — only kicks in if the MCP
# server ignores our `limit` and returns an unbounded list. The normal
# path is bounded by the caller's `top_k` (currently `cfg.memory_top_k`,
# default 10). Generous so legitimate top_k bumps don't get silently
# truncated; tight enough that a misbehaving server can't blow up the
# prompt with thousands of rows.
MAX_RETRIEVAL_ITEMS = 50


def _is_trivial_query(message: str) -> bool:
    """Decide whether a user message is too short / too generic to retrieve for.

    Two checks, either of which short-circuits retrieval:
      1. The trimmed message matches a known greeting / acknowledgement.
      2. The trimmed message is shorter than the min-length floor.

    Returns True when we should skip both `memory_search` and `knowledgebase_search`.
    """
    stripped = (message or "").strip()
    if len(stripped) < _MIN_QUERY_LENGTH_FOR_RETRIEVAL:
        return True
    if _TRIVIAL_QUERY_RX.match(stripped):
        return True
    return False


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


class MemoryHit(NamedTuple):
    """One structured memory hit for the UI's Agent Reasoning panel.

    `kind` separates Mem0's three parallel arrays so the UI can label them:
        "mem"       — vector-matched fact (from `results`)
        "graph"     — Apache AGE graph triple (from `relations`)
        "promoted"  — relation elevated to the tenant KB (from `promoted_relations`)

    `score` is Mem0's cosine-similarity score [0..1]; None for graph triples
    (relations aren't scored). `char_count` is a cheap proxy for token cost
    — we don't have a tokenizer in the gateway so we surface length instead.
    """

    kind: str
    text: str
    score: Optional[float] = None
    char_count: int = 0


class KbHit(NamedTuple):
    """One structured KB chunk hit for the UI's Agent Reasoning panel.

    `score` is the similarity in [0..1] derived from `distance` (cosine
    distance in [0..2], lower = better) so the UI can show a consistent
    "higher = better" number alongside memory hits. `distance` is the raw
    pgvector value, kept for advanced debugging / future tuning.
    """

    document_id: str
    chunk_text: str
    score: Optional[float] = None
    distance: Optional[float] = None
    char_count: int = 0


class ProcessingStep(NamedTuple):
    """One operation in the per-turn observability timeline.

    `name` matches the MCP tool / pipeline stage the user can correlate
    against the full Langfuse trace. `status`:
        "ok"      — completed successfully
        "error"   — raised; chain continued in best-effort mode
        "skipped" — short-circuited (trivial query, missing datapack id,
                    fire-and-forget background task, cached conversation id)
    """

    name: str
    duration_ms: int
    status: str
    detail: str = ""


class ChatTurnResult(NamedTuple):
    """Outcome of a single chat turn.

    `message_id` is the Langfuse message id minted by MCP
    `conversation_add_message`; the UI uses it to deep-link into the
    trace drawer. May be empty when the persistence call missed (e.g. tool
    unavailable, transient MCP error) — best-effort by design.

    `memory_context` / `kb_context` are the bulleted blocks we feed into the
    system prompt. Surfaced back so callers / tests can see the exact text
    the LLM saw. The structured `memory_hits` / `kb_hits` lists are what
    the UI's Agent Reasoning panel renders.

    `processing_steps` is an ordered timeline of pipeline stages with
    durations — drives the per-bubble observability summary so the user
    can see at a glance which step took how long without opening the full
    trace drawer.
    """

    assistant_text: str
    message_id: str
    memory_context: str = ""
    kb_context: str = ""
    memory_hits: Tuple[MemoryHit, ...] = ()
    kb_hits: Tuple[KbHit, ...] = ()
    processing_steps: Tuple[ProcessingStep, ...] = ()


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
    """Run one chat turn end-to-end. Returns the assistant text + Langfuse
    message_id + structured retrieval hits + a processing-steps timeline
    the UI's Agent Reasoning panel renders."""
    # Step list grows in invocation order as the pipeline runs; surfaces
    # to the UI as the per-turn observability timeline.
    steps: List[ProcessingStep] = []

    # 1. Ensure we have an MCP conversation_id for this session.
    conversation_id = await session_store.get_conversation_id(session_id)
    if conversation_id is None:
        step_start = time.monotonic()
        try:
            conversation_id = await _create_conversation(
                tools_by_name=tools_by_name,
                agent_id=cfg.agent_id,
                datapack_id=datapack_id,
                title=_truncate(user_message, 80),
            )
        except Exception as exc:
            steps.append(
                ProcessingStep(
                    "conversation_create",
                    int((time.monotonic() - step_start) * 1000),
                    "error",
                    str(exc),
                )
            )
            raise
        steps.append(
            ProcessingStep(
                "conversation_create",
                int((time.monotonic() - step_start) * 1000),
                "ok",
            )
        )
        await session_store.set_conversation_id(session_id, conversation_id)
        logger.info(
            "created MCP conversation %s for session %s", conversation_id, session_id,
        )

    # 2. Parallel retrieval: memory_search + knowledgebase_search.
    # Both are best-effort; either failing returns [] via its helper and
    # records its own step (status=error / skipped / ok). `_search_knowledge`
    # no-ops when datapack_id is missing.
    #
    # Short-circuit for trivial queries ("hi", "thanks", 2-char inputs):
    # the embedding of a generic greeting is too vague to produce useful
    # hits — vector search returns top-K regardless, so we'd just feed the
    # LLM five random memories. Skip both round-trips entirely and record
    # the skip explicitly so the UI shows it.
    if _is_trivial_query(user_message):
        logger.info(
            "skipping retrieval for trivial query session=%s len=%d",
            session_id, len(user_message.strip()),
        )
        memory_hits: List[MemoryHit] = []
        kb_hits: List[KbHit] = []
        steps.append(ProcessingStep("memory_search", 0, "skipped", "trivial query"))
        steps.append(ProcessingStep("knowledgebase_search", 0, "skipped", "trivial query"))
    else:
        memory_hits, kb_hits = await asyncio.gather(
            _search_memories(
                tools_by_name=tools_by_name,
                agent_id=cfg.agent_id,
                query=user_message,
                conversation_id=conversation_id,
                datapack_id=datapack_id,
                top_k=cfg.memory_top_k,
                steps=steps,
            ),
            _search_knowledge(
                tools_by_name=tools_by_name,
                agent_id=cfg.agent_id,
                query=user_message,
                conversation_id=conversation_id,
                datapack_id=datapack_id,
                top_k=cfg.memory_top_k,
                steps=steps,
            ),
        )

    # Derive the bulleted system-prompt blocks from the structured hits.
    # The structured lists are the source of truth; the bullets are the
    # specific rendering for the LLM.
    memory_context = _memory_hits_to_bullets(memory_hits)
    kb_context = _kb_hits_to_bullets(kb_hits)

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

    llm_start = time.monotonic()
    try:
        result = await llm.ainvoke(messages)
    except Exception as exc:
        steps.append(
            ProcessingStep(
                "llm_invoke",
                int((time.monotonic() - llm_start) * 1000),
                "error",
                str(exc),
            )
        )
        raise
    steps.append(
        ProcessingStep("llm_invoke", int((time.monotonic() - llm_start) * 1000), "ok")
    )
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
    #     surface as errors to the user. Recorded as "skipped" in the
    #     timeline since we don't await it.
    addmsg_start = time.monotonic()
    try:
        message_id = await _add_message(
            tools_by_name=tools_by_name,
            conversation_id=conversation_id,
            agent_id=cfg.agent_id,
            user_message=user_message,
            assistant_message=assistant_text,
            datapack_id=datapack_id,
        )
        steps.append(
            ProcessingStep(
                "conversation_add_message",
                int((time.monotonic() - addmsg_start) * 1000),
                "ok",
            )
        )
    except Exception as exc:
        # `_add_message` is best-effort but doesn't currently raise — keep
        # this branch defensively in case its error contract changes.
        steps.append(
            ProcessingStep(
                "conversation_add_message",
                int((time.monotonic() - addmsg_start) * 1000),
                "error",
                str(exc),
            )
        )
        message_id = ""
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
    steps.append(
        ProcessingStep("memory_add", 0, "skipped", "background task — see trace")
    )

    # 6. Update local session state.
    await session_store.append(session_id, "user", user_message)
    await session_store.append(session_id, "assistant", assistant_text)

    return ChatTurnResult(
        assistant_text=assistant_text,
        message_id=message_id,
        memory_context=memory_context,
        kb_context=kb_context,
        memory_hits=tuple(memory_hits),
        kb_hits=tuple(kb_hits),
        processing_steps=tuple(steps),
    )


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
    steps: Optional[List[ProcessingStep]] = None,
) -> List[MemoryHit]:
    """Run `memory_search` and return the structured hits.

    Best-effort: a missing tool / transient failure logs a warning and
    returns []. Appends a step to `steps` if provided so the per-turn
    timeline reflects what actually happened (ok / skipped / error).
    """
    start = time.monotonic()
    tool = tools_by_name.get("memory_search")
    if tool is None:
        logger.warning("memory_search tool unavailable; skipping memory grounding")
        if steps is not None:
            steps.append(ProcessingStep("memory_search", 0, "skipped", "tool unavailable"))
        return []
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
        if steps is not None:
            steps.append(
                ProcessingStep(
                    "memory_search",
                    int((time.monotonic() - start) * 1000),
                    "error",
                    str(exc),
                )
            )
        return []
    hits = _extract_memory_hits(_parse_json(raw))
    if steps is not None:
        steps.append(
            ProcessingStep(
                "memory_search",
                int((time.monotonic() - start) * 1000),
                "ok",
                f"{len(hits)} hit{'' if len(hits) == 1 else 's'}",
            )
        )
    return hits


async def _search_knowledge(
    *,
    tools_by_name: Dict[str, BaseTool],
    agent_id: str,
    query: str,
    conversation_id: str,
    datapack_id: Optional[str],
    top_k: int,
    steps: Optional[List[ProcessingStep]] = None,
) -> List[KbHit]:
    """Run `knowledgebase_search` and return structured hits.

    Skips entirely when `datapack_id` is missing (kb_search requires it)
    — recorded as "skipped" in the step list. Same best-effort semantics
    as `_search_memories` on tool failures.
    """
    start = time.monotonic()
    if not datapack_id:
        if steps is not None:
            steps.append(ProcessingStep("knowledgebase_search", 0, "skipped", "no datapack_id"))
        return []
    tool = tools_by_name.get("knowledgebase_search")
    if tool is None:
        logger.warning("knowledgebase_search tool unavailable; skipping KB grounding")
        if steps is not None:
            steps.append(ProcessingStep("knowledgebase_search", 0, "skipped", "tool unavailable"))
        return []
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
        if steps is not None:
            steps.append(
                ProcessingStep(
                    "knowledgebase_search",
                    int((time.monotonic() - start) * 1000),
                    "error",
                    str(exc),
                )
            )
        return []
    hits = _extract_kb_hits(_parse_json(raw))
    if steps is not None:
        steps.append(
            ProcessingStep(
                "knowledgebase_search",
                int((time.monotonic() - start) * 1000),
                "ok",
                f"{len(hits)} hit{'' if len(hits) == 1 else 's'}",
            )
        )
    return hits


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


def _extract_memory_hits(payload: object) -> List[MemoryHit]:
    """Pull structured memory hits out of whatever `memory_search` returned.

    Meko's `memory_search` returns three parallel arrays — all worth
    surfacing to the LLM:

      * ``results``            — vector-matched Mem0 facts. Each item has
                                 a ``memory`` text field (the documented
                                 key) and a ``score`` (cosine similarity 0..1).
      * ``relations``          — graph triples from the per-agent Apache
                                 AGE store. Shape: ``{source, relationship,
                                 destination}``. Not scored.
      * ``promoted_relations`` — relations elevated to the tenant KB.
                                 Same shape as ``relations``.

    Older / alternate MCP servers may surface vector hits under
    ``memories`` or ``data`` instead of ``results``; we keep those as
    fallbacks. A bare list is treated as the vector hits with no relations.

    Score filtering: vector hits below `MEMORY_MIN_SCORE` are dropped.
    Items missing a `score` are kept (legacy servers / string items don't
    carry one and we'd rather surface than silently drop).
    """
    vector_items: list[Any] = []
    relations: list[Any] = []
    promoted_relations: list[Any] = []

    if isinstance(payload, dict):
        for key in ("results", "memories", "data"):
            candidate = payload.get(key)
            if isinstance(candidate, list):
                vector_items = candidate
                break
        if isinstance(payload.get("relations"), list):
            relations = payload["relations"]
        if isinstance(payload.get("promoted_relations"), list):
            promoted_relations = payload["promoted_relations"]
    elif isinstance(payload, list):
        vector_items = payload

    hits: List[MemoryHit] = []

    # Vector hits — `memory` is the documented Meko key; the others are
    # kept for shape-tolerance against older MCP servers. The slice is
    # only a backstop in case the server ignored our `limit` — the
    # normal path is already bounded by `top_k` upstream.
    for item in vector_items[:MAX_RETRIEVAL_ITEMS]:
        if isinstance(item, dict):
            raw_score = item.get("score")
            score: Optional[float] = (
                float(raw_score) if isinstance(raw_score, (int, float)) else None
            )
            if score is not None and score < MEMORY_MIN_SCORE:
                continue
            text = (
                item.get("memory")
                or item.get("data")
                or item.get("text")
                or item.get("content")
                or ""
            )
            if text:
                hits.append(MemoryHit(kind="mem", text=text, score=score, char_count=len(text)))
        elif isinstance(item, str):
            hits.append(MemoryHit(kind="mem", text=item, score=None, char_count=len(item)))

    # Graph triples — flatten subject ─predicate→ object.
    for item in relations[:MAX_RETRIEVAL_ITEMS]:
        triple = _format_relation_triple(item)
        if triple:
            hits.append(MemoryHit(kind="graph", text=triple, score=None, char_count=len(triple)))

    # Promoted relations — same shape, different provenance tag so the
    # UI (and a future debugger) can tell them apart.
    for item in promoted_relations[:MAX_RETRIEVAL_ITEMS]:
        triple = _format_relation_triple(item)
        if triple:
            hits.append(
                MemoryHit(kind="promoted", text=triple, score=None, char_count=len(triple))
            )

    return hits


def _extract_kb_hits(payload: object) -> List[KbHit]:
    """Pull structured KB chunk hits out of whatever `knowledgebase_search` returned.

    Tolerates `{results: [...]}`, `{matches: [...]}`, `{documents: [...]}`,
    or a bare list of chunks. `chunk_text` is the current Meko key; the
    others are fallbacks for older / alternate MCP servers.

    Distance filtering: drops hits beyond `KB_MAX_DISTANCE`. Distance is
    cosine distance (lower = better); we also compute a `score = max(0,
    1 - distance)` for UI display so memory and KB hits both sort by the
    same "higher = better" convention.
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

    hits: List[KbHit] = []
    for item in items[:MAX_RETRIEVAL_ITEMS]:
        if isinstance(item, dict):
            raw_distance = item.get("distance")
            distance: Optional[float] = (
                float(raw_distance) if isinstance(raw_distance, (int, float)) else None
            )
            if distance is not None and distance > KB_MAX_DISTANCE:
                continue
            # Cosine distance → similarity score in [0..1] (clamp the
            # negative tail that orthogonal-or-beyond vectors produce).
            score: Optional[float] = (
                max(0.0, min(1.0, 1.0 - distance)) if distance is not None else None
            )
            text = (
                item.get("chunk_text")
                or item.get("text")
                or item.get("content")
                or item.get("snippet")
                or item.get("chunk")
                or ""
            )
            # `document_id` (UUID) is the only attribution Meko returns
            # today — not as nice as a filename but enough for grouping.
            source = (
                item.get("document_name")
                or item.get("source")
                or item.get("source_uri")
                or item.get("document_id")
                or ""
            )
            # Trim long source paths to a basename so it stays readable
            # in the UI / system prompt without losing identity.
            if source and "/" in source:
                source = source.split("/")[-1]
            if text:
                hits.append(
                    KbHit(
                        document_id=source,
                        chunk_text=text,
                        score=score,
                        distance=distance,
                        char_count=len(text),
                    )
                )
        elif isinstance(item, str):
            hits.append(
                KbHit(document_id="", chunk_text=item, score=None, distance=None, char_count=len(item))
            )
    return hits


def _format_relation_triple(item: Any) -> str:
    """Render a graph relation as ``"<source> <relationship> <destination>"``.

    Tolerates the alternate field names that earlier Mem0 / Apache AGE
    versions used (``subject``/``object``/``predicate``) so the formatter
    keeps working if the MCP server changes its mind about naming.
    Returns an empty string when any of the three legs is missing — we'd
    rather drop a malformed triple than feed half a fact to the LLM.
    """
    if not isinstance(item, dict):
        return ""
    source = item.get("source") or item.get("subject") or ""
    target = (
        item.get("destination")
        or item.get("target")
        or item.get("object")
        or ""
    )
    relation = (
        item.get("relationship")
        or item.get("relation")
        or item.get("predicate")
        or ""
    )
    if not (source and target and relation):
        return ""
    return f"{source} {relation} {target}"


def _memory_hits_to_bullets(hits: List[MemoryHit]) -> str:
    """Render structured memory hits as the bulleted text block we feed
    into the LLM's system prompt. Tagged by `kind` so the LLM can weight
    each line.
    """
    return "\n".join(f"- [{h.kind}] {h.text}" for h in hits)


def _kb_hits_to_bullets(hits: List[KbHit]) -> str:
    """Render structured KB hits as bulleted text for the system prompt.
    Each line is prefixed with the document_id (or basename) when known,
    matching the legacy `_format_knowledge_context` output verbatim.
    """
    lines: List[str] = []
    for h in hits:
        if h.document_id:
            lines.append(f"- [{h.document_id}] {h.chunk_text}")
        else:
            lines.append(f"- {h.chunk_text}")
    return "\n".join(lines)


# Backward-compatible wrappers — used by the existing
# `tests/test_chain_formatters.py` (which pass raw MCP payloads). New
# call sites should use the `_extract_*_hits` + `_*_hits_to_bullets`
# pair directly so they can also surface the structured data.
def _format_memory_context(payload: object) -> str:
    return _memory_hits_to_bullets(_extract_memory_hits(payload))


def _format_knowledge_context(payload: object) -> str:
    return _kb_hits_to_bullets(_extract_kb_hits(payload))


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
