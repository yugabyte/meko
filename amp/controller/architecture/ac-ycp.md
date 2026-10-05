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
                                                  (migration 0005)           environments.resources=[…]
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
user on first use. The `extension` kind does the same for the admin its verbs
require: a fixed agent user created with `is_admin`.

## Extension kind

`extension`, named by catalog name, registers an image in the controller's catalog
(`PUT /extensions/{name}?user_id=<agent admin>`, spec = that body) and withdrawing it
deregisters it. Builtins are not listed, so they are never collected; every other
registered extension is, like any undeclared resource — including one registered by
hand through amp-ui, so an environment's existing imports must be declared before an
agent with this kind reaches it. The controller refuses a deregister (409) while a
branch enables the extension; that is retried on backoff, not `rejected`, because it
clears without a spec change and a permanent mark on a delete never retires (a delete
is planned at generation 0 every tick). The verbs are synchronous and the row carries every spec field, so the
kind is observable, as below. A cluster create or branch extension add naming an
extension not yet in the catalog is held back with a retryable error rather than sent:
nothing orders a registration ahead of them in a tick, and the controller's 400 would
latch them `rejected`.

## Upgrade kinds

Two kinds sit outside that hierarchy and move software rather than resources. Both
are singletons with a fixed name, so a second declaration is unambiguous drift.

| Kind | Name | What the agent does |
|------|------|--------------------|
| `controller` | `controller` | `helm upgrade` of the release the agent itself is a member of, setting `controller.image.tag`, `agent.image.tag` and `controller.endpoint.image.tag` — the last is the pin new endpoints are created at, so an upgrade that skipped it would move the controller and nothing it deploys. helm runs as a Job outside the release, not in the agent: the rollout replaces the agent's pod, and a client killed while helm waits on the release's hooks leaves it `pending-upgrade`, blocking every later upgrade. The release name is read off helm's own `meta.helm.sh/release-name` annotation rather than configured, since the chart does not fix it. |
| `fleet-version` | `fleet` | `spec.version` is the version every endpoint should run. Once the controller deploys it (its pin), `POST /endpoint-upgrades` rolls the fleet onto it; the kind converges when `GET /fleet-version` reports the fleet at `spec.version` (`ac-endpoint-upgrade.md` §6.1). A running roll's done/total is reported as progress, so a long roll is not flagged stuck. |

`Parent(fleet-version)` is `controller`, so a declared controller upgrade runs
first and the roll waits until the controller is Ready at its version.

Neither kind echoes a generation — there is nothing to stamp. They are **observable**
instead, and `Update` returns nil only once the read agrees with the spec, because
returning nil writes the engine's last-applied cache and latches the resource as a
no-op until its spec changes.

What is read back differs by kind. For `controller` it is the **image tag on the
controller Deployment's pod template**, plus a completed rollout; `GET /api/v1/version`
is consulted only for whether the controller answers at all, because it reports the
binary's ldflags stamp (`0.260714.0-199ec1a`) and a spec declares an image tag
(`custom-28386c402c60`) — comparing those never converges. For `fleet-version` it is
the fleet version on `GET /fleet-version`, which the controller advances only when
every endpoint is at a roll's target.

The roll in flight comes from the same `GET /fleet-version` (its `roll`), and its
progress from that task's subtasks.

Three properties keep them from destabilising the rest of the tick:

- **Both rows are Preexisting**, so an undeclared one is reported and never deleted.
  The controller row is listed on every tick; the fleet row only once a declaration
  has reached this process (until then the engine plans a Create, which is the same
  call), so an environment that declares no fleet never reads the route.
- **A controller upgrade takes the controller down**, so neither row fails `List`: an
  unreachable controller, or one too old to serve `GET /fleet-version`, is a
  not-Ready row in state `upgrading`. A plain error aborts the whole tick, which would
  freeze reconciliation for exactly as long as the upgrade it is supposed to be
  driving.
