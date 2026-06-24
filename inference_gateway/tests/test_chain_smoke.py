"""Smoke test for the chat chain.

Mocks the LLM and the MCP tools so the test runs without any network or
provider credentials. Verifies:

  - `conversation_create` is called once per session and only on the first turn
  - `memory_search` runs every turn with the right scope/query/conversation_id
  - the LLM receives `[system, ...history, user]` in that order
  - `conversation_add_message` records both user input and assistant output
  - the session store accumulates the turn correctly

Memory extraction is now handled inside the MCP server's `conversation_add_message`
implementation (background thread, `infer=True`). The gateway no longer runs a
second LLM extraction call or calls `memory_add` directly.
"""

from __future__ import annotations

import json
from typing import Any
from unittest.mock import AsyncMock, MagicMock

import pytest
from langchain_core.messages import AIMessage, HumanMessage, SystemMessage

from inference_gateway.chain import run_chat_turn
from inference_gateway.config import AppConfig
from inference_gateway.session_store import InMemorySessionStore


def _make_cfg() -> AppConfig:
    return AppConfig(
        llm_provider="openai",
        llm_model="gpt-4o-mini",
        mcp_endpoint="http://example.local/mcp",
        aws_region="us-east-1",
        port=8088,
        cors_origins=("http://localhost:5173",),
        agent_id="agent",
        max_history=10,
        memory_top_k=5,
    )


def _mock_tool(response: Any) -> MagicMock:
    """Build a fake LangChain tool that returns the given response from `ainvoke`."""
    tool = MagicMock()
    if isinstance(response, (dict, list)):
        tool.ainvoke = AsyncMock(return_value=json.dumps(response))
    else:
        tool.ainvoke = AsyncMock(return_value=str(response))
    return tool


class _FakeLLM:
    """Minimal stand-in for `BaseChatModel`.

    The chain calls `ainvoke` once per turn (main chat reply only —
    memory extraction is now inside the MCP server, not here).
    """

    def __init__(self, responses: "list[str]") -> None:
        self._responses = list(responses)
        self.calls: list[list[Any]] = []

    async def ainvoke(self, messages: list[Any], *_, **__) -> AIMessage:
        self.calls.append(messages)
        if not self._responses:
            return AIMessage(content="")
        return AIMessage(content=self._responses.pop(0))


async def test_full_turn_wires_everything() -> None:
    cfg = _make_cfg()
    llm = _FakeLLM(["hello back"])
    create = _mock_tool({"conversation_id": "conv-uuid-1"})
    mem_search = _mock_tool({"results": [{"memory": "user prefers hiking"}]})
    kb_search = _mock_tool(
        {"results": [{"document_name": "europe.md", "text": "Patagonia trek W circuit"}]}
    )
    addmsg = _mock_tool({"status": "accepted", "message_id": "m1"})

    tools = {
        "conversation_create": create,
        "memory_search": mem_search,
        "knowledgebase_search": kb_search,
        "conversation_add_message": addmsg,
    }
    store = InMemorySessionStore(max_messages_per_session=10)

    out = await run_chat_turn(
        cfg=cfg,
        llm=llm,  # type: ignore[arg-type]
        tools_by_name=tools,
        session_store=store,
        session_id="s1",
        user_message="plan a trip",
        datapack_id="dp-1",
    )

    assert out.assistant_text == "hello back"
    # message_id is extracted from `conversation_add_message`'s response
    # so the UI can deep-link into the trace drawer.
    assert out.message_id == "m1"

    create.ainvoke.assert_awaited_once_with(
        {
            "scope": "write",
            "agent_id": "agent",
            "title": "plan a trip",
            "datapack_id": "dp-1",
        }
    )

    mem_search.ainvoke.assert_awaited_once_with(
        {
            "scope": "read",
            "query": "plan a trip",
            "conversation_id": "conv-uuid-1",
            "agent_id": "agent",
            "limit": 5,
            "datapack_id": "dp-1",
        }
    )

    kb_search.ainvoke.assert_awaited_once_with(
        {
            "scope": "read",
            "query": "plan a trip",
            "conversation_id": "conv-uuid-1",
            "datapack_id": "dp-1",
            "agent_id": "agent",
            "limit": 5,
        }
    )

    addmsg.ainvoke.assert_awaited_once_with(
        {
            "scope": "write",
            "conversation_id": "conv-uuid-1",
            "agent_id": "agent",
            "input": "plan a trip",
            "output": "hello back",
            "index_for_search": True,
            "datapack_id": "dp-1",
        }
    )

    # One LLM call: main chat only. Memory extraction is now inside the MCP server.
    assert len(llm.calls) == 1
    main_msgs = llm.calls[0]
    assert isinstance(main_msgs[0], SystemMessage)
    assert "user prefers hiking" in main_msgs[0].content       # memory_context
    assert "Patagonia trek W circuit" in main_msgs[0].content  # kb_context
    assert "[europe.md]" in main_msgs[0].content               # KB source attribution
    assert isinstance(main_msgs[-1], HumanMessage)
    assert main_msgs[-1].content == "plan a trip"

    # Session store updated.
    history = await store.get_messages("s1")
    assert history == [("user", "plan a trip"), ("assistant", "hello back")]
    assert (await store.get_conversation_id("s1")) == "conv-uuid-1"


