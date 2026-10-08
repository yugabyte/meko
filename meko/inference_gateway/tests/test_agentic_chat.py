"""Tests for agentic chat (MEKO-407, Phases 0-2).

Covers the `_agentic_llm_invoke` tool loop in isolation and one full
`run_chat_turn` pass with the flag on. Everything is mocked — no network,
no provider credentials.

Behavior under test:
  - flag off / no tool call  → exactly one LLM call, byte-identical to today
  - tool call                → executed via MCP with gateway-enforced identity
                               (agent_id / conversation_id / datapack_id are
                               never trusted from the model), result fed back,
                               ProcessingStep recorded
  - round cap                → a final answer is always forced without tools
  - unknown tool / tool error→ structured error result, turn never crashes
"""

from __future__ import annotations

import json
from typing import Any
from unittest.mock import AsyncMock, MagicMock

import pytest
from langchain_core.messages import AIMessage, HumanMessage, SystemMessage, ToolMessage

from inference_gateway.chain import (
    ProcessingStep,
    _agentic_llm_invoke,
    _scoped_tool_names,
    run_chat_turn,
)
from inference_gateway.config import AppConfig
from inference_gateway.session_store import InMemorySessionStore

pytestmark = pytest.mark.asyncio


def _make_cfg(**overrides: Any) -> AppConfig:
    defaults: dict[str, Any] = dict(
        llm_provider="openai",
        llm_model="gpt-4o-mini",
        mcp_endpoint="http://example.local/mcp",
        aws_region="us-east-1",
        port=8088,
        cors_origins=("http://localhost:5173",),
        agent_id="agent",
        max_history=10,
        memory_top_k=5,
        agentic_chat_enabled=True,
        agent_tools=("memory_get_all", "datapack_list"),
        max_tool_rounds=2,
    )
    defaults.update(overrides)
    return AppConfig(**defaults)


def _agent_tool(name: str, response: Any, schema_keys: tuple[str, ...]) -> MagicMock:
    """A fake LangChain tool with a real `.name`, `.args` schema, and ainvoke."""
    tool = MagicMock()
    tool.name = name
    tool.args = {k: {"type": "string"} for k in schema_keys}
    payload = response if isinstance(response, str) else json.dumps(response)
    tool.ainvoke = AsyncMock(return_value=payload)
    return tool


class _ScriptedAgentLLM:
    """Tool-capable fake: `bind_tools` returns self; `ainvoke` pops scripted
    responses. Records every message list it was invoked with, and whether
    the invocation happened through the bound (tools) or plain object —
    both share one script since the implementation reuses `messages`."""

    def __init__(self, responses: list[AIMessage]) -> None:
        self._responses = list(responses)
        self.calls: list[list[Any]] = []
        self.bound_tool_names: list[str] | None = None

    def bind_tools(self, tools: list[Any]) -> "_ScriptedAgentLLM":
        self.bound_tool_names = [t.name for t in tools]
        return self

    async def ainvoke(self, messages: list[Any], *_, **__) -> AIMessage:
        self.calls.append(list(messages))
        if not self._responses:
            return AIMessage(content="(exhausted)")
        return self._responses.pop(0)


def _tool_call(name: str, args: dict, call_id: str = "c1") -> dict:
    return {"name": name, "args": args, "id": call_id, "type": "tool_call"}


# ---------------------------------------------------------------------------
# _agentic_llm_invoke unit tests
# ---------------------------------------------------------------------------


async def test_no_tool_call_is_single_invoke() -> None:
    cfg = _make_cfg()
    llm = _ScriptedAgentLLM([AIMessage(content="plain answer")])
    tools = {"memory_get_all": _agent_tool("memory_get_all", {}, ("conversation_id",))}
    steps: list[ProcessingStep] = []

    result, results, _pending = await _agentic_llm_invoke(
        cfg=cfg,
        llm=llm,
        tools_by_name=tools,
        messages=[HumanMessage(content="hi")],
        steps=steps,
        conversation_id="conv-1",
        datapack_id="dp-1",
    )

    assert result.content == "plain answer"
    assert len(results) == 1
    assert len(llm.calls) == 1
    assert steps == []  # no tool executed, no agent steps
    # Only allowlisted-and-resolved tools get bound.
    assert llm.bound_tool_names == ["memory_get_all"]


