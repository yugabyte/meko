# Runbook: amp controller operations

Operational debugging for the **amp controller** (control plane) and the endpoints it deploys on
Kubernetes. Use this when a controller is deployed but a *mutation task* (create/delete cluster,
branch, endpoint) fails, or an endpoint pod won't come up. This is the **top-down flow**: start at
the task's recorded error and walk down through Helm → pod → the endpoint supervisor → the
individual binaries. For the deep, per-component config/serde errors you land on at the bottom of
that flow, hand off to [`startup-failures.md`](startup-failures.md); for how the controller deploys
endpoints at all, see [`../controller/architecture/ac-k8s-deploy.md`](../controller/architecture/ac-k8s-deploy.md)
and the task model in [`../controller/architecture/ac-tasks.md`](../controller/architecture/ac-tasks.md).

To **validate** (rather than debug) a deployment, run
[`deploy/smoke-test.sh`](../deploy/smoke-test.sh) — it walks the full async-task lifecycle and writes
+ reads real data through a created endpoint, exiting non-zero on any failure. When it fails, this
runbook is where you find out why.

## Conventions

```bash
export NS=amp-test                                  # the controller's namespace
# Reach the controller API (NodePort, or port-forward from anywhere with kubectl):
kubectl -n "$NS" port-forward svc/amp-controller 8080:8080 &
export BASE=http://127.0.0.1:8080/api/v1
export AWS_PROFILE=yugadev                           # for ECR (aws sso login first if expired)
```

Since #113, **every mutating API runs as a write-ahead task**: the handler returns `202 + a Task`
(with a `Location: /api/v1/tasks/{id}` header) and the worker does the work. That changes debugging:
**a mutation that "failed" is a `task` row whose `status=failed` and whose `error` is the cause** —
not an HTTP 5xx in your client. Always start there.

---

## Endpoint create/deploy task fails — the debugging flow

This is the worked example behind the "supervisor ↔ binary skew" section below, generalized into a
repeatable process. The specific bug matters less than the **order of operations**: each step narrows
the failure by one layer and tells you whether to go deeper or stop.

### 1. Read the task's recorded error first — don't guess

The task framework captures the failure verbatim. Get it before touching `kubectl`:

```bash
# the most recent failed create_endpoint task and its error:
curl -s "$BASE/tasks?limit=50&kind=create_endpoint" | jq -r '.tasks[] | select(.status=="failed") | .error' | tail -1
```

A deploy failure looks like:
`deploy endpoint: helm upgrade --install yb-ep-<id> … --wait --timeout 10m0s: exit status 1: …`.
That tells you the failure is in the **Helm deploy step**, almost always because the endpoint pod
never became `Ready` (so `helm --wait` gave up). It does **not** tell you *why* the pod is unhealthy —
that's the next layers. (A `helm … --wait` that fails fast with `Error: services "…" not found` is the
same root cause: the release is rolling back because the pod isn't coming up.)

### 2. Classify the pod state — crash-loop vs. not-ready vs. pending

```bash
kubectl -n "$NS" get pods | grep yb-ep
kubectl -n "$NS" get events --sort-by=.lastTimestamp | grep -iE 'yb-ep|back-off|unhealthy|failed'
```

Three failure modes, three different next moves:

| Pod state | What it means | Where to look |
|---|---|---|
| `CrashLoopBackOff` / `restartCount` climbing | a process inside the container exits non-zero on start | container logs (step 3) |
| `Running` but `READY 0/1`, `restartCount=0` | container stays up but the readiness probe never passes | the service that backs the readiness probe; SC registration |
| `ContainerCreating` / `Pending` | image pull, scheduling, PVC binding — including `Insufficient ephemeral-storage`, which means the pool cannot back this endpoint's 20 GiB/vCPU data volume (a 32-vCPU endpoint asks for 640 GiB; size the node pool's local SSDs to match) | events; `describe pod` |

Confirm with the container's terminated state:
```bash
POD=$(kubectl -n "$NS" get pods -o name | grep yb-ep | head -1)
kubectl -n "$NS" get "$POD" -o jsonpath='{.status.containerStatuses[0].lastState.terminated}{"\n"}'
# exitCode=1, startedAt == finishedAt  ⇒  the process died *immediately* on start
```

### 3. Get the real error — two gotchas, two techniques

The crash log is what you actually need, but two things conspire to hide it:

