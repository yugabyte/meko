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

Since #635 that error carries the **pod's own reason**, so in the common cases it is the whole
answer and steps 2-3 are unnecessary:

```
deploy endpoint: pod yb-ep-<id>-7d9 could not be scheduled: 0/14 nodes are available:
1 node(s) were unschedulable, 13 Insufficient ephemeral-storage.: release yb-ep-<id> did not
become ready within 10m0s: helm upgrade --install yb-ep-<id>: exit status 1:
Error: context deadline exceeded
```

The leading clause is one of: the scheduler's message, an `ImagePullBackOff` and its image, a
container's exit code, or the pod's recent events. A scheduling failure also carries a
`; recent events: …` tail, which is where cluster-autoscaler's `NotTriggerScaleUp` reason lands —
"the cluster could not add a node", as distinct from "no existing node fits". The `10m0s` is
`--deploy-timeout`'s default and moves with it; since #960 it also sets the pod's startupProbe budget
and the Deployment's `progressDeadlineSeconds`, so a longer timeout really does buy the pod more time
rather than just the caller.

You still need the layers below when the leading clause names no cause — `is running but not ready`
(usually a slow or wedged supervisor: go to step 3 for its log), `is Pending with no scheduling
decision yet`, `reports no failure`, or a `could not be scheduled` with no detail after it. Any of
these may carry a `; recent events: …` tail, which is often the answer on its own. A different
sentence, `its state could not be read`, means the pod was already gone — as does the enrichment
being absent entirely, which happens when helm failed for a reason other than the wait expiring
(a chart render refusal, say, whose own message is then the whole story). (A `helm … --wait` that
fails fast with `Error: services "…" not found` is the same root cause: the release is rolling back
because the pod isn't coming up.)

### 2. Classify the pod state — crash-loop vs. not-ready vs. pending

```bash
kubectl -n "$NS" get pods | grep yb-ep
kubectl -n "$NS" get events --sort-by=.lastTimestamp | grep -iE 'yb-ep|back-off|unhealthy|failed'
```

Three failure modes, three different next moves:

| Pod state | What it means | Where to look |
|---|---|---|
| `CrashLoopBackOff` / `restartCount` climbing | a process inside the container exits non-zero on start. Since #960 this no longer covers "bootstrap took too long" — the kubelet does not kill a container inside its startup budget — so this is a real non-zero exit, e.g. the supervisor's own `BRANCH_ACTIVE_TIMEOUT` (180s) firing | container logs (step 3) |
| `Running` but `READY 0/1`, `restartCount=0` | **usually still bootstrapping.** The startupProbe suspends liveness until the compute first listens, so a slow bootstrap now sits here rather than crash-looping (#960). Check the container log for progress before treating it as stuck; only once the startup budget is spent is the readiness probe the thing to chase | container logs; then the service behind the readiness probe; SC registration |
| `ContainerCreating` / `Pending` | image pull, scheduling, PVC binding — including `Insufficient ephemeral-storage`, which means the pool cannot back this endpoint's 20 GiB/vCPU data volume (a 32-vCPU endpoint asks for 640 GiB; size the node pool's local SSDs to match). On a test cluster, pin a flat size instead: `controller.endpoint.diskSizeOverride=10Gi` | events; `describe pod` |

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

**(b) Catch the pod during the `helm --wait` window** — the controller's deploy waits for
`--deploy-timeout` (10 min by default, 35m in the functional harness), so the unhealthy pod exists for
a while. Create one endpoint and watch it live:
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

## `ensure endpoint record: conflict` — a failed create is holding the writer slot

A `create_endpoint` task that fails with **`ensure endpoint record: conflict`** on a
branch with no working writer is almost always this: an *earlier* failed create left
its row behind. The step records the endpoint before deploying, so the row already
holds the branch's single-writer slot when the deploy fails, and
`endpoints_one_writer_per_branch` rejects every later writer for that branch.
(Note the failure is a **failed task**, not a synchronous 409 — the single-writer
rule is enforced in the step, not the handler.)

The tell is an endpoint row with no `address`, whose creating task failed:

```sql
-- Addressless endpoints on the branch, with the task that tried to create each.
-- Run against the controller's database.
SELECT e.id, e.branch_id, e.created_at, t.kind, t.status, t.updated_at AS failed_at
  FROM amp_registry.endpoints e
  JOIN task_steps s ON s.kind = 'create_endpoint' AND (s.args->>'endpoint_id')::uuid = e.id
  JOIN tasks t ON t.id = s.task_id
 WHERE e.branch_id = '<branch-uuid>' AND e.address IS NULL
 ORDER BY t.updated_at DESC;
```

The GC sweeper reclaims it without help, but not promptly: it only considers an
orphan **once `--gc-grace` (30m) has passed** since the failure, so the earliest a
`delete_endpoint` appears is 30m after, plus up to one `--gc-interval` (5m). The
window exists so a fresh failure can be inspected — so if you have finished
inspecting and don't want to wait, delete it yourself:

```bash
curl -s -X DELETE "$BASE/storage-pools/$POOL/clusters/$PROJ/branches/$BR/endpoints/$EP"
```

If the row is *still* there long after the grace window, check the sweeper is
running before digging further: `--gc-enabled` not turned off, and
`amp_controller_gc_orphans_total{resource="endpoint"}` against a rising
`amp_controller_gc_sweeps_total`. A skipped count that climbs means the precheck
keeps disqualifying it — most often because the endpoint *does* have an address, in
which case it is a real endpoint and GC is right to leave it alone.

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

- **`/readyz` 503s for ~2 minutes after the backing pod is replaced, while endpoints serve fine.**
  The pool is holding ESTABLISHED sockets to the old pod IP. The peer vanished rather than sending
  a RST, so a write to one blocks in kernel retransmit and the caller burns its whole deadline
  discovering a single dead connection. The tell is in the shape, not the message: every `/readyz`
  takes *exactly* its 1s probe budget and every `/metrics` scrape exactly its 5s, and the error is
  a bare `context deadline exceeded` with **no address in it** — the call never reached a dial.
  (Contrast a dial that did happen, where the error names an address:
  `10.x.y.z:5433 (…-yb-tserver-0.…): dial error: …`. Both shapes can appear in one incident at
  different moments; the address-less one is what says the pool is the problem.)

  `db.Connect` bounds this at two levels (#961). Pool-level (`PingTimeout`, `ConnectTimeout`,
  `MaxConnIdleTime`, `HealthCheckPeriod`, `MaxConnLifetime`) — pgx already sets most of these, at
  1h/30m/1m, so this is a tightening rather than switching something on. The exception is
  `PingTimeout`, which really does default to *no timeout*, and that is what let one dead socket
  eat a whole probe. Socket-level (`TCP_USER_TIMEOUT` plus a TCP keepalive, in `dialer.go`) — the
  pool bounds only shed one dead connection per caller, whereas the kernel fails all of them
  within one `tcpUserTimeout` window.

  **This does not rule out DNS.** In run 44551e5d the collected controller log ends at 23:05:07,
  before `/readyz` recovered, and #961 quotes a GC sweep at 23:05:29 still dialing the departed pod
  IP by name — 2m09s after the replacement came up. A stale address outlived the window the log
  covers, and these bounds do not close that path; they only cap each attempt at `connectTimeout`.
  What the log does establish is that from 23:03:26 to where it ends, no dial was attempted at
  all — every call hung on a pooled socket instead.

  `TCP_USER_TIMEOUT` is Linux-only and set through `net.Dialer.Control`, so it is silently absent
  on a macOS dev box (`dialer_other.go`) — and it would also be silently absent if the Control
  hook stopped running, which is why the test reads the value back off a real socket instead of
  asserting the constant. It does not bound slow queries: the timer only runs while data is
  unacknowledged at the TCP layer, so a server that ACKs and then thinks is not affected.

  The numbers are shared with the compute side rather than invented here, so one failure has one
  answer in both languages: `connectTimeout` 5s is `ysql_client`'s `DEFAULT_CONNECT_TIMEOUT_MS`,
  `tcpUserTimeout` 3s is `endpoint_supervisor`'s `TCP_USER_TIMEOUT_MS` / `LEASE_TCP_USER_TIMEOUT_MS`,
  and `maxConnLifetime` 15m + 25% jitter is `DEFAULT_CONNECTION_LIFETIME_MS` and the envelope
  `jittered()` spreads over. `PingTimeout`, `MaxConnIdleTime` and `HealthCheckPeriod` are pgxpool's
  alone — the Rust clients have no acquire-time ping or background reaper to match.

- **DB URL must match how YugabyteDB was stood up.** There are four shapes, and the
  controller crash-loops on `hostname resolving error: lookup … no such host` whenever the
  DSN names one it did not get:
  - `deploy/controller/redeploy.sh` defaults `DB_URL`/`YB_HOST_PORTS` to `yb-tserver.$NS.svc…`;
  - `deploy/up.sh` and `deploy/up-gke.sh` (the default `FOUNDATION=yugabyted`) bring up a
    *yugabyted* StatefulSet at `yugabyted-0.yugabyted.$NS.svc.cluster.local:5433`;
  - the same two with **`TLS=1`** bring up the upstream `yugabyte` chart instead, at
    `yb-tserver-0.yb-tservers.$NS.svc.cluster.local:5433` behind TLS. The DSN then needs
    `sslmode` and `sslrootcert` (the `amp-ca-bundle` ConfigMap, mounted at
    `/etc/amp/tls/ca/ca.crt`);
  - `up-gke.sh` with **`FOUNDATION=operator`** brings up an operator-managed universe and
    takes the address from `foundation connstr` — the chart-created ClusterIP
    `<universe>-<suffix>-yb-tserver-conn.$NS.svc.cluster.local:5433`, which carries YSQL on
    5433 and the tserver RPC port on 9100. The name carries a YBA-generated suffix, so find
    it with `kubectl -n "$NS" get svc -l serviceName=yb-tserver-conn,scope=Namespaced`
    rather than guessing.

  Running `redeploy.sh` standalone with the default DB URL against any of the others is the
  usual way to hit this. Pass the real `DB_URL`/`YB_HOST_PORTS`, or re-run the bring-up that
  set them — and re-run it with the same `FOUNDATION` and `TLS` values, since those are what
  decide which shape you get. Confirm with:
  `kubectl -n "$NS" get pod <pod> -o jsonpath='{..env[?(@.name=="AMP_DATABASE_URL")].value}'`.

  Deleting the foundation out from under a running controller produces the same symptom:
  `foundation down` leaves the namespace up by default, so the controller and its endpoints
  keep the old Service name in their env (Kubernetes does not rewrite env in a live
  container) and crash-loop once it stops resolving. Scale them down before tearing a
  foundation down, or expect to redeploy them after.

- **`FOUNDATION=operator`: the controller crash-loops on `no pg_hba.conf entry ... no
  encryption`.** The universe requires TLS for client connections while the DSN says
  `sslmode=disable`. `enableClientToNodeEncrypt` defaults to **true** in the CRD, so a
  foundation spec that does not set it explicitly gets TLS — and the field is create-time
  only, so the universe has to be rebuilt rather than edited. The checked-in spec sets both
  encryption flags false; if you are looking at a universe that predates that, rebuild it,
  and note the deployer derives `sslmode` from the field, so a TLS universe gets a
  `require` DSN once the compute side can supply certs for the 9100 lane
  ([#1241](https://github.com/yugabyte/meko/issues/1241), behind
  [#1114](https://github.com/yugabyte/meko/issues/1114)).

- **`FOUNDATION=operator`: the CRs sit with an EMPTY status and no DB pods appear.** The
  operator needs YBA to have exactly one customer and throws before it can write a status,
  so the CR looks untouched rather than failed. Confirm it in one call — from the yugaware
  pod, `curl -s localhost:9000/api/v1/customer_count` must return `{"count":1}` — and read
  the operator log for `Customer list does not have exactly one customer`. The customer is
  created by the chart's **post-install** hook, so it is never created on an upgrade: a
  release first installed without `yba.values.yugaware.defaultUser` set will stay broken
  through any number of re-runs. Either register it once against the `yugaware-ui` Service
  (`POST /api/register` with `{email, password, code: "operator", name}`) or
  `helm uninstall` the release so a fresh install runs the hook. `foundation up` now fails
  at the install with this reason rather than waiting the universe out.

- **`FOUNDATION=operator`: DB pods in `ImagePullBackOff`, or a universe that never leaves
  `Creating`.** The DB image registry comes from the foundation spec's `provider` block,
  and its pull secret is **YBA's**: YBA derives one from its own
  (`yba.values.image.pullSecret`, which `up-gke.sh` mints from `PULL_SECRET`) and copies it
  beside the DB pods. So the usual cause is that robot lacking pull access to the DB image
  repo, not a name mismatch — do **not** add
  `universe.kubernetesOverrides.Image.pullSecretName`, which would name a Secret that need
  not exist where the DB pods run. Read the reason off the CRs, which is where YBA puts it:
  `kubectl -n "$NS" get ybprovider,ybuniverse -o yaml` (`status.actions[]` carries the
  failing task and its message), then the operator's own log —
  `kubectl -n "$NS" logs "statefulset/$NS-yugaware-yugaware" -c yugaware --tail=300`.
  Select on `release=$NS-yugaware` if you need a selector: the yugaware chart sets no
  `app.kubernetes.io/*` labels, and `kubectl logs -l` with a selector that matches nothing
  exits 0 with no output, which reads as an empty log.
- **A single-host DB URL single-homes the control plane.** pgx dials only what the DSN lists, in
  order, so a DSN naming one tserver pod leaves the controller nothing to fail over to when that
  pod goes, even where the endpoints have the whole `YB_HOST_PORTS` CSV. List every tserver
  (`postgres://u@t0:5433,t1:5433,t2:5433/db`) or point it at the tservers' headless Service.
  Names, never pod IPs — an IP survives a reschedule as a dead address, whereas a name
  re-resolves on the next dial. `redeploy.sh` and `up-gke.sh` escape the commas for you before
  `helm --set-string` (which otherwise splits on them and fails the install); if you invoke helm
  by hand, escape them yourself. Note this is not what caused the two-minute stall in meko#961:
  that was a pool holding sockets to a departed pod, and a multi-host DSN would not have helped.
- **Rolling updates briefly run two controllers (two managed controllers) on one DB.** With
  `replicas: 1` + the default RollingUpdate (`maxSurge` makes 2 pods during the swap). If that ever
  causes trouble, `kubectl -n "$NS" delete deploy amp-controller` then redeploy for a clean single pod.
- **`ImagePullBackOff` → refresh the `pull-secret`.** It's built from an ECR login
  token that **expires ~12h**, so a namespace idle between deploys goes stale. `up.sh`
  and `redeploy.sh` both re-mint it each run (creds via `deploy/ecr-creds.sh`);
  otherwise refresh it with the `dshubin_bot` creds via the idempotent `kubectl create
  secret docker-registry … | kubectl apply -f -` one-liner in `deploy/README.md`
  ("Refreshing the pull secret").
- **`verify endpoint <release>: its safekeeper did not answer GET http://…:7676/v1/status`**
  is the post-deploy check failing *after* a Helm install that succeeded, so the pod is usually
  fine and something between the controller and the safekeeper is not. The error carries the
  last failure, which splits the triage: a **refused connection** is the Service without a
  backend — `kubectl -n "$NS" get endpoints <release>` lists no ready address, so start at the
  pod's readiness. A **status code** means the Service routes and the safekeeper answered badly:
  go to the safekeeper's own logs in the endpoint pod. It leaves the release installed — clean
  it up with the one-liner below before re-creating.
- **Stale endpoint Helm releases pile up** from failed `create_endpoint` tasks (left `pending-install`).
  Clean up: `for r in $(helm -n "$NS" list -aq | grep '^yb-ep-'); do helm -n "$NS" uninstall "$r"; done`.

## Resize an endpoint's compute (in place)

Change a running endpoint's CPU/memory without recreating it. The controller does
two writes — PATCH the pod's `resize` subresource, then persist the size to the
endpoint `config` once the deployer accepted it — so the current pod adopts the new
size live and a controller-driven redeploy keeps it. The Deployment template is **not** patched
(that would trigger a rollout and recreate the pod). Caveat: an involuntary
recreate (node loss / eviction) before the next redeploy brings the pod back at
the old size until a redeploy re-renders from `config`. Needs k8s ≥ 1.33.

```bash
# resize -> 202 + a resize_endpoint Task (omit a field to keep its current value)
curl -s -X POST "$BASE/storage-pools/$POOL/clusters/$PROJ/branches/$BR/endpoints/$EP/resize" \
  -H 'content-type: application/json' -d '{"resources":{"cpu":"4","memory":"32Gi"}}'
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
- **`400 invalid_argument`, "below the N GiB per vCPU this control plane
  requires"** — the shape the resize would leave the endpoint at is under the
  memory-per-vCPU floor. It applies to the resulting pair, not to what the
  request named, so growing cpu alone is the usual way to meet it: name memory in
  the same request, at least the figure the error quotes. An endpoint already
  under the floor is not held to it — those may resize as long as the move does
  not lower the memory each core has, and a resize that would is refused with
  "this endpoint is below the memory-per-vCPU floor already". The floor is per
  deployment (`--deploy-memory-gib-per-vcpu`, 8 by default).
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
- **The resize never completes and the pod reports `PodResizeInProgress ...
  cannot decrease memory limits ... memory limit (4) below current usage`** — the
  endpoint's stored memory is a unitless byte count, accepted by a controller
  predating the meko#492 validation (`select config->>'memory' from
  amp_registry.endpoints where id = '<ep>'`). Migration `0009` rewrites those to
  `4Gi` when the controller upgrades, so this clears on upgrade; before then, pass
  an explicit `memory` (e.g. `"4Gi"`) with the resize.

## `409` on `POST /endpoint-upgrades` — a fleet roll is already in flight

At most one `upgrade_endpoints` task may be non-terminal at a time, and a second
roll is **refused rather than queued** — two rolls naming different versions would
flip the fleet twice
([`ac-endpoint-upgrade.md`](../controller/architecture/ac-endpoint-upgrade.md) §5.6).
The check is per kind, so re-firing an *identical* roll is refused too. Find the one
holding it:

```bash
curl -s "$BASE/tasks?kind=upgrade_endpoints&limit=20" \
  | jq -r '.tasks[] | "\(.id)\t\(.status)\t\(.created_at)\t\(.error // "")"'
# per-endpoint detail for the roll holding it (children never show in GET /tasks):
curl -s "$BASE/tasks/$TASK/subtasks" \
  | jq -r '.subtasks[] | "\(.args.endpoint_id)\t\(.status)\t\(.error // "")"'
```

- **`running`** — it is being executed. Wait and re-run: a child inside `helm --wait`
  can take minutes per endpoint, and the second roll skips whatever the first landed.
- **`pending`** — nobody has claimed it. If it stays that way there is no worker to
  claim it (no controller up, or one running with the task worker off), and it
  **never reaches terminal**, so it blocks every later roll indefinitely. The resize
  `409`'s advice — wait for the in-flight task — is wrong for this one.

There is no cancel verb and no `force`; an escape hatch is
[meko#1007](https://github.com/yugabyte/meko/issues/1007). Until it exists the only
remedy for the stuck case is to retire the row by hand — and only after confirming
nothing is executing it, or you are stamping a status over a live roll:

```sql
-- no row = no worker holds the task's lease (500 = the skew grace, in ms)
SELECT * FROM amp_registry.token_holder('task:<task-id>', 500);
-- still pending is part of the predicate, not just the check above
UPDATE controller.tasks
   SET status = 'failed', error = 'retired by hand: never claimed', updated_at = now()
 WHERE id = '<task-id>' AND status = 'pending';
```

A roll retired this way never ran its step, so it created no subtasks and no
endpoint moved; re-issue the upgrade once a worker is running.

## `ingress_*` create fails: `no matches for kind "IngressRouteTCP"`

**Symptom.** A create with an `ingress_*` mode allocates a slot and then fails its
task at helm apply:

```
helm deploy yb-ep-<id>: ... no matches for kind "IngressRouteTCP" in version "traefik.io/v1alpha1"
```

The shard itself installed cleanly and looks healthy, which is the confusing part.

**Cause.** The Traefik CRDs are not installed. The shard chart renders Traefik
directly rather than through its upstream chart, so nothing installs them as a
side effect any more — they are a prerequisite, like external-dns. A shard whose
CRDs are missing comes up, passes its probes, and serves nothing, because its
provider has no objects to read.

**Check:**

```sh
kubectl get crd ingressroutetcps.traefik.io middlewaretcps.traefik.io
```

**Fix.** Apply the CRDs for the pinned Traefik version, then re-run the create —
the endpoint keeps the slot it was already allocated, so nothing leaks:

```sh
kubectl apply -k "https://github.com/traefik/traefik-helm-chart/traefik/crds/?ref=v3.7.13"
```

`deploy/controller/redeploy.sh` does this before installing a shard, so a
deployment stood up that way already has them.

## `ingress_*` endpoint answers on the wrong load balancer

**Symptom.** An `ingress_internal` endpoint is reachable from the internet, or a
client on one endpoint's port reaches a different database.

**Cause.** A shard's Traefik is programming routes that belong to another shard.
Each shard watches only its own, by label:

```
--providers.kubernetescrd.labelSelector=amp.yugabyte.com/ingress-shard=<shard>
```

Every shard is built from the same `--deploy-ingress-port-range`, so `pgin-0` and
`pgin-0-ext` both declare `pg-20000…`; without that selector each proxy programs
every route in the namespace.

**Check** that the selector is set on each shard and that the endpoint's route
carries the matching label:

```sh
kubectl -n "$NS" get deploy -l amp.yugabyte.com/ingress-shard \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.template.spec.containers[0].args}{"\n"}{end}' \
  | grep -o 'labelSelector=[^ ]*'
kubectl -n "$NS" get ingressroutetcp -L amp.yugabyte.com/ingress-shard
```

A route with an empty or wrong shard label is served by whichever proxies match
it. Re-running the endpoint's deploy re-renders the label from its allocation.

## `nlb_internal` create fails: `needs service.cloud to be one of aws, gcp, azure`

**Symptom.** A create with `connectivity.mode: nlb_internal` fails its task with a
helm render error:

```
deploy endpoint: helm deploy yb-ep-<id>: helm upgrade --install yb-ep-<id>: exit status 1:
Error: execution error at
(yb-amp-endpoint/templates/service-lb.yaml:28:4): service.connectivity=nlb_internal
needs service.cloud to be one of aws, gcp, azure (got "") — without it the Service
would default to an internet-facing load balancer
```

`nlb_external` is unaffected — it needs no annotation on any provider.

**Cause.** The controller could not name the cloud. It normally reads a node's
`spec.providerID`, which needs `nodes get, list`. The render is refused rather
than defaulted because every provider defaults a LoadBalancer Service to
internet-facing, so guessing would publish what the caller asked to keep private.

**Check** what the controller decided, from its own log — it says which branch it
took on each `nlb_*` create:

```sh
kubectl -n "$NS" logs deploy/amp-controller -c controller \
  | grep -E 'detected cloud|could not (list nodes|determine the cloud)'
```

`detected cloud for load-balancer annotations cloud=gcp` means detection works and
this is not your failure. Either warning means it did not.

**Do not** diagnose this with `kubectl auth can-i list nodes --as=...`: on GKE that
returns `no` for a ServiceAccount that demonstrably can list nodes, so it is a false
negative that sends you to the wrong fix. To test the permission directly, use the
ServiceAccount's own token:

```sh
TOK=$(kubectl -n "$NS" create token amp-controller --duration=10m)
API=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
curl -s -k -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOK" \
  "$API/api/v1/nodes?limit=1"     # 200 = granted, 403 = not
```

Whether the override is set at all:

```sh
kubectl -n "$NS" get deploy amp-controller -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="AMP_DEPLOY_CLOUD")].value}{"\n"}'
```

**Fix**, either:

```sh
# grant the read the controller already knows how to use (chart default)
helm upgrade <release> <chart> -n "$NS" --reuse-values --set rbac.nodeRead=true

