# ac — MCP server

**Status:** Draft
**Last updated:** 2026-06-30

How the **amp controller** is exposed to AI agents over the
[Model Context Protocol](https://modelcontextprotocol.io). The MCP server is a
thin, stateless wrapper around the controller's REST API (`ac-apis.md`): it
translates a small, curated catalog of MCP **tools** into HTTP calls against
`/api/v1` and returns the controller's JSON unchanged. It owns no state, no
database, and no auth — everything authoritative still lives behind the
controller.

It lives in [`controller/mcp/`](../mcp/) as a self-contained Python project and
is built/tested with [`uv`](https://docs.astral.sh/uv/), separate from the Go
module.

## Purpose & non-goals

The controller already serves an OpenAPI-described REST surface and a Swagger
explorer (`ac-apis.md`). The MCP server exists so an MCP-speaking client (Claude
Desktop, an agent runtime) can drive that surface with tool calls instead of
hand-rolling HTTP. It is deliberately small:

- **It is a client, not a peer of the controller.** No DB, no task framework, no
  storage-controller coordination — it makes HTTP requests and shapes the
  results. If the controller is down, every tool fails with a transport error.
- **It does not re-implement business rules.** Validation, the single-writer
  rule, name uniqueness, async planning — all of that stays in the controller.
  The server forwards the request and surfaces whatever the controller returns
  (including a `failed` task).
- **No auth, no telemetry side-channels.** Matching the controller's current
  no-auth posture (`ac-k8s-deploy.md` Assumptions), the server adds no
  authentication of its own; it is meant to sit behind the same private network
  boundary as the controller it calls.

## Architecture — FastMCP over a singleton httpx client

The server is built on [FastMCP](https://github.com/jlowin/fastmcp). The shape is
intentionally flat:

```
controller/mcp/src/
  server.py              config (flags + env) → FastMCP setup → register 18 tools → run
  controller_client.py   singleton ControllerClient (httpx) → /api/v1 → ControllerApiError
  tools.py               18 tool functions: typed args → client method → JSON back
  server_instructions.py the instructions string handed to the model
```

- **`controller_client.py`** holds a single `httpx.Client` with
  `base_url = <controller-url>/api/v1`, created once on server start and closed on
  shutdown (a FastMCP lifespan). One method per controller operation. A private
  `_request` helper centralizes the wire contract: on a 2xx it returns
  `resp.json()` **directly** — the controller returns the raw resource, not a
  `data`-wrapped envelope — falling back to `{}` for an empty 2xx body; on the
  error envelope `{"error": {"code", "message"}}` (or any `status >= 400`) it
  raises `ControllerApiError(code, message, status_code)`, which FastMCP renders
  as a tool error.
- **`tools.py`** is the MCP surface. Each tool is a typed Python function whose
  arguments map to path parameters, query parameters, and request-body fields;
  it calls the matching client method and returns the JSON. Path ids are plain
  `str` UUID arguments in snake_case (`pool_id`, `project_id`, `branch_id`,
  `endpoint_id`, `task_id`, `user_id`).
- **`server.py`** wires config (CLI flags layered over `AMP_*` env vars, flags
  winning), registers each tool with its annotation preset, attaches the
  instructions string, and runs the chosen transport.
- **`server_instructions.py`** is a single module-level string (no heavy
  imports) describing the resource ladder, async-task polling, and the
  `owner_user_id` requirement, so the model gets the same mental model a human
  reads in `ac-apis.md`.

## The tool catalog — 18 tools

The catalog is a curated subset of the REST operations, one tool per operation,
named in snake_case. Each carries MCP annotation hints so a client can reason
about safety: **read-only** (a `GET`), **write** (a non-destructive mutation),
or **destructive** (a delete); all are `openWorldHint: true` (they reach an
external service).

| Tool | REST operation (under `/api/v1`) | Kind |
|---|---|---|
| `get_user` | `GET /users/{userId}` | read-only |
| `create_storage_pool` | `POST /storage-pools` (body `{name, owner_user_id}`) | write |
| `list_storage_pools` | `GET /storage-pools` | read-only |
| `get_storage_pool` | `GET /storage-pools/{poolId}` | read-only |
| `create_project` | `POST /storage-pools/{poolId}/projects` | write (async) |
| `list_projects` | `GET /storage-pools/{poolId}/projects` | read-only |
| `get_project` | `GET …/projects/{projectId}` | read-only |
| `delete_project` | `DELETE …/projects/{projectId}` | destructive (async) |
| `create_branch` | `POST …/projects/{projectId}/branches` | write (async) |
| `list_branches` | `GET …/projects/{projectId}/branches` | read-only |
| `get_branch` | `GET …/branches/{branchId}` | read-only |
| `delete_branch` | `DELETE …/branches/{branchId}` | destructive (async) |
| `create_endpoint` | `POST …/branches/{branchId}/endpoints` | write (async) |
| `list_endpoints` | `GET …/branches/{branchId}/endpoints` | read-only |
| `get_endpoint` | `GET …/endpoints/{endpointId}` | read-only |
| `delete_endpoint` | `DELETE …/endpoints/{endpointId}` | destructive (async) |
| `list_tasks` | `GET /tasks` (`status`, `kind`, `project_id` filters) | read-only |
| `get_task` | `GET /tasks/{taskId}` | read-only |

The list tools take the controller's pagination contract verbatim — `limit`
(`1..1000`, default 100) and `offset` (`>= 0`, default 0) — and the response
carries `total` (all matches) and `count` (this page) alongside the items array
under its tag-named key (`users`, `storage_pools`, `projects`, `branches`,
`endpoints`, `tasks`).

### Why no `create_user` / `list_users`

The REST API has `createUser` and `listUsers` (`ac-apis.md`), but they are
**not** exposed as tools. Creating users — and, with no auth, freely minting
admins — is an operator concern, not something an agent should drive; and a tool
that lists every user in the placement group is more identity surface than an
agent needs. Read-only `get_user` is kept (resolving a known id is harmless).

That leaves one gap: `create_storage_pool` needs an owner. Rather than invent a
server-level default user (there is none), the decision is to make
**`owner_user_id` a required argument** wherever the API needs an owner. The
caller names the owner explicitly per call; the server holds no ambient identity.
This mirrors where the REST API itself puts the owner — in the create body, not
the path (`ac-apis.md` §Users and identity) — and stays correct when auth later
moves the owner onto an authenticated principal.

## Async tasks: tools return a Task, you poll `get_task`

The six world-changing operations — `create_project`, `delete_project`,
`create_branch`, `delete_branch`, `create_endpoint`, `delete_endpoint` — are
**asynchronous** on the controller (`ac-apis.md` §The surface,
[`ac-tasks.md`](ac-tasks.md)). The controller runs cheap synchronous validation,
plans a write-ahead task, and returns `202 Accepted` with a **`Task`** object in
the body (and a `Location: /api/v1/tasks/{id}` header).

The MCP server does **not** hide this. Those six tools return the `Task` JSON
unchanged, and their docstrings instruct the caller to **poll
`get_task(task_id=<id>)`** until `status` is `succeeded` or `failed` (read
`error` on failure), then read the materialized resource back with the relevant
`get_*` / `list_*` tool. The server does no polling on the caller's behalf — a
single tool call is a single HTTP request, so the agent stays in control of the
wait and the model can interleave other reasoning. Because the work runs on the
task, a duplicate name or a single-writer collision surfaces as a `failed` task,
not a synchronous error (again matching the REST contract).

The synchronous operations behave as the REST API does: `create_storage_pool`
returns the created pool (`201`), and all the reads return their resource
directly.

## Transports — stdio and http

The server runs one of two FastMCP transports, selected by config:

- **`stdio`** (default) — the server speaks MCP over stdin/stdout, the shape a
  desktop/local MCP client launches as a subprocess. Best for a developer
  pointing a local client at a controller (e.g. a port-forwarded or
  `localhost:8080` controller).
- **`http`** — the server listens on a host/port and speaks MCP over HTTP. This
  is the in-cluster shape: the MCP pod runs `--transport http` and other pods (or
  a port-forward) reach it on its Service.

Under the `http` transport the server adds a root `GET /healthz` (FastMCP
`@mcp.custom_route`) returning `{"status": "ok"}` for k8s probes; the chart's MCP
pod uses it as an HTTP liveness/readiness probe (a 200 confirms the MCP app is
serving, not merely that the port is bound).

## Configuration

CLI flags layered over environment variables, **flags win**. There is no
user-id config — the owner is always a per-call `owner_user_id` argument.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--controller-url` | `AMP_CONTROLLER_URL` | `http://localhost:8080` | controller base URL; the client appends `/api/v1` |
| `--transport` | `AMP_MCP_TRANSPORT` | `stdio` | `stdio` or `http` |
| `--host` | `AMP_MCP_HOST` | `0.0.0.0` | http bind host |
| `--port` | `AMP_MCP_PORT` | `8000` | http bind port |
| `--request-timeout` | `AMP_MCP_REQUEST_TIMEOUT` | `30` | httpx request timeout, seconds |
| `--log-level` | `AMP_MCP_LOG_LEVEL` | `INFO` | logging level |

Note the controller's health endpoints (`/healthz`, `/readyz`) sit at the
controller **root**, not under `/api/v1`; only the `/api/v1` resource paths go
through the client's `base_url`.

## Deployment — its own pod in the `yb-amp-controller` chart

The controller and the MCP server are packaged together in the
[`yb-amp-controller`](../../deploy/helm/yb-amp-controller) Helm chart (which
replaced the inline `kubectl apply` manifests in `deploy/controller/redeploy.sh`).
The MCP server ships as **its own pod** — a separate `amp-controller-mcp`
Deployment + ClusterIP Service — not as a sidecar of the controller, so it scales
and restarts independently.

The MCP pod runs the `http` transport and reaches the controller over its
in-cluster Service DNS:

- `AMP_CONTROLLER_URL = http://<controller-svc>.<ns>.svc.cluster.local:8080`
  (defaulted by a chart helper to the controller Service, overridable via
  `mcp.controllerUrl`),
- `AMP_MCP_TRANSPORT=http`, `AMP_MCP_HOST=0.0.0.0`, `AMP_MCP_PORT=8000`,
- container port `mcp:8000`, exposed **the same way as the controller** — an
  always-on in-cluster ClusterIP `amp-controller-mcp`, plus a matching external
  Service (`amp-controller-mcp-np` / `amp-controller-mcp-lb`) whenever
  `controller.service.type` gives the controller one. So a dev release (`nodeport` /
  `none`) keeps the MCP endpoint internal (reach it via `kubectl port-forward`),
  and a prod `external-lb` exposes it externally — the one `controller.service.type`
  knob drives both,
- an HTTP `GET /healthz` liveness/readiness probe on the `mcp` port.

The pod is gated by **`mcp.enabled`** (default `true`): set `mcp.enabled=false`
and the chart drops the MCP Deployment and Service entirely, leaving a
controller-only release. See
[`deploy/controller/README.md`](../../deploy/controller/README.md) and the chart's
own README for the full values surface.

## Local testing

The project is built and tested with `uv`; tests are `pytest` (with
`pytest-asyncio`) and need no running controller or network:

```sh
uv run --project controller/mcp pytest          # the full suite
```

The tests cover four layers:

- **Client unit tests** drive `ControllerClient` against an
  `httpx.MockTransport` backend — asserting the method/path/query params each
  method sends, that a 2xx body is returned raw (no `data` unwrap), and that an
  error envelope or a `4xx`/`5xx` raises `ControllerApiError` with the right code
  (e.g. `404` → `not_found`).
- **Tool unit tests** call the tool functions with a mocked client and assert
  the backend call and the returned shape.
- **A registration test** reads `server.py` statically and asserts that all 18
  tools are registered and that `create_user` / `list_users` are **not** —
  guarding the product decision against regression.
- **An in-memory integration test** runs a FastMCP `Client(mcp)` against the
  server with an `httpx.MockTransport` backend — a full MCP round-trip with no
  network and no subprocess. A real-controller round-trip is guarded behind
  `AMP_CONTROLLER_URL` (skipped when unset).

The server is also containerized (a `uv`-based `Dockerfile`, defaulting to the
`http` transport) so the chart can run it as the `amp-controller-mcp` pod:

```sh
docker build -t yb-light/controller-mcp:dev controller/mcp
docker run --rm -p 8000:8000 \
  -e AMP_CONTROLLER_URL=http://host.docker.internal:8080 \
  -e AMP_MCP_TRANSPORT=http \
  yb-light/controller-mcp:dev
```
