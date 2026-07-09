"""Chat-turn orchestration.

`run_chat_turn` executes the agreed flow for one user message:

    1. Ensure the session has an MCP `conversation_id`
       (lazily create on the first turn via `conversation_create`).
    2. **PARALLEL** retrieval — `asyncio.gather`:
         - `memory_search` (Mem0 long-term)
         - `knowledgebase_search` (per-datapack RAG; skipped if no datapack_id)
    3. LLM call with: system + memory_context + kb_context + user.
       Prior turns are NOT replayed — Mem0 is the long-term memory; the
       session store still records turns but they aren't fed back to the
       model (saves input tokens; relies on Mem0 to surface what matters).
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

import json
import logging
import re
import time
from typing import Any, Dict, List, NamedTuple, Optional, Set, Tuple

from langchain_core.language_models import BaseChatModel
from langchain_core.messages import BaseMessage, HumanMessage, SystemMessage
from langchain_core.tools import BaseTool

from .config import AppConfig
from .session_store import InMemorySessionStore

logger = logging.getLogger(__name__)


def _format_exc_chain(exc: BaseException) -> str:
    """Flatten an exception chain into a single-line description that
    survives `logger.warning("... %s", ...)` formatting.

    Python 3.11+'s `asyncio.TaskGroup` wraps every sub-task failure in
    an `ExceptionGroup`; logging it directly yields the useless
    "unhandled errors in a TaskGroup (1 sub-exception)" line, hiding
    the real cause. This walks the group recursively so operators see
    which upstream call (memory_search / kb_search / conversation_add_message)
    actually failed and why.

    Also chases the exception chain:
      - `__cause__`   — explicit `raise X from Y`.
      - `__context__` — implicit chaining when an exception is raised
        inside an `except` block without `from …`. Python sets this
        automatically; skipping it drops the root cause for the common
        "wrapped and re-raised" pattern.
    `__suppress_context__` is honored so `raise X from None` correctly
    hides the implicit context.

    Newlines in exception messages (multi-line HTTP bodies, SQL errors)
    are collapsed to a literal `\\n` marker so the output stays a single
    line — grep-friendly for log-aggregator dashboards.
    """
    parts: List[str] = []
    seen: Set[int] = set()

    def _sanitize(msg: str) -> str:
        # Replace real newlines/CRs with visible markers so the log stays
        # on one line but the payload is still readable in the aggregator.
        return msg.replace("\r\n", "\\n").replace("\n", "\\n").replace("\r", "\\n")

    def _walk(e: BaseException, indent: int = 0) -> None:
        if id(e) in seen:
            return
        seen.add(id(e))
        pad = "  " * indent
        parts.append(f"{pad}{type(e).__name__}: {_sanitize(str(e))}")
        # ExceptionGroup / BaseExceptionGroup — recurse into every child.
        subs = getattr(e, "exceptions", None)
        if isinstance(subs, (list, tuple)):
            for i, sub in enumerate(subs):
                parts.append(f"{pad}  [{i}]")
                _walk(sub, indent + 2)
        # Explicit chain wins. If absent, fall back to the implicit
        # context (unless the raiser explicitly suppressed it via
        # `raise X from None`).
        if e.__cause__ is not None:
            parts.append(f"{pad}  caused by:")
            _walk(e.__cause__, indent + 2)
        elif not e.__suppress_context__ and e.__context__ is not None:
            parts.append(f"{pad}  during handling of:")
            _walk(e.__context__, indent + 2)

    _walk(exc)
    return " | ".join(parts)


async def _track_token_usage(
    tools_by_name: Dict[str, BaseTool],
    *,
    conversation_id: str,
    name: str,
    input_tokens: int,
    output_tokens: int,
    total_tokens: int,
    model: Optional[str] = None,
    message_id: Optional[str] = None,
    datapack_id: Optional[str] = None,
) -> None:
    """Fire-and-forget call to the MCP `track_token_usage` tool so the
    GENERATION shows up in Langfuse and per-user SUM picks it up.
    Failures log + swallow — token accounting must never break a chat turn."""
    in_tok = int(input_tokens or 0)
    out_tok = int(output_tokens or 0)
    tot_tok = int(total_tokens or 0)
    # Always log the spend even when the MCP tool is unavailable — gives
    # ops a record of token cost per turn straight from the gateway log,
    # independent of Langfuse availability.
    logger.info(
        "token_usage name=%s message_id=%s input=%d output=%d total=%d model=%s",
        name, message_id or "-", in_tok, out_tok, tot_tok, model or "-",
    )
    tool = tools_by_name.get("track_token_usage")
    if tool is None:
        logger.debug("track_token_usage tool unavailable; skipping (name=%s)", name)
        return
    args: Dict[str, Any] = {
        "scope": "write",
        "conversation_id": conversation_id,
        "name": name,
        "input_tokens": in_tok,
        "output_tokens": out_tok,
        "total_tokens": tot_tok,
    }
    if model:
        args["model"] = model
    if message_id:
        args["message_id"] = message_id
    if datapack_id:
        args["datapack_id"] = datapack_id
    try:
        await tool.ainvoke(args)
    except Exception as exc:  # noqa: BLE001 — best-effort
        logger.warning(
            "track_token_usage(%s) failed: %s", name, _format_exc_chain(exc), exc_info=exc,
        )


def _usage_from_langchain_result(result: Any) -> Tuple[int, int, int, Optional[str]]:
    """Extract `(input, output, total, model)` from a langchain AIMessage.
    Tries, in order:
      1. `result.usage_metadata` — canonical LangChain 0.2+ field,
         standardized across providers (OpenAI, Bedrock, Anthropic, …)
      2. `response_metadata.usage` / `response_metadata.token_usage` —
         provider-specific raw payload (OpenAI prompt_/completion_tokens,
         Bedrock input_/output_tokens)."""
    meta = getattr(result, "response_metadata", None) or {}

    # 1. Canonical LangChain field — populated by chat models that adopt
    #    the standard interface (most do as of langchain-core 0.2).
    usage_md = getattr(result, "usage_metadata", None) or {}
    input_tokens = int(usage_md.get("input_tokens") or 0)
    output_tokens = int(usage_md.get("output_tokens") or 0)
    total_tokens = int(usage_md.get("total_tokens") or 0)

    # 2. Fall back to provider-specific shapes in response_metadata.
    if not (input_tokens or output_tokens or total_tokens):
        usage = meta.get("usage") or meta.get("token_usage") or {}
        input_tokens = int(usage.get("input_tokens") or usage.get("prompt_tokens") or 0)
        output_tokens = int(usage.get("output_tokens") or usage.get("completion_tokens") or 0)
        total_tokens = int(usage.get("total_tokens") or (input_tokens + output_tokens))

    if not total_tokens:
        total_tokens = input_tokens + output_tokens

    model = meta.get("model_name") or meta.get("model") or meta.get("model_id")

    # Debug: dump raw shapes when we still can't find tokens, so we can see
    # exactly which provider field the model is actually using. Log keys
    # only (no values) to avoid leaking message content.
    if not (input_tokens or output_tokens):
        logger.warning(
            "usage extract miss: response_metadata keys=%s usage_metadata=%s response_metadata=%s",
            list(meta.keys()), usage_md, meta,
        )

    return input_tokens, output_tokens, total_tokens, model




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


# Prior-conversation turns fed to the SUMMARY model on a cache hit. The MCP
# server already caps the thread it returns (CONVERSATION_MAX_THREAD_TURNS);
# this is the gateway-side bound applied when building the summary prompt.
CONV_SUMMARY_MAX_TURNS = 8
# Reused turns surfaced in the decision-trace "REUSED CONVERSATIONS" card —
# observability only; does not affect the answer.
CONV_TRACE_MAX_CHUNKS = 8


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


SYSTEM_TEMPLATE = """You are the Meko playground assistant. Be concise and helpful.