# or name the cloud and skip the lookup, for an install that may hold no
# cluster-scoped binding
helm upgrade <release> <chart> -n "$NS" --reuse-values --set controller.endpoint.cloud=gcp
```

Detection caches only a successful answer, so granting the read takes effect on
the next create with no controller restart. `controller.endpoint.cloud` restarts
the pod, and a value outside `aws|gcp|azure` fails the render rather than
installing a controller that exits at startup.

## Endpoint stuck on a pod IP after an `nlb_*` create

**Symptom.** An endpoint created with `connectivity.mode` `nlb_internal` or
`nlb_external` reports a pod IP (or a NodePort/Service address) as its `address`
instead of a load balancer hostname, and that address dies the next time the pod
is replaced. The create logged
`endpoint load balancer not ready in time; falling back to the Service-type address`.

**Cause.** Cloud load balancer provisioning routinely takes longer than
`lbAddressPollTimeout` (2 min). `CreateEndpoint` falls back rather than failing an
endpoint that is up and serving — but **nothing re-polls afterwards**. The
recorded address stays the fallback until the endpoint's next controller-driven
redeploy re-runs the resolution.

**Check** the load balancer actually came up (it almost always does, minutes after
the create):

```sh
kubectl -n "$NS" get svc "$(printf 'yb-ep-%s-lb' "$EP")" \
  -o jsonpath='{.status.loadBalancer.ingress[0].hostname}{"\n"}'
