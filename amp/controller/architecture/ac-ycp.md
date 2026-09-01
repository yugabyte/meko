# ac — YCP integration (reconcile-agent)

**Status:** Draft
**Last updated:** 2026-08-31

How an amp controller is driven by **YCP** (the Yugabyte Control Plane — meko's
`api_server` run with `--control-plane-mode`), so a fleet operator declares an amp
environment's resources in one console and sees the inventory converge there.

The controller does **not** talk to YCP itself. A standalone **reconcile-agent**
(a separate pod, code in `ycp/cmd/reconcile-agent` + `ycp/reconcile`) is the sole
YCP client for the environment: it enrolls, pulls YCP's desired state, and drives
this controller over its REST API to converge, reporting the resulting inventory
back on each heartbeat. The controller's only YCP-specific surface is the
write-once binding endpoint the agent pins itself to.

```
  reconcile-agent (in the customer cluster)     amp controller            YCP (control-plane mode)
  ┌─────────────────────────────────┐           ┌──────────────┐          ┌────────────────────────┐
  │ enroll (kind=amp) ──────────────┼───────────┼──────────────┼─────────►│ POST /agent/enroll     │
  │ pin binding ────────────────────┼──────────►│ POST /ycp/    │          │  -> environment id     │
  │                                 │           │      binding  │          │                        │
  │ every reconcile interval:       │           │              │          │ GET  /agent/desired... │
  │  1. GET desired state ──────────┼───────────┼──────────────┼─────────►│                        │
  │  2. diff vs. controller List ───┼──────────►│ REST /api/v1 │          │                        │
  │  3. create/update/delete ───────┼──────────►│  (async task)│          │                        │
  │  4. report inventory ───────────┼───────────┼──────────────┼─────────►│ POST /agent/status     │
  └─────────────────────────────────┘           └──────────────┘          └────────────────────────┘
                                                  ycp_binding                environments.kind='amp'
                                                  (migration 0007)           environments.resources=[…]
```

Every agent → YCP call authenticates with the environment's YBM API key
(`YCP_API_KEY`, or the `YCP_DEV_ADMIN_SECRET` + `YCP_DEV_API_KEY_ID` header pair
against a standalone control plane). Enrollment is an idempotent check-in the
agent repeats at every startup; the state Secret (`reconcile-agent-state`) keeps
only the identity it confirmed, no credential.

The engine, the amp backend (how desired-state kinds map onto amp's
pool → cluster → branch → endpoint hierarchy), and the enroll/report client all
live in the `ycp` module (`ycp/reconcile`, `ycp/reconcile/ampbackend`,
`ycp/internal/ycpclient`). The agent is authoritative: it garbage-collects any
controller resource not present in desired state.

A pool's `owner_user_id` is the one thing the mapping materializes outside the
declared kinds. Users are not a YCP kind — there is no user delete, so the engine
must not List/GC them — but a pool needs an existing owner. So before creating a
pool the backend ensures that user (`POST /api/v1/users` with the explicit id,
idempotent on it); the owner UUID carried in the pool spec becomes a controller
user on first use.

## Convergence status

Each heartbeat carries per-resource health, not just the inventory, so an
operator sees *why* a resource has not converged without reading agent or
controller logs. The engine distinguishes four cases and the agent folds each
onto the resource's `ResourceStatus.Error` (`ycp/internal/api`):

- **Failed** — a retryable rejection (the controller refused a create/resize, or
  the task it planned reached `failed`, whose own error is carried through).
  Retried on backoff; makes the environment `DEGRADED`.
- **Updating** — an update the backend has issued but cannot yet confirm: it just
  planned the task, or the task it planned earlier is still `pending`/`running`.
  Neither applied nor failed, so nothing is re-issued, nothing backs off, and it
  does **not** degrade the environment. Reported as the resource's `updating` state.
- **Stuck** — no progress past `Options.StuckAfter` (default 10m): a resource sat
  non-Ready in the same backend state (an endpoint that never provisions), or an
  update stayed `Updating` that long — a wedged worker, the one case reading the
  task's status cannot detect. Also `DEGRADED`, with a "no progress" reason.
- **Permanent drift** — a verb that cannot succeed for the spec as declared: a
  storage pool has no delete, pools and clusters have no update, a branch's
  `extensions` cannot be removed (and is the one field it cannot be *created*
  with — below), and the controller refuses some bodies outright with a `400`.
  The engine reports it and stops re-issuing it (no backoff flap) rather than
  treating an unfixable call as a failure; it does **not** by itself degrade the
  environment. Backends flag these with `reconcile.Permanent`. The mark is held
  against the resource's **generation**, so a rewritten spec retires it and the
  verb is asked again — otherwise a refused create would wedge the resource for
  the life of the agent process, since its action stays planned every tick.

