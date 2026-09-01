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
| `cmd/agent` | `meko-agent` | Per-environment pull agent for a **Meko** data plane. Reads cluster facts from the in-cluster k8s API, enrolls, reconciles the environment's declared stack directly against k8s, reports status. |
| `cmd/reconcile-agent` | `ycp-reconciler-agent` | The same pull-agent shape for an **amp** data plane (`kind=amp`): it enrolls, reads desired state, and drives the amp controller's REST API — rather than k8s — to converge it, then reports the controller's inventory back. The sole YCP client for its environment; see [`amp/controller/architecture/ac-ycp.md`](../amp/controller/architecture/ac-ycp.md). |
| `reconcile` | — | Backend-agnostic reconcile engine `cmd/reconcile-agent` is built on: a generation-diff planner over a pluggable `Backend` (amp's is `reconcile/ampbackend`). Level-triggered, so a missed tick self-heals. |
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
  └────────────────────────────┘   │   │ Meko stack │  (one per environment)│
                                    │   │ (database, │                       │
                                    │   │  mcp-server│                       │
                                    │   └────────────┘                       │
                                    └────────────────────────────────────────┘
```

All arrows from the data plane point **out**: agent → k8s API (in-cluster) and
agent → YCP. YCP never dials into a customer cluster.

## The pull loop

1. **Register** — YBM (or an operator) `POST`s `/api/v1/environments` with a **name**
   and the **`api_key_id`** of the YBM API key the environment's agent will
   authenticate with. YCP resolves the Meko release the data plane should run (six image
   references, pinned in `meko/api_server/internal/service/dataplane_release.go`), stores
   it as that environment's desired state, and returns the environment with an install
   hint. `GET /api/v1/environments/{id}` renders the same hint, so an operator who did
   not run the commands there and then can still find them on the environment; the
   `agent_base_url` the registration gave is stored for exactly that. The key id is
   unique fleet-wide: one key drives one environment.
2. **Enroll** — `meko-agent` starts in the customer cluster, reads its
   service-account token + cluster facts, and `POST`s `/agent/enroll` — like every
   agent call, authenticated with the API key. YCP resolves the environment bound to
   the key and returns its identity **together with the stack** the environment runs.
   The environment moves to `ATTESTING`. Enrollment is an idempotent check-in the
   agent repeats at every startup.
3. **Deploy** — the agent has everything it needs in that one response, so it deploys
   the whole data plane on its first tick. It persists the stack in its state Secret,
   which means a restart during a control-plane outage brings Meko back with no
   successful control-plane call in the path. Nothing has to be commanded, and a
   control plane that is down cannot stop an already-enrolled agent from reconciling.
4. **Reconcile** — on each tick the agent `GET`s `/agent/desired-state`. Desired
   state is a generic, resource-typed set (`{kind, name, spec,
   generation}`, where the control plane bumps `generation` only when a resource's spec
   changes, so an agent can skip an unchanged resource without keeping its own store);
   it is where registration wrote the stack and where an operator's later changes go, so
   **when it can be read it is authoritative** — including when it is empty, which is how
   an environment is told to run nothing. **When it cannot be read the enrolled stack
   stands**, and the tick proceeds: an unreachable control plane no longer leaves the
   data plane untouched. Garbage collection is the one thing that needs a desired state
   that was actually read — the agent cannot know what an operator withdrew from a
   response it never got.
5. **Report** — the agent `POST`s `/agent/status` with real facts,
   component health. This drives the fleet registry and the console's live view.

Because desired state is resource-typed, adding a new managed resource needs
only a new agent-side reconciler — no protocol change.

`POST /environments` still accepts `"desired": [...]` to declare an environment's
resources explicitly — including `[]` for one that runs nothing until told — and
`--no-bootstrap-defaults` turns seeding off fleet-wide. An agent that enrolls into an
environment with no stack runs only what desired state declares, which is the behaviour
that predates this.

### What the agent runs

| Kind | What the agent does | Where |
|------|--------------------|-------|
| `meko-stack` | **The whole data plane, as one resource.** What registration seeds and enrollment hands over. YugabyteDB with the `mage` and `pg_dist_rag` extensions, then Meko's `api-server`, `mcp-server`, `rag-worker`, `inference-gateway` and `ui` over it. | `cmd/agent/mekostack.go` |
| `database` | Runs the data-plane database itself (StatefulSet + Service + a password Secret it generates), or resolves an external one the operator points it at. Everything else derives its DSN from this. | `cmd/agent/stack.go` |
| `mcp-server` | Deploys `meko-mcp-server` over that database — the tenant-facing MCP endpoint on `:8000`. | `cmd/agent/stack.go` |
| `rag-worker` | Deploys the RAG indexing worker over the same database. | `cmd/agent/stack.go` |

`meko-stack` is built **on** the other three rather than beside them: it resolves its
database through the same code path as kind `database`, and delegates the MCP server and
RAG worker to their own reconcilers, lending them its database and its GC key. So
there is one env contract per workload rather than two that drift — `meko-mcp-server`'s
alone is fourteen variables split across a ConfigMap, a composed-DSN Secret and an
`envFrom`.

### One ordering constraint is real

The extensions have to exist before the api-server starts. `api_server` runs its
migrations on boot and the data set references `dist_rag.vector_indexes` and the
`dist_rag` enum types, so an api-server that reaches a database without `pg_dist_rag`
fails its own migration and crash-loops. The reconciler therefore goes: database →
`CREATE DATABASE meko` → `CREATE EXTENSION mage, pg_dist_rag CASCADE` → everything else.

Everything else is deployed whether or not that succeeded, for the same reason the
mcp-server reconciler already worked that way: a workload that is declared and retrying
is visible and diagnosable, and an empty namespace while the agent waits for a database
to accept connections is not.

`mage` additionally needs the `ysql_yb_enable_mage=true` gflag on both the master and
the tserver — a start-time flag, so the agent renders it into the `yugabyted` command
rather than issuing SQL. And the image matters: `mage` and `pg_dist_rag` are in
Yugabyte's Meko build (`yugabyte-db/ybdb-meko-release`), not in the public `yugabytedb`
image, so `images.database` has no default. A guessed public tag produces a database
that starts perfectly and then cannot install a single extension the schema needs.

Two properties the specs hold to:

- **Secret values never travel in desired state**, only Secret *names*. Operator
  credentials (`OPENAI_API_KEY`, AWS keys) reach a workload by `envFrom`
  secretRef, so the control plane never stores them and the agent never copies
  them. The one exception is a composed connection string that cannot be
  assembled from `secretKeyRef`s, which the agent writes into a Secret beside the
  password it derives from.
- **Every spec field is optional.** `{"kind":"database","name":"meko"}` with no
  spec brings up a working database; an unknown field from a newer control plane
  is ignored rather than fatal. The exception is a private image: `mcp-server`
  and `rag-worker` refuse to deploy without `spec.image`, because a guessed tag
  fails minutes later as `ImagePullBackOff` instead of immediately as a bad spec.

Applies are **server-side apply**, not create-if-absent: a changed image in
desired state has to actually roll out, and the earlier "POST and treat 409 as
success" was create-once.

### Environment state is observed, not narrated

The reported state comes from the components the agent read back from its own
API server: `READY` when every declared component has all replicas ready,
`PROVISIONING` while a stack is still coming up, `DEGRADED` once it has been
`READY` and something since failed. It used to be a fixed three-tick walk to
`READY` regardless of what was running.

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

The agent's credential is a **YBM-minted API key**, shown once by YBM and
revocable there. Registration binds its id (`api_key_id`, a uuid — not the key
itself) to the environment, uniquely fleet-wide. Every agent → YCP call carries
the key as a bearer; YBM's agent pass-through validates it and forwards the call
to meko with `X-Meko-Admin-Secret` + `X-Meko-Api-Key-ID`, and meko resolves the
environment from the key id alone. Against a standalone control plane (dev, the
e2e — no YBM), the agent sends that header pair itself
(`YCP_DEV_ADMIN_SECRET` + `YCP_DEV_API_KEY_ID`).

There is no bootstrap credential and nothing minted at enroll time: enrollment is
an idempotent check-in the agent repeats at every startup, so a restart can never
lock an agent out. A 401 means the key was revoked; the agent logs it loudly and
keeps reconciling what it last read. Recovery is a redeploy with a fresh key
Secret carrying a newly-minted key -- an environment is never rebound to a
different key once registered, so a revoked key means registering a new
environment for the replacement. A check-in that fails at startup falls back to
the persisted identity when there is one, for the same reason: a control plane
that cannot be reached must not take the data plane down with it.

The `meko-agent-state` Secret holds no credential: only the identity the last
check-in confirmed, the **stack** the agent enrolled with, and the last desired
state — so a restart during a control-plane outage begins reconciling before its
first request succeeds. The remaining gap — CA pinning — is tracked in
[meko#401](https://github.com/yugabyte/meko/issues/401).

## Testing

```bash
go test ./...                 # unit tests: no database or cluster needed
ycp/test/e2e/run.sh           # build + deploy on kind; needs docker + kind
```

- `cmd/agent` — decoding of both response envelopes, including the case that
  broke the agent: a `{"data":…}` wrapper decoded straight into the payload
  struct, yielding zero values instead of an error.
  tools/call) plus the unknown-method error path.
- `test/e2e` — builds the api-server and agent images, stands them up on a
  throwaway kind cluster, and drives register → bootstrap → command → GC. This is
  the only test that exercises the real images, the real migrations, RBAC and the
  token handshake over the wire; see the header comment in `test/e2e/run.sh`.

The registry's own unit tests live with the implementation, in
`api_server/internal/service/` and `api_server/internal/repository/`.