- **An instantly-exiting container has no retained logs.** `kubectl logs <pod> --previous` returns
  *"unable to retrieve container logs"* because the container terminated before the runtime captured
  a stream. (`startedAt == finishedAt` from step 2 is the tell.)
- **A `helm --wait` pod is ephemeral.** When the deploy times out/rolls back, the Deployment + pod
  are torn down, so by the time you look, the pod is gone.

Two reliable ways around this, fastest first:

**(a) Reproduce locally with `docker run`** — the endpoint image is self-contained; run it with no env
to see the supervisor's startup sequence and the exact failure, with zero cluster round-trip:
```bash
docker run --rm 454529406029.dkr.ecr.us-west-2.amazonaws.com/amp/endpoint:<tag> 2>&1 | head -40
# It will fail at the first thing that needs the cluster (e.g. "failed to connect to YugabyteDB at
# 127.0.0.1:5433") — but a *config/binary* error surfaces BEFORE that and is the real signal.
```

**(b) Catch the pod during the `helm --wait` window** — the controller's deploy waits up to 10 min, so
the crash-looping pod exists for a while. Create one endpoint and watch it live:
```bash
# fire a create_endpoint via the API, then:
until kubectl -n "$NS" get pods -o name | grep -q yb-ep; do sleep 2; done
POD=$(kubectl -n "$NS" get pods -o name | grep yb-ep | head -1)
# wait for it to crash once, then read the previous container (now retained across a restart):
until kubectl -n "$NS" get "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}' | grep -qE '[1-9]'; do sleep 3; done
kubectl -n "$NS" logs "$POD" --previous --tail=80
```

**(c) Read it back from the log store** — where `otel.collectors.logs` is deployed, the node's
collector ships container stdout to VictoriaLogs as it is written, so lines survive the pod being
torn down (gotcha 2) even though `kubectl logs` cannot reach them. Grafana → *amp — Endpoint logs*,
or by pod name:
```logsql
{k8s.pod.name="yb-ep-..."} | sort by (_time)
```
This does not rescue gotcha 1: a container that dies before the runtime writes a stream leaves
nothing on disk for the collector to tail either.

> Rule of thumb: if the container dies in well under a second, skip `kubectl` and go straight to
> `docker run` locally — it's faster and the logs aren't truncated.

### 4. Read the supervisor's failure as a layer in a known sequence

The endpoint supervisor (`compute/endpoint_supervisor`, PID 1) brings services up in a **fixed
order**, and logs each transition with a `service=` field:
`safekeeper → pageserver → compute_ctl`. The last line *before* the crash tells you
which layer failed:

```
INFO ...supervise: started service=safekeeper pid=14           ← last line ⇒ safekeeper failed
INFO ...supervise: healthy service=safekeeper
INFO ...supervise: started service=pageserver pid=31
Error: unknown configuration item(s) in pageserver.toml: …      ← pageserver rejected its config
```

Between the pageserver becoming healthy and the compute starting there is one more step, which logs
`branch_id=`: the supervisor waits for the pageserver's branch to reach `Active`, reads its
`last_record_lsn`, and creates the branch on the safekeeper. A stall at
`waiting for the pageserver branch` means the pageserver is up but its timeline is not — look at the
pageserver's own log, not the supervisor's.

How far it got is diagnostic: "safekeeper healthy, pageserver fails" means the
backing YB is fine and you've narrowed it to the pageserver's config or binary.

Raise the supervisor's own verbosity with `RUST_LOG=debug` on the pod/container if the `info` lines
aren't enough.

### 5. Confirm pre-existing vs. your change, then fix at the authoritative layer

Before "fixing" anything, decide whether you broke it:
```bash
git status compute/endpoint_supervisor deploy/endpoint/   # pristine ⇒ not your change
git log --oneline -- compute/endpoint_supervisor deploy/endpoint/Dockerfile   # find the skew-introducing commit
```
**The freshly-built binary is authoritative** — it defines the binary names that exist and the config
keys it accepts. If the supervisor and the binary disagree, fix the supervisor to match the binary
(not the other way around), unless the binary itself regressed.

### 6. Verify the fix locally before the expensive round-trip

