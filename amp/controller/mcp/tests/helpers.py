"""Shared test helpers: a recording httpx.MockTransport for the controller API."""

import json
from typing import Callable, Optional

import httpx


class RecordingHandler:
    """An httpx.MockTransport handler that records requests and replies from a map.

    `routes` maps "(METHOD, path)" -> response spec. A response spec is either a
    dict (sent as 200 JSON), or a tuple (status_code, json_body). Unmatched
    requests get a 404 not_found envelope. Every request is appended to
    `self.requests` for assertions.
    """

    def __init__(self, routes: Optional[dict] = None):
        self.routes = routes or {}
        self.requests: list[httpx.Request] = []

    def __call__(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        key = f"{request.method} {request.url.path}"
        spec = self.routes.get(key)
        if spec is None:
            return httpx.Response(
                404, json={"error": {"code": "not_found", "message": "no route"}}
            )
        if isinstance(spec, tuple):
            status, body = spec
            return httpx.Response(status, json=body)
        return httpx.Response(200, json=spec)

    @property
    def last(self) -> httpx.Request:
        return self.requests[-1]

    def last_json(self) -> dict:
        return json.loads(self.last.content.decode() or "{}")


def make_transport(handler: Callable) -> httpx.MockTransport:
    return httpx.MockTransport(handler)
