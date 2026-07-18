# Controller TODO — hand-off snapshot

**Purpose:** a hand-off summary of forward-looking control-plane / controller work
items so someone picking this up has the context in one place. This is a **point-in-time
snapshot (2026-06-30)**, not a live tracker — the **GitHub issues are the source of
truth**. If this doc and an issue disagree, trust the issue.

All items are area **`control plane`**, milestone **Early Access** (see "Milestones" at
the bottom). They are the prerequisites for putting AMP in front of external users.

---

## 1. User authentication for the control plane — [#189](https://github.com/yugabyte/amp/issues/189)

The control plane has **no auth today**. `controller/internal/api/openapi_spec.yaml:11`
says it outright: "there is no auth yet (the user/grant model is designed but
authentication is deferred)", and "Until auth lands, anyone may create an admin". The API
(`controller/internal/api`), the UI (`controller/internal/api/ui`), and the MCP server
(#114) all trust a client-supplied `user_id`.

**Key decision to make first:** does AMP **own** auth (local accounts / API keys / OIDC)
or **delegate** it to **YBA** or **Meko**? Record it as a short ADR under
`controller/architecture/`, then build the auth middleware (today `server.go` wires only
Recover / RequestID / logging) and map the authenticated principal onto the *existing*
designed user/grant model.

**Not the same as #123**, which is client auth *to the Postgres computes* (`pg_hba` /
LDAP, data plane). #189 is auth *to the control plane* (API / UI / MCP).

---

## 2. MCP server wrapper around the AMP API — [#114](https://github.com/yugabyte/amp/issues/114) (already tracked)

The MCP server itself is tracked by **#114** (thin CRUD wrapper over storage pools,
timelines, endpoints; modeled on the meko MCP server).

**Added for hand-off:** the deployment topology — it should run in **its own pod,
alongside the controller, on k8s** (not embedded in the controller process). That deploy
artifact (a chart/pod for the MCP server) does not exist yet; it's captured as a component
of the foundation installer (#193) and noted in a comment on #114. Auth for it is #189.

---

## 3. Idle CPU footprint of an endpoint pod — [#190](https://github.com/yugabyte/amp/issues/190)

To plan fleet density (how many extra / pre-warmed endpoints we can pack — feeds #192) we
need the real idle cost of one endpoint pod. The chart currently sets **no requests /
limits** (`resources: {}` in `deploy/helm/yb-amp-endpoint/values.yaml`).

One endpoint = one pod running the **full co-located stack** under
`deploy/endpoint/supervisor.py` (pageserver, safekeeper, endpoint_storage, compute_ctl,
Postgres). Measure idle CPU + RSS of that stack, set chart `resources` defaults from it.

**Expected real constraint:** the kubelet **~110-pods-per-node** ceiling will very likely
bind before CPU does — so plan "pods/node first, CPU second", but quantify both.

---

## 4. Configurable endpoint connectivity — [#191](https://github.com/yugabyte/amp/issues/191)

Today connectivity is per-endpoint only: each endpoint gets its own k8s `Service`
(`deploy/helm/yb-amp-endpoint/templates/service.yaml`), `ClusterIP` by default with
`NodePort` / `LoadBalancer` selectable. No external front door, no shared routing layer —
a LoadBalancer-per-endpoint won't scale to a large fleet.

Two directions, make the mechanism **configurable** per deployment:
- **Ingress for direct connect** — Postgres-wire ingress / TCP routing so a client reaches
  a specific endpoint from outside the cluster, keyed by endpoint identity (SNI/host/port),
  without one cloud LB each.
- **Pooler-based shared LB** — investigate YugabyteDB's connection pooler (YSQL Connection
  Manager) as the base for a shared, AMP-exposed LB fronting many endpoints.

---

## 5. Make endpoint creation appear instant — [#192](https://github.com/yugabyte/amp/issues/192) (epic)

Customer "create endpoint" should feel instant. Today it provisions a fresh pod end-to-end
(schedule + image pull + bring-up + bootstrap) via
`controller/internal/task/step_create_endpoint.go` — seconds-to-minutes.

**Plan:** keep a pool of pre-created **unconfigured ("warm") endpoint pods**; "create
endpoint" **reconfigures a warm pod** instead of deploying a new one. Needs an **endpoint
fleet manager** → this is an **epic**, expect multiple tickets.

**Key investigations:**
- **Config delivery:** static spec/config file + restart **vs.** compute_ctl's config-URI
  fetch (`compute/compute_tools/src/bin/yb-compute-ctl.rs` `control_plane_uri` /
  `get_config_from_control_plane`). Today config is baked into a ConfigMap at startup
  (deploy-time, not reconfigure-time).
- **Forcing a refresh:** compute_ctl already has a reconfigure route
  (`compute/compute_tools/src/http/routes/configure.rs`, `ConfigurationRequest`). Work out how to
  drive a warm pod unconfigured → serving a branch, ideally without a process restart.

Sub-tickets to split out: fleet manager · warm-pod spec · reconfigure path · controller
create-endpoint rework · density input from #190.

---

## 6. Foundation install workflow — [#193](https://github.com/yugabyte/amp/issues/193)

A repeatable one-shot install of a whole AMP **foundation** onto a fresh k8s cluster
(somewhat outside the controller proper — it's the bootstrap that creates the controller).

**Components:** RF3 YugabyteDB universe (the durable backend — AMP doesn't run YB itself;
the endpoint chart just points at one via `yugabyte.hostPorts`) · amp controller
(`deploy/controller`, see `controller/architecture/ac-k8s-deploy.md`) · MCP server (#114,
own pod) · UI (`amp_ui`, own pod) · namespace + RBAC + pull secret + networking.

**Gap today:** the `deploy/helm/yb-amp-controller` chart now packages the controller, the
MCP server, and the `amp_ui` UI as their own pods (RBAC + Deployments + Services), and the
endpoint chart (`deploy/helm/yb-amp-endpoint`) covers endpoints — but
`deploy/controller/redeploy.sh` still assumes the namespace, backing YB, and pull secret
already exist. `deploy/up.sh` stands up a *single-node* YB + namespace + pull secret for dev;
no single workflow provisions the productized RF3 foundation. Related: #81 (yb_amp_local
deploy self-sufficiency).

---

## 7. Integrate the controller in-process — [#202](https://github.com/yugabyte/amp/issues/202) — DONE

The `storage_controller/` crate is removed. The controller now owns the
storage-layer control functions in Go: generation authority (attach-hook), the
node/branch/endpoint registries, and provisioning of `yb_storage` (all schemas)
via its golang-migrate migrations. It serves the pod-facing surface
(`/control/v1/...`, `/debug/v1/attach-hook`, `/ready`) on its API listener —
`controller/internal/registry/` — and the task framework's recipe store is an
in-process call over the same tables (`controller/architecture/sc-integration.md`).
`yb_amp_local` and the integration harness launch the `controller` binary in
place of the old `storage_controller`. One binary, one process, one migration system.

---

## Milestones

All seven are filed under **Early Access** (due 2026-07-31) — the "before external users"
bucket. The other open milestone, **K8 Deploy** (due 2026-07-07), is the *initial manual*
full-stack bring-up. #193 (the productized foundation installer) is the closest call and
could reasonably move to K8 Deploy if it's wanted sooner.
