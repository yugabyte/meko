"""Prometheus instrumentation, scraped by the chart's OTel collector
(deploy/helm/meko/templates/otel).

Served on its own port (`MEKO_METRICS_PORT`, 9464) rather than as a route on the
API port, for the same reason as the api-server's: whatever fronts the API must
not also expose the series.

Labels are bounded by construction: `route` is the matched route template, never
the raw path, and `model` comes from config, never from a request.
"""

from __future__ import annotations

import logging
import time
from typing import Any
from uuid import UUID

from langchain_core.callbacks import BaseCallbackHandler
from langchain_core.outputs import LLMResult
from prometheus_client import Counter, Gauge, Histogram, start_http_server

logger = logging.getLogger(__name__)

DEFAULT_METRICS_PORT = 9464

# Unmatched requests share one label: by raw path, any scanner mints a series per probe.
UNMATCHED_ROUTE = "unmatched"

# Likewise the method, which is client-controlled. The shipped image parses with
# httptools (uvicorn[standard]), whose method table is fixed but holds ~30 verbs
# beyond these; under the h11 fallback any token reaches the app.
OTHER_METHOD = "OTHER"
_KNOWN_METHODS = frozenset({"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"})

HTTP_REQUESTS = Counter(
    "meko_gateway_http_requests",
    "HTTP requests handled, by method, route template and response status code.",
    ["method", "route", "status"],
)
HTTP_DURATION = Histogram(
    "meko_gateway_http_request_duration_seconds",
    "HTTP request latency in seconds, by method and route template. A chat turn "
    "runs every LLM and tool round inside one request, so this is turn latency.",
    ["method", "route"],
    buckets=(0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120),
)
HTTP_INFLIGHT = Gauge(
    "meko_gateway_http_requests_inflight",
    "HTTP requests currently being served.",
)
LLM_REQUESTS = Counter(
    "meko_gateway_llm_requests",
    "LLM calls, by model and outcome (ok, error).",
    ["model", "outcome"],
)
LLM_DURATION = Histogram(
    "meko_gateway_llm_request_duration_seconds",
    "LLM call latency in seconds, by model.",
    ["model"],
    buckets=(0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120),
)
LLM_TOKENS = Counter(
    "meko_gateway_llm_tokens",
    "Tokens the LLM reported, by model and direction (input, output).",
    ["model", "direction"],
)


def start_metrics_server(port: int) -> None:
    """Serve the default registry on `port`; 0 or less turns it off."""
    if port <= 0:
        logger.info("metrics server disabled")
        return
    start_http_server(port)
    logger.info("metrics server listening on :%d", port)


def _route_of(scope: dict) -> str:
    route = scope.get("route")
    return getattr(route, "path", None) or UNMATCHED_ROUTE


class HTTPMetricsMiddleware:
    """Pure ASGI rather than BaseHTTPMiddleware, which buffers the response."""

    def __init__(self, app: Any) -> None:
        self.app = app

    async def __call__(self, scope: dict, receive: Any, send: Any) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return

        # 500 unless the app starts a response: an exception that escapes it is
        # what the server turns into a 500.
        status = {"code": 500}

        async def send_with_status(message: dict) -> None:
            if message["type"] == "http.response.start":
                status["code"] = message["status"]
            await send(message)

        start = time.perf_counter()
        HTTP_INFLIGHT.inc()
        try:
            await self.app(scope, receive, send_with_status)
        finally:
            HTTP_INFLIGHT.dec()
            # The router writes the matched route into the scope it was handed,
            # so it is readable here once the app returns.
            route = _route_of(scope)
            method = scope.get("method", "")
            if method not in _KNOWN_METHODS:
                method = OTHER_METHOD
            HTTP_REQUESTS.labels(method, route, str(status["code"])).inc()
            HTTP_DURATION.labels(method, route).observe(time.perf_counter() - start)


class LLMMetricsCallback(BaseCallbackHandler):
    """Times every call a chat model makes. Attached where the model is built, so
    it covers every call site without any of them knowing."""

    def __init__(self, model: str) -> None:
        self.model = model
        self._started: dict[UUID, float] = {}

    def on_chat_model_start(
        self, serialized: Any, messages: Any, *, run_id: UUID, **kwargs: Any
    ) -> None:
        self._started[run_id] = time.perf_counter()

    def on_llm_start(self, serialized: Any, prompts: Any, *, run_id: UUID, **kwargs: Any) -> None:
        self._started[run_id] = time.perf_counter()

    def on_llm_end(self, response: LLMResult, *, run_id: UUID, **kwargs: Any) -> None:
        self._finish(run_id, "ok")
        for gens in response.generations:
            for gen in gens:
                usage = getattr(getattr(gen, "message", None), "usage_metadata", None) or {}
                if usage.get("input_tokens"):
                    LLM_TOKENS.labels(self.model, "input").inc(usage["input_tokens"])
                if usage.get("output_tokens"):
                    LLM_TOKENS.labels(self.model, "output").inc(usage["output_tokens"])

    def on_llm_error(self, error: BaseException, *, run_id: UUID, **kwargs: Any) -> None:
        self._finish(run_id, "error")

    def _finish(self, run_id: UUID, outcome: str) -> None:
        LLM_REQUESTS.labels(self.model, outcome).inc()
        start = self._started.pop(run_id, None)
        if start is not None:
            LLM_DURATION.labels(self.model).observe(time.perf_counter() - start)