The full loop — rebuild image → push to ECR → redeploy controller → create endpoint → inspect — is
~10–25 min. Don't spend it to test a one-line guess. Rebuild and `docker run` locally first; you've
fixed *this* layer once the supervisor gets **past** it (and fails on the next thing, e.g. "no YB at
127.0.0.1:5433", which is expected with no cluster). Only then push + redeploy.

### 7. Expect to iterate — skews come in clusters

A supervisor↔binary skew usually means the image hasn't been freshly built+run in a while, so several
skews have accumulated. Each fix reveals the next service's failure (binary-name skew → past it →
pageserver config skew → past it → Ready). Use the fast local loop (step 6) for each, and **time-box
it**: if you're fixing endpoint internals unrelated to your actual change, fix what blocks you, then
file the rest separately.

---

## Supervisor ↔ binary skew

The recurring class the flow above exists to catch. The endpoint image bakes in **two things that must
agree**: the supervisor (which renders configs and spawns processes) and the binaries it spawns. When a
refactor renames a binary or changes a config schema but only one side is updated, the image builds
fine and crash-loops at runtime.

The supervisor is now Rust in the same cargo workspace as the binaries, which closes one half of this
and leaves the other open:

- **Config-schema skew is largely closed.** `config::render` deserializes each rendered document into
  the daemon's own type (`pageserver_api::config::ConfigToml`,
  `safekeeper_api::config::SafekeeperConfigToml`) before writing it, so a key the daemon no longer
  accepts fails in the supervisor with a clear error — and in the crate's unit tests — instead of at
  daemon startup. The historical instance was `availability_zone='local'`, which the current
  `pageserver` rejects: `Error: unknown configuration item(s) in pageserver.toml:
  availability_zone … Aborting start up`. Same class as `startup-failures.md` §A/§K.
- **Binary-name and argv skew is still open.** Children are spawned by path and flag *strings*
  (`boot::services`), so a renamed binary or a changed flag is not a compile error. The historical
  instance was the `yb-` prefix landing on the binaries (e.g. `1d60bbfd6`) while the supervisor still
  spawned the bare names — symptom: instant exit, "No such file or directory". The same shape bit
  `compute_ctl`'s argv during the Rust port: `--spec-path` for what is really `--config`, plus a
  missing required `--compute-id`, which fails clap parsing before the process does any work.

`boot::tests` pins the compute's argv against the flags `compute-ctl` declares, so a regression *in
the supervisor* is caught. It cannot catch a change to the other binary's own flags — only running a
fresh image does.

**Generalize, don't memorize the specific keys.** Any time you build a fresh endpoint image:
`docker run` it once and watch the supervisor walk its start order. A missing binary or a rejected flag
is name/argv skew — compare `docker run --entrypoint ls <img> /usr/local/bin/` and the child's
`--help` against `boot::services`. A process rejecting its config is schema skew, and the binary is
authoritative (`<bin> --help`, or the error names the bad key). Either way the fix is in
`compute/endpoint_supervisor`, and you verify it with another local `docker run` before redeploying.

> Note on rebuilds: the supervisor is a cargo bin in the endpoint image's build stage, so changing it
> re-triggers the Rust build. On a *warm* BuildKit cache that incremental build can fail to regenerate
> `communicator/communicator_bindings.h` (a cbindgen build-script output) → `yb-amp-pg-ext` then can't
> find it. A clean build (`--no-cache`, or fresh cache) is unaffected. To iterate without a full image
> rebuild, build the binary alone
> (`cargo build --release --bin endpoint-supervisor`) and layer it onto the last good image
> (`FROM <good-image>` + `COPY target/release/endpoint-supervisor /usr/local/bin/`) — it is the last
> binary COPY in the full Dockerfile, so the result matches a from-scratch build with the fix.

---

## Controller deploy gotchas

- **DB URL must match how YugabyteDB was stood up.** `deploy/controller/redeploy.sh` defaults
  `DB_URL`/`YB_HOST_PORTS` to `yb-tserver.$NS.svc…`, but `deploy/up.sh` brings up a *yugabyted*
  StatefulSet at `yugabyted-0.yugabyted.$NS.svc.cluster.local:5433`. Running `redeploy.sh` standalone
  with the default DB URL → the new controller pod crash-loops on
  `hostname resolving error: lookup yb-tserver.$NS.svc.cluster.local … no such host`. Pass the real
  `DB_URL`/`YB_HOST_PORTS`, or just re-run `up.sh` (which sets them). Confirm with:
  `kubectl -n "$NS" get pod <pod> -o jsonpath='{..env[?(@.name=="AMP_DATABASE_URL")].value}'`.