- **A running roll reports its done/total as `Actual.Progress`**, and a change
  restarts both of the engine's stuck timers, so a long roll that keeps moving is not
  flagged and one that stops is. A roll whose task nothing has claimed reports
  `queued`, with no progress.

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
  with — below), an endpoint spec declares anything but `resources` (below), and
  the controller refuses some bodies outright with a `400`.
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
`memory`, `connectivity_mode` and `connectivity_allowed_cidrs`. A branch also
reports its `pg_settings` and `write_token_settings`, an endpoint its
`pg_settings`, `pageserver_settings`, `safekeeper_settings` and
`extended_config` — see [below](#settings-are-reported-whole). A branch and an
endpoint also report `applied_generation` — see below.
The connectivity pair is the observed counterpart of the create-time field
[endpoints cannot converge](#what-an-endpoint-spec-can-declare-and-not-get).
`connectivity_mode` is emitted by every agent at this release or later, and is
**resolved** — an endpoint predating the field reports none and behaves as
`cluster_ip` — so the inventory records the exposure that was *asked* for, which
the address alone cannot show. Against a private address that is conclusive for
`nlb_external` only; an `nlb_internal` LB is itself privately addressed on GKE/AKS.
`connectivity_allowed_cidrs` (comma-joined and sorted, as `extensions` is joined)
exists *only* under an `nlb_*` mode, where an empty value means **unrestricted**;
absent means there is no load balancer to restrict, so a console can warn about an
open `nlb_external` endpoint without misreading every `cluster_ip` one.

That last rule holds **only once `connectivity_mode` is present**. An older agent
emits neither key — the customer upgrades on their own schedule, so the two sides
are routinely skewed — and reading that absence as the default would report an
`nlb_external` endpoint with unrestricted CIDRs as having no load balancer to
restrict, silently dropping the warning for the one case the present-but-empty
encoding exists to surface. A missing `connectivity_mode` means *unknown*, never
`cluster_ip`. The wire contract is `ycp/internal/api/types.go`, which is the
authority ([`REVIEW.md`](../../../REVIEW.md) treats it as one).
A branch's `id`
pairs with a fork's `parent_branch_id` so a console can rebuild the fork tree
(the containment tree is already derivable from `ID`/`Parent`). The controller
exposes no live/head LSN, so the only LSN reported is the branch fork point.

## Settings are reported whole

Each settings surface is one attr holding a JSON **object**, keys sorted, not one
attr per setting.

Whole because that is the unit desired state replaces. [A declared surface owns the
map](#settings-are-replaced-not-merged), so anything editing settings must render
what is stored before it writes — and the inventory is the only place it can read
that. `applied_generation` cannot stand in: it says an update landed, not what it
landed on, so it cannot separate "converged on the map I sent" from "converged,
having dropped three keys I could not see". Flattening to `pg_settings.work_mem`
reads better in a table but loses the surface as a unit.

An empty surface and one never set are both `{}` — the controller's GET omits both,
so nothing on the wire tells them apart, and both say no key is stored. The skew
rule of `connectivity_mode` holds here too: an **absent** key means the agent
predates the release that added these, never that the surface is empty. Read as
empty, it is a blank editor for a resource full of settings and every one of them
deleted on save.

`extended_config` is reported although the settings PATCH cannot write it. It is
declarable at endpoint create, so a spec that disagrees is permanent drift, and this
is the only way to see what it disagrees with.

## The applied generation

Almost every mutating controller call is asynchronous, so `202` means *task
planned*, not *task done*. The engine is level-triggered on **existence** — a
failed create leaves the resource absent and a failed delete leaves it present, so
the next diff re-issues both — but an update's only staleness signal is the
generation. Holding that in the agent and writing it from the 202 meant a task that
failed afterwards left the resource unconverged with the agent reporting healthy,
permanently (meko#985).

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
- The two settings PATCHes are the exception, because they are **synchronous**:
  they return once the row is written, with no task to follow. (Not the API's only
  synchronous verbs — `createUser`, `createStoragePool` and the pool rename/delete
  plan no task either — but the only ones that carry a generation.) There the handler stamps, in the same statement as the settings —
  which satisfies the rule above rather than breaking it, since for a synchronous
  verb the handler *is* the work. A `400` returns before the write, so a refused
  request stores neither the settings nor the generation.
- `ampbackend.List` reads it into `Actual.ObservedGen` for those two kinds; pools
  and clusters have no update path and report 0.

**A 202 is reported as Pending, not applied**, and that is what makes the rest
safe. `Engine.cache` is still a floor for `effGen`, since a backend that cannot echo
has nothing else — but it is now only ever written for work that *completed*,
because an `ampbackend` verb that plans a task returns `reconcile.Pending`, which
records nothing.

**The generation rides the last call the agent issues for a resource in a tick** —
synchronous, so the handler stamps; asynchronous, so the worker does. An endpoint
declaring `resources` sends its settings PATCH first without one and lets the resize
carry it; a settings-only endpoint issues no resize at all and the PATCH carries it.
A branch works the same way with `add_branch_extensions` as its asynchronous tail.
The PATCH going first is what makes the ordering safe: it is synchronous, so by the
time a task stamps anything the settings have provably landed, and a retryable PATCH
failure returns before any task is planned.

A branch is the case that forces the rule. `add_branch_extensions` unions the branch
row **before** it redeploys the branch's endpoints, so a tick landing after the union
sees nothing missing. Treating that as converged would cache the generation and
no-op the branch forever — the same latch, reached from the other side. So when
nothing needs changing but the generation is still unrecorded, the agent sends an
**empty** settings PATCH carrying only the generation: every surface omitted means
"do not touch", and the controller skips notifying endpoints for a body that names
none, so it is a stamp and nothing else.

That replaced re-sending the whole extension set, which was the only branch call
that could carry a generation before this. The cost of the old shape was an
`add_branch_extensions` task on *every* generation bump of a branch declaring
extensions; and the redeploy pass it dragged along is no longer needed to re-drive
an endpoint a failed task never reached, because the task now redeploys exactly the
endpoints whose recorded set is behind the branch's.

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

That map is the **engine's** view of tasks, and deliberately narrow: it holds only the
task this agent planned for a resource it is converging. The console needs the whole
log — including tasks an operator or the amp UI planned directly — so a separate
`ampbackend.TaskCollector` reads `GET /tasks?updated_since=` as a change feed and the
agent relays the delta on its status heartbeat. The two do not share state: losing the
inflight map costs a verdict, while the collector's watermark is what guarantees
delivery, so folding them together would trade one's failure mode for the other's. See
[ac-tasks.md](ac-tasks.md) §4.1 for why the feed orders by `updated_at`.

Adding an update path keeps this by construction. Every `Backend.Update` arm that
plans a **task** hands its call to one `applyUpdate` helper that owns the task
lookup, the verdict and recording the new task, so an arm bypassing it loses the
whole mechanism rather than one detail — the synchronous PATCHes go through
`patchSettings` instead, which is the deliberate exception because they leave no
task to inherit any of it. `updateFuncs` is the only place the updatable set is written down;
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
update sends only the names the controller does not already report, because the add
**redeploys the endpoints that are behind** — the ImageVolume set is fixed at pod
start — so expect a compute bounce on the tick that applies one. Only endpoints
whose recorded mount set differs from the branch's are rolled: the task records what
each endpoint mounts after its deploy returns, so a replay redeploys exactly the
endpoints a failed run never reached, and a task whose set is already applied
everywhere deploys nothing. (Names only, so a catalog revision that moves an
extension's *image* without renaming it does not land through this path — the fleet
upgrade re-renders extensions for that.) The endpoint's
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

### Settings are replaced, not merged

A branch's `pg_settings` and `write_token_settings`, and an endpoint's
`pg_settings` / `pageserver_settings` / `safekeeper_settings`, converge through the
two settings PATCHes. **A declared surface owns the whole map**: what the spec says
the surface IS is what the row is set to, so a key stored there that the spec does
not name is removed. Omitting a surface leaves it unmanaged; declaring `{}` clears
it. That is the PATCH's own semantics, and the only reading consistent with desired
state being authoritative.

Create and update disagree about this, which is the part that bites: a create
**merges** over what a fork inherited, this PATCH **replaces**. A fork copies its
ancestor's `pg_settings`, and `createBranch`'s own settings merge over that set — so
a fork that inherited `work_mem` and whose spec declares only `shared_buffers` loses
`work_mem` on the first update tick, and a branch created *with* `pg_settings`
converges to replace semantics on its first generation bump.

`write_token_settings` is reachable *only* this way — nothing inherits it on a fork
and no create request carries it — so, like `extensions`, `Create` refuses a branch
spec that declares it rather than dropping the field, and it is declared in a later
write once the branch exists.

A setting the controller refuses is permanent drift, not a retry: the `400` stored
nothing, so every retry would send the same rejected body. Two independent checks
produce one — Postgres itself refusing a `pg_settings` name or value, and the
controller's generated key set refusing a daemon key — and the agent reports either
as the controller's own message. A `200` that lists settings in `not_applied`, or an
endpoint in `unreachable`, is **not** a convergence failure: the row is stored, so it
takes effect when that compute next starts.

One operational difference: a storage daemon's config has no reload path, so
changing `pageserver_settings` or `safekeeper_settings` restarts that daemon in the
pod, where most `pg_settings` reload in place.

### What an endpoint spec can declare and not get

An endpoint converges `resources` and its three settings surfaces. What is left
over — `connectivity`, `role`, `tags`, `extended_config` — is permanent drift on an
**existing** endpoint, named in the resource's `Error`. Omitting a field leaves it
unmanaged.

The reasons differ. `connectivity`, `role` and `tags` are create-time in the controller:
no update verb carries them, so they can never converge and recreating the endpoint
is the only path. `extended_config` is the odd one out — it is the single surface
`updateEndpointSettings` does *not* take, so it stays unfixable for a reason of its
own now that its three neighbours converge.

`disk_type` is a third case: the `Endpoint` response does not report it, so a
declared value cannot be told from the one the endpoint was created with and is
not checked. The agent keeps each endpoint row from `List` to make this
comparison, the way it keeps branch rows.

The resize is issued only when the spec declares `resources`. It used to go out on
every tick, which was harmless in itself — the handler keeps a dimension the request
does not name, so an empty body resized the endpoint to the size it already had —
but it was also the only thing that could stamp the generation, so a settings-only
spec planned a task purely to get one. The PATCH carries it now.

Reporting the rest matters more than the refusal reading well. A dropped field that
still stamped `ycp_generation` would have the engine Noop the endpoint forever,
converged on a spec that never applied — and for `connectivity` that means an
endpoint silently unreachable, or an `allowed_cidrs` narrowing that never took
effect.

## Environment `kind`

An environment is registered as **`amp`** or **`meko`**. The kind is fixed at
registration. The id is a bare `slug(name)` for either kind and encodes nothing —
the kind column is what distinguishes them, so a name is unique per account across
kinds. (Ids minted before this carry an `amp-` prefix and still resolve.)
Enrollment carries the kind and YCP rejects a mismatch (409), so an
agent configured for one kind cannot attach to the other's environment. An amp
environment seeds **no** desired state at registration — an operator declares its resources afterwards through
`POST /environments/{id}/desired`.

## Write-once binding

The controller records the single YCP environment it belongs to and refuses to be
rebound to a different one, so a second agent pointed at another control plane
cannot quietly take it over.

- **`POST /api/v1/ycp/binding`** `{ycp_url, environment_id}` — the reconcile-agent
  pins the controller after it enrolls. First write wins; an identical re-bind is
  idempotent (200); a bind that differs from the incumbent is rejected (409, body
  carries the incumbent). **`GET`** returns the current binding or 404.
- State lives in `ycp_binding` (migration 0005), a singleton row in the
  controller's own schema. The routes sit outside the OpenAPI spec, like
  `/healthz` and `/metrics` — the binding is operational state, not a resource.

Request-time enforcement (gating mutating calls on the binding) is deferred; today
the binding is a durable record and a bind-time tripwire.

## History: the retired outbound loop

The controller used to enroll and report to YCP itself, on a background goroutine
(`ycp.Loop`) with its identity persisted in a `ycp_identity` table. That
was the "register + report" half of a design that always intended reconciliation
to follow. When the standalone agent landed — which must be the *sole* YCP client
(one API key drives one environment) — the controller loop was retired
(its table, the `--ycp-*` flags and the `controller.ycp.*` Helm values are
gone). The binding endpoint is all that remains on the controller.

## Deploying YCP itself

The control plane is `api_server --control-plane-mode`. Its image is built and
pushed on demand (Quay: `quay.io/yugabyte/ycp-api-server`). The reconcile-agent
image is `quay.io/yugabyte/ycp-reconciler-agent` (`ycp/Dockerfile.reconcile-agent`).

## Testing

The controller-side unit coverage is the binding store + endpoint
(`internal/ycp/binding.go`, `internal/api/ycp_binding_test.go`) and, on the
control-plane side, the kind-independent id, enroll kind-mismatch, and desired /
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
- Retiring the standalone meko stack agent. The same agent now serves
  `ENV_KIND=meko` through `ycp/reconcile/mekobackend`, but only for the `datapack`
  kind — the meko stack itself is still `ycp/cmd/agent`'s, and `knowledge-base` and
  `agent` wait on their `api_server` routes moving onto the PAT-accepting group.
