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
    tool = _mock_tool({"memory_id": "mem-123"})
    with patch(
        "inference_gateway.routes.chat.build_mcp_tools",
        AsyncMock(return_value={"memory_add": tool}),
    ):
        resp = await memory_add(MemoryAddRequest(**_BASE_FIELDS), _fake_request(_make_cfg()))

    assert resp.ok is True
    assert resp.memory_id == "mem-123"
    tool.ainvoke.assert_awaited_once_with(
        {
            "text": _BASE_FIELDS["text"],
            "agent_id": "agent",
            "datapack_id": "dp-1",
            "metadata": {"source": "labs-memory-lesson"},
        }
    )


async def test_tool_reported_error_maps_to_502() -> None:
    tool = _mock_tool({"error": "free_tier_limit_reached", "detail": "Memory limit reached."})
    with patch(
        "inference_gateway.routes.chat.build_mcp_tools",
        AsyncMock(return_value={"memory_add": tool}),
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
