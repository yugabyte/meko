"""Tests for POST /memory/add (Labs' 'add a memory' lesson) -- request-model
validation directly against MemoryAddRequest (test_chat_request_validation.py's
convention), plus the route function called directly with a mocked MCP tool
(test_chain_smoke.py's `_mock_tool` convention). No FastAPI TestClient --
this package doesn't use one anywhere else.
"""

from __future__ import annotations

import json
from types import SimpleNamespace
from typing import Any
from unittest.mock import AsyncMock, MagicMock, patch

import pytest
from fastapi import HTTPException
from pydantic import ValidationError

from inference_gateway.config import AppConfig
from inference_gateway.routes.chat import MemoryAddRequest, memory_add

_BASE_FIELDS = {
    "text": "We squash-merge feature branches.",
    "user_token": "tok",
    "datapack_id": "dp-1",
}


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


def _fake_request(cfg: AppConfig) -> SimpleNamespace:
    return SimpleNamespace(app=SimpleNamespace(state=SimpleNamespace(config=cfg)))


def _mock_tool(response: Any) -> MagicMock:
    tool = MagicMock()
    if isinstance(response, (dict, list)):
        tool.ainvoke = AsyncMock(return_value=json.dumps(response))
    else:
        tool.ainvoke = AsyncMock(return_value=str(response))
    return tool


def test_text_within_cap_is_accepted() -> None:
    req = MemoryAddRequest(**_BASE_FIELDS)
    assert req.text == _BASE_FIELDS["text"]


def test_empty_text_is_rejected() -> None:
    with pytest.raises(ValidationError):
        MemoryAddRequest(**{**_BASE_FIELDS, "text": ""})


def test_text_over_cap_is_rejected() -> None:
    with pytest.raises(ValidationError):
        MemoryAddRequest(**{**_BASE_FIELDS, "text": "x" * 2001})


def test_datapack_id_is_required() -> None:
    with pytest.raises(ValidationError):
        MemoryAddRequest(text="hi", user_token="tok")


async def test_success_returns_memory_id_and_sends_the_right_args() -> None:
    convo_tool = _mock_tool({"conversation_id": "conv-1"})
    tool = _mock_tool({"memory_id": "mem-123"})
    with patch(
        "inference_gateway.routes.chat.build_mcp_tools",
        AsyncMock(return_value={"conversation_create": convo_tool, "memory_add": tool}),
    ):
        resp = await memory_add(MemoryAddRequest(**_BASE_FIELDS), _fake_request(_make_cfg()))

    assert resp.ok is True
    assert resp.memory_id == "mem-123"
    convo_tool.ainvoke.assert_awaited_once_with(
        {
            "scope": "write",
            "agent_id": "agent",
            "title": _BASE_FIELDS["text"],
            "datapack_id": "dp-1",
        }
    )
    # conversation_id comes from conversation_create (memory_add has no
    # conversation to resume -- see the route's own comment) and metadata is
    # a JSON string, matching memory_add's real Pydantic signature
    # (Optional[str], parsed with json.loads server-side), not a raw dict.
    tool.ainvoke.assert_awaited_once_with(
        {
            "text": _BASE_FIELDS["text"],
            "agent_id": "agent",
            "conversation_id": "conv-1",
            "datapack_id": "dp-1",
            "metadata": json.dumps({"source": "labs-memory-lesson"}),
        }
    )


async def test_long_text_truncates_the_conversation_title() -> None:
    convo_tool = _mock_tool({"conversation_id": "conv-1"})
    tool = _mock_tool({"memory_id": "mem-123"})
    long_text = "x" * 200
    with patch(
        "inference_gateway.routes.chat.build_mcp_tools",
        AsyncMock(return_value={"conversation_create": convo_tool, "memory_add": tool}),
    ):
        await memory_add(
            MemoryAddRequest(**{**_BASE_FIELDS, "text": long_text}), _fake_request(_make_cfg())
        )

    # Matches chain.py's own _truncate exactly, now reused directly rather
    # than a separately-derived slice (77 chars + "...").
    sent_title = convo_tool.ainvoke.await_args.args[0]["title"]
    assert sent_title == "x" * 79 + "…"


async def test_conversation_create_response_wrapped_in_mcp_content_blocks() -> None:
    # Regression test: a real .ainvoke() call (no tool-call id, exactly what
    # this route does) gets langchain-mcp-adapters' actual wire shape --
    # [{"type": "text", "text": "<json>"}] -- not the bare dict/string
    # _mock_tool's other cases produce. Confirmed live against a real
    # conversation_create call; the naive "dict or json.loads(str)" check
    # this route used before fell through to `{}` on exactly this shape.
    convo_tool = MagicMock()
    convo_tool.ainvoke = AsyncMock(
        return_value=[{"type": "text", "text": json.dumps({"id": "conv-live"})}]
    )
    tool = _mock_tool({"memory_id": "mem-123"})
    with patch(
        "inference_gateway.routes.chat.build_mcp_tools",
        AsyncMock(return_value={"conversation_create": convo_tool, "memory_add": tool}),
    ):
        resp = await memory_add(MemoryAddRequest(**_BASE_FIELDS), _fake_request(_make_cfg()))

    assert resp.ok is True
    assert tool.ainvoke.await_args.args[0]["conversation_id"] == "conv-live"


