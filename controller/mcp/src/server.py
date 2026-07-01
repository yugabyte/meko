"""amp controller MCP server.

A thin, stateless FastMCP server exposing the amp controller's `/api/v1` REST
surface as MCP tools. Configured via CLI flags (which win) and `AMP_*` env vars.
"""

import argparse
import logging
import os
import sys
from contextlib import asynccontextmanager
from dataclasses import dataclass
from typing import AsyncIterator, Optional

import httpx
from fastmcp import FastMCP
from starlette.requests import Request
from starlette.responses import JSONResponse

from controller_client import (
    DEFAULT_CONTROLLER_URL,
    DEFAULT_REQUEST_TIMEOUT,
    controller_client,
)
from server_instructions import AMP_CONTROLLER_SERVER_INSTRUCTIONS
from tools import (
    create_branch,
    create_endpoint,
    create_project,
    create_storage_pool,
    delete_branch,
    delete_endpoint,
    delete_project,
    get_branch,
    get_endpoint,
    get_project,
    get_storage_pool,
    get_task,
    get_user,
    list_branches,
    list_endpoints,
    list_projects,
    list_storage_pools,
    list_tasks,
)

logger = logging.getLogger("amp-mcp.server")

SERVER_NAME = "amp-controller-mcp"


@dataclass
class ServerConfig:
    controller_url: str
    transport: str
    host: str
    port: int
    request_timeout: float
    log_level: str


def parse_config(argv: list[str] | None = None) -> ServerConfig:
    parser = argparse.ArgumentParser(
        prog="amp-controller-mcp",
        description="MCP server for the amp controller REST API.",
    )
    parser.add_argument(
        "--controller-url",
        default=os.environ.get("AMP_CONTROLLER_URL", DEFAULT_CONTROLLER_URL),
        help="Controller base URL; the client appends /api/v1 (env: AMP_CONTROLLER_URL)",
    )
    parser.add_argument(
        "--transport",
        default=os.environ.get("AMP_MCP_TRANSPORT", "stdio"),
        choices=["stdio", "http"],
        help="stdio | http (env: AMP_MCP_TRANSPORT)",
    )
    parser.add_argument(
        "--host",
        default=os.environ.get("AMP_MCP_HOST", "0.0.0.0"),
        help="HTTP bind host (env: AMP_MCP_HOST)",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=int(os.environ.get("AMP_MCP_PORT", "8000")),
        help="HTTP bind port (env: AMP_MCP_PORT)",
    )
    parser.add_argument(
        "--request-timeout",
        type=float,
        default=float(
            os.environ.get("AMP_MCP_REQUEST_TIMEOUT", DEFAULT_REQUEST_TIMEOUT)
        ),
        help="httpx request timeout in seconds (env: AMP_MCP_REQUEST_TIMEOUT)",
    )
    parser.add_argument(
        "--log-level",
        default=os.environ.get("AMP_MCP_LOG_LEVEL", "INFO"),
        help="Logging level (env: AMP_MCP_LOG_LEVEL)",
    )

    args = parser.parse_args(argv)
    return ServerConfig(
        controller_url=args.controller_url,
        transport=args.transport,
        host=args.host,
        port=args.port,
        request_timeout=args.request_timeout,
        log_level=args.log_level,
    )


def _annotations() -> dict[str, dict]:
    return {
        "ro": {"readOnlyHint": True, "destructiveHint": False, "openWorldHint": True},
        "write": {
            "readOnlyHint": False,
            "destructiveHint": False,
            "openWorldHint": True,
        },
        "dest": {
            "readOnlyHint": False,
            "destructiveHint": True,
            "idempotentHint": False,
            "openWorldHint": True,
        },
    }


def build_server(
    config: ServerConfig,
    transport: Optional[httpx.BaseTransport] = None,
) -> FastMCP:
    @asynccontextmanager
    async def lifespan(server: FastMCP) -> AsyncIterator[dict]:
        controller_client.initialize(
            controller_url=config.controller_url,
            request_timeout=config.request_timeout,
            transport=transport,
        )
        logger.info("amp controller MCP server ready (transport=%s)", config.transport)
        try:
            yield {}
        finally:
            controller_client.shutdown()

    mcp = FastMCP(
        SERVER_NAME,
        instructions=AMP_CONTROLLER_SERVER_INSTRUCTIONS,
        lifespan=lifespan,
    )

    ann = _annotations()
    registrations = [
        (get_user, ann["ro"]),
        (create_storage_pool, ann["write"]),
        (list_storage_pools, ann["ro"]),
        (get_storage_pool, ann["ro"]),
        (create_project, ann["write"]),
        (list_projects, ann["ro"]),
        (get_project, ann["ro"]),
        (delete_project, ann["dest"]),
        (create_branch, ann["write"]),
        (list_branches, ann["ro"]),
        (get_branch, ann["ro"]),
        (delete_branch, ann["dest"]),
        (create_endpoint, ann["write"]),
        (list_endpoints, ann["ro"]),
        (get_endpoint, ann["ro"]),
        (delete_endpoint, ann["dest"]),
        (list_tasks, ann["ro"]),
        (get_task, ann["ro"]),
    ]
    for fn, annotations in registrations:
        mcp.tool(fn, annotations=annotations)
    logger.info("Registered %d tools", len(registrations))

    @mcp.custom_route("/healthz", methods=["GET"])
    async def healthz(request: Request) -> JSONResponse:
        return JSONResponse({"status": "ok"})

    return mcp


def _configure_logging(level_name: str) -> None:
    level = getattr(logging, level_name.upper(), logging.INFO)
    handler = logging.StreamHandler(sys.stderr)
    handler.setFormatter(
        logging.Formatter(
            "%(asctime)s [%(levelname)s] %(name)s: %(message)s",
            datefmt="%Y-%m-%d %H:%M:%S",
        )
    )
    root = logging.getLogger("amp-mcp")
    root.setLevel(level)
    root.addHandler(handler)


def main(argv: list[str] | None = None) -> None:
    config = parse_config(argv)
    _configure_logging(config.log_level)
    logger.info(
        "Starting amp-controller-mcp (transport=%s, controller_url=%s)",
        config.transport,
        config.controller_url,
    )

    mcp = build_server(config)

    if config.transport == "http":
        mcp.run(
            transport="http",
            host=config.host,
            port=config.port,
            log_level=config.log_level.lower(),
        )
    else:
        mcp.run(transport="stdio")


if __name__ == "__main__":
    main()