```

Empty means the LB is still provisioning or failed — describe the Service and read
its events before anything else; a missing subnet tag or an exhausted NLB quota
(50 per region) stops it here.

For a BYOC environment the same endpoint reports `attrs.connectivity_mode` beside
its `address`, which is the only way to see the requested mode without cluster
access — a `cluster_ip` endpoint and one that asked for a load balancer otherwise
look alike. It is **conclusive only for `nlb_external`**, where a private address
is this bug outright. An `nlb_internal` LB on GKE/AKS is itself assigned a private
VPC IP in the same `<addr>:<port>` shape as the fallback, so there the mode only
tells you to go check the Service above.

```sh
# YCP fleet API, not the controller's: see ycp/DEPLOY.md for these values.
export YCP=https://<control-plane> ENV=<environment-id>
export SECRET=<admin-secret> ACCOUNT=<account-id>

curl -sS -H "X-Meko-Admin-Secret: $SECRET" -H "X-Meko-Account-ID: $ACCOUNT" \
  "$YCP/api/v1/environments/$ENV" \
  | jq -r '.data.resources[]?|select(.kind=="endpoint")
           |"\(.name) \(.attrs.connectivity_mode) \(.attrs.address)"'
```

**Recover.** Any controller-driven redeploy of that endpoint re-resolves and
re-records the address. Roll just that endpoint — it is redeployed even when it is
already at the target version, which is what makes this work as a trigger:

```sh
curl -sX POST "$AMP/endpoint-upgrades" -H 'Content-Type: application/json' \
  -d "{\"endpoint_ids\":[\"$EP\"]}"