async def test_tool_call_executes_with_gateway_enforced_identity() -> None:
    """The model's own agent_id/conversation_id/datapack_id proposals are
    overridden by the gateway (schema-permitting) — never trusted."""
    cfg = _make_cfg()
    tool = _agent_tool(
        "memory_get_all",
        {"memories": [{"memory": "fact"}]},
        ("agent_id", "conversation_id", "datapack_id", "promoted"),
    )
    llm = _ScriptedAgentLLM(
        [
            AIMessage(
                content="",
                tool_calls=[
                    _tool_call(
                        "memory_get_all",
                        # Model tries to wander: wrong agent, foreign datapack.
                        {"promoted": False, "agent_id": "evil", "datapack_id": "someone-elses"},
                    )
                ],
            ),
            AIMessage(content="you have 1 memory"),
        ]
    )
    steps: list[ProcessingStep] = []

    result, results, _pending = await _agentic_llm_invoke(
        cfg=cfg,
        llm=llm,
        tools_by_name={"memory_get_all": tool},
        messages=[HumanMessage(content="list all my memories")],
        steps=steps,
        conversation_id="conv-1",
        datapack_id="dp-1",
    )

    assert result.content == "you have 1 memory"
    assert len(results) == 2
    tool.ainvoke.assert_awaited_once_with(
        {
            "promoted": False,
            "agent_id": "agent",  # forced to cfg.agent_id
            "conversation_id": "conv-1",  # forced to the turn's conversation
            "datapack_id": "dp-1",  # forced to the turn's datapack
        }
    )
    # One observability step for the tool call.
    assert [s.name for s in steps] == ["agent_tool:memory_get_all"]
    assert steps[0].status == "ok"
    assert steps[0].tool_args["agent_id"] == "agent"
    # The tool result went back to the model as a ToolMessage.
    second_call = llm.calls[1]
    assert isinstance(second_call[-1], ToolMessage)
    assert "fact" in second_call[-1].content


async def test_round_cap_forces_final_answer_without_tools() -> None:
    cfg = _make_cfg(max_tool_rounds=2)
    tool = _agent_tool("datapack_list", [], ("conversation_id",))
    greedy = AIMessage(content="", tool_calls=[_tool_call("datapack_list", {})])
    llm = _ScriptedAgentLLM([greedy, greedy, AIMessage(content="forced answer")])
    steps: list[ProcessingStep] = []

    result, results, _pending = await _agentic_llm_invoke(
        cfg=cfg,
        llm=llm,
        tools_by_name={"datapack_list": tool},
        messages=[HumanMessage(content="loop forever")],
        steps=steps,
        conversation_id="conv-1",
        datapack_id=None,
    )

    assert result.content == "forced answer"
    # 2 tool rounds + 1 forced final = 3 LLM calls, 3 results for usage summing.
    assert len(results) == 3
    assert len(llm.calls) == 3
    assert tool.ainvoke.await_count == 2
    assert [s.name for s in steps] == ["agent_tool:datapack_list"] * 2


async def test_unknown_tool_returns_structured_error() -> None:
    cfg = _make_cfg()
    llm = _ScriptedAgentLLM(
        [
            AIMessage(content="", tool_calls=[_tool_call("datapack_delete", {"datapack_id": "x"})]),
            AIMessage(content="cannot do that"),
        ]
    )
    steps: list[ProcessingStep] = []

    result, _, _pending = await _agentic_llm_invoke(
        cfg=cfg,
        llm=llm,
        tools_by_name={"memory_get_all": _agent_tool("memory_get_all", {}, ("conversation_id",))},
        messages=[HumanMessage(content="delete my datapack")],
        steps=steps,
        conversation_id="conv-1",
        datapack_id="dp-1",
    )

    assert result.content == "cannot do that"
    assert steps[0].name == "agent_tool:datapack_delete"
    assert steps[0].status == "error"
    tool_msg = llm.calls[1][-1]
    assert isinstance(tool_msg, ToolMessage)
    assert json.loads(tool_msg.content)["error"] == "tool_not_available"


async def test_tool_exception_becomes_error_result_not_crash() -> None:
    cfg = _make_cfg()
    tool = _agent_tool("memory_get_all", {}, ("conversation_id",))
    tool.ainvoke = AsyncMock(side_effect=RuntimeError("mcp down"))
    llm = _ScriptedAgentLLM(
        [
            AIMessage(content="", tool_calls=[_tool_call("memory_get_all", {})]),
            AIMessage(content="sorry, could not fetch"),
        ]
    )
    steps: list[ProcessingStep] = []

    result, _, _pending = await _agentic_llm_invoke(
        cfg=cfg,
        llm=llm,
        tools_by_name={"memory_get_all": tool},
        messages=[HumanMessage(content="list memories")],
        steps=steps,
        conversation_id="conv-1",
        datapack_id="dp-1",
    )

    assert result.content == "sorry, could not fetch"
    assert steps[0].status == "error"
    tool_msg = llm.calls[1][-1]
    assert json.loads(tool_msg.content) == {"error": "tool_failed", "detail": "mcp down"}


# ---------------------------------------------------------------------------
# run_chat_turn end-to-end with the flag on
# ---------------------------------------------------------------------------


def _mock_tool(response: Any) -> MagicMock:
    tool = MagicMock()
    payload = response if isinstance(response, str) else json.dumps(response)
    tool.ainvoke = AsyncMock(return_value=payload)
    return tool


