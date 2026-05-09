"""Smoke test for the 6-step chain.

Mocks the LLM and the MCP tools so the test runs without any network or
provider credentials. Verifies:

  - `conversation_create` is called once per session and only on the first turn
  - `memory_search` runs every turn with the right scope/query/conversation_id
  - the LLM receives `[system, ...history, user]` in that order
  - `conversation_add_message` records both user input and assistant output
  - `memory_add` is fired (fire-and-forget) with the right messages payload
  - the session store accumulates the turn correctly
"""

from __future__ import annotations

import asyncio
import json
from typing import Any
from unittest.mock import AsyncMock, MagicMock

import pytest
from langchain_core.messages import AIMessage, HumanMessage, SystemMessage

from inference_gateway import chain as chain_module
from inference_gateway.chain import run_chat_turn
from inference_gateway.config import AppConfig
from inference_gateway.session_store import InMemorySessionStore


async def _drain_background_tasks() -> None:
    """Wait for all `_spawn_background`-scheduled tasks to finish.

    `memory_add` is now fire-and-forget so we have to explicitly drain
    those tasks before asserting on their mock invocations.
    """
    pending = list(chain_module._BACKGROUND_TASKS)
    if pending:
        await asyncio.gather(*pending, return_exceptions=True)


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
    """Minimal stand-in for `BaseChatModel` — returns scripted AIMessages."""

    def __init__(self, responses: list[str]) -> None:
        self._responses = list(responses)
        self.calls: list[list[Any]] = []

    async def ainvoke(self, messages: list[Any], *_, **__) -> AIMessage:
        self.calls.append(messages)
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
    addmem = _mock_tool({"status": "accepted"})

    tools = {
        "conversation_create": create,
        "memory_search": mem_search,
        "knowledgebase_search": kb_search,
        "conversation_add_message": addmsg,
        "memory_add": addmem,
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
            "datapack_id": "dp-1",
        }
    )

    # memory_add is fire-and-forget — wait for the background task to finish
    # before we assert on the mock's invocation.
    await _drain_background_tasks()

    # memory_add — verify the messages JSON shape Mem0 expects.
    addmem.ainvoke.assert_awaited_once()
    addmem_args = addmem.ainvoke.call_args.args[0]
    assert addmem_args["scope"] == "write"
    assert addmem_args["conversation_id"] == "conv-uuid-1"
    assert addmem_args["agent_id"] == "agent"
    assert addmem_args["datapack_id"] == "dp-1"
    parsed = json.loads(addmem_args["messages"])
    assert parsed == [
        {"role": "user", "content": "plan a trip"},
        {"role": "assistant", "content": "hello back"},
    ]

    # LLM saw [system, user]; system contains BOTH memory and KB context.
    assert len(llm.calls) == 1
    msgs = llm.calls[0]
    assert isinstance(msgs[0], SystemMessage)
    assert "user prefers hiking" in msgs[0].content       # memory_context
    assert "Patagonia trek W circuit" in msgs[0].content  # kb_context
    assert "[europe.md]" in msgs[0].content               # KB source attribution
    assert isinstance(msgs[-1], HumanMessage)
    assert msgs[-1].content == "plan a trip"

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
    addmem = _mock_tool({"status": "accepted"})

    tools = {
        "conversation_create": create,
        "memory_search": mem_search,
        "knowledgebase_search": kb_search,
        "conversation_add_message": addmsg,
        "memory_add": addmem,
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
    # Drain fire-and-forget memory_add tasks before asserting their counts.
    await _drain_background_tasks()

    # conversation_create only on the first turn.
    create.ainvoke.assert_awaited_once()
    # everything else runs every turn.
    assert mem_search.ainvoke.await_count == 2
    assert kb_search.ainvoke.await_count == 2
    assert addmsg.ainvoke.await_count == 2
    assert addmem.ainvoke.await_count == 2

    # On the second turn the LLM should see prior turn in history.
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
    addmem = _mock_tool({"status": "accepted"})

    tools = {
        "conversation_create": create,
        "memory_search": mem_search,
        "knowledgebase_search": kb_search,
        "conversation_add_message": addmsg,
        "memory_add": addmem,
    }
    store = InMemorySessionStore()

    out = await run_chat_turn(
        cfg=cfg, llm=llm, tools_by_name=tools, session_store=store,  # type: ignore[arg-type]
        session_id="s1", user_message="hi", datapack_id=None,
    )
    assert out.assistant_text == "fine"

    # memory_search runs (datapack_id is optional there).
    mem_search.ainvoke.assert_awaited_once()
    # kb_search must NOT run when datapack_id is missing.
    kb_search.ainvoke.assert_not_awaited()
    # System prompt should still render with the empty kb placeholder.
    assert "(none yet)" in llm.calls[0][0].content


async def test_search_failure_does_not_break_turn() -> None:
    """A flaky `memory_search` should degrade to no-context, not blow up.

    Also covers `knowledgebase_search` failure and `memory_add` failure —
    all three are best-effort and must not fail the user's request.
    """
    cfg = _make_cfg()
    llm = _FakeLLM(["ok"])
    create = _mock_tool({"conversation_id": "c1"})
    addmsg = _mock_tool({"status": "accepted"})

    flaky_mem = MagicMock()
    flaky_mem.ainvoke = AsyncMock(side_effect=RuntimeError("memory upstream 500"))
    flaky_kb = MagicMock()
    flaky_kb.ainvoke = AsyncMock(side_effect=RuntimeError("kb upstream 500"))
    flaky_addmem = MagicMock()
    flaky_addmem.ainvoke = AsyncMock(side_effect=RuntimeError("memory_add 500"))

    tools = {
        "conversation_create": create,
        "memory_search": flaky_mem,
        "knowledgebase_search": flaky_kb,
        "conversation_add_message": addmsg,
        "memory_add": flaky_addmem,
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
    # Drain the fire-and-forget memory_add task (which raised internally —
    # _add_memory's try/except catches it; this just makes sure the task
    # is fully resolved before the test ends so pytest doesn't see a
    # warning about pending tasks).
    await _drain_background_tasks()


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
    addmem = _mock_tool({"status": "accepted"})

    tools = {
        "conversation_create": wrapped_create,
        "memory_search": wrapped_mem,
        "knowledgebase_search": wrapped_kb,
        "conversation_add_message": addmsg,
        "memory_add": addmem,
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