```

Adding a branch extension used to be the usual trigger, and is no longer reliable:
`add_branch_extensions` now skips an endpoint already mounting the branch's set, so
re-adding a name the branch already has redeploys nothing. Adding a genuinely new
extension still rolls every endpoint, but that is a bigger change than an address
fix needs. Failing either, correct the stored value directly and let clients
re-read it:

```sql
UPDATE amp_registry.endpoints SET address = '<lb-hostname>:55433' WHERE id = '<ep>';
```

**Prevention.** The endpoint is reachable in-cluster the whole time; only the
recorded client address is stale. Callers that resolve the endpoint through the
API on each connect pick up the corrected value with no further action.


## Every API call answers `503 service_unavailable`

The controller cannot reach the YugabyteDB holding `yb_storage` — the tserver
serving it is restarting, being replaced, or gone. The controller is fine; it is
saying so on purpose (#886), and `Retry-After` says when to come back. Retry, and
if it does not clear:

```bash
# a second opinion on the store (a bare Ping, so it can disagree at the edges):
curl -s -o /dev/null -w '%{http_code}\n' $BASE/../readyz     # 503 = store unreachable
# what it cannot reach, and why:
kubectl -n "$NS" logs deploy/amp-controller | grep 'state store unavailable' | tail -5
kubectl -n "$NS" get pods | grep yb-tserver     # the host in the log line is one of these
```

A **mutation** that answers `500` while the store is down is the other half of
the same outage: the connection dropped with the statement already sent, so the
write may have committed and a blind retry could double-apply it. The log line
`state store unavailable mid-request; outcome unknown` marks those. Read the
resource back before retrying.

## The controller exits 66 / crash-loops with `pglease: lease ... lost`

It is fenced out, not crashed. The controller runs each task under a lease in
`amp_registry.resource_token`; when a lease cannot be proven held — no renewal
confirmed for `--task-lease-ttl` minus its safety margin — the process dumps the
cause and every goroutine to stderr and exits **66**, deliberately, rather than
carrying on with work it can no longer prove is its own
([`ac-locking.md`](../controller/architecture/ac-locking.md)). 66 is the compute
fence's code too, so the same rule recognises both.

The cause is almost always the backing database, because that is the only thing
that stops renewals:

```bash
kubectl -n "$NS" logs deploy/amp-controller --previous | grep -m1 'pglease: lease'
kubectl -n "$NS" get pods | grep yb-tserver
curl -s -o /dev/null -w '%{http_code}\n' $BASE/../readyz     # 503 = store unreachable
```

A restart loop here means the database is still unreachable; fix that and the loop
clears on its own. **Nothing needs reclaiming by hand**: the in-flight tasks are
left `running` with lapsed leases, and the restarted controller (or another
instance) picks each up once its lease has lapsed — up to one `--task-lease-ttl`
(30s) plus the skew grace per task, and the ones that had renewed most recently
wait longest. Steps are idempotent, so the replay converges.

To see what is still held, and by whom:

```sql
SELECT resource_id, client_id, to_timestamp(token/1e6) AS expires_at
  FROM amp_registry.resource_token ORDER BY token;
