# BYOC upgrades

**Status:** Draft design — for review. 2026-09-14.

Aeon upgrades the software in many BYOC amp deployments, driven by Yugabyte SREs
rather than by customers. First the amp controller and its endpoints; the shape is
generic so the foundation, the reconciler and the meko stack slot in later. Facts
are what the code says today; `Q:` is open; `→` is a take, offered to be argued
with.

Related: amp [`ac-endpoint-upgrade.md`](../../amp/controller/architecture/ac-endpoint-upgrade.md)
([meko#809](https://github.com/yugabyte/meko/issues/809) — the roll itself),
[`ac-ycp.md`](../../amp/controller/architecture/ac-ycp.md) (the reconciler),
[meko#455](https://github.com/yugabyte/meko/issues/455) (policy / windows / canary),
[meko#1139](https://github.com/yugabyte/meko/issues/1139) (fleet upgrades from
YCP — this supersedes its "operations channel" draft),
[meko#1140](https://github.com/yugabyte/meko/issues/1140) (rollback),
[meko#725](https://github.com/yugabyte/meko/issues/725) /
[PR #1212](https://github.com/yugabyte/meko/pull/1212) (foundation on the YBA
operator), [meko#729](https://github.com/yugabyte/meko/issues/729) (controller
mutates foundation CRs),
[meko#1007](https://github.com/yugabyte/meko/issues/1007) (no way to clear a
wedged task).

## 0. Pre-requisite: an SRE path into YCP

Cloud-admin must be able to call YCP and trigger an upgrade. Today YCP has **one
operator tier**: every fleet route is gated by the shared `X-Meko-Admin-Secret`, and
Aeon's apiserver forwards customer calls with that same secret plus
`X-Meko-Account-ID` (`YcpApiControllerImp`, `YcpClientImpl`). YCP cannot tell an SRE
from a customer. `api_server` also has an `/api/v1/admin/*` group — same secret, the
backend `meko_admin` proxies to — with nothing about fleets in it.

→ Pre-req, owned on the Aeon side: cloud-admin → apiserver → YCP, with SRE-only
authorization enforced in apiserver (Teleport roles exist there), and YCP's
upgrade-writing routes kept off the customer pass-through. Reuse YCP's existing
admin-secret `/admin` tier as the home for those routes, so the customer-facing fleet
group never gains a write that moves software.

The YCP-down workflow (§3–§7) does not depend on this and is built first; until the
pre-req lands it is driven from the YCP console or API directly.

Q: does cloud-admin call YCP through apiserver (one more pass-through, SRE-gated), or
does YCP grow a second secret for the admin tier? Aeon's call; YCP only needs the
routes to be separable.

## 1. Scope

**In:** two desired-state kinds (`controller`, `fleet-version`), the controller API
changes they need, the reconciler changes, and rollback for both.

**Out, sketched for genericity only (§9):** `foundation`, `meko-stack`, and a
separate `reconciler` kind should it ever leave the controller chart. **Carried as discussion (§10):** maintenance windows,
waves across environments, rollback triggers, shipping and bundles.

## 2. What exists today

**YCP** holds desired state (`environment_desired_resources`: `{kind, name, spec,
generation}`) and the inventory the agent last reported. It has no scheduler, no
task system, no imperative record — lifecycle actions just write
`environments.state`. Adding a kind needs no protocol change.

**The reconciler** (`ycp/reconcile` + `ycp/reconcile/ampbackend`) is a
level-triggered generation diff over a `Backend` per kind: creates parent-first and
gated on the parent being Ready, `Pending` / `Updating` / `Failed` / `Stuck`
(`StuckAfter` 10m) per resource, and an **applied-generation echo** — the controller
stamps `ycp_generation` on a row when a task completes, so a `202` is never mistaken
for done. It drives the controller's REST API only; it is a component of the
`yb-amp-controller` Helm chart (`agent.*` values).

**The controller** has the fleet roll: `POST /api/v1/endpoint-upgrades {version,
endpoint_ids?, max_parallel}` → one parent task, one subtask per endpoint, bounded
pool, fail-fast, one roll at a time (`409`), per-endpoint result lines,
`GET /tasks/{id}/subtasks`. Endpoints record `version` / `target_version`;
`Endpoint.version` is on the read API. A **new** endpoint runs the controller's
static `--endpoint-image-tag` unless `create_endpoint` args carry a version.
Migrations run on every startup; the controller is deletable and recreatable.
`GET /api/v1/version` reports the running build. The bundle also carries the
storage controller endpoints register against.

**Delivery:** images on Quay (`amp-{controller,endpoint,pg}`,
`ycp-reconciler-agent`); **no chart is published** — the install hint runs `helm`
from a repo checkout.

## 3. Principle

YCP converges state; it does not run workflows. So an upgrade is **desired state**,
the reconciler converges it through the engine it already has, and the **controller
owns the upgrade logic** — enumeration, fail-fast, ordering, rollback policy — where
the endpoints and the task framework are. This replaces the "operations channel" in
the [meko#1139](https://github.com/yugabyte/meko/issues/1139) draft: no new protocol, two new kinds.

**One version per environment.** The endpoint version is not chosen anywhere: it is
**pinned to the controller release** — `--endpoint-image-tag` (or the tag of
`--deploy-endpoint-image`) is already what an endpoint created by that controller
runs. So the only thing that moves an environment's endpoint version is upgrading
its controller, and a fleet upgrade is two steps in a fixed order: move the
controller, then roll the endpoints onto its pin.

That is what makes the two kinds below small. An earlier draft of this document had
the fleet carry its own version, which needed a persisted fleet default on the
controller (an API, a singleton table, a migration) purely to answer "what version
should a NEW endpoint be?" — a question the controller release already answers — and
it allowed a fleet default of Y under a controller pinned at X, with nothing
preventing it. The cost of the pin is that endpoint versions cannot move
independently of the controller. That is the trade, taken deliberately.

## 4. Kinds

Both are singletons per environment; `name` is fixed (`controller`, `fleet`) so the
engine's `(kind, name)` key is stable and a second one is unambiguous drift. The name
carries no information because the singletons already exist: one reconciler bound
write-once to one controller (`ycp_binding`), one controller per foundation, one
fleet per controller (`/endpoint-upgrades` is top-level; `fleet:endpoints` is one
lock). If per-cluster pinning ([meko#455](https://github.com/yugabyte/meko/issues/455)) arrives, a scoped `fleet-version` is named by
path like the existing kinds — `<pool>/<cluster>` beside the environment-wide
`fleet` — with a scope on `/fleet/version` and the roll's enumeration.

### 4.1 `controller`

```json
{"kind": "controller", "name": "controller",
 "spec": {"version": "0.260901.0-abc1234", "chart_version": "0.260901.0",
          "values": {}}}
```

- `version` is the controller image tag — a release of `software_type`
  `amp-controller`. `chart_version` moves with it; `values` are chart overrides the
  environment carries (registry, storage class), passed through verbatim.
- **Actual** (`List`): `GET /api/v1/version` → running version; Ready when the
  Deployment rollout is complete and the reported version equals `spec.version`.
- **Update**: `helm upgrade` of the controller release to `chart_version`, setting
  the per-image tags of the workloads the release runs — `controller.image.tag`,
  `agent.image.tag`, **and `controller.endpoint.image.tag`** — and **not** the
  chart-global `imageTag`, which is only the fallback for an image with no tag of
  its own.

  The endpoint tag is in that list and is not optional: `_helpers.tpl` resolves it
  from `controller.endpoint.image.tag` first, so an upgrade that sets only the
  other two leaves `AMP_ENDPOINT_IMAGE_TAG` where it was. The new controller's pin
  would then equal the old one and the roll in §4.2 would converge a fleet that is
  already converged — the upgrade would appear to succeed and move nothing.
  Setting it to the same tag as the controller image is also what makes the pin
  *predictable*: it is `spec.version`, which is what lets §4.2 compute Ready
  without a read route for it. **Without `--wait`**. The reconciler is a component of that
  chart, so the rollout replaces the pod running the upgrade; a client killed
  mid-`--wait` leaves the release `pending-upgrade`, which blocks every later
  upgrade. Helm returns once the manifests are submitted, the rollout moves both
  pods, and the *new* reconciler decides on its next tick from `List`. Before
  re-running helm the backend compares the release's deployed values to the spec,
  so a tick that lands mid-rollout does not bump the revision again.
- **Rollback** is `spec.version` set back. Migrations are up-only at startup, so a
  downgrade past a schema change is not safe: a release's `metadata` carries its
  migration floor, and the reconciler reports a downgrade below it as `Permanent`
  drift instead of running it.

Q: what does a controller rollout do to running endpoints? The bundle carries the
storage controller endpoints register against, so its pod restarting is a blip for
every endpoint. Measure before deciding whether the Deployment needs
`maxUnavailable: 0`.

### 4.2 `fleet-version`

```json
{"kind": "fleet-version", "name": "fleet", "spec": {}}
```

**The spec is empty, and that is the whole point.** There is no version to name —
the target is the controller's pin (§3) — and no wave plan, because staging is a
later PR (§7). Declaring this kind means "the endpoints of this environment should
match the controller", which is a perfectly good level-triggered declaration: it
converges continuously, so an endpoint left behind by a failed roll is picked up on
a later tick rather than needing a human to re-issue anything.

- **Parent** is `controller`. The engine then orders a controller upgrade before the
  roll and gates the roll on the controller being Ready at its version — the
  top-down ordering for free, and the reason the two kinds are worth having
  separately.
- **Actual**: every live endpoint's `version`, read through the existing endpoint
  routes, plus the roll read from the task routes
  (`GET /api/v1/tasks?kind=upgrade_endpoints`). Converged when no roll is in flight
  and every live endpoint's `version` equals the pin.

  The pin itself is on **no read route** — it is `Provisioning.EndpointVersion`,
  and nothing under `internal/api` puts it on the wire; `GET /api/v1/version`
  reports the controller image's version, which is a different tag. So the
  reconciler does not read the pin, it *knows* it: the controller upgrade above
  sets the endpoint tag to `spec.version`, so the value to compare against is the
  version this environment already declares. That holds only for as long as the
  upgrade sets all three tags together — if that ever diverges, this kind needs the
  pin exposed on a read route, and §5 would have to add it. Everything else here
  is directly observable, so the kind needs **no applied-generation echo**: `upgrade_endpoint`
  writes an endpoint's `version` only after the deploy reports back, and the
  in-flight task id comes from the task log, not from the reconciler's memory.
  (Branches needed the echo because their observable lies — the extension union
  lands before any redeploy.)
- **Update**: `POST /api/v1/endpoint-upgrades` with no body; `202` → `Pending`,
  tracked through `applyUpdate` against the roll task. Re-issuing is safe without
  any adopt machinery on the controller: a roll already in flight answers `409`,
  which the reconciler reads as "in flight", and a roll issued after one has
  finished enumerates nothing because every endpoint is already at the pin.
- **Cancel** is withdrawing the resource: the reconciler cancels the roll in flight
  (`POST /tasks/{taskId}/cancel`, §5) and stops managing the fleet. Children already
  deploying finish, as under fail-fast.
- **Rollback** is not expressed here at all. Moving the fleet back means moving the
  **controller** back (§4.1), which changes the pin; the next roll then walks the
  fleet to it. Bounded by the controller's migration floor and, across a PG major,
  by `Policy` ([meko#1140](https://github.com/yugabyte/meko/issues/1140)).

Inventory rows for both kinds carry `version`, and for the fleet the roll's
`done/total` and state, so the console and Aeon see progress without a task API of
their own. Endpoint rows gain `version` (from `Endpoint.version`).

> **Open: is this a kind at all?** With an empty spec, "the endpoints should match
> the controller" could just as well be part of what `controller` converging means
> — one kind whose Ready is "the controller is at X and every endpoint is at X's
> pin", which would delete the parent/child ordering between them. Kept separate for
> now so a roll can be declared, cancelled and observed on its own, and because
> per-cluster pinning ([meko#455](https://github.com/yugabyte/meko/issues/455)) would
> give it a spec again. The name is already wrong, though — it carries no version;
> `endpoint-roll` fits. Renaming is cheap now and expensive once anything has
> declared it.

## 5. Controller API changes

| change | why |
|---|---|
| `POST /endpoint-upgrades` loses `version` — the body becomes optional, carrying at most `endpoint_ids` and `max_parallel` | the target is the controller's pin, so there is nothing for a caller to name, and no way to ask for an inconsistent fleet |
| `POST /tasks/{taskId}/cancel` — the roll parent re-reads the flag between children, so a roll stops without finishing the fleet; running children finish, the task ends terminal with a cancel reason, its locks release | generic ([meko#1007](https://github.com/yugabyte/meko/issues/1007)), and the roll is its first consumer: cancel and rollback both need the roll terminal |
| `plan_endpoint_upgrades` orders its work spread across clusters, read-only before read-write | the bound is `max_parallel`, and without ordering those are all one customer's endpoints |

What is **not** here, and was in an earlier draft: a persisted fleet default
(`GET /fleet`, `PUT /fleet/version`) and its migration, wave and approval arguments
on the roll, and adopt-in-flight so a re-issued roll could edit a running one. The
pin removes the first and §7 defers the rest. No `ycp_generation` on the roll, and
no stamp: the fleet converges on observation (§4.2), unlike the branch and endpoint
updates that carry one.

Fail-fast: a failing child halts the roll and the task fails; nothing rolls back on
its own. A cancelled roll ends the same way, minus the failed child — the parent's
result names what was upgraded and what was never attempted. The parent holds
`fleet:endpoints` and its task lease for the whole roll, which is what "one roll at
a time" means.

## 6. Reconciler changes

- `ampbackend`: `controller` and `fleet-version` in `allKinds` / `updateFuncs`;
  `Parent(fleet-version) = controller`, claimed only where a controller is declared.
  Both singletons exist on the controller whether or not anyone declared them, and
  anything listed that desired state does not declare is deleted every tick — which
  for the fleet means cancelling the roll — so the engine hands the backend each
  tick's desired set and `List` emits a singleton only for a kind the environment
  manages. The `Update` arms go through `applyUpdate`
  so the task lookup and the verdict are not re-implemented. The table-driven test
  that holds every kind to "`Permanent` on `Update`, or echoes a generation" gains a
  third category — **observable** — for these two, each with the observation that
  stands in for the echo.
- **Helm.** Settled: the agent execs the `helm` binary, which the agent image
  will have to carry. Not the Go SDK — `helm.sh/helm/v3` would pull client-go and apimachinery
  into a module that deliberately has neither — and not render-and-server-side-apply
  as `ycp/cmd/agent` does for the meko stack, because the controller is a Helm
  release with hooks and its own endpoint deploys already shell out to helm. The
  release name has to be configuration: the chart does not fix it. The agent's RBAC
  has to cover the release's objects plus `escalate`/`bind` on its Roles and
  RoleBindings, which is what k8s requires of anything that rewrites RBAC it does not
  already hold — and which makes the agent at least as privileged as the
  controller. That is the real cost of this design and it is worth stating plainly
  wherever the chart is documented.
- **The chart is resolved at runtime, not baked.** The agent image's build context
  is `ycp/`, so it cannot copy `amp/deploy/helm` the way the controller image copies
  its endpoint chart. The chart reference is a chart value instead, defaulting to the
  OCI ref in §10 and taking a local path for a cluster test; publishing that chart is
  still open.
- **The reconciler stays in the controller chart.** Its image tag already follows
  the chart's `imageTag`, so a `controller` upgrade upgrades the reconciler with it
  and there is no separate kind to build. The cost is the self-replacement above,
  and that a bad reconciler image ships with a controller release. Guarding that —
  a readiness probe gating the agent Deployment's rollout so a broken image never
  displaces a working pod — is worth doing and is its own change, not part of the
  upgrade path. One day the two may split (independent cadence, a reconciler fix
  without a controller roll); not now.
- **Long-running updates.** A roll in flight is not stuck. The engine does not time
  a resource whose state says it is progressing by design, and a roll over a large
  fleet takes as long as it takes. A live roll reads as `rolling`, which is exempt;
  a roll whose task no worker has claimed reports `queued` instead — the case the
  exemption must not swallow.
- **Charts have to come from somewhere the reconciler can pull**: publish the
  controller and reconciler charts as OCI to Quay (§10).

## 7. Waves, and where they would live

Nothing above is staged: a roll moves the whole fleet at `max_parallel`, ordered to
spread the damage but with no percentage stages, no soak and no approval gate. That
is a deliberate first slice, not a judgement that staging is unnecessary — a BYOC
SRE moving software inside a customer's cluster has good reason to stop half way and
look. When it lands, two shapes are open:

| | controller-owned waves | YCP-computed waves |
|---|---|---|
| controller changes | wave plan on the roll, somewhere to record where it is holding, an approval route | none |
| where wave logic lives | controller | cloud-admin (above YCP) |
| gate | an approval call, or a field in the spec | writing the next set |
| ticks per wave | one write, controller runs it | one write per wave plus a read of inventory |
| spec size | fixed | grows with the fleet; ids can go stale between read and write |

Controller-owned matches §3 and keeps the spec fixed-size. YCP-computed needs
**nothing new on the controller at all** — `endpoint_ids` is already in the request,
and an endpoint named there is rolled even if it is already at the pin — so it is
available today for anyone who wants to stage a roll by hand.

## 8. Rollback

- **Endpoints**: not expressed on the fleet at all. Roll back the **controller**,
  which moves the pin, and the next roll walks the fleet onto it — refused by
  `Policy` across a PG major ([meko#1140](https://github.com/yugabyte/meko/issues/1140)). No finalize step — a pod
  swap. PG-major rollback is the open discussion: forward-fix only, or a fork at the
  pre-upgrade `fork_lsn` with endpoints pointed back at the parent branch.
- **Controller**: `controller.spec.version` set back, bounded by the migration
  floor (§4.1).
- **Foundation** (later): YBDB's two-phase upgrade → rollback *or* finalize —
  Aeon's `rollbackLatestUpgrade` / `finalizeUpgrade` model. The operator's
  `YBUniverse` CRD carries `ybSoftwareVersion` and nothing for finalize or rollback
  (checked in the charts CRD bundle at 2025.2.5), so that step needs the YBA API or
  a newer operator.

## 9. Later kinds

The same shape — a singleton kind, `spec.version`, actual read back, converge:

- `reconciler` — only if it ever leaves the controller chart (§6). Until then it
  moves with `controller`, and the escape hatch when a release breaks it is a human
  with `helm`.
- `foundation` — `chartVersion` + `ybSoftwareVersion` on the spec [meko#1212](https://github.com/yugabyte/meko/pull/1212) applies;
  the controller mutating CRs ([meko#729](https://github.com/yugabyte/meko/issues/729)) is the same move from inside.
- `meko-stack` — already a kind: rewrite the images, `ycp/cmd/agent` converges.
  Q: does a meko data plane share amp's foundation universe in a customer cluster?

## 10. Discussion carried over

**Aeon already has the SRE machinery** (`~/code/yugabyte-cloud`): `DSoftwareReleaseTrack`
↔ accounts (per-org `BYOC <orgId>` tracks), `DSoftwareRelease`,
`DScheduledUpgradeExecution` (bulk by tier/cloud/region, `availableTokens`
throttle) → `DClusterUpgradeItem` (rolling, `rollMaxBatchSize`, precheck),
`DMaintenanceSchedule` (cron + duration, exclusions, capped deferral), and
cloud-admin (Teleport roles) exposing all of it. Aeon's unit is a *cluster*; BYOC's
is an *environment*.

- **Waves across environments** = a scheduled execution over a subset; canary = a
  track that internal environments sit on. Within an environment = §4.2.
- **Maintenance windows**: reuse `DMaintenanceSchedule`, scoped per environment or
  per account — a future item; nothing below YCP knows about windows.
- **Rollback trigger**: operator-initiated first. Automatic needs a defined signal
  (per-wave metrics carry `environment_id`, [meko#965](https://github.com/yugabyte/meko/pull/965)) and a defined blast radius; a
  wave gate and a rollback trigger are the same measurement.
- **Shipping**: publish charts as OCI to Quay (same registry and pull secret as the
  images); a per-release manifest by digest. Hauler covers collect / transfer /
  serve for air-gapped customers and is one consumer of that manifest; it does not
  cover compatibility or orchestration.
- **Bundle vs per component**: the *policy* unit is a stack bundle (one Aeon
  release in a track → the manifest); the *mechanism* is the kinds above, each
  no-op when already at its version. The releases registry ([meko#1139](https://github.com/yugabyte/meko/issues/1139)) keys on
  `(software_type, version)` with opaque `metadata`; `software_type` gains
  `amp-controller`, later `amp-stack`.

## Open questions

1. §0 — cloud-admin reaches YCP through apiserver or a second YCP secret.
2. §4.1 — what a controller rollout does to running endpoints.
3. §4.2 — does the fleet deserve to be its own kind with an empty spec, or does it
   fold into `controller`? And if it stays, rename it `endpoint-roll`.
4. §6 — per-kind `StuckAfter`. (helm vs server-side-apply is settled: exec helm.)
5. §7 — when waves land, controller-owned or YCP-computed.
6. §8 — PG-major rollback: forward-fix only, or fork-based.
7. §9 — meko on amp's foundation or its own.
