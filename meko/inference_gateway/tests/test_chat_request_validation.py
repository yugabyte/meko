"""Pin ChatRequest's Pydantic-layer validation for the guided-customization
fields. `run_chat_turn` itself never enforces the 300-char cap on `focus` —
that's a request-shape concern, checked here directly against the model
rather than through the full chain (no LLM/MCP mocking needed).
"""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from inference_gateway.routes.chat import ChatRequest

_BASE_FIELDS = {"message": "hello", "user_token": "tok", "user_id": "user-1"}


def test_focus_within_cap_is_accepted() -> None:
    req = ChatRequest(**_BASE_FIELDS, focus="x" * 300)
    assert req.focus == "x" * 300


def test_focus_over_cap_is_rejected() -> None:
    with pytest.raises(ValidationError):
        ChatRequest(**_BASE_FIELDS, focus="x" * 301)


def test_focus_omitted_defaults_to_none() -> None:
    req = ChatRequest(**_BASE_FIELDS)
    assert req.focus is None
    assert req.enable_knowledge_base is None
    assert req.enable_memory_search is None
    assert req.tone is None
    assert req.strictness is None


def test_tone_accepts_only_the_closed_set() -> None:
    for value in ("concise", "detailed", "encouraging"):
        assert ChatRequest(**_BASE_FIELDS, tone=value).tone == value
    with pytest.raises(ValidationError):
        ChatRequest(**_BASE_FIELDS, tone="sarcastic")


def test_strictness_accepts_only_the_closed_set() -> None:
    for value in ("lenient", "balanced", "strict"):
        assert ChatRequest(**_BASE_FIELDS, strictness=value).strictness == value
    with pytest.raises(ValidationError):
        ChatRequest(**_BASE_FIELDS, strictness="ruthless")