async def test_run_chat_turn_agentic_end_to_end() -> None:
    cfg = _make_cfg(agent_tools=("memory_get_all",))
    agent_tool = _agent_tool(
        "memory_get_all",
        {"memories": [{"memory": "user prefers hiking"}], "total": 1},
        ("agent_id", "conversation_id", "datapack_id"),
    )
    llm = _ScriptedAgentLLM(
        [
            AIMessage(content="", tool_calls=[_tool_call("memory_get_all", {})]),
            AIMessage(content="You have 1 memory: user prefers hiking"),
        ]
    )
    tools = {
        "conversation_create": _mock_tool({"conversation_id": "conv-uuid-1"}),
        "memory_search": _mock_tool({"results": []}),
        "knowledgebase_search": _mock_tool({"results": []}),
        "conversation_add_message": _mock_tool({"status": "accepted", "message_id": "m1"}),
        "memory_get_all": agent_tool,
    }
    store = InMemorySessionStore(max_messages_per_session=10)

    router = _ScriptedAgentLLM([AIMessage(content="memory_get_all")])

    out = await run_chat_turn(
        cfg=cfg,
        llm=llm,
        tools_by_name=tools,  # type: ignore[arg-type]
        session_store=store,
        session_id="s1",
        user_message="show me all my memories",
        datapack_id="dp-1",
        summary_llm=router,  # type: ignore[arg-type]
    )

    assert out.assistant_text == "You have 1 memory: user prefers hiking"
    assert len(router.calls) == 1  # Phase 3 router consulted once
    # Identity was gateway-enforced on the agent tool call.
    agent_tool.ainvoke.assert_awaited_once_with(
        {
            "agent_id": "agent",
            "conversation_id": "conv-uuid-1",
            "datapack_id": "dp-1",
        }
    )
    # The agent tool call is visible in the observability timeline, and the
    # llm_invoke step notes the extra round.
    names = [s.name for s in out.processing_steps]
    assert "agent_tool:memory_get_all" in names
    llm_steps = [s for s in out.processing_steps if s.name == "llm_invoke"]
    assert llm_steps and llm_steps[0].status == "ok"
    assert "2 llm round(s)" in llm_steps[0].detail
    # Guidance is appended to the system prompt in agentic mode.
    assert "confirmation card" in llm.calls[0][0].content


# ---------------------------------------------------------------------------
# Phase 1: memory_add auto-executes (explicit save intent)
# ---------------------------------------------------------------------------


async def test_memory_add_auto_executes_verbatim() -> None:
    cfg = _make_cfg(agent_tools=("memory_add",))
    tool = _agent_tool(
        "memory_add",
        {"results": [{"id": "mem-1"}]},
        ("text", "agent_id", "conversation_id", "datapack_id"),
    )
    llm = _ScriptedAgentLLM(
        [
            AIMessage(
                content="",
                tool_calls=[
                    _tool_call("memory_add", {"text": "deploy scripts run from repo root only"})
                ],
            ),
            AIMessage(content="Saved."),
        ]
    )
    steps: list[ProcessingStep] = []

    result, _, pending = await _agentic_llm_invoke(
        cfg=cfg,
        llm=llm,
        tools_by_name={"memory_add": tool},
        messages=[HumanMessage(content="remember this: deploy scripts run from repo root only")],
        steps=steps,
        conversation_id="conv-1",
        datapack_id="dp-1",
    )

    assert result.content == "Saved."
    assert pending is None  # memory_add is auto-tier, not confirm-tier
    tool.ainvoke.assert_awaited_once_with(
        {
            "text": "deploy scripts run from repo root only",
            "agent_id": "agent",
            "conversation_id": "conv-1",
            "datapack_id": "dp-1",
        }
    )
    assert steps[0].name == "agent_tool:memory_add"
    assert steps[0].status == "ok"


# ---------------------------------------------------------------------------
# Phase 2: confirm-tier tools propose, never execute
# ---------------------------------------------------------------------------


async def test_confirm_tool_proposes_pending_action_without_executing() -> None:
    cfg = _make_cfg(agent_tools=(), agent_confirm_tools=("datapack_create",))
    tool = _agent_tool(
        "datapack_create", {"datapack_id": "new"}, ("name", "conversation_id", "datapack_id")
    )
    llm = _ScriptedAgentLLM(
        [
            AIMessage(
                content="", tool_calls=[_tool_call("datapack_create", {"name": "analytics"})]
            ),
            AIMessage(content="I've prepared the datapack creation — please confirm."),
        ]
    )
    steps: list[ProcessingStep] = []

    result, results, pending = await _agentic_llm_invoke(
        cfg=cfg,
        llm=llm,
        tools_by_name={"datapack_create": tool},
        messages=[HumanMessage(content="create a datapack called analytics")],
        steps=steps,
        conversation_id="conv-1",
        datapack_id="dp-1",
        routed_tool="datapack_create",
    )

    # NOT executed — proposed for confirmation.
    tool.ainvoke.assert_not_awaited()
    assert pending is not None
    assert pending["tool_name"] == "datapack_create"
    assert pending["args"] == {
        "name": "analytics",
        "conversation_id": "conv-1",  # identity frozen at proposal time
        "datapack_id": "dp-1",
    }
    assert pending["action_id"]
    # The model was told, and produced user-facing text about confirmation.
    tool_msg = llm.calls[1][-1]
    assert isinstance(tool_msg, ToolMessage)
    assert json.loads(tool_msg.content)["status"] == "confirmation_required"
    assert "confirm" in result.content.lower()
    assert steps[0].status == "skipped"
    assert "awaiting user confirmation" in steps[0].detail


