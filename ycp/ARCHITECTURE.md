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
| `cmd/reconcile-agent` | `ycp-reconciler-agent` | The one pull agent, for either kind. It enrolls, reads desired state, and drives a REST API to converge it, then reports the inventory back. It never applies a manifest — a data plane's workloads come from its chart. `ENV_KIND` picks the backend — `amp` (the default) drives the amp controller, `meko` drives meko's api_server. It also converges the software the environment runs: a declared `controller` kind is a `helm upgrade` of the release the agent is itself a member of, and a declared `fleet-version` rolls the endpoints onto it. The sole YCP client for its environment; see [`amp/controller/architecture/ac-ycp.md`](../amp/controller/architecture/ac-ycp.md). |
| `reconcile` | — | Backend-agnostic reconcile engine `cmd/reconcile-agent` is built on: a generation-diff planner over a pluggable `Backend` (`reconcile/ampbackend`, `reconcile/mekobackend`). Level-triggered, so a missed tick self-heals. |
| `reconcile/mekobackend` | — | The `Backend` for meko's tenant API: the `agent` and `datapack` kinds, converged over `api_server`'s PAT-authenticated routes. Its verbs are synchronous, so unlike amp it needs no applied-generation echo. |
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
  │  --control-plane-mode      │   │  ┌──────────────────┐  outbound only    │
  │  ┌──────────────────────┐  │◀──┼──│ reconcile-agent  │────────┐          │
  │  │ fleet API + console  │  │enroll│ cmd/reconcile-   │        │          │
  │  ├──────────────────────┤  │desired│      agent      │        │ REST     │
  │  │ Postgres / YugabyteDB│  │status └──────────────────┘        ▼          │
  │  │ (fleet registry)     │  │   │                    ┌──────────────────┐ │
  │  └──────────────────────┘  │   │                    │ amp controller,  │ │
  └────────────────────────────┘   │                    │ or meko's        │ │
                                    │                    │ api_server       │ │
                                    │                    │ (chart-installed)│ │
                                    │                    └──────────────────┘ │
                                    └────────────────────────────────────────┘