Ground your answer in the memories and knowledge below when relevant;
ignore them when they don't apply.

Sources — when they conflict, the KB wins:
  - KB = user-curated documents; authoritative on whatever it covers.
  - Memories = user intent/preferences/context; use to tailor the
    answer, not to override KB facts.

Response budget: ~{max_tokens} tokens (~{max_chars} chars). Prefer a
complete brief answer to a sprawling one that gets cut off.

Memories (from `memory_search`):
{memory_context}

Knowledge (from `knowledgebase_search`):
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

    `query` is populated only for retrieval steps (`memory_search`,
    `knowledgebase_search`); it's the exact string we sent as the
    `query` argument to MCP. Lets the UI's Agent Reasoning panel show
    "we searched for X" — useful when the user's prompt is long and
    they want to know what slice the gateway actually used.

    `tool_args` is the rest of the args we sent to MCP for this step —
    everything except `query`, which has its own dedicated field. We
    surface the full payload (`scope`, `conversation_id`, `agent_id`,
    `limit`, `datapack_id`, etc.) so the "MCP args" sub-expand in the
    Agent Reasoning panel is an honest "what we sent" view, not a
    curated subset. None when the step doesn't have extra args worth
    surfacing (`llm_invoke`, `conversation_add_message`, fire-and-forget
    `memory_add`).
    """

    name: str
    duration_ms: int
    status: str
    detail: str = ""
    query: Optional[str] = None
    tool_args: Optional[Dict[str, Any]] = None


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


_CONV_SUMMARY_SYSTEM = (
    "You answer the user's CURRENT question using ONLY the prior conversation "
    "excerpts below, taken from this shared workspace. The excerpts are turns "
    "from one or more prior conversations, ordered oldest→newest; a later turn "
    "may refine or correct an earlier one, so treat later turns as updates. Use "
    "ONLY the excerpts relevant to the current question and IGNORE unrelated "
    "turns — the excerpts may include off-topic content. Synthesize one direct, "
    "helpful answer from the relevant turns. If none of the excerpts actually "
    "answer THIS question, say so briefly — do NOT answer a different question "
    "or invent details."
)


async def _summarize_conversation_hits(
    summary_llm: BaseChatModel,
    query: str,
    hits: List[Dict[str, Any]],
) -> str:
    """Summarize retrieved prior-conversation chunks into an answer to `query`
    using the small summary model. Falls back to the closest answer verbatim if
    the model call fails — we never want a tracing/model hiccup to drop the turn."""
    if not hits:
        return ""
    # Fallback answer when the SLM returns empty or errors: the CLOSEST matched
    # turn by distance — never blindly the oldest turn (which, in a multi-topic
    # conversation, is often unrelated to the question).
    def _closest_answer() -> str:
        scored = [h for h in hits if isinstance(h.get("distance"), (int, float))]
        if scored:
            return min(scored, key=lambda h: h["distance"]).get("answer", "")
        return hits[0].get("answer", "")
    blocks = []
    # Cap so a long thread can't blow up the summary prompt; fewer is fine
    # (most threads are far shorter). Turns arrive oldest→newest.
    for i, h in enumerate(hits[:CONV_SUMMARY_MAX_TURNS], 1):
        blocks.append(f"[{i}] Q: {h.get('question', '')}\n    A: {h.get('answer', '')}")
    context = "\n\n".join(blocks)
    messages = [
        SystemMessage(content=_CONV_SUMMARY_SYSTEM),
        HumanMessage(content=f"User question:\n{query}\n\nPrior conversation excerpts:\n{context}"),
    ]
    try:
        result = await summary_llm.ainvoke(messages)
        in_tok, out_tok, tot_tok, model_id = _usage_from_langchain_result(result)
        logger.info(
            "token_usage name=Conversation Summary input=%d output=%d total=%d model=%s turns=%d",
            in_tok, out_tok, tot_tok, model_id or "-", len(hits),
        )
        text = _coerce_text(result.content).strip()
        if not text:
            logger.warning("conversation summary returned empty; falling back to closest answer")
            return _closest_answer()
        return text
    except Exception as exc:  # noqa: BLE001
        logger.warning(
            "conversation summary failed, falling back to closest answer: %s",
            _format_exc_chain(exc),
            exc_info=exc,
        )
        return _closest_answer()


_FOLLOWUP_CLASSIFY_SYSTEM = (
    "The user was just shown an answer that was SUMMARIZED FROM THEIR PRIOR "
    "CONVERSATIONS. Classify their next message as EXACTLY one word:\n"
    "- dissatisfied : they reject / aren't happy with that answer, with NO new requirement\n"
    "- refine : they reject it AND add a new requirement or correction\n"
    "- other : a normal new or unrelated request (not a rejection)\n"
    "Reply with ONLY one word: dissatisfied, refine, or other."
)


async def _classify_followup(model: BaseChatModel, prior_query: str, message: str) -> str:
    """Classify the message that follows a cache-hit answer as one of
    `dissatisfied` / `refine` / `other`. Uses the small summary model (cheap,
    one-word output). Defaults to `other` on any error so we never bypass the
    cache on a false positive."""
    try:
        result = await model.ainvoke([
            SystemMessage(content=_FOLLOWUP_CLASSIFY_SYSTEM),
            HumanMessage(content=f"Prior question: {prior_query}\nUser's next message: {message}"),
        ])
        label = _coerce_text(result.content).strip().lower()
        for v in ("dissatisfied", "refine", "other"):
            if v in label:
                return v
    except Exception as exc:  # noqa: BLE001
        logger.warning(
            "follow-up classify failed (defaulting to 'other'): %s",
            _format_exc_chain(exc),
            exc_info=exc,
        )
    return "other"


async def run_chat_turn(
    *,
    cfg: AppConfig,
    llm: BaseChatModel,
    tools_by_name: Dict[str, BaseTool],
    session_store: InMemorySessionStore,
    session_id: str,
    user_message: str,
    datapack_id: Optional[str],
    user_id: Optional[str] = None,
    resume_conversation_id: Optional[str] = None,
    summary_llm: Optional[BaseChatModel] = None,
    bypass_conversation_cache: bool = False,
) -> ChatTurnResult:
    """Run one chat turn end-to-end. Returns the assistant text + Langfuse
    message_id + structured retrieval hits + a processing-steps timeline
    the UI's Agent Reasoning panel renders.

    When `resume_conversation_id` is supplied AND the session store has no
    mapping yet for this `session_id`, the gateway adopts the supplied id
    instead of minting a new conversation via MCP — used by the UI when
    the user picks a previous chat from the dropdown. Every subsequent
    turn of that session reads the id back from the store, so the caller
    only has to send `conversation_id` once. If the session already has
    a mapping, the supplied id is ignored (the existing one wins — we
    don't want to silently switch conversations mid-session).
    """
    # Step list grows in invocation order as the pipeline runs; surfaces
    # to the UI as the per-turn observability timeline.
    steps: List[ProcessingStep] = []

    # 1. Ensure we have an MCP conversation_id for this session.
    #
    # Three branches, in priority order:
    #   a) Session already mapped — reuse (no step recorded; cache hit).
    #   b) Caller supplied `resume_conversation_id` — adopt it, seed the
    #      session store, record a "skipped" conversation_create step so
    #      the Agent Reasoning panel still has a timeline entry for this
    #      stage. No MCP round-trip.
    #   c) Otherwise — mint a fresh conversation via MCP.
    conversation_id = await session_store.get_conversation_id(session_id)
    if conversation_id is None and resume_conversation_id:
        # Branch (b): adopt the resumed id.
        await session_store.set_conversation_id(session_id, resume_conversation_id)
        conversation_id = resume_conversation_id
        steps.append(
            ProcessingStep(
                "conversation_create",
                0,
                "skipped",
                "resumed existing conversation",
                tool_args={"conversation_id": resume_conversation_id},
            )
        )
        logger.info(
            "resumed MCP conversation %s for session %s",
            resume_conversation_id, session_id,
        )
    elif conversation_id is None:
        # Branch (c): mint a fresh one.
        step_start = time.monotonic()
        # Pre-compute the title so we can surface it on the step's
        # `tool_args` for the Agent Reasoning panel without recomputing.
        conv_title = _truncate(user_message, 80)
        # Capture every arg we'll send to MCP (minus `query`, which has
        # its own dedicated field on ProcessingStep). The Agent Reasoning
        # panel's "MCP args" sub-expand renders this verbatim so users
        # see EVERY field, not a curated subset — easier to debug.
        # conversation_create doesn't take a conversation_id (it mints
        # one) so that field is naturally absent here.
        conv_tool_args: Dict[str, Any] = {
            "scope": "write",
            "agent_id": cfg.agent_id,
            "title": conv_title,
        }
        if datapack_id:
            conv_tool_args["datapack_id"] = datapack_id
        try:
            conversation_id = await _create_conversation(
                tools_by_name=tools_by_name,
                agent_id=cfg.agent_id,
                datapack_id=datapack_id,
                title=conv_title,
            )
        except Exception as exc:
            steps.append(
                ProcessingStep(
                    "conversation_create",
                    int((time.monotonic() - step_start) * 1000),
                    "error",
                    str(exc),
                    tool_args=conv_tool_args,
                )
            )
            raise
        steps.append(
            ProcessingStep(
                "conversation_create",
                int((time.monotonic() - step_start) * 1000),
                "ok",
                tool_args=conv_tool_args,
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
    memory_hits: List[MemoryHit] = []
    kb_hits: List[KbHit] = []
    # Did the user reject the previous cache answer? Either via the UI button
    # (bypass_conversation_cache) or by typing dissatisfaction on the turn right
    # after a cache hit. On a reject we bypass the conversation cache and answer
    # via the full memory + KB + LLM path.
    bypass = bypass_conversation_cache
    if _is_trivial_query(user_message):
        logger.info(
            "skipping retrieval for trivial query session=%s len=%d",
            session_id, len(user_message.strip()),
        )
        for nm in ("conversation_search", "memory_search", "knowledgebase_search"):
            steps.append(ProcessingStep(nm, 0, "skipped", "trivial query", query=user_message))
    else:
        # Typed-rejection detection: only on the turn right after a cache hit,
        # classify the message — dissatisfied/refine → bypass the cache.
        if not bypass:
            was_hit, prior_query = await session_store.get_cache_state(session_id)
            if was_hit and prior_query:
                verdict = await _classify_followup(summary_llm or llm, prior_query, user_message)
                if verdict in ("dissatisfied", "refine"):
                    bypass = True
                    if verdict == "dissatisfied":
                        # No new requirement — re-answer the prior question.
                        user_message = prior_query
                    logger.info("conversation cache rejected via typed '%s' — bypassing", verdict)

        if bypass:
            steps.append(
                ProcessingStep("conversation_search", 0, "skipped", "user rejected cache answer", query=user_message)
            )

        # 2. Knowledge-base search runs FIRST and (when enabled and NOT bypassed)
        #    performs conversation-history search INSIDE it. Returns either a
        #    conversation hit (chunks + thread → summarize) or KB chunks (a miss
        #    → then run memory search). `skip_conversation` forces pure KB when
        #    the user rejected the cache.
        kb_hits, conv_result = await _search_knowledge(
            tools_by_name=tools_by_name,
            agent_id=cfg.agent_id,
            query=user_message,
            conversation_id=conversation_id,
            datapack_id=datapack_id,
            steps=steps,
            skip_conversation=bypass,
        )

        if conv_result:
            hits = conv_result.get("hits") or []
            # Summarize the WHOLE matched conversation (oldest→newest) so later
            # turns supersede earlier ones; fall back to the matched chunks if
            # the thread wasn't returned.
            turns = conv_result.get("thread") or hits
            summary_model = cfg.conv_summary_model or cfg.llm_model
            top = hits[0] if hits else (turns[0] if turns else {})
            source_conv_id = top.get("conversation_id")
            summ_start = time.monotonic()
            assistant_text = await _summarize_conversation_hits(
                summary_llm or llm, user_message, turns
            )
            steps.append(
                ProcessingStep(
                    "conversation_summarize",
                    int((time.monotonic() - summ_start) * 1000),
                    "ok",
                    f"{summary_model} summarized {len(turns)} turn(s) of conversation {source_conv_id}",
                    tool_args={"summary_model": summary_model, "conversation_id": source_conv_id},
                )
            )
            steps.append(ProcessingStep("memory_search", 0, "skipped", "conversation cache hit"))
            steps.append(ProcessingStep("knowledgebase_search", 0, "skipped", "conversation cache hit"))
            steps.append(
                ProcessingStep("llm_invoke", 0, "skipped", "answered from conversation history (small-model summary)")
            )
            # Provenance + observability: closest chunk for the primary deep-link,
            # the summarized turns, and the summary model.
            answered_from_cache = {
                "conversation_id": source_conv_id,
                "message_id": top.get("message_id"),
                "distance": top.get("distance"),
                "summary_model": summary_model,
                "chunks": turns,
            }
            addmsg_start = time.monotonic()
            message_id = await _add_message(
                tools_by_name=tools_by_name,
                conversation_id=conversation_id,
                agent_id=cfg.agent_id,
                user_message=user_message,
                assistant_message=assistant_text,
                datapack_id=datapack_id,
                index_for_search=False,  # don't re-index a summary of existing chunks
                answered_from_cache=answered_from_cache,
            )
            steps.append(
                ProcessingStep(
                    "conversation_add_message",
                    int((time.monotonic() - addmsg_start) * 1000),
                    "ok",
                )
            )
            steps.append(
                ProcessingStep("memory_add", 0, "skipped", "history hit — no memory extraction")
            )
            # Arm typed-rejection detection for the NEXT turn: if the user now
            # types "no, that's wrong", we re-answer this query with the cache
            # bypassed.
            await session_store.set_cache_state(session_id, True, user_message)
            await session_store.append(session_id, "user", user_message)
            await session_store.append(session_id, "assistant", assistant_text)
            return ChatTurnResult(
                assistant_text=assistant_text,
                message_id=message_id,
                memory_context="",
                kb_context="",
                memory_hits=(),
                kb_hits=(),
                processing_steps=tuple(steps),
            )

        # 2b. No conversation hit → KB chunks are already in hand from the call
        #     above; run memory search to complete the memory + KB grounding.
        memory_hits = await _search_memories(
            tools_by_name=tools_by_name,
            agent_id=cfg.agent_id,
            query=user_message,
            conversation_id=conversation_id,
            datapack_id=datapack_id,
            top_k=cfg.memory_top_k,
            user_id=user_id,
            steps=steps,
        )

    # Derive the bulleted system-prompt blocks from the structured hits.
    # The structured lists are the source of truth; the bullets are the
    # specific rendering for the LLM.
    memory_context = _memory_hits_to_bullets(memory_hits)
    kb_context = _kb_hits_to_bullets(kb_hits)

    logger.debug(
        "run_chat_turn: retrieval complete session=%s conversation_id=%s "
        "datapack_id=%s memory_hits=%d kb_hits=%d",
        session_id, conversation_id, datapack_id,
        len(memory_hits), len(kb_hits),
    )

    # 3. LLM call. Prior turns from the session store are NOT replayed —
    # Mem0 + KB carry the cross-turn context we care about and replaying
    # full history per turn was the dominant input-token cost. The system
    # prompt's "response length" hint advertises the same budget set in
    # `cfg.llm_max_tokens` so the model self-paces (~4 chars per token).
    messages: List[BaseMessage] = [
        SystemMessage(
            content=SYSTEM_TEMPLATE.format(
                memory_context=memory_context or "(none yet)",
                kb_context=kb_context or "(none yet)",
                max_tokens=cfg.llm_max_tokens,
                max_chars=cfg.llm_max_tokens * 4,
            )
        ),
        HumanMessage(content=user_message),
    ]

    logger.debug(
        "run_chat_turn: invoking LLM session=%s conversation_id=%s "
        "system_prompt_chars=%d user_message_chars=%d",
        session_id, conversation_id,
        len(messages[0].content) if messages else 0, len(user_message),
    )

    llm_start = time.monotonic()
    try:
        result = await llm.ainvoke(messages)
    except Exception as exc:
        logger.warning(
            "run_chat_turn: LLM invocation FAILED session=%s conversation_id=%s "
            "llm_ms=%d err=%s",
            session_id, conversation_id,
            int((time.monotonic() - llm_start) * 1000), exc,
        )
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

    logger.debug(
        "run_chat_turn: LLM done session=%s conversation_id=%s "
        "llm_ms=%d assistant_chars=%d",
        session_id, conversation_id,
        int((time.monotonic() - llm_start) * 1000), len(assistant_text),
    )

    # Capture token usage from the main LLM call here, but DEFER the
    # `track_token_usage` invocation until after `_add_message` returns
    # — we want to stamp the observation with the conversation's
    # `message_id` so the token observation correlates to a specific
    # turn in Langfuse. The same prompt also subsumes "conversation
    # summarization" when KB search returns conversation-history hits
    # (they're rendered into `kb_context` and synthesised inline), so
    # one observation covers both.
    in_tok, out_tok, tot_tok, model_id = _usage_from_langchain_result(result)

    # 5. Persistence.
    #
    # `conversation_add_message` is awaited (~0.5s). We want the row in
    # Langfuse before we return so the right-rail "Recent conversations"
    # panel sees it on the post-turn refetch. We also capture the returned
    # message_id so the UI can deep-link into the trace drawer for this turn.
    #
    # Memory extraction is now handled inside the MCP server: when
    # `conversation_add_message` receives a substantive turn it spawns its
    # own background thread that calls Mem0 with `infer=True`. We no longer
    # run a second LLM extraction call here, so `memory_add` is dropped
    # from the gateway pipeline entirely.
    logger.debug(
        "run_chat_turn: persisting turn session=%s conversation_id=%s",
        session_id, conversation_id,
    )

    addmsg_start = time.monotonic()
    try:
        message_id = await _add_message(
            tools_by_name=tools_by_name,
            conversation_id=conversation_id,
            agent_id=cfg.agent_id,
            user_message=user_message,
            assistant_message=assistant_text,
            datapack_id=datapack_id,
            index_for_search=True,  # cache genuine Q&A for future reuse
            rejected_cache=bypass,  # full re-answer after a rejected cache hit
        )
        logger.debug(
            "run_chat_turn: persisted turn session=%s conversation_id=%s "
            "message_id=%s addmsg_ms=%d",
            session_id, conversation_id, message_id or "<empty>",
            int((time.monotonic() - addmsg_start) * 1000),
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
        logger.warning(
            "run_chat_turn: persist FAILED session=%s conversation_id=%s "
            "addmsg_ms=%d err=%s",
            session_id, conversation_id,
            int((time.monotonic() - addmsg_start) * 1000), exc,
        )
        steps.append(
            ProcessingStep(
                "conversation_add_message",
                int((time.monotonic() - addmsg_start) * 1000),
                "error",
                str(exc),
            )
        )
        message_id = ""
    # Token accounting for the just-completed main LLM call, stamped
    # with the message_id so the GENERATION row in Langfuse correlates
    # to this specific turn.
    await _track_token_usage(
        tools_by_name,
        conversation_id=conversation_id,
        name="LLM Response",
        input_tokens=in_tok,
        output_tokens=out_tok,
        total_tokens=tot_tok,
        model=model_id,
        message_id=message_id or None,
        datapack_id=datapack_id,
    )
    steps.append(
        ProcessingStep("memory_add", 0, "skipped", "handled inside MCP conversation_add_message")
    )

    # 6. Update local session state. This turn was NOT a conversation-cache
    #    hit (it went through the full memory + KB + LLM path), so clear the
    #    cache state — the next turn shouldn't be classified as a rejection.
    await session_store.set_cache_state(session_id, False, None)
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
    user_id: Optional[str] = None,
    steps: Optional[List[ProcessingStep]] = None,
) -> List[MemoryHit]:
    """Run `memory_search` and return the structured hits.

    The MCP server resolves the user from its auth context (the forwarded
    bearer token); `user_id` is no longer passed explicitly.

    Best-effort: a missing tool / transient failure logs a warning and
    returns []. Appends a step to `steps` if provided so the per-turn
    timeline reflects what actually happened (ok / skipped / error).
    """
    start = time.monotonic()
    # Capture every MCP arg we'll send (minus `query`, which has its
    # own dedicated ProcessingStep field). Drives the "MCP args"
    # sub-expand in the Agent Reasoning panel — the user sees the
    # entire payload, not a curated subset.
    mem_tool_args: Dict[str, Any] = {
        "scope": "read",
        "conversation_id": conversation_id,
        "agent_id": agent_id,
        "limit": top_k,
    }
    if datapack_id:
        mem_tool_args["datapack_id"] = datapack_id

    tool = tools_by_name.get("memory_search")
    if tool is None:
        logger.warning("memory_search tool unavailable; skipping memory grounding")
        if steps is not None:
            steps.append(
                ProcessingStep(
                    "memory_search", 0, "skipped", "tool unavailable",
                    query=query, tool_args=mem_tool_args,
                )
            )
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
        logger.warning(
            "memory_search failed (agent_id=%s, datapack_id=%s, query=%r): %s",
            agent_id, datapack_id, query, _format_exc_chain(exc),
            exc_info=exc,
        )
        if steps is not None:
            steps.append(
                ProcessingStep(
                    "memory_search",
                    int((time.monotonic() - start) * 1000),
                    "error",
                    str(exc),
                    query=query,
                    tool_args=mem_tool_args,
                )
            )
        return []
    # Vector hits are capped by `top_k` server-side; graph/promoted relations
    # are not, so cap them at 2× top_k to bound the prompt.
    hits = _extract_memory_hits(_parse_json(raw), relation_cap=2 * top_k)
    # One line per hit so dev logs show
    # exactly which memories fed the prompt and at what similarity. DEBUG:
    # unbounded per-turn volume + carries user content — run the gateway
    # with --log-level debug (INFERENCE_LOG_LEVEL=debug) to see these.
    for h in hits:
        logger.debug(
            "memory_search: kept hit kind=%s score=%s text=%r",
            h.kind,
            f"{h.score:.3f}" if h.score is not None else "-",
            _truncate(h.text, 80),
        )
    if steps is not None:
        steps.append(
            ProcessingStep(
                "memory_search",
                int((time.monotonic() - start) * 1000),
                "ok",
                f"{len(hits)} hit{'' if len(hits) == 1 else 's'}",
                query=query,
                tool_args=mem_tool_args,
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
    steps: Optional[List[ProcessingStep]] = None,
    skip_conversation: bool = False,
) -> Tuple[List[KbHit], Optional[Dict[str, Any]]]:
    """Run `knowledgebase_search` and return (kb_hits, conv_result).

    When MEKO_ENABLE_CONVERSATION_SEARCH is on, the MCP `knowledgebase_search`
    tool runs conversation-history search INTERNALLY and, on a hit, returns
    `conversation_hits` + `thread` instead of KB chunks. We surface that as
    conv_result (`{"hits", "thread"}`) so the caller can summarize it and skip
    memory. On a miss, conv_result is None and kb_hits holds the KB chunks.

    Skips entirely when `datapack_id` is missing. Best-effort on failures.
    """
    start = time.monotonic()
    # Capture every MCP arg we'll send (minus `query`). Same shape as
    # memory_search above — the panel shows the full payload, not a
    # curated subset. When datapack_id is missing we still surface
    # whatever we WOULD have sent so the skip reason is contextualised.
    kb_tool_args: Dict[str, Any] = {
        "scope": "read",
        "conversation_id": conversation_id,
        "agent_id": agent_id,
    }
    if datapack_id:
        kb_tool_args["datapack_id"] = datapack_id

    if not datapack_id:
        if steps is not None:
            steps.append(
                ProcessingStep(
                    "knowledgebase_search", 0, "skipped", "no datapack_id",
                    query=query, tool_args=kb_tool_args,
                )
            )
        return [], None
    tool = tools_by_name.get("knowledgebase_search")
    if tool is None:
        logger.warning("knowledgebase_search tool unavailable; skipping KB grounding")
        if steps is not None:
            steps.append(
                ProcessingStep(
                    "knowledgebase_search", 0, "skipped", "tool unavailable",
                    query=query, tool_args=kb_tool_args,
                )
            )
        return [], None
    args: Dict[str, Any] = {
        "scope": "read",
        "query": query,
        "conversation_id": conversation_id,
        "datapack_id": datapack_id,
        "agent_id": agent_id,
    }
    # User rejected the cache → force pure KB (no internal conversation search).
    if skip_conversation:
        args["skip_conversation"] = True
        kb_tool_args["skip_conversation"] = True
    try:
        raw = await tool.ainvoke(args)
    except Exception as exc:  # noqa: BLE001
        logger.warning(
            "knowledgebase_search failed (agent_id=%s, datapack_id=%s, query=%r): %s",
            agent_id, datapack_id, query, _format_exc_chain(exc),
            exc_info=exc,
        )
        if steps is not None:
            steps.append(
                ProcessingStep(
                    "knowledgebase_search",
                    int((time.monotonic() - start) * 1000),
                    "error",
                    str(exc),
                    query=query,
                    tool_args=kb_tool_args,
                )
            )
        return [], None
    parsed = _parse_json(raw)
    # Conversation hit — knowledgebase_search ran conversation search internally
    # and returned the matched conversation's chunks + thread instead of KB rows.
    if isinstance(parsed, dict) and parsed.get("conversation_hits"):
        ch = parsed["conversation_hits"]
        thread = parsed.get("thread")
        if steps is not None:
            top_d = ch[0].get("distance") if ch else None
            steps.append(
                ProcessingStep(
                    "conversation_search",
                    int((time.monotonic() - start) * 1000),
                    "ok",
                    f"{len(ch)} chunk(s)" + (f", top_distance={top_d}" if ch else ""),
                    query=query,
                    tool_args=kb_tool_args,
                )
            )
        return [], {"hits": ch, "thread": thread if isinstance(thread, list) else []}
    hits = _extract_kb_hits(parsed)
    # One line per KEPT chunk so dev logs show exactly which KB chunks fed
    # the prompt, with their similarity/distance. DEBUG for the same reason
    # as the memory-hit lines above (volume + user content).
    for h in hits:
        logger.debug(
            "knowledgebase_search: kept chunk doc=%s score=%s distance=%s text=%r",
            h.document_id or "-",
            f"{h.score:.3f}" if h.score is not None else "-",
            f"{h.distance:.3f}" if h.distance is not None else "-",
            _truncate(h.chunk_text, 80),
        )
    if steps is not None:
        steps.append(
            ProcessingStep(
                "knowledgebase_search",
                int((time.monotonic() - start) * 1000),
                "ok",
                f"{len(hits)} hit{'' if len(hits) == 1 else 's'}",
                query=query,
                tool_args=kb_tool_args,
            )
        )
    return hits, None


async def _add_message(
    *,
    tools_by_name: Dict[str, BaseTool],
    conversation_id: str,
    agent_id: str,
    user_message: str,
    assistant_message: str,
    datapack_id: Optional[str],
    index_for_search: bool = False,
    answered_from_cache: Optional[Dict[str, Any]] = None,
    rejected_cache: bool = False,
) -> str:
    """Append the turn to the conversation. Returns the Langfuse `message_id`
    minted by MCP (used by the UI to deep-link into the trace drawer).

    `index_for_search` asks MCP to embed + cache this turn for future reuse;
    `answered_from_cache` (set on a cache hit) tags the trace with provenance
    so observability can show which prior conversation answered it.
    `rejected_cache` marks a full re-answer the user requested after rejecting
    a prior cache-hit summary — tagged in the trace so dashboards can tell the
    cache was overridden.

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
        "index_for_search": index_for_search,
    }
    if datapack_id:
        args["datapack_id"] = datapack_id
    if answered_from_cache:
        # This conversation-search metadata (answered_from, summary_model, and
        # the chunks below) is persisted to Langfuse via the MCP
        # `conversation_add_message` tool call below (args["metadata"]); the
        # api_server decision-trace API reads it back from user_metadata so the
        # UI can show which prior conversation answered the query.
        # Slim chunk list for the decision-trace card: show ALL retrieved turns
        # (up to the 50-turn retrieval cap) so the trace reflects exactly what we
        # summarized. Each Q/A is still length-trimmed to keep metadata bounded.
        chunks = answered_from_cache.get("chunks") or []
        slim_chunks = [
            {
                "question": (c.get("question") or "")[:300],
                "answer": (c.get("answer") or "")[:600],
                "distance": c.get("distance"),
                "conversation_id": c.get("conversation_id"),
                "message_id": c.get("message_id"),
                "user_id": c.get("user_id"),
            }
            for c in chunks[:CONV_TRACE_MAX_CHUNKS]
            if isinstance(c, dict)
        ]
        meta_payload = {
            "answered_from": "conversation_cache",
            "source_conversation_id": answered_from_cache.get("conversation_id"),
            "source_message_id": answered_from_cache.get("message_id"),
            "match_distance": answered_from_cache.get("distance"),
        }
        if answered_from_cache.get("summary_model"):
            meta_payload["summary_model"] = answered_from_cache["summary_model"]
        if slim_chunks:
            meta_payload["conversation_chunks"] = slim_chunks
        args["metadata"] = json.dumps(meta_payload)
    elif rejected_cache:
        # Full re-answer after the user rejected a cache-hit summary. No source
        # provenance (we deliberately bypassed the cache); just mark it.
        args["metadata"] = json.dumps({"answered_from": "conversation_cache_rejected"})
    try:
        raw = await tool.ainvoke(args)
    except Exception as exc:  # noqa: BLE001
        logger.warning(
            "conversation_add_message failed (agent_id=%s, conversation_id=%s, datapack_id=%s): %s",
            agent_id, conversation_id, datapack_id, _format_exc_chain(exc),
            exc_info=exc,
        )
        return ""
    payload = _parse_json(raw)
    if isinstance(payload, dict):
        mid = payload.get("message_id")
        if isinstance(mid, str) and mid:
            return mid
    # The tool call SUCCEEDED transport-wise but returned no message_id —
    # typically an application-level error body like
    # {"error": "not_found"} / {"error": "free_tier_..."}. Without this
    # line the turn vanishes silently (best-effort persistence swallows
    # it). Log only the error fields (never the full payload — the tool
    # may echo the user's message, and warnings must stay PII-free).
    if isinstance(payload, dict):
        error_detail = "; ".join(
            _truncate(str(payload[k]), 150)
            for k in ("error", "message", "detail")
            if payload.get(k)
        ) or f"keys: {list(payload.keys())}"
    else:
        error_detail = f"non-dict payload of type {type(payload).__name__}"
    logger.warning(
        "conversation_add_message returned no message_id "
        "(agent_id=%s, conversation_id=%s, datapack_id=%s): %s",
        agent_id, conversation_id, datapack_id,
        error_detail,
    )
    return ""


# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #


def _extract_memory_hits(payload: object, relation_cap: Optional[int] = None) -> List[MemoryHit]:
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

    All vector hits returned by memory_search are passed through — relevance
    filtering is owned by the MCP server (top-K limit sent with the query).
    Items with no score at all are kept (legacy / string shapes).
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

    mem_hits: List[MemoryHit] = []
    graph_hits: List[MemoryHit] = []
    promoted_hits: List[MemoryHit] = []

    # Vector hits — `memory` is the documented Meko key; the others are
    # kept for shape-tolerance against older MCP servers. Bounded upstream by
    # memory_top_k (the limit sent to memory_search), so we iterate all returned.
    for item in vector_items:
        if isinstance(item, dict):
            raw_score = item.get("score")
            score: Optional[float] = (
                float(raw_score) if isinstance(raw_score, (int, float)) else None
            )
            text = (
                item.get("memory")
                or item.get("data")
                or item.get("text")
                or item.get("content")
                or ""
            )
            if text:
                mem_hits.append(MemoryHit(kind="mem", text=text, score=score, char_count=len(text)))
        elif isinstance(item, str):
            mem_hits.append(MemoryHit(kind="mem", text=item, score=None, char_count=len(item)))

    # Sort mem hits by score DESC so the highest-relevance memory ends
    # up at the top of the panel and at the front of the prompt. Mem0
    # doesn't guarantee an order on the server side, so we make it
    # explicit here. Items missing a score (legacy / bare-string shapes)
    # sort to the bottom rather than dropping; treat None as 0.
    mem_hits.sort(
        key=lambda h: h.score if h.score is not None else 0.0,
        reverse=True,
    )

    # Graph triples — flatten subject ─predicate→ object. Relations aren't
    # bounded by the vector `limit`, so cap them (relation_cap) to keep the
    # prompt from ballooning. Order is the MCP response order (unscored).
    if relation_cap is not None:
        relations = relations[:relation_cap]
        promoted_relations = promoted_relations[:relation_cap]
    for item in relations:
        triple = _format_relation_triple(item)
        if triple:
            graph_hits.append(MemoryHit(kind="graph", text=triple, score=None, char_count=len(triple)))

    # Promoted relations — same shape, different provenance tag so the
    # UI (and a future debugger) can tell them apart.
    for item in promoted_relations:
        triple = _format_relation_triple(item)
        if triple:
            promoted_hits.append(
                MemoryHit(kind="promoted", text=triple, score=None, char_count=len(triple))
            )

    # Preserve kind grouping in the returned list (mem → graph → promoted)
    # so downstream renderers don't have to bucket them again.
    return mem_hits + graph_hits + promoted_hits