async def test_second_turn_reuses_conversation_and_includes_history() -> None:
    """conversation_create runs once; everything else runs every turn."""
    cfg = _make_cfg()
    llm = _FakeLLM(["a", "b"])
    create = _mock_tool({"conversation_id": "conv-uuid-1"})
    mem_search = _mock_tool({"results": []})
    kb_search = _mock_tool({"results": []})
    addmsg = _mock_tool({"status": "accepted", "message_id": "m"})

    tools = {
        "conversation_create": create,
        "memory_search": mem_search,
        "knowledgebase_search": kb_search,
        "conversation_add_message": addmsg,
    }
    store = InMemorySessionStore(max_messages_per_session=10)

    await run_chat_turn(
        cfg=cfg, llm=llm, tools_by_name=tools, session_store=store,  # type: ignore[arg-type]
        session_id="s1", user_message="first", datapack_id="dp-1",
    )
    await run_chat_turn(
        cfg=cfg, llm=llm, tools_by_name=tools, session_store=store,  # type: ignore[arg-type]
        session_id="s1", user_message="second", datapack_id="dp-1",
    )

    # conversation_create only on the first turn.
    create.ainvoke.assert_awaited_once()
    # Search + persistence run every turn.
    assert mem_search.ainvoke.await_count == 2
    assert kb_search.ainvoke.await_count == 2
    assert addmsg.ainvoke.await_count == 2

    # One LLM call per turn (no extraction call).
    assert len(llm.calls) == 2
    second_call = llm.calls[1]
    roles = [type(m).__name__ for m in second_call]
    # system, user(first), assistant(a), user(second)
    assert roles == ["SystemMessage", "HumanMessage", "AIMessage", "HumanMessage"]
    assert second_call[1].content == "first"
    assert second_call[2].content == "a"
    assert second_call[3].content == "second"