- **Rolling updates briefly run two controllers (two managed controllers) on one DB.** With
  `replicas: 1` + the default RollingUpdate (`maxSurge` makes 2 pods during the swap). If that ever
  causes trouble, `kubectl -n "$NS" delete deploy amp-controller` then redeploy for a clean single pod.
- **`ImagePullBackOff` → refresh the `pull-secret`.** It's built from an ECR login
  token that **expires ~12h**, so a namespace idle between deploys goes stale. `up.sh`
  and `redeploy.sh` both re-mint it each run (creds via `deploy/ecr-creds.sh`);
  otherwise refresh it with the `dshubin_bot` creds via the idempotent `kubectl create
  secret docker-registry … | kubectl apply -f -` one-liner in `deploy/README.md`
  ("Refreshing the pull secret").
- **Stale endpoint Helm releases pile up** from failed `create_endpoint` tasks (left `pending-install`).
  Clean up: `for r in $(helm -n "$NS" list -aq | grep '^yb-ep-'); do helm -n "$NS" uninstall "$r"; done`.

## Resize an endpoint's compute (in place)

Change a running endpoint's CPU/memory without recreating it. The controller does
two writes — persist the size to the endpoint `config`, then PATCH the pod's
`resize` subresource — so the current pod adopts the new size live and a
controller-driven redeploy keeps it. The Deployment template is **not** patched
(that would trigger a rollout and recreate the pod). Caveat: an involuntary
recreate (node loss / eviction) before the next redeploy brings the pod back at
the old size until a redeploy re-renders from `config`. Needs k8s ≥ 1.33.

```bash
# resize -> 202 + a resize_endpoint Task (omit a field to keep its current value)
curl -s -X POST "$BASE/storage-pools/$POOL/clusters/$PROJ/branches/$BR/endpoints/$EP/resize" \
  -H 'content-type: application/json' -d '{"resources":{"cpu":"4","memory":"8Gi"}}'
# poll the task; its result reports the in-place outcome:
curl -s "$BASE/tasks/$TASK" | jq '{status, result}'   # result.in_place_resize: completed|pending|infeasible|unsupported
```

Verify the live pod took it **without a restart** (same pod, RESTARTS unchanged):

```bash
kubectl -n "$NS" get pod -l app.kubernetes.io/instance=yb-ep-$EP \
  -o jsonpath='{.items[0].status.containerStatuses[?(@.name=="endpoint")].resources}{"\n"}'
kubectl -n "$NS" get pod -l app.kubernetes.io/instance=yb-ep-$EP \
  -o jsonpath='{.items[0].status.conditions[?(@.type=="PodResizePending")].message}{"\n"}'  # empty = applied
```

- **`in_place_resize: infeasible`** — the node can't fit the new size right now.
  The size is still persisted in `config`, so a controller-driven redeploy lands
  it; free capacity or let the pod reschedule, then redeploy.
- **`409` on the resize call** — a resize of this endpoint is already in flight
  (first-writer-wins lock). Wait for the in-flight task to finish (`GET
  /tasks?kind=resize_endpoint` → its status) and retry.
- **`500` on the resize call, with `duplicate key value violates unique
  constraint "tasks_one_active_resize_per_endpoint"` in the controller log** — the
  same in-flight-resize conflict as above, on a controller predating the #400 fix
  (it read the constraint name from a pgx field YSQL leaves empty, so the conflict
  was never translated to `409`). Treat it as a `409` — retry after the in-flight
  task reaches terminal — and upgrade the controller.
- **`in_place_resize: unsupported`** — the deployer is Docker (no in-place); only
  the Kubernetes deployer resizes in place.
- **`403` on the resize / `pods/resize` forbidden** — the controller Role is
  missing the `pods/resize` subresource grant (older chart); re-deploy the
  `yb-amp-controller` chart.

## A dropped branch's data isn't going away

