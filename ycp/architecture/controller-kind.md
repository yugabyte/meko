# The `controller` kind — upgrading the amp controller from desired state

**Status:** Draft
**Last updated:** 2026-09-18

An operator declares the version the amp controller release should run; the agent
`helm upgrade`s it. Moving a BYOC controller stops being a human with a repo
checkout and `kubectl`.

This documents the reconciler side — what was added to `ycp/` and why each piece
is there. The plan it sits under is
[`byoc_upgrades.md`](byoc_upgrades.md); the kind's place among the others is
[`amp/controller/architecture/ac-ycp.md`](../../amp/controller/architecture/ac-ycp.md).

## The shape

```json
{"kind": "controller", "name": "controller",
 "spec": {"version": "0.260901.0-abc1234", "chart_version": "0.260901.0",
          "values": {}}}
```

A singleton: one reconciler is bound write-once to one
controller, so a second name would be two declarations driving one helm release.

It converges on **helm and the Kubernetes API**, not on the amp REST API. That is
the fact everything below follows from — the thing being upgraded is the thing the
rest of the backend talks to.

## What it needs that no other kind did

### 1. A helm client, exec'd

`ycp/cmd/reconcile-agent/helm.go`. One command, stdout back, a non-zero exit as an
error carrying stderr.

`exec` rather than `helm.sh/helm/v3` because linking the SDK pulls client-go and
apimachinery into a module that deliberately has neither. It also gets
authentication for free: in-cluster helm reads the pod's ServiceAccount token file
itself, on each invocation, so unlike the Go client in `k8sstate.go` it is immune
to rotation.

`Runner` is an interface so the arm is testable without a cluster; the unit
tests drive it through a fake, and `amp/deploy/ycp-controller-upgrade-test.sh`
drives the real thing on a cluster against a published chart.

### 2. Two reads of Kubernetes

`ycp/cmd/reconcile-agent/k8sstate.go`.

**`rolledOut`** answers "is this Deployment fully on its current pod template" —
the question `kubectl rollout status` answers, and the one that says an upgrade has
landed rather than merely been submitted. Gated on `observedGeneration`, without
which a read landing between the helm upgrade and the Deployment controller
noticing reports the old, settled ReplicaSet as complete.

It also returns the image tag the pod template carries, which is what a declared
version is compared against. **Not** the version the controller reports about
itself: `/version` answers with the binary's ldflags stamp
(`0.260714.0-199ec1a`) and a spec declares an image tag
(`custom-28386c402c60`). Comparing those two never converges — the row would never
go Ready, and anything gated behind it as its parent would defer forever.
That was found by deploying to a cluster, not by the unit tests, whose fixture had
set both values equal and so encoded the same assumption as the code.

**`releaseName`** discovers the helm release the agent is a member of, from the
`meta.helm.sh/release-name` annotation helm stamps on every object it manages.
Discovered rather than configured because the chart does not fix the release name,
and a wrong guess fails in the one place it matters: `helm upgrade <wrong-name>`
answers `release: not found`, once per tick, for as long as the environment
declares a controller. Every member of a release carries the same annotation, so
any Deployment answers.

**Per-request ServiceAccount token.** The agent used to read its token once at
startup. That never bit while the only Kubernetes calls happened seconds after
boot; this kind polls a Deployment every tick for the life of the pod, and the
kubelet rotates projected tokens on disk and expires the old one. The token is now
re-read per request, keeping the last good one if the read fails — a token with
life left in it beats no `Authorization` header.

### 3. `PartialError` — the one change to the shared engine

`ycp/reconcile/engine.go`, `loop.go`. About forty lines, and the piece most worth
understanding, because it exists for exactly one row.

**The problem.** `List` is the whole-environment actual-state read, and every kind
but this one is read through the amp REST API. A controller upgrade restarts the
controller, so that API is down at precisely the moment this kind has work. A plain
`List` error returns from `Reconcile` before anything converges — which means the
controller kind never runs, and a bad version the agent itself deployed can never
be rolled back through desired state. Recovery would need a human with helm on the
cluster, which is the failure this kind exists to remove.

**Why not just return the rows with no error.** This is the real reason there is a
type. The engine derives creates *and* deletes from the difference between desired
and actual: in desired but not actual → Create, in actual but not desired →
Delete. A truncated list that claims to be complete tells the engine every endpoint
in the environment is gone. It would issue a Create for each — duplicating whatever
the unreachable controller actually holds — and a Delete for anything withdrawn.
Absence has to stay distinguishable from unread.

**Why not an ordinary error alongside the rows.** Because the engine's contract,
and Go's, is that a non-nil error means the values are unusable. Honouring rows
beside an error would change that for every backend, including the four places
`listTree` returns a plain error mid-traversal, where it genuinely means stop. The
wrapper makes it an opt-in: only a backend that says `reconcile.Partial(err)` has
its rows read.

**What the engine does with it**, three lines, each the conservative reading:

| action | on a partial list | why |
|---|---|---|
| Create | deferred, not issued | missing may mean unread |
| Delete | skipped | same reading, destructive direction |
| Update | still issued | an update targets a row that *was* read |