async def test_kb_search_skipped_when_no_datapack_id() -> None:
    """kb_search requires datapack_id; gateway must skip it (not error) when missing."""
    cfg = _make_cfg()
    llm = _FakeLLM(["fine"])
    create = _mock_tool({"conversation_id": "c1"})
    mem_search = _mock_tool({"results": []})
    kb_search = _mock_tool({"results": []})
    addmsg = _mock_tool({"status": "accepted"})

    tools = {
        "conversation_create": create,
        "memory_search": mem_search,
        "knowledgebase_search": kb_search,
        "conversation_add_message": addmsg,
    }
    store = InMemorySessionStore()

    # Use a non-trivial message so the trivial-query short-circuit doesn't
    # mask what this test is actually verifying (kb_search skipped because
    # of missing datapack_id, not because the query is "hi").
    out = await run_chat_turn(
        cfg=cfg, llm=llm, tools_by_name=tools, session_store=store,  # type: ignore[arg-type]
        session_id="s1", user_message="plan a trip to spain", datapack_id=None,
    )
    assert out.assistant_text == "fine"

    # memory_search runs (datapack_id is optional there).
    mem_search.ainvoke.assert_awaited_once()
    # kb_search must NOT run when datapack_id is missing.
    kb_search.ainvoke.assert_not_awaited()
    # System prompt should still render with the empty kb placeholder.
    assert "(none yet)" in llm.calls[0][0].content


async def test_search_failure_does_not_break_turn() -> None:
    """A flaky `memory_search` or `knowledgebase_search` should degrade to
    no-context, not blow up. Both are best-effort and must not fail the
    user's request.
    """
    cfg = _make_cfg()
    llm = _FakeLLM(["ok"])
    create = _mock_tool({"conversation_id": "c1"})
    addmsg = _mock_tool({"status": "accepted"})

    flaky_mem = MagicMock()
    flaky_mem.ainvoke = AsyncMock(side_effect=RuntimeError("memory upstream 500"))
    flaky_kb = MagicMock()
    flaky_kb.ainvoke = AsyncMock(side_effect=RuntimeError("kb upstream 500"))

    tools = {
        "conversation_create": create,
        "memory_search": flaky_mem,
        "knowledgebase_search": flaky_kb,
        "conversation_add_message": addmsg,
    }
    store = InMemorySessionStore()

    # datapack_id set so kb_search is actually attempted (and then fails).
    out = await run_chat_turn(
        cfg=cfg, llm=llm, tools_by_name=tools, session_store=store,  # type: ignore[arg-type]
        session_id="s1", user_message="hi", datapack_id="dp-1",
    )
    assert out.assistant_text == "ok"
    # System message should still render with empty placeholders for both.
    sys_content = llm.calls[0][0].content
    assert sys_content.count("(none yet)") == 2  # memory + kb


async def test_unwraps_langchain_mcp_content_block_format() -> None:
    """Real `langchain-mcp-adapters` wraps text results in a content-block list.

    Regression for a real-world bug where conversation_create's UUID was
    nested inside `[{"type": "text", "text": "<json>", "id": "lc_..."}]`
    and the parser returned the outer list unchanged.
    """
    cfg = _make_cfg()
    llm = _FakeLLM(["wrapped-ok"])

    inner_payload = {
        "id": "1c9f1fb23f2146ca8b79e3071fabe41c",
        "agent_id": "agent",
        "user_id": "user-1",
        "title": "Plan a hiking trip",
    }
    wrapped_create = MagicMock()
    wrapped_create.ainvoke = AsyncMock(
        return_value=[
            {"type": "text", "text": json.dumps(inner_payload), "id": "lc_abc"}
        ]
    )

    wrapped_mem = MagicMock()
    wrapped_mem.ainvoke = AsyncMock(
        return_value=[
            {
                "type": "text",
                "text": json.dumps({"results": [{"memory": "user prefers hiking"}]}),
                "id": "lc_def",
            }
        ]
    )

    wrapped_kb = MagicMock()
    wrapped_kb.ainvoke = AsyncMock(
        return_value=[
            {
                "type": "text",
                "text": json.dumps({"results": []}),
                "id": "lc_ghi",
            }
        ]
    )

    addmsg = _mock_tool({"status": "accepted"})

    tools = {
        "conversation_create": wrapped_create,
        "memory_search": wrapped_mem,
        "knowledgebase_search": wrapped_kb,
        "conversation_add_message": addmsg,
    }
    store = InMemorySessionStore()

    out = await run_chat_turn(
        cfg=cfg, llm=llm, tools_by_name=tools, session_store=store,  # type: ignore[arg-type]
        session_id="s1", user_message="plan a trip", datapack_id="dp-1",
    )
    assert out.assistant_text == "wrapped-ok"
    # UUID extracted from the inner JSON's `id` field
    assert (await store.get_conversation_id("s1")) == "1c9f1fb23f2146ca8b79e3071fabe41c"
    # Memory context unwrapped + bullet-formatted into the system prompt
    sys_msg = llm.calls[0][0].content
    assert "user prefers hiking" in sys_msg


