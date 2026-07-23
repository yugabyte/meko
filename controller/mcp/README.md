# amp controller MCP server

A [FastMCP](https://gofastmcp.com) server that exposes the **amp controller**
REST API (`/api/v1`) as Model Context Protocol tools. It is a thin, stateless
HTTP client of the controller — no database, no auth, no extra state. MCP
clients (Claude Desktop, IDE assistants, agents) drive the control plane through
it: create storage pools, projects, branches, and endpoints, and poll the
asynchronous tasks those mutations plan.

See `controller/architecture/ac-mcp.md` for the design.

## Tools

18 tools mapped to the controller's operations:

- **Users (read-only):** `get_user`. User create/list are intentionally not
  exposed; where the API needs an owner, the tool takes `owner_user_id` as a
  required argument.
- **Storage pools:** `create_storage_pool` (requires `owner_user_id`),
  `list_storage_pools`, `get_storage_pool`.
- **Projects:** `create_project` (optional `pg_version`, default 18 — only 18
  is accepted today), `list_projects`, `get_project`, `delete_project`.
- **Branches:** `create_branch`, `list_branches`, `get_branch`, `delete_branch`.
- **Endpoints:** `create_endpoint`, `list_endpoints`, `get_endpoint`,
  `delete_endpoint`.
- **Tasks:** `list_tasks`, `get_task`.

`create_storage_pool` is **synchronous** — it returns the created storage pool
directly. The `create_project` / `create_branch` / `create_endpoint` tools and
every `delete_*` tool are **asynchronous**: they return a `Task` object
immediately. Poll `get_task(task_id=<id>)` until the task's `status` reaches
`succeeded` or `failed`, then read the resource to see its final state.

## Configuration

Flags win over environment variables.

| flag                | env                       | default                 | meaning                                        |
|---------------------|---------------------------|-------------------------|------------------------------------------------|
| `--controller-url`  | `AMP_CONTROLLER_URL`      | `http://localhost:8080` | controller base URL; the client appends `/api/v1` |
| `--transport`       | `AMP_MCP_TRANSPORT`       | `stdio`                 | `stdio` or `http`                              |
| `--host`            | `AMP_MCP_HOST`            | `0.0.0.0`               | HTTP bind host                                 |
| `--port`            | `AMP_MCP_PORT`            | `8000`                  | HTTP bind port                                 |
| `--request-timeout` | `AMP_MCP_REQUEST_TIMEOUT` | `30`                    | httpx request timeout (seconds)                |
| `--log-level`       | `AMP_MCP_LOG_LEVEL`       | `INFO`                  | logging level                                  |

When running with `--transport http`, a health endpoint is served at
`GET /healthz` on the HTTP port, returning `{"status": "ok"}`. The MCP endpoint
itself is at `/mcp`.

## Running

```sh
# stdio (local clients such as Claude Desktop)
uv run src/server.py --controller-url http://localhost:8080

# http (in-cluster pod)
uv run src/server.py --transport http --port 8000 \
  --controller-url http://amp-controller:8080
```

## Development

```sh
uv sync --dev
uv run pytest -q
```

Tests use `httpx.MockTransport` for the controller backend and FastMCP's
in-memory `Client` for the MCP round-trip, so they need no network and no real
controller. A real-controller round-trip test runs only when
`AMP_CONTROLLER_URL` is set.

## Docker

```sh
docker build -t amp-controller-mcp .
docker run -p 8000:8000 \
  -e AMP_CONTROLLER_URL=http://host.docker.internal:8080 \
  amp-controller-mcp
```

The image defaults to HTTP transport on port 8000. It is deployed as its own pod
by the `deploy/helm/yb-amp-controller` Helm chart.