async def test_second_confirm_call_in_one_turn_is_rejected() -> None:
    cfg = _make_cfg(agent_tools=(), agent_confirm_tools=("datapack_create",))
    tool = _agent_tool("datapack_create", {}, ("name", "conversation_id"))
    llm = _ScriptedAgentLLM(
        [
            AIMessage(
                content="",
                tool_calls=[
                    _tool_call("datapack_create", {"name": "one"}, call_id="c1"),
                    _tool_call("datapack_create", {"name": "two"}, call_id="c2"),
                ],
            ),
            AIMessage(content="one action at a time"),
        ]
    )
    steps: list[ProcessingStep] = []

    _, _, pending = await _agentic_llm_invoke(
        cfg=cfg,
        llm=llm,
        tools_by_name={"datapack_create": tool},
        messages=[HumanMessage(content="create two datapacks")],
        steps=steps,
        conversation_id="conv-1",
        datapack_id=None,
        routed_tool="datapack_create",
    )

    tool.ainvoke.assert_not_awaited()
    assert pending is not None and pending["args"]["name"] == "one"
    second_msg = llm.calls[1][-1]
    assert json.loads(second_msg.content)["error"] == "pending_action_exists"
    assert [s.status for s in steps] == ["skipped", "error"]


async def test_run_chat_turn_stores_pending_action() -> None:
    cfg = _make_cfg(agent_tools=(), agent_confirm_tools=("datapack_create",))
    create_tool = _agent_tool("datapack_create", {}, ("name", "conversation_id"))
    llm = _ScriptedAgentLLM(
        [
            AIMessage(
                content="", tool_calls=[_tool_call("datapack_create", {"name": "analytics"})]
            ),
            AIMessage(content="Please confirm the datapack creation."),
        ]
    )
    tools = {
        "conversation_create": _mock_tool({"conversation_id": "conv-uuid-1"}),
        "memory_search": _mock_tool({"results": []}),
        "knowledgebase_search": _mock_tool({"results": []}),
        "conversation_add_message": _mock_tool({"status": "accepted", "message_id": "m1"}),
        "datapack_create": create_tool,
    }
    store = InMemorySessionStore(max_messages_per_session=10)

    out = await run_chat_turn(
        cfg=cfg,
        llm=llm,
        tools_by_name=tools,  # type: ignore[arg-type]
        session_store=store,
        session_id="s1",
        user_message="create a datapack called analytics",
        datapack_id="dp-1",
        summary_llm=_ScriptedAgentLLM([AIMessage(content="datapack_create")]),  # type: ignore[arg-type]
    )

    assert out.pending_action is not None
    action_id = out.pending_action["action_id"]
    create_tool.ainvoke.assert_not_awaited()
    # Stored in the session store for /chat/confirm-action to claim.
    claimed = await store.pop_pending_action("s1", action_id)
    assert claimed is not None and claimed["tool_name"] == "datapack_create"
    # Claiming is one-shot.
    assert await store.pop_pending_action("s1", action_id) is None


async def test_pending_action_pop_rejects_stale_id() -> None:
    store = InMemorySessionStore()
    await store.set_pending_action("s1", {"action_id": "a1", "tool_name": "x", "args": {}})
    await store.set_pending_action("s1", {"action_id": "a2", "tool_name": "y", "args": {}})
    assert await store.pop_pending_action("s1", "a1") is None  # replaced
    assert await store.pop_pending_action("missing", "a2") is None  # no session
    claimed = await store.pop_pending_action("s1", "a2")
    assert claimed is not None and claimed["tool_name"] == "y"


# ---------------------------------------------------------------------------
# /chat/confirm-action endpoint
# ---------------------------------------------------------------------------


def _make_confirm_app(cfg: AppConfig, store: InMemorySessionStore):
    from fastapi import FastAPI

    from inference_gateway.routes import chat as chat_routes

    app = FastAPI()
    app.include_router(chat_routes.router)
    app.state.config = cfg
    app.state.session_store = store
    app.state.llm = None
    app.state.summary_llm = None
    return app


def test_confirm_action_executes_frozen_args(monkeypatch) -> None:
    import asyncio

    from fastapi.testclient import TestClient

    from inference_gateway.routes import chat as chat_routes

    cfg = _make_cfg()
    store = InMemorySessionStore()
    asyncio.run(
        store.set_pending_action(
            "s1",
            {
                "action_id": "a1",
                "tool_name": "datapack_create",
                "args": {"name": "analytics", "conversation_id": "conv-1"},
            },
        )
    )
    tool = _mock_tool({"datapack_id": "dp-new", "datapack_name": "analytics"})
    addmsg = _mock_tool({"status": "accepted"})

    async def fake_build(endpoint: str, token: str):
        assert token == "tok"
        return {"datapack_create": tool, "conversation_add_message": addmsg}

    monkeypatch.setattr(chat_routes, "build_mcp_tools", fake_build)

    client = TestClient(_make_confirm_app(cfg, store))
    resp = client.post(
        "/chat/confirm-action",
        json={
            "session_id": "s1",
            "action_id": "a1",
            "user_token": "tok",
        },
    )

    assert resp.status_code == 200
    body = resp.json()
    assert body["status"] == "executed"
    assert body["result"]["datapack_id"] == "dp-new"
    tool.ainvoke.assert_awaited_once_with({"name": "analytics", "conversation_id": "conv-1"})
    addmsg.ainvoke.assert_awaited_once()  # action recorded as a turn

    # Second confirm of the same action: already claimed -> 404.
    resp2 = client.post(
        "/chat/confirm-action",
        json={
            "session_id": "s1",
            "action_id": "a1",
            "user_token": "tok",
        },
    )
    assert resp2.status_code == 404