The drop itself only **tombstones** the branch (`dropped_at`). The controller's
branch-drop reaper deletes the branch's `amp_storage` rows a retention window later
— `--branch-gc-retention`, **24h** by default — and then stamps `gc_completed_at`
([`../controller/architecture/sc-integration.md`](../controller/architecture/sc-integration.md#branch-drop-gc)).
Storage that hasn't shrunk within that window, plus a compaction cycle, is what
this section is for.

```sql
-- Where every tombstoned branch stands. Run against the controller's database.
SELECT branch_id, dropped_at, reap_after, gc_completed_at,
       now() >= reap_after AS due
  FROM amp_registry.branches
 WHERE dropped_at IS NOT NULL AND gc_completed_at IS NULL
 ORDER BY reap_after;
```

Read it in this order:

- **`reap_after` is NULL** (`due` is NULL too) — the branch is tombstoned and not
  yet stamped. Normally transient: the reaper stamps every unstamped tombstone at
  the start of each tick, from that row's own `dropped_at`, so this clears within
  one `--branch-gc-interval`. Nothing to do by hand — writing a tombstone without
  a deadline is fine, and that is the point.

  Persisting past a tick means the reaper is not running at all (see below); the
  deadline is not the problem.
- **Not due yet** (`due = f`) — nothing is wrong; the window has not elapsed. To
  reclaim sooner, **clear the deadline** (`UPDATE amp_registry.branches SET
  reap_after = NULL WHERE branch_id = ...`) and let the next tick re-stamp it at
  the current `--branch-gc-retention`; or set it directly (`SET reap_after =
  now()`). Lowering the flag alone is not enough — an already-stamped deadline is
  a snapshot and does not move.
- **Due, and the branch is still listed** — either no reaper is running or it is
  skipping this branch. Check both in the controller log:

  ```bash
  kubectl -n "$NS" logs deploy/amp-controller | grep component=branch_gc | tail -20
  ```

  - no `branch-drop reaper started` line ⇒ it is off (`--branch-gc=false`).
  - the reaper is running but this branch never produces a `branch data reclaimed`
    line ⇒ it is being **filtered out of the due scan**, which is silent by design:
    the two guards are `NOT EXISTS` clauses inside the scan's own SELECT, so a
    held-back branch is never fetched and never logged. There is nothing to grep
    for — ask the database which guard holds it:

    ```sql
    SELECT (SELECT count(*) FROM amp_registry.branches c
             WHERE c.parent_branch_id = :id AND c.dropped_at IS NULL) AS live_children,
           (SELECT count(*) FROM amp_registry.endpoints e
             WHERE e.branch_id = :id)                                 AS endpoints;
    ```

    Either count above zero means **do not delete the data by hand.** A live child
    fork still reads this branch's pre-fork history; a surviving endpoint means a
    compute may be serving it. Both mean the drop path let through something it
    should have refused — retire the referencing resource first, and treat the skew
    itself as a bug.
  - `branch-drop reap paused on the tick's row budget` ⇒ working as intended on a
    large branch: it drains over many ticks
    (`--branch-gc-rows-per-tick`, 20 000/tick — with the default 30s
    `--branch-gc-interval` that is ≈ 2.4M rows/hour, or ≈ 670 rows/s).
    The rate is the budget divided by the interval, so raising the budget or
    shortening the interval both speed it up — but remember these deletes land on
    the same six `pages` / `wals` tablets live branches are ingesting into.
- **`gc_completed_at` set but disk hasn't dropped** — the rows are gone; what
  remains is DocDB tombstones, reclaimed at compaction (gated by
  `timestamp_history_retention_interval_sec`, 900s by default). Wait it out.
- **A parent branch's GC cutoff is stuck** at a dropped child's fork point — that
  pin is the child's `amp_storage.branch_state` row, and the reaper deletes it first,
  so this resolves as soon as the child is reaped. See
  [`../compute/architecture/pageserver-gc.md`](../compute/architecture/pageserver-gc.md).

## Fast triage checklist

```bash
# 1. controller up + reachable?
kubectl -n "$NS" get deploy amp-controller; curl -s -o /dev/null -w '%{http_code}\n' $BASE/../healthz
# 2. the failing task's error
curl -s "$BASE/tasks?limit=50" | jq -r '.tasks[] | select(.status=="failed") | "\(.kind)\t\(.error)"'
# 3. endpoint pod state + events
kubectl -n "$NS" get pods | grep yb-ep; kubectl -n "$NS" get events --sort-by=.lastTimestamp | tail -20
# 4. the real crash reason (local repro beats kubectl for instant-exit containers)
docker run --rm <ecr>/amp/endpoint:<tag> 2>&1 | head -40
```