```

All arrows from the data plane point **out**: agent → YCP, and agent → the
chart-installed API it converges (in-cluster). YCP never dials into a customer
cluster.

## The pull loop

1. **Register** — YBM (or an operator) `POST`s `/api/v1/environments` with a **name**
   and the **`api_key_id`** of the YBM API key the environment's agent will
   authenticate with. Registration declares nothing of its own: an environment starts
   with whatever the caller passed as `desired`, or with nothing. It returns the
   environment plus an install hint, and `GET /api/v1/environments/{id}` renders the
   same hint, so an operator who did not run the commands there and then can still
   find them; the `agent_base_url` the registration gave is stored for exactly that.
   The key id is unique fleet-wide: one key drives one environment.
2. **Install** — the operator installs the data plane from its chart:
   `yb-amp-controller` for an amp environment, the `meko` chart for a meko one. The
   agent is a component of that release. Nothing about the workloads travels through
   the control plane, which is why the install hint is the only place an operator is
   told what to run.
3. **Enroll** — the agent starts in the customer cluster, reads its service-account
   token + cluster facts, and `POST`s `/agent/enroll` — like every agent call,
   authenticated with the API key. YCP resolves the environment bound to the key and
   returns its identity. The environment moves to `ATTESTING`. Enrollment is an
   idempotent check-in the agent repeats at every startup, so a restart can never lock
   an agent out.

   The agent also **attests its telemetry** here — whether otel and byoc shipping
   are on, and the metrics endpoint it is configured with, read from the env its chart
   set from the same values its collectors render from. Both charts set it; a meko
   agent that reports nothing is on a chart predating its collector and is left alone,
   where an amp one is degraded. YCP compares that endpoint
   against its own `--byoc-metrics-url`, which is the same value it printed in the
   install hint. This is the only check a customer cannot edit away: a data plane
   installs from a checkout they have, so the endpoint is not theirs to choose but
   every rule inside the chart is theirs to edit.

   A mismatch **degrades the environment; it never refuses it.** An environment that
   cannot ship metrics is still one an operator needs to see, and one we would rather
   have registered and visibly `DEGRADED` than turned away and invisible — refusing it
   hides the very thing the refusal is complaining about. It also keeps the invariant
   above intact: enrolment repeats at every agent startup, so a refusal would be a
   restart-time lockout, killing a running environment's convergence on its next pod
   recycle over a telemetry problem. Nothing else enforces it either, for now — the
   chart warns through NOTES.txt and its post-install preflight reports to its own log
   without failing the release — because the BYOC door is still being rolled out fleet
   by fleet, and an install that cannot reach it is as likely to be a fleet problem as
   an operator mistake.

   The reason rides a synthetic `telemetry` component (0/1, with the reason as its
   error), because the console already renders a component's error and that needs no
   new column. An agent too old to attest anything is degraded the same way rather
   than silently passed.

4. **Reconcile** — on each tick the agent `GET`s `/agent/desired-state`. Desired
   state is a generic, resource-typed set (`{kind, name, spec, generation}`, where the
   control plane bumps `generation` only when a resource's spec changes, so an agent
   can skip an unchanged resource without keeping its own store). **When it can be
   read it is authoritative** — including when it is empty, which is how an
   environment is told to hold nothing. Garbage collection is the one thing that needs
   a desired state that was actually read: the agent cannot know what an operator
   withdrew from a response it never got.

   The mirror of that rule applies to **actual**, and a backend author implementing
   `List` has to know it: the engine derives creates from a resource's *absence*
   from actual and deletes from its *presence*, so a reading that is missing part
   of the environment cannot be returned as if it were whole. A backend that can
   reach only part of its remote returns a row per resource it could not read
   (`Actual.DiscoveryError`) rather than a short list. Nothing is issued for such a
   row — no create, which would duplicate whatever is really there; no update,
   which would be written blind; no delete, even of a withdrawn resource, since
   the row is no evidence it is still there — and the tick reports DEGRADED,
   because it converged only the part it could see.

   The other exception to "listed and not desired means collect it" is a resource
   the backend did not create and cannot delete — the amp controller, which the
   agent runs inside. Such a row is marked `Preexisting` and is reported but never
   collected, so an environment that has never declared one is not perpetually
   told its controller is drift.

   Because the agent obeys an empty set, the *write* side has to be sure the operator
   meant one. On top of the body checks every write shares (below),
   `POST /environments/{id}/desired` requires an explicit `resources` and rejects
   unknown fields at *every* level, so a misspelled key is a 400 rather than a
   tree-emptying command; `"resources": []` additionally needs `"allow_empty": true`.
   `spec` is exempt — it is passed through opaquely, so a field only a newer agent
   knows still reaches it. Registration keeps its plain `"desired": []` (below): there
   is nothing yet to delete.
5. **Report** — the agent `POST`s `/agent/status` with real facts, the workload health it
   read from its own namespace (`components`: one row per Deployment and StatefulSet,
   healthy-of-total plus the image tag — the only way the console can say what is up,
   since nothing dials in to a BYOC data plane), and the inventory it
   read back. This drives the fleet registry and the console's live view. An amp agent
   also relays the controller's **task log** here — see below — and re-sends its
   telemetry attestation every tick, so a later `helm upgrade` that turns telemetry off
   shows up as a `DEGRADED` environment with a `telemetry` component saying why, rather
   than as a dashboard that gradually empties. It is the same `check()` with the same
   degrade-only outcome as at enrolment — there is no enroll/status distinction to
   explain.

   That attestation carries **runtime** state as well as configuration, because the two
   fail independently: a deployment can be configured perfectly and ship nothing when
   the door goes unreachable, a credential is rotated or a collector crashloops. The
   agent reads the controller collector's own
   `otelcol_exporter_sent_metric_points{exporter="otlphttp/byoc"}` each tick and reports
   how long it has sat still; the control plane degrades an environment whose counter
   has not moved for `TelemetryPolicy.StaleAfter`. The **delta is computed in the
   agent** on purpose — detecting a stall needs the previous reading, and the agent is
   already the thing polling on a loop, so keeping the history server-side would mean a
   column and a migration to learn the same fact.

   Three details make that check trustworthy rather than noisy. It watches the
   **controller** collector, which always has a target (the amp controller itself), not
   the compute one — that scrapes endpoint pods, so an environment owning no endpoints
   has a legitimately flat counter. The collectors scrape on a fixed clock, so a healthy
   counter climbs whether or not the environment is busy, which is what lets "flat" mean
   broken rather than idle. And a probe the agent could not perform is reported as
   *unknown*, never as a confirmed stall. Queue depth and the failed-send counter ride
   along as diagnosis: with the counter flat, an empty queue means nothing is reaching
   the exporter and a full one means the door is not taking it.

Because desired state is resource-typed, adding a new managed resource needs only a
new case in a backend — no protocol change.

### A resource that will not converge says which kind of "will not"

A resource the agent stops trying to converge reports one of three states, and they
are not interchangeable — each asks something different of whoever reads the row:

| `state` | what happened | what to do |
|---|---|---|
| `drift` | no verb exists for this resource as declared — a kind with no update/delete path, a field only create carries | nothing; the resource is like that |
| `rejected` | this declaration cannot be applied but another could — the remote refused it with a 400, or the agent refused it before sending | re-declare it; `error` says what it wants |
| `blocked` | the agent is not permitted: credentials, a scope it lacks, a quota no rewrite clears | fix the agent's access or the account's |

All three are permanent in the same way — issued once, not re-issued until the spec
changes, and not enough on their own to make the environment DEGRADED. Only the state
differs, because only the state reaches the console, which renders a different
sentence for each: a refused declaration reported as `drift` gets an operator "this
resource type cannot be changed in place" in place of the reason (CLOUDGA-36992). A
reader that meets a state it does not know must show `error`, not that sentence.

A fourth state, `unreadable`, is not one of these and behaves the opposite way. It
means the agent could not read the resource at all, so nothing was attempted rather
than attempted and refused — and it **does** degrade the environment, because a tick
that converged only what it could see has not converged what was declared. It is also
the one state that overrides the row's `attrs`, clearing them: a resource nobody could
read has nothing currently observed to report, and leaving the last reading in place
would present it as fresh.

### Tasks: the operations behind the inventory

Inventory says an endpoint is `updating`; the amp controller's task log says *why*, and
whether the operation failed. That log lives inside the customer's cluster, and there
is no inbound path to a BYOC data plane — `agent_base_url` is the YBM pass-through the
agent *dials*, not an address the control plane can call — so the agent relays it on
the same status heartbeat, and `GET /api/v1/environments/{id}/tasks` serves it back.

The relay is a **change feed, not a snapshot**. The agent reads
`GET /tasks?updated_since=<watermark>` (ordered `updated_at` ascending) and reports only
what moved, so a quiet tick carries nothing. Ordering by `updated_at` is what catches a
task created and finished inside one tick — `create_branch` routinely is — which
polling non-terminal tasks never observes at all.

Two properties are load-bearing:

- **The control plane holds the history.** The agent's memory of what it relayed dies
  with its pod, and nothing can re-read the log on its behalf, so
  `meko_system.environment_tasks` is the durable copy. Its retention is therefore the
  console's history depth — see [DEPLOY.md](DEPLOY.md).
- **A delta is delivered once.** The agent retires one only after the whole report is
  accepted, so a failed heartbeat re-sends rather than loses it; the control plane's
  upsert is guarded on `task_updated_at` so a re-sent or reordered report cannot walk a
  terminal task back to `running`.

This does not soften "state is observed, not narrated": YCP invents no tasks and drives
no state machine of its own, it relays what the controller recorded.

### Extension catalog

The amp controller's `GET /extensions` — builtins plus every image an operator
registered — is what a cluster can be created with, and a console needs it to offer
anything beyond a hardcoded list. The agent relays it on its own call,
`POST /api/v1/agent/extensions`, not on the heartbeat: it sends only when the catalog's
digest changes, plus an hourly resend so a control plane that lost its copy recovers
without an agent restart, and backs off for five minutes after either the controller
read or the POST fails, so a controller, control plane or YBM passthrough without the
route costs one request per five minutes rather than one per tick.
`meko_system.environment_extensions` holds one row per environment, and
`GET /api/v1/environments/{id}/extensions` serves it.

A missing row is `extensions: null`, meaning **unknown** — an agent predating the
route, a meko environment, or one that has not reported yet — and a console falls back
on it. `[]` is a reported empty catalog. Neither side writes one for the other: the
agent sends nothing when the controller read fails, and the control plane rejects an
absent or null `extensions` rather than storing it as empty, and refuses a non-amp
environment's POST outright, so a meko environment reads `null` by rule.

`POST /environments` takes `"desired": [...]` to declare an environment's resources —
including `[]`, which is simply the same as omitting it. There is no server-side seed:
what an environment holds is what somebody declared.

### What a meko agent converges

| Kind | Name | What it is |
|------|------|-----------|
| `agent` | `"<agent>"` | A user-scoped agent in meko — the name datapacks are attributed to. `POST`/`GET`/`DELETE /agents`. |
| `datapack` | `"<agent>/<datapack>"` | A datapack under that agent. The path is the hierarchy, so the agent is created first and the datapack gated on it. |

The shape is `ampbackend`'s: the path yields the parent for the engine's ordering and,
at apply time, the parent's id for the request. amp cannot model *its* equivalent —
users have no delete, so its engine must not list them, and `ampbackend` materializes
a pool's owner inside `Create` instead. meko's agents do have a delete, so they are
resources.

Two things the mapping has to do that amp's does not:

- **Cascade.** The engine skips a child's delete when the parent is going too, on the
  contract that the backend cascades. meko's `DELETE /agents/{id}` removes the
  `agent_access` and `agents` rows and leaves the datapacks pointing at an id that no
  longer exists, so `mekobackend` deletes the datapacks first itself. Without that,
  withdrawing an agent with its datapacks orphans them permanently — the next list
  cannot see them, so nothing can ever collect them.
- **Leave the signup agent alone.** `meko_agent` is the row every account gets at
  signup and the id the share and invite grants are written under. Deleting it would
  revoke every datapack shared into the account and break every later create that
  names no agent, and nothing but a fresh signup restores it. It is account
  infrastructure, so it is excluded from the inventory entirely — the same call amp
  makes about users — along with anything attributed to it.

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

A state must be *present* on both `/agent/status` and `/agent/bootstrap-progress`:
an absent or blank one is a `400` that writes nothing, heartbeat included. Every
agent write must likewise carry `Content-Type: application/json` (`415` otherwise)
and a non-empty body (`400`). BYOC is pull-only, so the agent's self-report is the
*only* signal about a data plane: a call that succeeds for a report that did not
happen leaves the environment reading as healthy. An empty body used to do exactly
that, binding to a zero-valued report and blanking the state
([CLOUDGA-36984](https://yugabyte.atlassian.net/browse/CLOUDGA-36984)).

A state the control plane does not *recognise* is a different case and is stored,
with a warning in the control-plane log. The agent is upgraded on the customer's
schedule, so it can report a state added after the running control plane shipped;
refusing it would fail every heartbeat from every environment on that agent version,
and since a failed report is only logged (`reconcile/loop.go`) the fleet would go
stale with no other signal. That is the compatibility rule [`REVIEW.md`](../REVIEW.md)
states for `EnvState`, and the console follows it too — an unrecognised state renders
under its own name.

The known-state list is still duplicated in three places — `EnvState` here,
`envStates` in meko's `api_server`, and the enum in `meko/design/ycp-openapi.yaml` —
because the two sides are separate Go modules that cannot import each other. Drift no
longer strands a fleet, but it makes the published contract untrue and the
unknown-state warning misleading, so `TestEnvStateListsAgreeAcrossTheRepo` compares
them and the `meko_api_server` CI filter lists both non-Go copies so that a PR
touching only one still runs it.

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

The `reconcile-agent-state` Secret holds no credential: only the identity the last
check-in confirmed, and the kind it confirmed it as — environment names are unique
only within a kind, so the name alone cannot tell a resumed identity from a
same-named environment of the other kind. The remaining gap — CA pinning — is
tracked in [meko#401](https://github.com/yugabyte/meko/issues/401).

### The meko api-server has one caller

meko's `api_server` in a BYOC data plane has one *writer*: the reconcile-agent. The
release ships `meko-ui` and `meko-mcp-server` beside it and they read through it (see
below), but nothing else is expected to create. That is what makes the mapping amp's:
the agent is authoritative over everything the API holds,
exactly as it is over an amp controller's inventory, so it lists every agent and
every datapack and garbage-collects whatever desired state stops declaring.

The kinds carry their hierarchy in the name, as amp's do:

    agent     "<agent>"
    datapack  "<agent>/<datapack>"

so the engine creates the agent first and gates the datapack on it, and a create
takes its agent id from the path. amp cannot model users this way — there is no
user delete, so its engine must not list them, and `ampbackend` materializes a
pool's owner inside `Create` instead. meko's agents have a real delete, so they are
resources.

`MEKO_API_TOKEN` names the account whose inventory the agent converges; it is
optional only because a BYOC api-server runs Cognito-off and authenticates nobody.

The stack still ships `meko-ui` and `meko-mcp-server` beside the api-server, for
local debugging and inspection. They read; they are not expected to create
datapacks, and the rule if one ever did is amp's rule — a resource desired state
does not declare is collected on the next tick. That is the assumption to revisit
if the data plane ever gains a second writer: ownership would have to become a
property of the rows (`agent_details` is a JSONB column that could carry a mark)
rather than of the deployment.

## Testing

```bash
go test ./...                 # unit tests: no database or cluster needed
ycp/test/e2e/run.sh           # build + deploy on kind; needs docker + kind
ycp/test/local/meko-reconcile.sh   # the kind=meko reconcile loop, on a kept e2e
                                   # cluster; needs ECR credentials
```

- `internal/mekoclient` — decoding of the api-server's `{"data":…,"error":…}`
  envelope, including the case that once broke the agent: the wrapper decoded
  straight into the payload struct, yielding zero values instead of an error
  ([meko#399](https://github.com/yugabyte/meko/issues/399)).
- `reconcile/mekobackend` — the kind mapping, the cascade, and the rejections that
  must be permanent rather than retried.
- `test/e2e` — builds the control-plane image, stands it up on a throwaway kind
  cluster, and drives rollout → health → base path → auth gate → register → install
  hint. It exercises the real image, the real migrations and RBAC over the wire. It
  does **not** cover an agent converging anything; see the header comment for why,
  and `test/local/meko-reconcile.sh` for where that lives.

The registry's own unit tests live with the implementation, in
`api_server/internal/service/` and `api_server/internal/repository/`.
