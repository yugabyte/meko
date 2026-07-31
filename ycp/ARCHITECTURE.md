# YCP Architecture

YCP (Yugabyte Control Plane) is the BYOC (bring-your-own-cloud) control plane
for Meko. It is a **thin, stateless-at-the-edge** control plane: it holds the
fleet registry and desired state, while each customer's data plane runs a small
**pull agent** that reaches out to YCP over outbound HTTPS only. No inbound
connection is ever made to a customer cluster.

The design is [`meko/design/byoc-control-plane.md`](../meko/design/byoc-control-plane.md),
backing the OpenAPI subset in [`meko/design/ycp-openapi.yaml`](../meko/design/ycp-openapi.yaml).

## Where the control plane lives

**In `api_server/`, not here.** `api_server` run with `--control-plane-mode`
serves the fleet registry, the agent API, and the operator console. This
directory holds the *data-plane* side of the contract plus the wire types both
sides share.

Everything it serves can be mounted under a URL prefix with `--base-path=/ycp`,
which is how it is fronted at `https://devcloud.yugabyte.com/ycp/` — a subpath of
the YBM domain rather than its own subdomain, so browser auth tokens and session
storage carry over. The paths below are written relative to that mount; only
`GET /api/v1/health` is additionally served at the domain root, for probes. See
[`GETTING_STARTED.md`](./GETTING_STARTED.md#serving-it-under-a-subpath-ycp).

There used to be a second implementation of the control-plane contract in
`ycp/cmd/ycp` — a dependency-light reference version with an in-memory registry.
It was removed, because one contract with two implementations mostly produced
divergence: it returned its payloads flat while `api_server` wraps them in
`{"data":…}`, which broke the agent in a way CI could not see
([meko#399](https://github.com/yugabyte/meko/issues/399)), and the e2e that was
supposed to protect the control plane was exercising the copy nobody deploys.
The e2e in `test/e2e/` now runs `api_server --control-plane-mode`.

## Components

| Path | Binary | Role |
|------|--------|------|
| `cmd/agent` | `meko-agent` | Per-environment pull agent. Reads cluster facts from the in-cluster k8s API, enrolls, reconciles desired datapacks, reports status. |
| `cmd/dpmcp` | `dpmcp` | Per-datapack MCP server (JSON-RPC 2.0 over HTTP). One instance per datapack; the agent provisions it. |
| `internal/mcp` | — | The MCP protocol server + its memory store (`MemStore` / `PgStore`). |
| `internal/api` | — | Wire types + lifecycle states; JSON tags mirror the OpenAPI. The agent's half of the contract `api_server` implements. |
| `test/e2e` | — | Build-and-deploy smoke test on kind against `api_server --control-plane-mode`. |

The control-plane side: `api_server/internal/handlers/environment_handler.go`
(routes), `internal/service/environment_service.go` (tokens, lifecycle),
`internal/repository` (the registry), `internal/handlers/web/console.html` (the
console), `internal/db/migrations/controlplane/` (its schema).

## Topology

```
  Operator console (browser)
          │  HTTPS
          ▼
  ┌────────────────────────────┐   ┌──────────────────────────────────────┐
  │  YCP control plane         │   │      Customer data plane (k8s)         │
  │  api_server                │   │                                        │
  │  --control-plane-mode      │   │   ┌────────────┐   outbound only       │
  │  ┌──────────────────────┐  │◀──┼───│ meko-agent │──────────┐            │
  │  │ fleet API + console  │  │enroll │ cmd/agent  │          │            │
  │  ├──────────────────────┤  │desired└─────┬──────┘          ▼            │
  │  │ Postgres / YugabyteDB│  │status       │ provisions   in-cluster      │
  │  │ (fleet registry)     │  │   │         ▼              k8s API         │
  │  └──────────────────────┘  │   │   ┌────────────┐                       │
  └────────────────────────────┘   │   │ dpmcp ×N   │  (one per datapack)   │
                                    │   │ cmd/dpmcp  │                       │
                                    │   └────────────┘                       │
                                    └────────────────────────────────────────┘
```

All arrows from the data plane point **out**: agent → k8s API (in-cluster) and
agent → YCP. YCP never dials into a customer cluster.

## The pull loop

1. **Register** — an operator `POST`s `/api/v1/environments`. YCP mints an
   **enrollment token** (kubeadm-style bootstrap token, 30-min TTL) and returns
   it once, along with an install hint. Only the token *hash* is persisted.
2. **Enroll** — `meko-agent` starts in the customer cluster, reads its
   service-account token + cluster facts, and `POST`s `/api/v1/agent/enroll`
   with the enrollment token. YCP validates it and mints a long-lived env-scoped
   **status token** (returned once, hash persisted). The environment moves to
   `ATTESTING`.
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

Two token types, both stored only as SHA-256 hashes (`hashToken` in
`api_server/internal/service/environment_service.go`):

- **Enrollment token** (`mko_enr_…`) — a bootstrap credential with a 30-minute
  TTL, reusable within that window. Validated by matching hash + non-expiry.
- **Status token** (`mko_sts_…`) — long-lived, env-scoped. Sent as a bearer on
  every agent → YCP call.

Plaintext tokens are returned to the caller exactly once and never persisted.

**The agent persists its status token**, in a `meko-agent-state` Secret in its
own namespace, and resumes from it on restart. That is what keeps the enrollment
token a genuine first-contact-only credential: an agent that re-enrolled on
every restart would work for 30 minutes and then lock itself out. `test/e2e`
asserts the restart path for exactly this reason. Remaining gaps — CA pinning,
status-token rotation — are tracked in
[meko#401](https://github.com/yugabyte/meko/issues/401).

## Testing

```bash
go test ./...                 # unit tests: no database or cluster needed
ycp/test/e2e/run.sh           # build + deploy on kind; needs docker + kind
```

- `cmd/agent` — decoding of both response envelopes, including the case that
  broke the agent: a `{"data":…}` wrapper decoded straight into the payload
  struct, yielding zero values instead of an error.
- `internal/mcp` — end-to-end MCP protocol test (initialize → tools/list →
  tools/call) plus the unknown-method error path.
- `test/e2e` — builds the api-server and agent images, stands them up on a
  throwaway kind cluster, and drives register → enroll → READY → restart. This is
  the only test that exercises the real images, the real migrations, RBAC and the
  token handshake over the wire; see the header comment in `test/e2e/run.sh`.

The registry's own unit tests live with the implementation, in
`api_server/internal/service/` and `api_server/internal/repository/`.
