# MCCP Architecture

MCCP (Meko Cloud Control Plane) is the BYOC (bring-your-own-cloud) control plane
for Meko. It is a **thin, stateless-at-the-edge** control plane: it holds the
fleet registry and desired state, while each customer's data plane runs a small
**pull agent** that reaches out to MCCP over outbound HTTPS only. No inbound
connection is ever made to a customer cluster.

This directory is a runnable, dependency-light reference implementation of the
design in [`../design/byoc-control-plane.md`](../design/byoc-control-plane.md),
backing the OpenAPI subset in [`../design/mccp-openapi.yaml`](../design/mccp-openapi.yaml).

## Components

| Path | Binary | Role |
|------|--------|------|
| `cmd/mccp` | `mccp` | Control-plane API + embedded operator console (`web/console.html`). |
| `cmd/agent` | `meko-agent` | Per-environment pull agent. Reads cluster facts from the in-cluster k8s API, enrolls, reconciles desired datapacks, reports status. |
| `cmd/dpmcp` | `dpmcp` | Per-datapack MCP server (JSON-RPC 2.0 over HTTP). One instance per datapack; agent provisions it. |
| `internal/server` | — | HTTP router for the MCCP API and console host. |
| `internal/store` | — | Fleet-registry persistence: `Memory` (dev) and `Postgres` (durable) behind one `Store` interface. |
| `internal/mcp` | — | The MCP protocol server + its memory store (`MemStore` / `PgStore`). |
| `internal/api` | — | Wire types shared by server and agent; JSON tags mirror the OpenAPI. |
| `web` | — | Embedded operator console (served at `/`). |

## Topology

```
  Operator console (browser)
          │  HTTPS
          ▼
  ┌───────────────────────┐        ┌──────────────────────────────────────┐
  │        MCCP           │        │      Customer data plane (k8s)         │
  │  cmd/mccp             │        │                                        │
  │  ┌─────────────────┐  │        │   ┌────────────┐   outbound only       │
  │  │ server (API +   │  │◀───────┼───│ meko-agent │──────────┐            │
  │  │ console)        │  │ enroll │   │ cmd/agent  │          │            │
  │  ├─────────────────┤  │ desired│   └─────┬──────┘          ▼            │
  │  │ store (fleet    │  │ status │         │ provisions   in-cluster      │
  │  │ registry)       │  │        │         ▼              k8s API         │
  │  └─────────────────┘  │        │   ┌────────────┐                       │
  └───────────────────────┘        │   │ dpmcp ×N   │  (one per datapack)   │
                                    │   │ cmd/dpmcp  │                       │
                                    │   └────────────┘                       │
                                    └────────────────────────────────────────┘
```

All arrows from the data plane point **out**: agent → k8s API (in-cluster) and
agent → MCCP. MCCP never dials into a customer cluster.

## The pull loop

1. **Register** — an operator `POST`s `/api/v1/environments`. MCCP mints a
   single reusable **enrollment token** (kubeadm-style bootstrap token, 30-min
   TTL) and returns it once, along with an install hint. Only the token *hash*
   is persisted.
2. **Enroll** — `meko-agent` starts in the customer cluster, reads its
   service-account token + cluster facts, and `POST`s `/api/v1/agent/enroll`
   with the enrollment token. MCCP validates the token, consumes it, and mints a
   long-lived env-scoped **status token** (returned once, hash persisted). The
   environment moves to `ATTESTING`.
3. **Reconcile** — on each tick the agent `GET`s `/api/v1/agent/desired-state`
   (bearer: status token). Desired state is a generic, resource-typed set
   (`{kind, name, spec}`); the agent dispatches each resource to a reconciler
   for its `Kind` (today: `datapack` → a labeled namespace running a `dpmcp`).
4. **Report** — the agent `POST`s `/api/v1/agent/status` with real facts,
   component health, and per-datapack phases. This drives the fleet registry and
   the console's live view.

Because desired state is resource-typed, adding a new managed resource needs
only a new agent-side reconciler — no protocol change.

## State machine

Environments move through the §7 lifecycle states defined in `internal/api`:

```
PENDING → CLUSTER_PROVISIONING → ENROLLING → ATTESTING
        → PROVISIONING → VALIDATING → READY
READY ⇄ DEGRADED
READY → SUSPENDED (suspend) → READY (resume)
READY → DRAINING (drain) → DEREGISTERED (deregister)
```

- Enrollment advances an env to `ATTESTING`.
- Agent status reports set the state directly (e.g. `READY`, `DEGRADED`).
- `bootstrap-progress` (pre-agent installer) forces intermediate provisioning
  states.
- Console lifecycle actions (`suspend`/`resume`/`drain`/`deregister`) are applied
  via `PATCH`/`DELETE`.

## Authentication

Two token types, both stored only as SHA-256 hashes (`store.HashToken`):

- **Enrollment token** (`mko_enr_…`) — TTL-based, reusable within its window so
  an agent restart can re-enroll. Validated by matching hash + non-expiry.
- **Status token** (`mko_sts_…`) — long-lived, env-scoped. Sent as a bearer on
  every agent → MCCP call; validated per-environment by `ValidateStatusToken`.

Plaintext tokens are returned to the caller exactly once and never persisted.

## Persistence

`store.Store` has two implementations behind one interface:

- **`Memory`** — in-memory maps guarded by an `sync.RWMutex`. Used when no
  database is configured (dev/tests). Seeds a demo fleet on first use.
- **`Postgres`** — durable state (`store/postgres.go`), selected when
  `MCCP_DATABASE_URL` / `-db` is set.

The `dpmcp` memory store mirrors this: `mcp.MemStore` for dev, `mcp.PgStore`
(backed by the datapack's own database) in production.

## Testing

- `internal/store` — unit tests for the registry: token hashing, slugging,
  enrollment TTL, lifecycle transitions, seed idempotency, list ordering,
  desired-state, status application, datapack filtering.
- `internal/server` — HTTP-level integration tests over `httptest.Server`
  covering the full register → enroll → desired → status pull loop, the console
  read endpoints, lifecycle actions, and the agent-surface guard rails.
- `internal/mcp` — end-to-end MCP protocol test (initialize → tools/list →
  tools/call) plus the unknown-method error path.

Run them with `go test ./...` — everything is mocked/in-memory, so no database
or cluster is required.
