# Runbook: amp controller operations

Operational debugging for the **amp controller** (control plane) and the endpoints it deploys on
Kubernetes. Use this when a controller is deployed but a *mutation task* (create/delete project,
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

This is the worked example behind the "supervisor.py issues" section below, generalized into a
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
| `ContainerCreating` / `Pending` | image pull, scheduling, PVC binding | events; `describe pod` |

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

> Rule of thumb: if the container dies in well under a second, skip `kubectl` and go straight to
> `docker run` locally — it's faster and the logs aren't truncated.

### 4. Read the supervisor's failure as a layer in a known sequence

The endpoint supervisor (`deploy/endpoint/supervisor.py`, PID 1) brings services up in a **fixed
order**, and it logs each one: `endpoint_storage → safekeeper → yb-pageserver → compute_ctl`. The line
*before* the crash tells you which layer failed:

```
[supervisor] starting endpoint_storage: /usr/local/bin/yb-endpoint-storage …   ← if this is the last line, endpoint_storage failed
[supervisor] endpoint_storage is healthy
[supervisor] starting yb-pageserver: /usr/local/bin/yb-pageserver -D /data/pageserver
Error: unknown configuration item(s) in pageserver.toml: …                       ← pageserver rejected its config
```

How far it got is diagnostic: "endpoint_storage + safekeeper healthy, pageserver fails" means the
backing YB and JWT/keys are fine and you've narrowed it to the pageserver's config or binary.

### 5. Confirm pre-existing vs. your change, then fix at the authoritative layer

Before "fixing" anything, decide whether you broke it:
```bash
git status deploy/endpoint/            # pristine ⇒ not your change
git log --oneline -- deploy/endpoint/supervisor.py deploy/endpoint/Dockerfile   # find the skew-introducing commit
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

## "supervisor.py issues": supervisor ↔ binary version skew

The recurring class the flow above exists to catch. The endpoint image bakes in **two things that must
agree**: the Python `supervisor.py` (which renders configs and spawns processes) and the
freshly-compiled Rust/PG binaries it spawns. When a refactor renames a binary or changes a config
schema but only one side is updated, the image builds fine and crash-loops at runtime — invisible to
`cargo build`/`go build`/unit tests; it only shows up when a **fresh image is actually run**.

Two concrete instances found while bringing up a fresh build (both fixed in `supervisor.py`):

1. **Binary-name skew.** The cargo bins + Dockerfile install `yb-endpoint-storage`, `yb-safekeeper`,
   `yb-compute-ctl` (the `yb-` prefix landed in commits like `1d60bbfd6 "prefix compute_ctl binary
   with yb-"`), but `supervisor.py` still spawned the bare names `endpoint_storage` / `safekeeper` /
   `compute_ctl` (only `yb-pageserver` had been updated). Symptom: instant exit,
   `FileNotFoundError: '/usr/local/bin/endpoint_storage'`. Detection: compare
   `docker run --entrypoint ls <img> /usr/local/bin/` against `grep 'BIN_DIR /' supervisor.py`.

2. **Config-schema skew.** `supervisor.py`'s `write_pageserver_config()` emitted
   `availability_zone='local'`, which the current `yb-pageserver` no longer accepts. Symptom:
   `Error: unknown configuration item(s) in pageserver.toml: availability_zone … Aborting start up`.
   This is the same class as `startup-failures.md` §A/§K — the binary is strict about unknown config
   keys. Fix: drop the key from the rendered config.

**Generalize, don't memorize the specific keys.** Any time you build a fresh endpoint image:
`docker run` it once and watch the supervisor walk its start order. If a binary is missing, it's a
name skew (compare `ls /usr/local/bin` to the `BIN_DIR / "<name>"` spawns). If a process rejects its
config, it's a schema skew (the binary is authoritative — `<bin> --help` / the error names the bad
key). Either way the fix is in `supervisor.py`, and you verify it with another local `docker run`
before redeploying.

> Note on rebuilds: changing `supervisor.py` busts the Dockerfile's `COPY . .` layer, which re-triggers
> the Rust build stage. On a *warm* BuildKit cache that incremental Rust build can fail to regenerate
> `communicator/communicator_bindings.h` (a cbindgen build-script output) → `yb-amp-pg-ext` then can't
> find it. A clean build (`--no-cache`, or fresh cache) is unaffected. To iterate quickly without a
> cold rebuild, layer the supervisor fix onto the last good image
> (`FROM <good-image>` + `COPY … supervisor.py` + `chmod +x`) — `supervisor.py` is the final COPY in
> the full Dockerfile, so the result is identical to a from-scratch build with the fix.

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