def test_confirm_action_cancel_discards_without_executing(monkeypatch) -> None:
    import asyncio

    from fastapi.testclient import TestClient

    from inference_gateway.routes import chat as chat_routes

    cfg = _make_cfg()
    store = InMemorySessionStore()
    asyncio.run(
        store.set_pending_action(
            "s1",
            {
                "action_id": "a1",
                "tool_name": "datapack_create",
                "args": {},
            },
        )
    )
    fake_build = AsyncMock()
    monkeypatch.setattr(chat_routes, "build_mcp_tools", fake_build)

    client = TestClient(_make_confirm_app(cfg, store))
    resp = client.post(
        "/chat/confirm-action",
        json={
            "session_id": "s1",
            "action_id": "a1",
            "user_token": "tok",
            "approve": False,
        },
    )

    assert resp.status_code == 200
    assert resp.json()["status"] == "cancelled"
    fake_build.assert_not_awaited()  # cancelled actions never touch MCP


def test_default_confirm_tier_scope() -> None:
    """WorkbenchLM chats are scoped to one datapack: datapack_create doesn't
    fit this surface, memory_update is deferred, and the highest-blast-radius
    tools stay out entirely."""
    from inference_gateway.config import DEFAULT_AGENT_CONFIRM_TOOLS, DEFAULT_AGENT_TOOLS

    assert set(DEFAULT_AGENT_CONFIRM_TOOLS) == {
        "memory_delete_by_id",
        "memory_promote",
        "datapack_update",
    }
    for banned in ("datapack_create", "memory_update", "memory_delete_all", "datapack_delete"):
        assert banned not in DEFAULT_AGENT_CONFIRM_TOOLS
        assert banned not in DEFAULT_AGENT_TOOLS


async def test_confirm_action_pinned_to_chat_datapack() -> None:
    """The model cannot aim a confirm-tier action at another datapack — the
    proposal freezes datapack_id to the one this chat is scoped to."""
    cfg = _make_cfg(agent_tools=(), agent_confirm_tools=("memory_promote",))
    tool = _agent_tool(
        "memory_promote", {}, ("memory_ids", "agent_id", "conversation_id", "datapack_id")
    )
    llm = _ScriptedAgentLLM(
        [
            AIMessage(
                content="",
                tool_calls=[
                    _tool_call(
                        "memory_promote",
                        {"memory_ids": ["mem-1"], "datapack_id": "someone-elses-datapack"},
                    )
                ],
            ),
            AIMessage(content="please confirm the promotion"),
        ]
    )
    steps: list[ProcessingStep] = []

    _, _, pending = await _agentic_llm_invoke(
        cfg=cfg,
        llm=llm,
        tools_by_name={"memory_promote": tool},
        messages=[HumanMessage(content="promote mem-1 to the team KB")],
        steps=steps,
        conversation_id="conv-1",
        datapack_id="dp-chatting",
        routed_tool="memory_promote",
    )

    assert pending is not None
    assert pending["args"]["datapack_id"] == "dp-chatting"  # model's choice overridden
    tool.ainvoke.assert_not_awaited()


# ---------------------------------------------------------------------------
# SHOW_PG_AMP tool-tier composition
# ---------------------------------------------------------------------------


def test_show_pg_amp_extends_both_tiers(monkeypatch) -> None:
    from inference_gateway.main import parse_args

    monkeypatch.setenv("SHOW_PG_AMP", "true")
    cfg, _ = parse_args([])
    # Names must match the MCP server's registration (amp_ prefix) or the
    # allowlist resolves nothing at bind time.
    assert "amp_cluster_list" in cfg.agent_tools  # AMP reads auto-execute
    assert "amp_cluster_create" in cfg.agent_confirm_tools  # AMP writes need confirm
    assert "amp_cluster_create" not in cfg.agent_tools

    monkeypatch.delenv("SHOW_PG_AMP")
    cfg2, _ = parse_args([])
    assert "amp_cluster_list" not in cfg2.agent_tools
    assert "amp_cluster_create" not in cfg2.agent_confirm_tools


async def test_run_chat_turn_flag_off_never_binds_tools() -> None:
    cfg = _make_cfg(agentic_chat_enabled=False)
    llm = _ScriptedAgentLLM([AIMessage(content="plain")])
    tools = {
        "conversation_create": _mock_tool({"conversation_id": "conv-uuid-1"}),
        "memory_search": _mock_tool({"results": []}),
        "knowledgebase_search": _mock_tool({"results": []}),
        "conversation_add_message": _mock_tool({"status": "accepted", "message_id": "m1"}),
    }
    store = InMemorySessionStore(max_messages_per_session=10)

    out = await run_chat_turn(
        cfg=cfg,
        llm=llm,
        tools_by_name=tools,  # type: ignore[arg-type]
        session_store=store,
        session_id="s1",
        user_message="hello there, how are you today?",
        datapack_id="dp-1",
    )

    assert out.assistant_text == "plain"
    assert llm.bound_tool_names is None  # bind_tools never called
    assert len(llm.calls) == 1  # single LLM call, as today
    assert "confirmation card" not in llm.calls[0][0].content  # no guidance block