async def test_trivial_query_short_circuits_retrieval() -> None:
    """A greeting like "hi" must NOT trigger memory_search or kb_search.

    Vector search returns top-K regardless of relevance, so a vague query
    embeds into five random hits that smear the system prompt. The chain
    skips both round-trips and feeds the LLM empty grounding context
    (which renders as `(none yet)` in the prompt).
    """
    cfg = _make_cfg()
    llm = _FakeLLM(["hi back"])
    create = _mock_tool({"conversation_id": "c1"})
    # The mocks would error if invoked — we want to PROVE they aren't.
    mem_search = MagicMock()
    mem_search.ainvoke = AsyncMock(side_effect=AssertionError("memory_search must not be called for trivial query"))
    kb_search = MagicMock()
    kb_search.ainvoke = AsyncMock(side_effect=AssertionError("kb_search must not be called for trivial query"))
    addmsg = _mock_tool({"status": "accepted", "message_id": "m1"})

    tools = {
        "conversation_create": create,
        "memory_search": mem_search,
        "knowledgebase_search": kb_search,
        "conversation_add_message": addmsg,
    }
    store = InMemorySessionStore()

    out = await run_chat_turn(
        cfg=cfg, llm=llm, tools_by_name=tools, session_store=store,  # type: ignore[arg-type]
        session_id="s1", user_message="hi", datapack_id="dp-1",
    )
    assert out.assistant_text == "hi back"

    # Retrieval round-trips skipped.
    mem_search.ainvoke.assert_not_awaited()
    kb_search.ainvoke.assert_not_awaited()

    # System prompt rendered with empty placeholders for both retrieval blocks.
    sys_content = llm.calls[0][0].content
    assert sys_content.count("(none yet)") == 2  # memory + kb

    # The returned ChatTurnResult carries empty context for the UI's Agent
    # Reasoning panel — which self-hides when both are empty.
    assert out.memory_context == ""
    assert out.kb_context == ""


async def test_real_question_still_triggers_retrieval() -> None:
    """Counterpart to the trivial-query test — verifies the short-circuit
    doesn't accidentally skip legitimate prompts. A genuine question must
    still hit both searches."""
    cfg = _make_cfg()
    llm = _FakeLLM(["ok"])
    create = _mock_tool({"conversation_id": "c1"})
    mem_search = _mock_tool({"results": []})
    kb_search = _mock_tool({"results": []})
    addmsg = _mock_tool({"status": "accepted"})

    tools = {
        "conversation_create": create,
        "memory_search": mem_search,
        "knowledgebase_search": kb_search,
        "conversation_add_message": addmsg,
    }
    store = InMemorySessionStore()

    await run_chat_turn(
        cfg=cfg, llm=llm, tools_by_name=tools, session_store=store,  # type: ignore[arg-type]
        session_id="s1", user_message="what does John like?", datapack_id="dp-1",
    )

    mem_search.ainvoke.assert_awaited_once()
    kb_search.ainvoke.assert_awaited_once()