Each inventory row also carries `ResourceStatus.Attrs`, kind-specific details the
backend read while listing: every resource's amp `id` (a UUID, distinct from the
containment path in `ResourceStatus.ID`); a cluster's/branch's `pg_version`; a
branch's `extensions` (opted in per branch — seeded at create, inherited on fork;
the default branch's set is also mirrored on the cluster); a branch's `fork_lsn`,
`parent_branch_id`,
`is_default`; and an endpoint's `role`, `address`, `tailnet_address`, `cpu`,
`memory`. A branch and an endpoint also report `applied_generation` — see below.
A branch's `id`
pairs with a fork's `parent_branch_id` so a console can rebuild the fork tree
(the containment tree is already derivable from `ID`/`Parent`). The controller
exposes no live/head LSN, so the only LSN reported is the branch fork point.

## The applied generation

Every mutating controller call is asynchronous, so `202` means *task planned*, not
*task done*. The engine is level-triggered on **existence** — a failed create
leaves the resource absent and a failed delete leaves it present, so the next diff
re-issues both — but an update's only staleness signal is the generation. Holding
that in the agent and writing it from the 202 meant a task that failed afterwards
left the resource unconverged with the agent reporting healthy, permanently
(meko#985).

So the controller records it and echoes it back, the `metadata.generation` /
`status.observedGeneration` split `Actual.ObservedGen` was designed around:

- `amp_registry.endpoints.ycp_generation` and `.branches.ycp_generation` hold the
  highest generation **fully applied** to the row — the kinds with an update path;
  creates and deletes self-heal through presence and need no echo.
- A task declares the generation it applies (`tasks.ycp_resource` /
  `ycp_resource_id` / `ycp_generation`) and the **worker** stamps the row after
  every step has succeeded, never the handler that planned it. The write is
  `GREATEST(...)`, so an older task completing late cannot regress it. See
  [ac-tasks.md](ac-tasks.md).
- `ampbackend.List` reads it into `Actual.ObservedGen` for those two kinds; pools
  and clusters have no update path and report 0.

**A 202 is reported as Pending, not applied**, and that is what makes the rest
safe. `Engine.cache` is still a floor for `effGen`, since a backend that cannot echo
has nothing else — but it is now only ever written for work that *completed*,
because an `ampbackend` verb that plans a task returns `reconcile.Pending`, which
records nothing.

A branch is the case that forces this. `add_branch_extensions` unions the branch row
**before** it redeploys the branch's endpoints, so a tick landing after the union
sees nothing missing. Treating that as converged would cache the generation and
no-op the branch forever, while the task that unioned the row may still fail on a
redeploy — the same latch, reached from the other side. So the add goes out whenever
the generation is still unrecorded, carrying the full declared set when there is no
delta: the union is absolute, so the row does not move and a converged endpoint
renders an identical manifest, but the completing task can finally stamp the
generation, and an endpoint the first task never reached gets its redeploy.

Distinguishing **Updating** from **Failed** on later ticks needs no timer, because
the `202` body is the `Task`. The backend keeps that task id per resource and reads
its status before issuing anything: non-terminal → `Pending` again, nothing
re-issued; `failed` → the task's own error, so the backoff engages and the console
shows something actionable; `succeeded`, or a task the controller has forgotten →
re-issue, since the echo is the authority on whether the work landed. A `409` on
the update path is also pending: a first-writer-wins lock is held by a task already
doing this work. The map is in-process and lost on restart, which is safe — without
an entry the update is simply re-issued, and both updatable kinds answer that
`409`.

Adding an update path keeps this by construction. Every `Backend.Update` arm hands
its call to one `applyUpdate` helper that owns the task lookup, the verdict and
recording the new task, so an arm bypassing it loses the whole mechanism rather
than one detail; `updateFuncs` is the only place the updatable set is written down;
and a table-driven test walks `allKinds` asserting each is either `Permanent` on
`Update` or both sends a generation and has one echoed back.

The agent binds the controller with capped retry/backoff at startup (a binding
*conflict* is still fatal), so a fresh install where the controller is not yet up
does not crash-loop the agent pod.

## Branch extensions

A branch spec carries `extensions` — the externalized extensions every endpoint on
the branch mounts — but only for a branch that **already exists**. amp applies them
with `addBranchExtensions`, a separate call against a branch id the create has not
produced yet, so `Create` refuses a branch spec declaring them instead of dropping
the field: the operator gets a reason on the resource rather than a healthy-looking
branch that never got the extension. Enable them in a later write, once the branch
is in the inventory; that bumps its generation and the update applies them.

A new branch does not need them at create: a fork inherits its parent's set, and a
cluster's default branch is seeded by the cluster spec's own `extensions` (a real
`createCluster` field). That default branch is also the one case the refusal cannot
catch — the cluster create materializes `main`, so the agent never issues a branch
create for it, and declaring `extensions` on `main` in the same write that creates
its cluster is inert until a later edit. Seed it through the cluster spec instead.

An unknown name is a `400`, which the backend already treats as permanent drift, so
a typo surfaces as a reason on the resource rather than an endless backoff. The
update sends only the names the controller does not already report, because the
add **redeploys every endpoint on the branch** — the ImageVolume set is fixed at pod
start — so expect a compute bounce on the tick that applies one. The endpoint's
recorded address is never blanked (the redeploy re-records it, since a ClusterIP
address is the pod IP and the old one is dead), so the agent's readiness check —
address non-empty — stays true throughout, including the window where the row still
names a retired pod. The endpoint therefore never reads as non-Ready or stuck across
a redeploy, and never as failed if one goes wrong: what an endpoint actually mounts
is not in the amp API, so the agent cannot check it.

A branch's set only grows: there is no remove API, so a fork's endpoints always mount
a superset of what its inherited objects need. Dropping a name from the spec is
therefore permanent drift, named in the resource's `Error`. Omitting `extensions`
altogether leaves the branch's set unmanaged, which is what a fork inheriting its
parent's set wants.

Extensions are the only branch field the agent converges, not the only one the
controller can change: `PATCH …/branches/{id}/settings` (`updateBranchSettings`)
replaces a branch's `pg_settings`, and the generated client already has it. It is
not wired because create and update disagree about what a declared set means — a
create *merges* over what the fork inherited, that PATCH *replaces* — and picking
one is an operator-facing decision, not a mapping detail. Until it is, a
`pg_settings` value the branch does not already carry is reported as drift, naming
the settings that were not applied.

## Environment `kind`

An environment is registered as **`amp`** or **`meko`**. The kind is fixed at
registration. A meko id stays a bare `slug(name)`; an amp id is prefixed **`amp-`**,
so an amp and a meko environment of the same name never collide and the two coexist
in one fleet. Enrollment carries the kind and YCP rejects a mismatch (409), so the
agent cannot attach to a meko environment. An amp environment seeds **no** desired
state at registration — an operator declares its resources afterwards through
`POST /environments/{id}/desired`.

## Write-once binding

The controller records the single YCP environment it belongs to and refuses to be
rebound to a different one, so a second agent pointed at another control plane
cannot quietly take it over.

- **`POST /api/v1/ycp/binding`** `{ycp_url, environment_id}` — the reconcile-agent
  pins the controller after it enrolls. First write wins; an identical re-bind is
  idempotent (200); a bind that differs from the incumbent is rejected (409, body
  carries the incumbent). **`GET`** returns the current binding or 404.
- State lives in `ycp_binding` (migration 0007), a singleton row in the
  controller's own schema. The routes sit outside the OpenAPI spec, like
  `/healthz` and `/metrics` — the binding is operational state, not a resource.

Request-time enforcement (gating mutating calls on the binding) is deferred; today
the binding is a durable record and a bind-time tripwire.

## History: the retired outbound loop

The controller used to enroll and report to YCP itself, on a background goroutine
(`ycp.Loop`) with its identity persisted in `ycp_identity` (migration 0004). That
was the "register + report" half of a design that always intended reconciliation
to follow. When the standalone agent landed — which must be the *sole* YCP client
(one API key drives one environment) — the controller loop was retired
(migration 0008 drops `ycp_identity`; the `--ycp-*` flags and `controller.ycp.*`
Helm values are gone). The binding endpoint is all that remains on the controller.

## Deploying YCP itself

The control plane is `api_server --control-plane-mode`. Its image is built and
pushed on demand (Quay: `quay.io/yugabyte/ycp-api-server`). The reconcile-agent
image is `quay.io/yugabyte/ycp-reconciler-agent` (`ycp/Dockerfile.reconcile-agent`).

## Testing

The controller-side unit coverage is the binding store + endpoint
(`internal/ycp/binding.go`, `internal/api/ycp_binding_test.go`) and, on the
control-plane side, the `kind`-qualified id, enroll kind-mismatch, and desired /
`resources` round-trip (`meko/api_server/internal/service` and `.../repository`).
The engine, backend mapping, and agent live in the `ycp` module and are tested
there.

The end-to-end guard is the opt-in **`/ci amp_deploy_gke_ycp`** directive
([`amp-control-plane-gke.yml`](../../../.github/workflows/amp-control-plane-gke.yml),
see [`amp/docs/ci-directives.md`](../../docs/ci-directives.md)): it stands up a real
control plane next to a real amp deploy on GKE, deploys the reconcile-agent, and
runs [`ycp-reconcile-smoke.sh`](../../deploy/ycp-reconcile-smoke.sh) — declare a
pool → cluster → branch → endpoint through YCP desired state, assert the agent
creates the tree, reports it `READY`, and GCs it on withdrawal — against the shipped
images. Paired with `/ci amp_deploy_gke_extensions` it also asserts a branch-extensions
edit converges, endpoint redeploy included; on its own that part is skipped, because
only that directive pushes the extension images.

## Deferred

- Request-time binding enforcement (gating mutating controller calls on the bind).
- Self-healing a partial redeploy inside `add_branch_extensions`: the union lands, an
  endpoint redeploy fails, and the branch reports the new set while that endpoint still
  mounts the old one. Now visible (the task fails, so the branch degrades with the
  redeploy's error) but not converged, because the next update finds nothing missing
  and issues no call — the amp `Endpoint` schema exposes no extension list to compare
  against. [#1057](https://github.com/yugabyte/meko/issues/1057).
- Cluster facts beyond the basics in the agent's status report.
- A meko backend for the same agent (selected by environment kind), retiring the
  standalone meko stack agent.