# ---------------------------------------------------------------------------
# Phase 3: cheap-model tool router
# ---------------------------------------------------------------------------


def _run_turn_tools(**extra: Any) -> dict[str, Any]:
    tools: dict[str, Any] = {
        "conversation_create": _mock_tool({"conversation_id": "conv-uuid-1"}),
        "memory_search": _mock_tool({"results": []}),
        "knowledgebase_search": _mock_tool({"results": []}),
        "conversation_add_message": _mock_tool({"status": "accepted", "message_id": "m1"}),
    }
    tools.update(extra)
    return tools


async def test_router_action_intent_skips_conversation_cache() -> None:
    """A routed tool means the turn must reach the tool loop: the unified CPS
    call (whose conversation tier can end the turn with a cached summary) is
    replaced by the grounded legacy retrieval with conversation skipped."""
    cfg = _make_cfg(
        agent_tools=(),
        agent_confirm_tools=("amp_cluster_create",),
        contextual_prominence_search_enabled=True,
    )
    cps_tool = _mock_tool({"results": []})
    llm = _ScriptedAgentLLM(
        [
            AIMessage(content="", tool_calls=[_tool_call("amp_cluster_create", {"name": "pg1"})]),
            AIMessage(content="Please confirm the cluster creation."),
        ]
    )
    router = _ScriptedAgentLLM([AIMessage(content="amp_cluster_create")])
    tools = _run_turn_tools(
        context_search=cps_tool,
        amp_cluster_create=_agent_tool("amp_cluster_create", {}, ("name", "conversation_id")),
    )
    store = InMemorySessionStore(max_messages_per_session=10)

    out = await run_chat_turn(
        cfg=cfg,
        llm=llm,
        tools_by_name=tools,  # type: ignore[arg-type]
        session_store=store,
        session_id="s1",
        user_message="can you spin up an AMP cluster named pg1",
        datapack_id="dp-1",
        summary_llm=router,  # type: ignore[arg-type]
    )

    cps_tool.ainvoke.assert_not_awaited()  # cache can't short-circuit
    assert out.pending_action is not None  # the loop ran and proposed
    route_steps = [s for s in out.processing_steps if s.name == "tool_route"]
    assert route_steps and route_steps[0].detail.startswith("amp_cluster_create")
    # The route-forced retrieval is NOT a user cache rejection: the persisted
    # turn must not carry the conversation_cache_rejected telemetry tag.
    addmsg_args = tools["conversation_add_message"].ainvoke.await_args.args[0]
    assert "conversation_cache_rejected" not in str(addmsg_args.get("metadata", ""))


async def test_router_chatter_and_unknown_tool_default_to_cps() -> None:
    cfg = _make_cfg(contextual_prominence_search_enabled=True)
    for verbose_reply in ("I would pick cps for this one.", "datapack_delete"):
        cps_tool = _mock_tool({"results": []})
        llm = _ScriptedAgentLLM([AIMessage(content="hi!")])
        router = _ScriptedAgentLLM([AIMessage(content=verbose_reply)])
        store = InMemorySessionStore(max_messages_per_session=10)

        out = await run_chat_turn(
            cfg=cfg,
            llm=llm,
            tools_by_name=_run_turn_tools(context_search=cps_tool),  # type: ignore[arg-type]
            session_store=store,
            session_id="s1",
            user_message="tell me about my project preferences",
            datapack_id="dp-1",
            summary_llm=router,  # type: ignore[arg-type]
        )

        # datapack_delete isn't allowlisted; chatter names no tool — both
        # fall back to the default unified search.
        cps_tool.ainvoke.assert_awaited_once()
        route_steps = [s for s in out.processing_steps if s.name == "tool_route"]
        assert route_steps and route_steps[0].detail.startswith("cps")
        assert out.assistant_text == "hi!"


async def test_router_not_consulted_when_flag_off() -> None:
    cfg = _make_cfg(agentic_chat_enabled=False)
    router = _ScriptedAgentLLM([])
    store = InMemorySessionStore(max_messages_per_session=10)

    out = await run_chat_turn(
        cfg=cfg,
        llm=_ScriptedAgentLLM([AIMessage(content="plain")]),
        tools_by_name=_run_turn_tools(),  # type: ignore[arg-type]
        session_store=store,
        session_id="s1",
        user_message="hello there, how are you today?",
        datapack_id="dp-1",
        summary_llm=router,  # type: ignore[arg-type]
    )

    assert router.calls == []  # flag off ⇒ no router call
    assert all(s.name != "tool_route" for s in out.processing_steps)
    assert out.assistant_text == "plain"