async def test_processing_steps_capture_tool_args_for_observability() -> None:
    """Retrieval steps and `conversation_create` must surface their MCP
    args (other than `query`, `scope`, `conversation_id`) on the step's
    `tool_args` field — drives the "MCP args" sub-expand in the Agent
    Reasoning panel. Locks in what the UI relies on so a future refactor
    doesn't silently drop these fields from the response."""
    cfg = _make_cfg()
    llm = _FakeLLM(["ok"])
    create = _mock_tool({"conversation_id": "conv-uuid-tools"})
    mem_search = _mock_tool({"results": []})
    kb_search = _mock_tool({"results": []})
    addmsg = _mock_tool({"status": "accepted", "message_id": "m1"})

    tools = {
        "conversation_create": create,
        "memory_search": mem_search,
        "knowledgebase_search": kb_search,
        "conversation_add_message": addmsg,
    }
    store = InMemorySessionStore()

    out = await run_chat_turn(
        cfg=cfg, llm=llm, tools_by_name=tools, session_store=store,  # type: ignore[arg-type]
        session_id="s1", user_message="plan a trip to spain", datapack_id="dp-x",
    )

    by_name = {s.name: s for s in out.processing_steps}

    # tool_args mirrors the FULL payload sent to MCP (minus `query`,
    # which has its own dedicated ProcessingStep field). Users get an
    # honest "what we sent" view instead of a curated subset.

    # conversation_create takes scope/agent_id/title/datapack_id; it
    # mints a conversation_id rather than receiving one, so that
    # field is naturally absent.
    cc_args = by_name["conversation_create"].tool_args
    assert cc_args is not None
    assert cc_args["scope"] == "write"
    assert cc_args["agent_id"] == "agent"
    assert cc_args["title"].startswith("plan a trip to spain")
    assert cc_args["datapack_id"] == "dp-x"
    assert "conversation_id" not in cc_args  # this call MINTS it

    # memory_search payload — `query` lives on its own field, every
    # other arg is present.
    ms_args = by_name["memory_search"].tool_args
    assert ms_args is not None
    assert ms_args["scope"] == "read"
    assert ms_args["conversation_id"] == "conv-uuid-tools"
    assert ms_args["agent_id"] == "agent"
    assert ms_args["limit"] == cfg.memory_top_k
    assert ms_args["datapack_id"] == "dp-x"
    assert "query" not in ms_args  # query lives on its own field

    # knowledgebase_search — same shape as memory_search.
    ks_args = by_name["knowledgebase_search"].tool_args
    assert ks_args is not None
    assert ks_args["scope"] == "read"
    assert ks_args["conversation_id"] == "conv-uuid-tools"
    assert ks_args["agent_id"] == "agent"
    assert ks_args["limit"] == cfg.memory_top_k
    assert ks_args["datapack_id"] == "dp-x"
    assert "query" not in ks_args


