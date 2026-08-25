# YCP — current state

Checkpoint as of 2026-07-31. Snapshot of what is deployed and what is open; for how
to deploy see [`meko/deploy/helm/meko/YCP-RUNBOOK.md`](../meko/deploy/helm/meko/YCP-RUNBOOK.md)
and [`DEPLOY.md`](./DEPLOY.md), and for the design see
[`ARCHITECTURE.md`](./ARCHITECTURE.md).

## There is one implementation

`api_server --control-plane-mode` is YCP. It is deployed by the `meko` chart at
`meko/deploy/helm/meko` (`-f values-ycp.yaml`) and serves the fleet API, the agent
API and the operator console.

There were two until this change. `ycp/cmd/ycp` was a reference implementation of
the same contract — no Cognito/S3/Langfuse, in-memory registry — used by the demo
manifests and the kind e2e. It is gone, along with `internal/server`,
`internal/store`, `web/`, `Dockerfile` and `deploy/`. Two implementations of one
contract earned their keep exactly once and cost more than that: they drifted on
the response envelope (`{"data":…}` vs flat), which broke the agent
([meko#399](https://github.com/yugabyte/meko/issues/399)) while the e2e stayed
green — because the e2e was testing the copy nobody deploys.

`test/e2e/run.sh` now stands up `api_server --control-plane-mode` on a plain
Postgres in kind and drives register → enroll → READY → **restart, re-enrolling
and reporting again** → **command → bootstrap → GC**. The restart assertion is the
one that would have caught [meko#401](https://github.com/yugabyte/meko/issues/401).

What it deliberately does not cover: the full stack **running**. Every image in the
pinned release is private and the kind cluster has no registry credentials, so the e2e
asserts what a plain registration *seeds* (one `meko-stack`, six pinned images, the
MAGE-enabled database among them) on a throwaway environment, and points the agent it
actually runs at a public database image. Standing the real stack up is
`test/local/meko-stack.sh`, which needs AWS credentials and so cannot run in CI.

## Registering an environment deploys Meko

The newest change, and the one that moves the most: **a data plane is deployed from its
enrollment, not from a command.**

- `POST /api/v1/environments` takes a **name** and the **`api_key_id`** the agent
  authenticates with. The control plane resolves the whole Meko release itself — six
  images pinned in `meko/api_server/internal/service/dataplane_release.go` — and
  stores it as that environment's stack.
- The **enroll response carries the stack**. An agent that enrolls has everything it
  needs in that one reply, so it deploys the data plane on its first tick with no
  second call. It persists the stack in its state Secret, so a restart mid-outage
  needs the control plane for nothing at all.
- **Desired state is no longer in the bring-up path.** It stays authoritative whenever
  the agent can read it — that is where registration wrote the stack, and where an
  operator's changes go, so `POST /desired` still replaces or withdraws it. What changed
  is the failure mode: a control plane that cannot be reached no longer skips the tick.
  The agent deploys the stack it holds and skips only garbage collection, which is the
  one thing that genuinely needs to know what was withdrawn.
- **What comes up** is `yugabytedb` (Yugabyte's MAGE-enabled build) with the `mage` and
  `pg_dist_rag` extensions, then `api-server`, `mcp-server`, `rag-worker`,
  `inference-gateway` and `ui`. One `meko-stack` resource, one GC key: withdrawing it
  takes the environment down as a unit.

Four things this ran into that are worth not rediscovering:

- `yugabyted` turns `ysql_enable_auth` on as soon as `YSQL_PASSWORD` is set, so the
  readiness probe's `ysqlsh` needs `PGPASSWORD` too — without it a perfectly healthy
  database never reports ready, because the probe hits a password prompt.
- `api_server`'s data migrations reference `dist_rag.vector_indexes` and the `dist_rag`
  enum types, so the extensions are a hard gate on the api-server rather than a nicety.
- `api_server` pings its database five times over fifteen seconds and then exits, and a
  `yugabyted` takes about ninety seconds to come up. Deployed without a gate it spends
  its restarts before the database exists and lands in a five-minute
  `CrashLoopBackOff` — observed still backing off eleven minutes in, long after the
  database was serving. It gets an init container that waits for the YSQL port.
- The inference gateway builds its LangChain client while the app is constructed, so a
  missing `OPENAI_API_KEY` is fatal rather than degrading: the process exits with
  `Missing credentials` and the component is 0/1 forever.

Verified against the real image (`ybdb-meko-release:2026.1.0.0-b53`): `mage` 1.6.0 and
`pg_dist_rag` 0.0.1 install with `CASCADE`, pulling in `vector` 0.8.0, and create the
`mag_catalog` and `dist_rag` schemas.

Known gaps in it, all documented in `DEPLOY.md`: the UI's Cognito settings are baked
into its image at build time, so a data plane authenticates against whatever pool it was
built for; the stack has no Langfuse, so `api_server`'s observability endpoints return
503 there; and a withdrawn database's PersistentVolumeClaim is deliberately left behind
rather than deleted with its StatefulSet.

Two of these were found the hard way and are fixed rather than documented:

- **An outage must not rewind the data plane.** The agent reconciles toward the last
  desired state it *read*, not the stack it was registered with, and that value is
  persisted in the state Secret so a restart mid-outage keeps it. Verified by
  accident: a control plane's namespace was deleted and its agent held six components up
  for two and a half days, logging `holding what we last read` every tick. Before the
  fix, one eviction in that window would have reverted the environment to its
  registration-time stack.
- **An unparseable image is refused, not applied.** A documented example with a `…`
  placeholder, pasted verbatim, produced pods in `InvalidImageName` and components stuck
  at 0/1 with the reason only in pod status. `checkImage` now rejects it with the field
  named. The same class of mistake sank the old console's stack template, so this is the
  second time placeholders have reached a pod spec here.

One sharper edge, worth a fix rather than just a runbook entry: **a `yugabyted` whose
first start fails part-way is unrecoverable by restart.** It applies `YSQL_PASSWORD`
only on first install, so a crash between enabling auth and setting the password leaves
the database on the default password while everything else uses the generated one, and
every restart repeats it. Hit on a kind node that had run out of disk — `yugabyted`
fails inside `first_install_init_auth` with `insufficient disk space`. Today the only
way out is discarding the volume. The agent could close it: on a managed database, when
the configured password is rejected, `ALTER ROLE` it to the Secret's value.

## The agent acts on commands now

*The step before the one above, kept for the record. What it says about the seed — a
`database` plus an `mcp-server` — is what registration used to do; it now seeds one
`meko-stack`. The individual kinds still work and are still how an operator declares
something bespoke.*

Desired state was readable by the agent long before anything could act on it: the
only reconciler produced an empty namespace with no database to put anything in. So an environment could be commanded and quietly do nothing, and
the console's own button said "not available on this control plane".

What changed:

- **Registering an environment is enough.** Registration seeds desired state, so an
  agent that enrolls brings Meko up on its first tick instead of waiting for a
  command: a `database` always, plus an `mcp-server` when the control plane is given
  an image (`--dataplane-mcp-image`). `POST /environments` takes `"desired": [...]` to override per
  environment, and `--no-bootstrap-defaults` turns it off fleet-wide.
- **The agent reconciles the Meko stack.** Kinds: `database` (a StatefulSet it runs,
  or an external one it resolves), `mcp-server` (`meko-mcp-server` over that
  database), `rag-worker`.
- **Datapacks are no longer modelled by YCP.** An environment is the unit; what runs
  inside Meko is Meko's own concern, created through its api-server once the stack is
  up. `cmd/dpmcp` and `internal/mcp` are retired with them, and the `meko/dpmcp` image
  is no longer published.
- **The console registers and observes; it does not command.** `GET
  /api/v1/environments/{id}/desired` was added so what an agent was told can be read
  back, and the console shows exactly that. The editor that briefly sat beside it —
  a JSON textarea that replaced the whole resource set — is gone: opening it before
  the buffer loaded and pressing Apply deleted a running database and mcp-server,
  and its stack template offered `<registry>/…:<tag>` placeholders the browser could
  not fill, one click from a workload that can never pull. Registration is the only
  way the console tells an agent what to run, and registration already knows the
  right answer. Writing desired state stays available over the API.
- **Applies are server-side apply.** The old create-if-absent path accepted a
  changed image, reported it applied, and never rolled it out.
- **State and health are observed, not narrated.** Components carry real replica
  counts read back from the API server, `READY` means every declared component is
  up.

- **The install hint is runnable as printed.** `POST /environments` used to return a
  sketch: a chart that does not exist (`meko/meko-agent` from a `meko` Helm repo),
  `<ns>` and `<this control plane>` left for the reader, and
  `--set agent.enabled=true`, which switches the agent on without switching the other
  seven components off. It now renders the real chart, the URL the caller reached YCP
  on (base path included), and `-f values-agent.yaml`.

Verified on kind three ways — see [`GETTING_STARTED.md`](./GETTING_STARTED.md):
the e2e (level 2, no credentials), the real `meko-mcp-server` image via
`test/local/meko-stack.sh` (level 2.5, needs ECR access), and the operator path via
`test/local/ycp-helm-bootstrap.sh` (level 2.75 — register in YCP, run the Helm command
it prints, and the stack comes up on its own).

The chart change this needs landed alongside, in
[meko-devops](https://github.com/yugabyte/meko-devops): the agent ClusterRole gains
`patch` (server-side apply) plus `statefulsets` and `configmaps`. Without it an agent
enrolls and then 403s on everything it tries to reconcile. `test/e2e/agent.yaml` has
the same set, and `charts/meko/tests/agent-rbac_test.yaml` pins it.

## Deployed (meko-dev-eks)

| Namespace | Contents |
|---|---|
| `ycp-system` | `meko-api-server` (control-plane mode) + `ycp-postgres` StatefulSet, Helm release `ycp` |
| `ycp-agent` | `meko-agent`, Helm release `meko-agent`, enrolled as environment `meko-dev` |

- Console + API: **http://ycp.tail5955.ts.net** — tailnet only (device tagged
  `tag:k8s-meko`), because the console has no auth of its own. Off-tailnet:
  `kubectl -n ycp-system port-forward svc/meko-api-server-service 8080:80`.
- Fleet endpoints need `X-Meko-Admin-Secret`; the value is the `meko-admin-api-key`
  Secret in `ycp-system`. Without it they return 401.
- Control-plane state is a bundled Postgres, not the shared dev YugabyteDB. The
  api_server system migrations need YugabyteDB-only DDL, so control-plane mode runs
  its own portable set (`migrations/controlplane`, `-only-controlplane-migrate`).
- The tailnet Service is chart-managed now, via `apiServer.service.annotations`
  (meko-devops#43) — it survives a rebuild from the chart.
- Both releases were last deployed from branch builds of what is now merged. Rebuild
  the api-server image from `main` before treating the deploy as a release.

## Images

`meko/meko-agent` publishes to
`161908593886.dkr.ecr.us-east-2.amazonaws.com` automatically on merge to `main`
(`.github/workflows/ycp-images.yml`, OIDC — no secrets). `meko/ycp` and
`meko/dpmcp` are no longer built; nothing deploys them.

`meko/meko-api-server` is not published to ECR on merge, but there is now an
**on-demand build** of the control-plane image: `meko/api_server/build-image.sh`
builds it and pushes `quay.io/yugabyte/ycp-api-server` (tags `custom-<sha>` +
`latest`), wrapped by `.github/workflows/meko-control-plane-image.yml` on `main`
and `workflow_dispatch` — build-only, staying green, when the `QUAY_TOKEN` secret
is absent. Before this it was purely hand-built, which caused two false starts —
an image without `--control-plane-mode`, then one without
`-only-controlplane-migrate`. The e2e also builds it from source on every PR
touching `ycp/`, so the flags cannot silently disappear.

The **reconcile-agent** (`cmd/reconcile-agent`, the amp backend) ships the same
on-demand way: `ycp/build-reconciler-agent-image.sh` pushes
`quay.io/yugabyte/ycp-reconciler-agent` (tags `custom-<sha>` + `latest`), wrapped
by `.github/workflows/ycp-reconciler-agent-image.yml` on `main` and
`workflow_dispatch`, build-only when `QUAY_TOKEN` is absent. Quay rather than the
ECR registry `meko-agent` uses, so a cluster pulls it without ECR cross-account
pull-secret setup — the same reason the control plane ships on Quay.

## Open

| | |
|---|---|
| meko-devops#42 | Chart ergonomics, remainder. `mode: agent`, the mode presets and the runbook landed (#40, #41, #43, #47); Secrets are still out-of-band and the storage class is still per-cluster |
| meko#401 follow-ups | The lockout class is gone outright: the agent authenticates every call with a YBM API key and enrollment is an idempotent check-in, so there is no earned credential to lose and nothing to rotate. Still open in the design: no CA pinning, and `cert_fingerprint` is decorative |
| [meko#572](https://github.com/yugabyte/meko/issues/572) | Nothing publishes `meko/meko-api-server`. The control plane is the one YCP component whose image is hand-built — and the pinned data-plane release now names a commit build of it, so every registered environment depends on a tag no CI job produced |

Known gaps not yet filed:

- `langfuse` has no reconciler. Langfuse v3 needs ClickHouse, Redis and object
  storage alongside it, so it is a chart's job rather than three more spec structs
  here; a declared `langfuse` resource is logged and skipped.
- Resizing a managed database is rejected on every tick: `volumeClaimTemplates` are
  immutable, so a changed `storage_size` cannot be applied in place. The agent now
  says so explicitly and keeps the rest of the stack running, but it will not
  recreate the StatefulSet — deleting the workload that owns the data is an
  operator's decision.
- `console.html` is duplicated between the retired `ycp/web/` and
  `api_server/internal/handlers/web/` — with `ycp/web/` deleted this is now a single
  copy, which is the fix.
- Agent defaults are demo residue: `ENV_NAME=itest-mcs`, `ENV_PROVIDER=gke`,
  `ENV_REGION=us-west1`, `AGENT_VERSION=v1.4.2` hardcoded, and `DPMCP_IMAGE`
  defaulting to `postgrest/postgrest`. The chart sets all of them, so nothing is
  broken; an unset one enrolls as the wrong environment rather than failing.
- On a transient control-plane error the agent used to garbage-collect everything it
  manages; the desired-state guard now skips the tick instead. The GC is still
  unconditional on a *successful* empty desired state, which is correct but
  unforgiving.

## Next

1. Rebuild and push `meko/meko-api-server` from `main`, then redeploy `ycp-system`
   off a real tag instead of a branch build.
2. ~~Wire an api-server image build into CI~~ — done: `meko/api_server/build-image.sh`
   + `meko-control-plane-image.yml` publish `quay.io/yugabyte/ycp-api-server`. Still
   to do: configure the `QUAY_USER`/`QUAY_TOKEN` robot secret so the push is not
   skipped.
3. meko-devops#42 remainder — Secrets, and a storage class that is not per-cluster
   guesswork.

## amp environments

YCP now registers **amp** environments too, not just meko ones — an amp
environment is a running **amp controller** that reports the storage pools,
databases, branches and compute endpoints it owns. Registration takes a
`kind` (`amp` | `meko`, default meko); a meko id stays a bare `<slug>` while an
amp id is prefixed `amp-<slug>`, and the enroll handshake checks the kind, so the
two coexist in one fleet and neither attaches to the other's environment. An amp environment seeds no `meko-stack`;
its install hint installs the `yb-amp-controller` chart with the reconcile-agent
(`agent.enabled=true`) turned on — the agent is the sole YCP client and drives the
controller over its REST API. The controller's design for this is
[`amp/controller/architecture/ac-ycp.md`](../amp/controller/architecture/ac-ycp.md).