def _extract_kb_hits(payload: object) -> List[KbHit]:
    """Pull structured KB chunk hits out of whatever `knowledgebase_search` returned.

    Tolerates `{results: [...]}`, `{matches: [...]}`, `{documents: [...]}`,
    or a bare list of chunks. `chunk_text` is the current Meko key; the
    others are fallbacks for older / alternate MCP servers.

    Relevance + result count are owned by the MCP's hybrid search — the gateway
    neither filters by distance nor caps the count; it renders all returned
    chunks. `score = 1 - distance` (same value shape as mem0's memory score,
    unclamped — the UI clamps on display) is computed only for UI sort order.
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
    for item in items:
        if isinstance(item, dict):
            raw_distance = item.get("distance")
            distance: Optional[float] = (
                float(raw_distance) if isinstance(raw_distance, (int, float)) else None
            )
            # Cosine similarity = 1 - distance (same value shape as mem0's
            # memory score). Passed through unclamped — the UI clamps on
            # display and mem0 clamps memory upstream.
            score: Optional[float] = (1.0 - distance) if distance is not None else None
            text = (
                item.get("chunk_text")
                or item.get("text")
                or item.get("content")
                or item.get("snippet")
                or item.get("chunk")
                or ""
            )
            if not text:
                continue
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

    # Sort by distance ASC so the closest match leads the list (and the
    # prompt). MCP usually returns in this order already but the indexer
    # is not contractually obligated to — make it explicit. Items with
    # no distance (legacy / bare-string shapes) sort to the bottom
    # rather than the top, since we can't claim they're the strongest.
    #
    # Note: we deliberately do NOT dedupe chunks with identical
    # chunk_text. If the indexer surfaces the same chunk multiple times
    # (e.g. repeated source content), we render it as-is — that's the
    # honest view of what MCP returned.
    hits.sort(
        key=lambda h: h.distance if h.distance is not None else float("inf"),
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