async def test_resume_conversation_id_skips_create_and_seeds_session() -> None:
    """When the UI hands the gateway an existing `conversation_id` (user
    picked a previous chat from the dropdown), the chain must:

      - NOT call MCP `conversation_create` (that would mint a sibling
        conversation and split the thread in Langfuse).
      - Adopt the supplied id into the session store so subsequent turns
        in the same session reuse it without re-sending.
      - Record a `conversation_create` step with status="skipped" and a
        clear detail string — the Agent Reasoning panel still wants a
        timeline entry for this stage.
      - Pass the resumed id to `memory_search`, `knowledgebase_search`,
        and `conversation_add_message` so every persisted artifact for
        this turn references the SAME conversation row.
    """
    cfg = _make_cfg()
    llm = _FakeLLM(["resumed-ok"])
    # conversation_create must NEVER be invoked here. Wire it as an
    # AssertionError so a regression that calls it loudly fails the test.
    create = MagicMock()
    create.ainvoke = AsyncMock(
        side_effect=AssertionError(
            "conversation_create must not run when resume_conversation_id is set",
        )
    )
    mem_search = _mock_tool({"results": []})
    kb_search = _mock_tool({"results": []})
    addmsg = _mock_tool({"status": "accepted", "message_id": "m-resume-1"})

    tools = {
        "conversation_create": create,
        "memory_search": mem_search,
        "knowledgebase_search": kb_search,
        "conversation_add_message": addmsg,
    }
    store = InMemorySessionStore(max_messages_per_session=10)

    resumed_id = "existing-conv-uuid"
    out = await run_chat_turn(
        cfg=cfg, llm=llm, tools_by_name=tools, session_store=store,  # type: ignore[arg-type]
        session_id="s-resume", user_message="continue the trip planning",
        datapack_id="dp-1", resume_conversation_id=resumed_id,
    )

    assert out.assistant_text == "resumed-ok"
    # Session store seeded with the supplied id.
    assert (await store.get_conversation_id("s-resume")) == resumed_id

    # conversation_create skipped entirely.
    create.ainvoke.assert_not_awaited()

    # Downstream MCP calls all reference the resumed id.
    mem_search.ainvoke.assert_awaited_once()
    assert mem_search.ainvoke.call_args.args[0]["conversation_id"] == resumed_id
    kb_search.ainvoke.assert_awaited_once()
    assert kb_search.ainvoke.call_args.args[0]["conversation_id"] == resumed_id
    addmsg.ainvoke.assert_awaited_once()
    assert addmsg.ainvoke.call_args.args[0]["conversation_id"] == resumed_id

    # Timeline entry surfaces the skip so the Agent Reasoning panel can
    # show "resumed existing conversation" instead of an empty slot.
    by_name = {s.name: s for s in out.processing_steps}
    cc_step = by_name["conversation_create"]
    assert cc_step.status == "skipped"
    assert "resumed" in cc_step.detail.lower()
    assert cc_step.tool_args == {"conversation_id": resumed_id}


async def test_resume_conversation_id_ignored_when_session_already_has_one() -> None:
    """Once the session store already has a conversation_id for a session
    (e.g. the user's first turn already minted one), any subsequently
    supplied `resume_conversation_id` is silently ignored — the existing
    mapping wins. Prevents a misbehaving client from switching the
    conversation row mid-session and orphaning earlier messages."""
    cfg = _make_cfg()
    llm = _FakeLLM(["ok"])
    create = _mock_tool({"conversation_id": "minted-on-first-turn"})
    mem_search = _mock_tool({"results": []})
    kb_search = _mock_tool({"results": []})
    addmsg = _mock_tool({"status": "accepted"})

    tools = {
        "conversation_create": create,
        "memory_search": mem_search,
        "knowledgebase_search": kb_search,
        "conversation_add_message": addmsg,
    }
    store = InMemorySessionStore()
    # Pre-seed the session store as if a previous turn had minted an id.
    await store.set_conversation_id("s1", "minted-on-first-turn")

    await run_chat_turn(
        cfg=cfg, llm=llm, tools_by_name=tools, session_store=store,  # type: ignore[arg-type]
        session_id="s1", user_message="next question",
        datapack_id="dp-1", resume_conversation_id="some-other-id",
    )

    # conversation_create not called — session already had one.
    create.ainvoke.assert_not_awaited()
    # The existing mapping wins; the "some-other-id" is ignored.
    assert (await store.get_conversation_id("s1")) == "minted-on-first-turn"
    # Downstream calls use the existing id, not the supplied one.
    assert mem_search.ainvoke.call_args.args[0]["conversation_id"] == "minted-on-first-turn"


async def test_missing_conversation_create_raises_clearly() -> None:
    cfg = _make_cfg()
    llm = _FakeLLM(["unused"])
    tools = {
        # No conversation_create — the chain treats this as a hard error.
        "memory_search": _mock_tool({"results": []}),
        "conversation_add_message": _mock_tool({"status": "accepted"}),
    }
    store = InMemorySessionStore()

    with pytest.raises(RuntimeError, match="conversation_create"):
        await run_chat_turn(
            cfg=cfg, llm=llm, tools_by_name=tools, session_store=store,  # type: ignore[arg-type]
            session_id="s1", user_message="hi", datapack_id=None,
        )
