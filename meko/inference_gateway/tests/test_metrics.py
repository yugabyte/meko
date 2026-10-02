"""Pin the gateway's Prometheus series: bounded labels on the HTTP side, and LLM
calls timed and counted from the callback attached where the model is built."""

from __future__ import annotations

from typing import Any
from uuid import uuid4

import pytest
from fastapi import FastAPI, HTTPException
from fastapi.testclient import TestClient
from langchain_core.language_models.fake_chat_models import FakeListChatModel
from langchain_core.messages import AIMessage
from langchain_core.outputs import ChatGeneration, LLMResult
from prometheus_client import REGISTRY

from inference_gateway.metrics import UNMATCHED_ROUTE, HTTPMetricsMiddleware, LLMMetricsCallback


def _sample(name: str, labels: dict[str, str]) -> float:
    return REGISTRY.get_sample_value(name, labels) or 0.0


def _app() -> FastAPI:
    app = FastAPI()
    app.add_middleware(HTTPMetricsMiddleware)

    @app.get("/items/{item_id}")
    async def item(item_id: str) -> dict:
        return {"id": item_id}

    @app.get("/forbidden")
    async def forbidden() -> dict:
        raise HTTPException(status_code=403)

    return app


def test_http_requests_are_labelled_by_route_template() -> None:
    labels = {"method": "GET", "route": "/items/{item_id}", "status": "200"}
    before = _sample("meko_gateway_http_requests_total", labels)

    client = TestClient(_app())
    client.get("/items/a")
    client.get("/items/b")

    # Two ids, one series.
    assert _sample("meko_gateway_http_requests_total", labels) - before == 2
    assert _sample("meko_gateway_http_requests_total", {**labels, "route": "/items/a"}) == 0
    assert _sample("meko_gateway_http_requests_inflight", {}) == 0


def test_errors_and_unmatched_paths_keep_bounded_labels() -> None:
    forbidden = {"method": "GET", "route": "/forbidden", "status": "403"}
    unmatched = {"method": "GET", "route": UNMATCHED_ROUTE, "status": "404"}
    before = (_sample("meko_gateway_http_requests_total", forbidden),
              _sample("meko_gateway_http_requests_total", unmatched))

    client = TestClient(_app())
    client.get("/forbidden")
    client.get("/no/such/path/123")

    assert _sample("meko_gateway_http_requests_total", forbidden) - before[0] == 1
    assert _sample("meko_gateway_http_requests_total", unmatched) - before[1] == 1


def test_llm_calls_are_counted_and_timed_from_the_model_callback() -> None:
    model = "fake-model"
    ok = {"model": model, "outcome": "ok"}
    before = (_sample("meko_gateway_llm_requests_total", ok),
              _sample("meko_gateway_llm_request_duration_seconds_count", {"model": model}))

    llm = FakeListChatModel(responses=["hi"], callbacks=[LLMMetricsCallback(model)])
    llm.invoke("hello")

    duration = "meko_gateway_llm_request_duration_seconds_count"
    assert _sample("meko_gateway_llm_requests_total", ok) - before[0] == 1
    assert _sample(duration, {"model": model}) - before[1] == 1


def test_llm_token_usage_is_recorded() -> None:
    model = "usage-model"
    cb = LLMMetricsCallback(model)
    run = uuid4()
    cb.on_chat_model_start({}, [], run_id=run)
    usage = {"input_tokens": 12, "output_tokens": 5, "total_tokens": 17}
    msg = AIMessage(content="x", usage_metadata=usage)
    cb.on_llm_end(LLMResult(generations=[[ChatGeneration(message=msg)]]), run_id=run)

    tokens = "meko_gateway_llm_tokens_total"
    assert _sample(tokens, {"model": model, "direction": "input"}) == 12
    assert _sample(tokens, {"model": model, "direction": "output"}) == 5


def test_llm_errors_are_counted() -> None:
    model = "error-model"

    class Boom(FakeListChatModel):
        def _call(self, *args: Any, **kwargs: Any) -> str:
            raise RuntimeError("provider down")

    llm = Boom(responses=["unused"], callbacks=[LLMMetricsCallback(model)])
    with pytest.raises(RuntimeError):
        llm.invoke("hello")
    assert _sample("meko_gateway_llm_requests_total", {"model": model, "outcome": "error"}) == 1