def test_scoped_tool_names_per_route() -> None:
    """Router-scoped binding: only the routed family's schemas ride the call."""
    cfg = _make_cfg(
        agent_tools=(
            "memory_get_all",
            "memory_search",
            "knowledgebase_search",
            "amp_cluster_list",
            "amp_branch_list",
        ),
        agent_confirm_tools=("memory_promote", "amp_cluster_create"),
    )
    # cps/default: Meko auto tier only — no AMP, no confirm schemas.
    assert _scoped_tool_names(cfg, None) == (
        "memory_get_all",
        "memory_search",
        "knowledgebase_search",
    )
    # AMP route: the whole AMP family (reads + confirms) + the search reads.
    assert _scoped_tool_names(cfg, "amp_cluster_create") == (
        "amp_cluster_list",
        "amp_branch_list",
        "amp_cluster_create",
        "memory_search",
        "knowledgebase_search",
    )
    assert _scoped_tool_names(cfg, "amp_branch_list") == _scoped_tool_names(
        cfg, "amp_cluster_create"
    )
    # Meko confirm route: auto tier + exactly the routed tool.
    assert _scoped_tool_names(cfg, "memory_promote") == (
        "memory_get_all",
        "memory_search",
        "knowledgebase_search",
        "memory_promote",
    )
    # Routed auto tool is already in the tier — no duplicate.
    assert _scoped_tool_names(cfg, "memory_get_all") == (
        "memory_get_all",
        "memory_search",
        "knowledgebase_search",
    )


async def test_scoped_binding_excludes_unrouted_confirm_schema() -> None:
    """A cps-routed turn cannot propose a confirm-tier action: its schema is
    not bound, so the model's stray call errors as not-in-allowlist."""
    cfg = _make_cfg(
        agent_tools=("memory_get_all",),
        agent_confirm_tools=("memory_promote",),
    )
    promote = _agent_tool("memory_promote", {}, ("memory_ids",))
    llm = _ScriptedAgentLLM(
        [
            AIMessage(
                content="", tool_calls=[_tool_call("memory_promote", {"memory_ids": ["m1"]})]
            ),
            AIMessage(content="I can promote that once you ask explicitly."),
        ]
    )
    steps: list[ProcessingStep] = []

    _, _, pending = await _agentic_llm_invoke(
        cfg=cfg,
        llm=llm,
        tools_by_name={
            "memory_get_all": _agent_tool("memory_get_all", {}, ()),
            "memory_promote": promote,
        },
        messages=[HumanMessage(content="tell me about memory m1")],
        steps=steps,
        conversation_id="conv-1",
        datapack_id="dp-1",
    )

    assert llm.bound_tool_names == ["memory_get_all"]  # schema never sent
    promote.ainvoke.assert_not_awaited()
    assert pending is None
    assert steps[0].status == "error" and "not in allowlist" in steps[0].detail


async def test_amp_not_registered_read_check_blocks_proposal() -> None:
    """Guidance flow for an unregistered AMP deployment: the model checks with
    a read (auto-tier, executes immediately), sees AMP_NOT_REGISTERED, and
    answers in prose — no pending action for a create that must fail."""
    cfg = _make_cfg(
        agent_tools=("amp_cluster_list",),
        agent_confirm_tools=("amp_cluster_create",),
    )
    list_tool = _agent_tool(
        "amp_cluster_list",
        {
            "error": "AMP_NOT_REGISTERED",
            "detail": "AMP is not registered; an admin must register the AMP controller first",
        },
        ("conversation_id", "datapack_id"),
    )
    create_tool = _agent_tool("amp_cluster_create", {}, ("name",))
    llm = _ScriptedAgentLLM(
        [
            AIMessage(content="", tool_calls=[_tool_call("amp_cluster_list", {})]),
            AIMessage(
                content=(
                    "Your deployment is not registered to the AMP service — an admin "
                    "must register the AMP controller in the Meko Admin console first."
                )
            ),
        ]
    )
    steps: list[ProcessingStep] = []

    result, _, pending = await _agentic_llm_invoke(
        cfg=cfg,
        llm=llm,
        tools_by_name={"amp_cluster_list": list_tool, "amp_cluster_create": create_tool},
        messages=[HumanMessage(content="spin up an AMP cluster")],
        steps=steps,
        conversation_id="conv-1",
        datapack_id="dp-1",
        routed_tool="amp_cluster_create",
    )

    assert pending is None  # no Allow/Deny card
    create_tool.ainvoke.assert_not_awaited()  # nothing attempted the create
    list_tool.ainvoke.assert_awaited_once()  # the read check ran
    assert "not registered" in result.content
    # The error reached the model as the read tool's result.
    tool_msgs = [m for m in llm.calls[1] if isinstance(m, ToolMessage)]
    assert tool_msgs and "AMP_NOT_REGISTERED" in tool_msgs[-1].content