That last line is the one that lets the controller row converge while everything
else is dark. `Result.Partial` also reaches `Report`, so YCP shows the environment
degraded rather than quietly converging a slice of it.

In practice it is tighter than it reads: on a partial list the only row in `actual`
is the controller's, so every other resource lands in Create → deferred anyway.

## The kind itself

`ycp/reconcile/ampbackend/controller.go`, plus the wiring in `backend.go`.

**`List`.** The controller row is read first and kept even when the rest fails
(above). It is emitted on every successful `List`, healthy or not, and whether or
not desired state declares a controller — because anything gated on the controller
being Ready reads the actual map and never desired, so a child whose parent is
missing here is deferred forever and silently.

Reporting a resource nobody declared is safe because the row says what it is:
`Actual.Preexisting`, which tells the engine never to plan a Delete for it. That is
a property of the KIND — the agent runs inside the release it converges, so the
controller is on the remote whether or not YCP ever asked — rather than of any one
environment's declaration, which is why it rides on the row instead of being
something the backend has to be told. Without it the ordinary GC rule would collect
it, this kind's `Delete` would refuse as it must, and the refusal would latch a
permanent drift row on an environment where nothing is wrong.

Ready is three facts together: the pod template carries the declared tag, the
rollout is complete, and the controller answers. An unreachable controller is
`Ready=false` in state `upgrading`, which is also the truth.

**`Update`.** Deployed values are compared *before* helm runs, or a tick landing
mid-rollout bumps the revision again and the rollout never settles. Then `helm
upgrade`, **without `--wait`**: the agent is a member of the release it upgrades,
so its own rollout kills the helm client mid-wait and leaves the release
`pending-upgrade`, blocking every later upgrade. Helm returns once the manifests
are submitted; the next tick — quite possibly in the replacement pod — decides from
what it can observe.

It does not go through `applyUpdate`: helm is not a controller task, so there is no
task id for the next tick to read. The verdict comes from the next tick's
observation instead, which is what `observableKinds` means.

**`Delete`** is `Permanent`. The controller is not deleted through desired state —
it is the release the agent runs in.

**Ordering is what the kind is for.** Nothing else in this PR hangs off the
controller row yet, but the row exists so that something can: a declared upgrade
runs first, and whatever converges the endpoints waits until the controller is
Ready at its version. That gating is the engine's generic parent mechanism, so
the consumer supplies only the `Parent` link.

## What it costs on the cluster

The agent's Role gains write access to every object kind in the release, plus
`escalate` and `bind` on its Roles and RoleBindings — which is what Kubernetes
requires of anything that rewrites RBAC it does not already hold, and the
alternative (mirroring every verb the controller's Role grants) would hand the
agent direct power over pods, PVCs and Jobs it has no reason to touch. A
cluster-scoped pair is added when the release actually contains a cluster-scoped
object, which a stock install does through `rbac.nodeRead`.

**So an agent ends up at least as privileged as the controller.** The two do not
share a ServiceAccount, so this widens the agent alone, but it is a real privilege
increase and belongs in any review of this chart.

## What is not covered

**The chart is private, pulled with the customer's pull secret.** The default
reference `oci://quay.io/yugabyte/charts/yb-amp-controller` is published on every
`main` commit touching the chart (`amp/deploy/helm/publish-controller-chart.sh`),
as a next-patch prerelease `<major>.<minor>.<patch+1>-<timestamp>.g<sha>` and, once
per bump, `<version>`. The chart mounts
`agent.chartPullSecret` (default `imagePullSecrets[0]`) as helm's registry config,
so the robot behind that secret needs read on the chart repo as well as the images.

**A packaged chart reference cannot carry `spec.chart_version`.** helm silently
IGNORES `--version` against a `.tgz` rather than refusing it, so a declaration
that disagrees with the archive would upgrade, deploy the archive's own version,
see the mismatch again and upgrade forever. The backend refuses that combination
as `Permanent`; a chart repository reference honours `chart_version` normally.

**What has run.** `amp/deploy/ycp-controller-upgrade-test.sh` drives a real
upgrade on itest-dev entirely through YCP: it declares a fleet plus the controller
at the version already deployed (asserting no upgrade is issued), moves the
declaration, and asserts the release advances exactly one helm revision, the
controller and agent pod templates carry the new tag, the endpoint pin moves with
them, the endpoints running before the upgrade are the same Deployments five
minutes after it, and endpoints created afterwards come up on the new pin.

**Version skew across the release.** `controllerImageTagKeys` moves
`controller.image.tag`, `agent.image.tag` and `controller.endpoint.image.tag` — the
last because the endpoint pin is what a controller upgrade is FOR — and
deliberately not the MCP server or the UI, which run from the same stream: moving
them would make a controller upgrade a fleet-wide image bump by side effect. The
cost is a release version-skewed across those two pods, which nothing here
reports — `valuesDiffer` checks only the keys above, and readiness reads the
controller Deployment's rollout alone.

**A bad agent image ships with a controller release.** The agent is a component of
the chart it upgrades, so a release that breaks the agent replaces the working one.
Guarding that — a readiness probe gating the agent Deployment's rollout, so an
agent that cannot reach YCP never displaces a working pod — is worth doing and is
its own change, not part of this path.