async def test_conversation_create_id_under_a_different_key_still_works() -> None:
    # chain.py's own _create_conversation tries several keys
    # (conversation_id/id/uuid/session_id) since the tool's return shape
    # isn't documented -- this route mirrors that same fallback.
    convo_tool = _mock_tool({"id": "conv-2"})
    tool = _mock_tool({"memory_id": "mem-123"})
    with patch(
        "inference_gateway.routes.chat.build_mcp_tools",
        AsyncMock(return_value={"conversation_create": convo_tool, "memory_add": tool}),
    ):
        await memory_add(MemoryAddRequest(**_BASE_FIELDS), _fake_request(_make_cfg()))

    assert tool.ainvoke.await_args.args[0]["conversation_id"] == "conv-2"


async def test_conversation_create_missing_tool_returns_502() -> None:
    tool = _mock_tool({"memory_id": "mem-123"})
    with patch(
        "inference_gateway.routes.chat.build_mcp_tools",
        AsyncMock(return_value={"memory_add": tool}),
    ):
        with pytest.raises(HTTPException) as exc_info:
            await memory_add(MemoryAddRequest(**_BASE_FIELDS), _fake_request(_make_cfg()))

    assert exc_info.value.status_code == 502
    assert "conversation_create" in exc_info.value.detail
    tool.ainvoke.assert_not_awaited()


async def test_conversation_create_failure_returns_502() -> None:
    convo_tool = MagicMock()
    convo_tool.ainvoke = AsyncMock(side_effect=RuntimeError("mcp timeout"))
    tool = _mock_tool({"memory_id": "mem-123"})
    with patch(
        "inference_gateway.routes.chat.build_mcp_tools",
        AsyncMock(return_value={"conversation_create": convo_tool, "memory_add": tool}),
    ):
        with pytest.raises(HTTPException) as exc_info:
            await memory_add(MemoryAddRequest(**_BASE_FIELDS), _fake_request(_make_cfg()))

    assert exc_info.value.status_code == 502
    assert "conversation_create failed" in exc_info.value.detail
    tool.ainvoke.assert_not_awaited()


async def test_conversation_create_returns_no_id_returns_502() -> None:
    convo_tool = _mock_tool({"status": "ok"})  # no conversation_id/id/uuid/session_id key
    tool = _mock_tool({"memory_id": "mem-123"})
    with patch(
        "inference_gateway.routes.chat.build_mcp_tools",
        AsyncMock(return_value={"conversation_create": convo_tool, "memory_add": tool}),
    ):
        with pytest.raises(HTTPException) as exc_info:
            await memory_add(MemoryAddRequest(**_BASE_FIELDS), _fake_request(_make_cfg()))

    assert exc_info.value.status_code == 502
    assert "did not return an id" in exc_info.value.detail


async def test_conversation_create_tool_reported_error_surfaces_its_own_detail() -> None:
    # A tool-level error (invalid_title/invalid_metadata/create_failed -- see
    # meko-mcp-server's conversation_create) has no id key either, but must
    # not fall through to the generic "did not return an id: {raw payload}"
    # 502 -- that's a confusing, implementation-leaking message for what is
    # a clean, already-explained failure.
    convo_tool = _mock_tool({"error": "create_failed", "detail": "Langfuse write failed."})
    tool = _mock_tool({"memory_id": "mem-123"})
    with patch(
        "inference_gateway.routes.chat.build_mcp_tools",
        AsyncMock(return_value={"conversation_create": convo_tool, "memory_add": tool}),
    ):
        with pytest.raises(HTTPException) as exc_info:
            await memory_add(MemoryAddRequest(**_BASE_FIELDS), _fake_request(_make_cfg()))

    assert exc_info.value.status_code == 502
    assert exc_info.value.detail == "Langfuse write failed."
    tool.ainvoke.assert_not_awaited()
    tool.ainvoke.assert_not_awaited()


async def test_tool_reported_error_maps_to_502() -> None:
    convo_tool = _mock_tool({"conversation_id": "conv-1"})
    tool = _mock_tool({"error": "free_tier_limit_reached", "detail": "Memory limit reached."})
    with patch(
        "inference_gateway.routes.chat.build_mcp_tools",
        AsyncMock(return_value={"conversation_create": convo_tool, "memory_add": tool}),
    ):
        with pytest.raises(HTTPException) as exc_info:
            await memory_add(MemoryAddRequest(**_BASE_FIELDS), _fake_request(_make_cfg()))

    assert exc_info.value.status_code == 502
    assert exc_info.value.detail == "Memory limit reached."


async def test_missing_tool_returns_502() -> None:
    with patch("inference_gateway.routes.chat.build_mcp_tools", AsyncMock(return_value={})):
        with pytest.raises(HTTPException) as exc_info:
            await memory_add(MemoryAddRequest(**_BASE_FIELDS), _fake_request(_make_cfg()))

    assert exc_info.value.status_code == 502
    assert "unavailable" in exc_info.value.detail


async def test_mcp_discovery_failure_returns_502() -> None:
    with patch(
        "inference_gateway.routes.chat.build_mcp_tools",
        AsyncMock(side_effect=RuntimeError("mcp down")),
    ):
        with pytest.raises(HTTPException) as exc_info:
            await memory_add(MemoryAddRequest(**_BASE_FIELDS), _fake_request(_make_cfg()))

    assert exc_info.value.status_code == 502
    assert "discovery failed" in exc_info.value.detail