async def test_token_tracking_counts_only_tool_result_rounds() -> None:
    """Product decision: track_token_usage meters rounds 2+ (the calls that
    read tool results and synthesize the answer). Round 1, the tool-decision
    call, is excluded. Single-round turns are metered as before."""
    cfg = _make_cfg(agent_tools=("memory_get_all",))
    track = _mock_tool({"status": "ok"})
    tools = _run_turn_tools(
        memory_get_all=_agent_tool("memory_get_all", {"memories": []}, ()),
        track_token_usage=track,
    )
    llm = _ScriptedAgentLLM(
        [
            AIMessage(
                content="",
                tool_calls=[_tool_call("memory_get_all", {})],
                usage_metadata={"input_tokens": 3000, "output_tokens": 50, "total_tokens": 3050},
            ),
            AIMessage(
                content="you have no memories",
                usage_metadata={"input_tokens": 3200, "output_tokens": 100, "total_tokens": 3300},
            ),
        ]
    )
    store = InMemorySessionStore(max_messages_per_session=10)

    await run_chat_turn(
        cfg=cfg,
        llm=llm,
        tools_by_name=tools,  # type: ignore[arg-type]
        session_store=store,
        session_id="s1",
        user_message="list all my memories",
        datapack_id="dp-1",
        summary_llm=_ScriptedAgentLLM([AIMessage(content="memory_get_all")]),  # type: ignore[arg-type]
    )

    args = track.ainvoke.await_args.args[0]
    assert args["name"] == "LLM Response"
    assert args["input_tokens"] == 3200  # round 1's 3000 excluded
    assert args["output_tokens"] == 100
    assert args["total_tokens"] == 3300


async def test_unscoped_turn_strips_model_datapack_id() -> None:
    """Blocker regression: when the turn has NO datapack scope, a
    model-proposed datapack_id must be stripped (MCP resolves the caller's
    active datapack) — never passed through."""
    cfg = _make_cfg()
    tool = _agent_tool(
        "memory_get_all",
        {"memories": []},
        ("agent_id", "conversation_id", "datapack_id"),
    )
    llm = _ScriptedAgentLLM(
        [
            AIMessage(
                content="",
                tool_calls=[_tool_call("memory_get_all", {"datapack_id": "model-chosen-datapack"})],
            ),
            AIMessage(content="done"),
        ]
    )
    steps: list[ProcessingStep] = []

    await _agentic_llm_invoke(
        cfg=cfg,
        llm=llm,
        tools_by_name={"memory_get_all": tool},
        messages=[HumanMessage(content="list memories")],
        steps=steps,
        conversation_id="conv-1",
        datapack_id=None,
    )

    tool.ainvoke.assert_awaited_once_with(
        {
            "agent_id": "agent",
            "conversation_id": "conv-1",
            # no datapack_id at all — the model's value did not survive
        }
    )


async def test_no_proposal_turn_clears_stored_pending_action() -> None:
    """A turn that proposes nothing invalidates an earlier unconfirmed
    proposal — its action_id must no longer be claimable."""
    store = InMemorySessionStore(max_messages_per_session=10)
    await store.set_pending_action(
        "s1", {"action_id": "old-1", "tool_name": "memory_promote", "args": {}}
    )

    cfg = _make_cfg()
    out = await run_chat_turn(
        cfg=cfg,
        llm=_ScriptedAgentLLM([AIMessage(content="plain answer")]),
        tools_by_name=_run_turn_tools(memory_get_all=_agent_tool("memory_get_all", {}, ())),  # type: ignore[arg-type]
        session_store=store,
        session_id="s1",
        user_message="just a question, no action",
        datapack_id="dp-1",
        summary_llm=_ScriptedAgentLLM([AIMessage(content="cps")]),  # type: ignore[arg-type]
    )

    assert out.pending_action is None
    assert await store.pop_pending_action("s1", "old-1") is None  # cleared


async def test_cache_hit_turn_clears_stored_pending_action() -> None:
    """The conversation-cache early return must clear the stored pending
    action too — the UI drops its card on every response, so a stale
    action_id must not stay claimable via /chat/confirm-action."""
    store = InMemorySessionStore(max_messages_per_session=10)
    await store.set_pending_action(
        "s1", {"action_id": "old-1", "tool_name": "memory_promote", "args": {}}
    )

    cfg = _make_cfg(contextual_prominence_search_enabled=True)
    ctx_tool = _mock_tool(
        {
            "results": {
                "conversation": {
                    "hits": [
                        {
                            "question": "prior q",
                            "answer": "prior a",
                            "conversation_id": "c0",
                            "message_id": "m0",
                            "distance": 0.1,
                        }
                    ],
                    "thread": [],
                },
                "memory": [],
                "knowledge_base": [],
            }
        }
    )

    out = await run_chat_turn(
        cfg=cfg,
        llm=_ScriptedAgentLLM([]),  # cache path: main LLM never called
        tools_by_name=_run_turn_tools(context_search=ctx_tool),  # type: ignore[arg-type]
        session_store=store,
        session_id="s1",
        user_message="what did we conclude about the design?",
        datapack_id="dp-1",
        summary_llm=_ScriptedAgentLLM(
            [
                AIMessage(content="cps"),  # router verdict
                AIMessage(content="prior answer summary"),  # cache summarizer
            ]
        ),  # type: ignore[arg-type]
    )

    assert out.assistant_text == "prior answer summary"
    assert await store.pop_pending_action("s1", "old-1") is None  # cleared