```

`task:<uuid>` rows are task ownership and name the process that claimed it;
`endpoint:<uuid>` and `fleet:endpoints` rows name the unit of *work* holding them,
in a `task=<uuid>` field of `client_id`.

That id is usually a task — but a fleet upgrade runs its children as **subtasks**,
which are rows in `controller.subtasks` and never appear in `GET /tasks`, so
looking it up there can dead-end. Try both:

```sql
SELECT 'task' AS found_in, id::text, kind, status::text, NULL AS parent
  FROM controller.tasks WHERE id = '<uuid>'
UNION ALL
SELECT 'subtask', id::text, kind, status::text, parent_id::text
  FROM controller.subtasks WHERE id = '<uuid>';
```

A subtask row's `parent` is the `upgrade_endpoints` task to poll with
`GET /tasks/{id}`; the children's own state is `GET /tasks/{id}/subtasks`.

Do not delete rows to hurry a reclaim along — that is the fence, and removing it is how two controllers
end up on one endpoint. A row is takeable once `expires_at` plus the 500ms
clock-skew grace has passed, not at `expires_at` itself.

**Downgrading to a pre-lease controller strands in-flight tasks.** The old claim
query needed `lease_expiry < now()`, and a task claimed under a lease leaves that
column NULL — `NULL < now()` is NULL, so the older binary never offers the task
again. After a rollback, un-strand them once:

```sql
UPDATE tasks SET lease_expiry = now() WHERE status = 'running';
```

Do not run mixed versions: a lease-era controller holds leases an older one cannot
see, so both would claim the same task.

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
  one tick — up to 4 min if the reaper has backed off on an idle queue (the
  cadence is adaptive; see below). Nothing to do by hand — writing a tombstone
  without a deadline is fine, and that is the point.

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
    large branch: it drains over many ticks (`--branch-gc-rows-per-tick`,
    20 000/tick). A capped tick retries at a sixth of `--branch-gc-interval`
    rather than waiting a full one, so at the defaults a backlog drains at up to
    20 000 per 5s ≈ 4 000 rows/s; once it catches up the loop returns to the 30s
    base, and doubles to 4 min while nothing is due. The wait is measured from
    the end of the previous tick, so the real rate is the budget over
    `floor + however long the reap itself took` — treat 4 000/s as the ceiling,
    and read the actual spacing off the `branch data reclaimed` /
    `reap paused` log timestamps. Raising the budget or shortening the interval
    both speed it up — but remember these deletes land on the same six `pages` /
    `wals` tablets live branches are ingesting into.
- **`gc_completed_at` set but disk hasn't dropped** — the rows are gone; what
  remains is DocDB tombstones, reclaimed at compaction (gated by
  `timestamp_history_retention_interval_sec`, 900s by default). Wait it out.
- **A parent branch's GC cutoff is stuck** at a dropped child's fork point — that
  pin is the child's `amp_storage.branch_state` row, and the reaper deletes it first,
  so this resolves as soon as the child is reaped. See
  [`../compute/architecture/pageserver-gc.md`](../compute/architecture/pageserver-gc.md).

## Fast triage checklist

```bash
# 1. controller up + reachable?  (restarts with exit 66 = fenced out, see above)
kubectl -n "$NS" get deploy amp-controller; curl -s -o /dev/null -w '%{http_code}\n' $BASE/../healthz
# 2. the failing task's error
curl -s "$BASE/tasks?limit=50" | jq -r '.tasks[] | select(.status=="failed") | "\(.kind)\t\(.error)"'
# 3. endpoint pod state + events
kubectl -n "$NS" get pods | grep yb-ep; kubectl -n "$NS" get events --sort-by=.lastTimestamp | tail -20
# 4. the real crash reason (local repro beats kubectl for instant-exit containers)
docker run --rm <ecr>/amp/endpoint:<tag> 2>&1 | head -40
```
