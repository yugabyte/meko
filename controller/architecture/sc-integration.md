# Storage-controller integration — API surface inventory

Working doc for integrating the `storage_controller` (Rust, "SC") directly into
the amp controller (Go, `controller/`). It inventories which SC HTTP endpoints
are called, split by caller, because that split determines what can collapse into
in-process calls versus what must remain an HTTP surface after integration.

**Current as of `origin/main` `b1aa32c9c`** (2026-07-02). The model changed
substantially from this doc's first draft — `[#168]` collapsed
`(tenant_id, timeline_id)` into a single flat **`branch_id`**, `[#171]` deleted
the safekeeper registry, `[#16]` + upcall removals gutted the pageserver control
path, and `[#113]` moved amp's mutating APIs onto the write-ahead task framework.
Everything below reflects the branch model.

Source of truth for the SC API is
[`../../architecture/storage-controller-apis.md`](../../architecture/storage-controller-apis.md)
(router in `storage_controller/src/http.rs`). Work items are tracked in
[`sc-integration-worklist.md`](sc-integration-worklist.md).

## What the SC is now (current role)

A **cluster control-plane registry + generation authority**, off the data path
(no WAL/pages flow through it). All state is in YugabyteDB (`storage_controller`
schema in the shared `yb_storage` DB). amp runs it as a **managed subprocess**
(`controller/internal/storagecontroller/`: `bin.go` locates the binary,
`storagecontroller.go` supervises it, `client.go` is the HTTP client), sharing
amp's YB database.

It persists 7 branch-keyed tables: `branches` (the core row — **generation**,
generation_pageserver, placement/scheduling policy, config; the old `tenants`
table flattened), `timelines` (start_lsn, generation, deleted_at, branch_name),
`pending_timeline_inits` (the init recipe), `endpoints`, `nodes` (pageserver
identities), `metadata_health`, `timeline_imports`. **No `tenants` table**
(`#168`), **no `safekeepers` table** (`#171`).

What it actively does: **generation authority** (attach-hook → `branches.generation`),
pageserver **node registry** + a **PS-only heartbeater** (`#171` removed the SK
one), the **pending-init recipe registry**, **branch register / drop**, the
**endpoint registry**, and a **vestigial scheduler** (survives to pick a node at
branch create; amp doesn't drive that path). Removed vs upstream: the pageserver
reconciler (`#16`, ~8.4K lines), sharding (`#168`), the safekeeper
registry/heartbeat/`sk_set`/quorum (`#171`), the `re-attach`/`validate` upcalls,
membership-switch, storage broker, multi-controller leadership, drain/fill/migrate.

## SC remaining role & the integration end-state

Responsibility-by-responsibility, everything amp needs from the SC is now either
CRUD over YB tables or a counter — there is no non-trivial SC logic amp depends on:

| SC responsibility | amp needs it? | Plan |
|---|---|---|
| Generation authority (attach-hook → `branches.generation`) | yes | amp owns it — a DB counter (see [`sc-integration-migration.md`](sc-integration-migration.md)) |
| Pending-init recipe (`pending_timeline_inits`) | yes | amp owns the table (trivial CRUD; already drives it) |
| Branch records (`timelines`: start_lsn, branch_name, drop) | partly | trivial; largely redundant with amp's own `branch` table |
| Endpoint registry (`endpoints`) | no | amp already owns endpoints in its own schema |
| Node registry + PS heartbeater (`nodes`) | **kept for bookkeeping** | see decision below |
| Metadata health / scrubber | not yet | defer |
| Timeline imports | not in scope | defer |
| Scheduler (`any_available_node`) | no | amp picks the node (it deploys the endpoint) |
| `/v1` virtual-pageserver passthrough | no | amp reaches PS/SK directly via `node.Client` |

**End state:** amp subsumes the SC's amp-facing responsibilities **in Go** (table
CRUD + the generation counter), backed by its own YB schema. Nothing amp needs
requires the live SC subprocess or embedding the Rust `Service` (the heavy option
we worried about early on). The `storage_controller` binary is demoted to a
**dev/test artifact** — still driven by `yb_light_local` + `test_runner` on their
disjoint branches — not amp's runtime dependency.

**Decision — keep the node registry + heartbeater.** `node_register` is
bookkeeping-only today: no control action consumes node availability (post-`#16`
there's no reconciler; heartbeat transitions only null `observed.conf` or log —
`service.rs:3749`). Even so, amp **retains a node registry + liveness heartbeat**
for bookkeeping/observability (which pods exist, are they reachable), reimplemented
as a Go concern. It is *not* a reason to keep the SC subprocess.

Caveat: the pageserver still wants `control_plane_api` set but makes **no runtime
callbacks** (`controller_upcall_client` removed), so a placeholder URL is fine —
dropping the subprocess doesn't break the pageserver.

## A. Called by the amp controller directly (Go `controller/`)

All through `internal/storagecontroller/client.go`, wired as
`api.Provisioning.SC` (`*storagecontroller.Client`), constructed in
`cmd/controller/main.go` from `cfg.StorageController.ListenAddr`.

| Method + path | Go client method | Call sites | Status |
|---|---|---|---|
| `GET /ready` | `Ready` | `api/server.go:58` (startup gate), `process/supervisor.go:175` (subprocess readiness probe) | **live** |
| `POST /control/v1/branch/{branch_id}/pending_init` | `RecordPendingTimelineInit` | `cmd/controller/main.go:70` via `scRecipeStore.RecordBranchRecipe` → the task framework's `RecipeStore` port (`#113`) | **live** |
| `DELETE /control/v1/branch/{branch_id}/pending_init` | `DeletePendingTimelineInit` | `cmd/controller/main.go:79` via `ClearBranchRecipe` | **live** |
| `GET /control/v1/pending_timeline_inits` | `ListPendingTimelineInits` | defined; **not yet wired** into a production path | new |

That's the whole Set A: amp uses the SC as a **readiness-gated pending-init
recipe registry**. The three dead `/v1/...` create/delete methods this doc used
to flag (`CreateTenant`/`CreateTimeline`/`DeleteTimeline`) **are gone** — removed
upstream in the `#168`/`#171` cleanups. `ListPendingTimelineInits` was added but
has no caller yet (likely for a future startup-reconcile of recipes).

### Plus one direct-to-safekeeper call (not the SC)

Via `internal/node/client.go` — amp reaching an endpoint's node directly, not
through the SC. It reads the ancestor safekeeper's `flush_lsn` to resolve a
branch's default fork point (co-located topology; the SC's own describe would
route to the wrong pageserver once a branch has >1 endpoint, `#133`). This is the
safekeeper's own HTTP API, out of SC scope; it stays as-is after integration.

## B. Called by the endpoint pods (`deploy/endpoint/supervisor.py`)

Only when `STORAGE_CONTROLLER_URL` is set (SC-coordinated mode), in start order.
After `#171` this is **3 endpoints**:

| Method + path | Purpose | Scope | Required? |
|---|---|---|---|
| `POST /control/v1/node` | Register this pod's pageserver node (`supervisor.py:546`) | Infra | **required** |
| `POST /debug/v1/attach-hook` | Bump the pageserver generation; body `{branch_id, node_id}`; returns `gen` (`supervisor.py:553`) | Admin | **required** (generation fencing) |
| `POST /control/v1/branch/{branch_id}/register` | Record the branch's `start_lsn` (`supervisor.py:694`) | Admin | best-effort (409 on restart is OK) |

> ℹ️ **supervisor.py drift (resolved):** the dead /control/v1/safekeeper/{id} and /control/v1/safekeeper/{id}/scheduling_policy calls, along with the sk_set field in the branch-register body, have been removed to align with #171.

**Not callers of the SC** (verified): `compute_ctl` runs with a local `--spec`
file, so it never calls the SC; the pageserver/safekeeper Rust binaries make
**no** runtime callbacks (`controller_upcall_client` removed, `/upcall/v1/*`
handlers vestigial).

### Set B is not just `supervisor.py` — caller map

Each Set B endpoint also has a parallel caller in the **Rust `control_plane` dev
harness** (`yb_light_local` + its `StorageController`/`PageServer` wrappers) and
the **`test_runner` integration fixtures**. All are **out-of-process** clients; none live
in the daemon crates (`compute/pageserver/src`, `compute/safekeeper/src`, `compute/compute_tools/src` —
verified empty).

| Endpoint | Pod (`supervisor.py`) | Dev harness (`control_plane/`) | Tests |
|---|---|---|---|
| `POST /control/v1/node` | `:546` | `node_register` (`storage_controller.rs:1038`, called from `pageserver.rs:~407`) | `test_runner` fixtures |
| `POST /debug/v1/attach-hook` | `:553` | `attach_hook(branch_id, …)` (`storage_controller.rs:976`, called `pageserver.rs:407`) | `attach_hook_issue`/`_drop` (many regress tests) |
| `POST /control/v1/branch/{id}/register` | `:694` | `timeline_register` (`storage_controller.rs:1070`, called `yb_light_local.rs:1806,1963`) | — |

Implications: (1) `supervisor.py` and the `control_plane` Rust launcher are two
parallel implementations of the same register→attach→register-branch sequence;
(2) there is **zero in-process Rust coupling** — Set B is purely an external
client/server contract; (3) at least three client classes must keep working (pod
supervisor, `yb_light_local`, integration fixtures), with `attach-hook` the most heavily
exercised.

## Set B handler trace (what logic each route runs)

Handlers in `storage_controller/src/http.rs` are thin shims (permission check →
JSON parse → delegate to `state.service.<method>()`); the logic lives in
`Service`/`persistence`. What would move in-process is the **service method**.

| Endpoint | Handler (`http.rs`) | Service method | What it does | State touched |
|---|---|---|---|---|
| `POST /control/v1/node` | `handle_node_register` (681) | `node_register` (`service.rs:3504`) | dedup vs existing reg (**409** on addr mismatch), **DNS-resolve** the HTTP host (`service.rs:3571`), persist node, insert in-mem | DB `nodes` + in-mem `nodes` + scheduler `node_upsert` + node lock + DNS |
| `POST /debug/v1/attach-hook` | `handle_attach_hook` (159) | `attach_hook` (`service.rs:777`) | auto-create **branch** row if missing (DB+mem), **`increment_generation`** on `branches` (`persistence.rs:704`), update placement policy, set in-mem scheduler intent; **returns new gen** | DB `branches`+gen + in-mem `branches`/scheduler + branch lock |
| `POST /control/v1/branch/{id}/register` | `handle_timeline_register` (608) | `timeline_register` (`safekeeper_service.rs:18`) | branch-name uniqueness check, **`insert_timeline`** (idempotent); `sk_set` no longer in the request | **DB `timelines` only — no in-mem** |

Auth scopes: node=`Infra`, attach-hook=`Admin`, branch-register=`Admin`.
`timeline_register` is nearly pure-DB (easiest to reimplement in Go);
`attach_hook`/`node_register` are coupled to the Service's in-memory state
(`self.inner` → nodes/branches/scheduler via `parts_mut`) and have side effects
(generation increment, DNS check).

## Can Set B go in-process like Set A?

Not the same way — the *caller* differs. Set A collapses to in-process calls
because **amp is the caller**. For Set B the caller is the endpoint pod's
`supervisor.py`, a **separate container on another host**; you can't make a
remote process's call in-process, so the pod→SC hop stays a network API. And
Set B isn't pod-only: `yb_light_local` and the `test_runner` integration suite call the
same routes over HTTP, so the routes can't be deleted without migrating them too.

The real version of the idea is **invert ownership**: have amp perform the
registrations in-process at deploy time (it already knows the pod's addresses —
it sets `SC_REACHABLE_HOST` + the published ports — and can compute `NODE_ID`).

- `node_register` — ✅ feasible, **but** the handler DNS-resolves the node host,
  so amp must register only after the pod's DNS name exists (ordering).
- `attach-hook` — ✅ feasible, **but** the generation must be in `pageserver.toml`
  before the pod's pageserver starts: register node → attach-hook in-process →
  inject the resulting `GENERATION` into the pod spec → deploy. (Pod already reads
  a `GENERATION` env, default 1.)
- `branch register` — ⚠️ the holdout: needs `start_lsn`, which for a fresh
  bootstrap is the pageserver's `last_record_lsn`, produced *inside the pod* after
  the branch goes Active (`supervisor.py:670`). amp only learns it after boot (it
  can read it from the safekeeper via `node.Client`). Most naturally stays
  pod-driven, or becomes an amp post-deploy step.

Net: (1) pod→SC stays a network surface regardless; (2) ownership inversion can
shrink what the pod calls, with `node_register`/`attach-hook` carrying ordering
constraints and `branch register` the holdout; (3) the HTTP routes survive for
`yb_light_local` + integration tests either way; (4) "in-process" still presumes the
logic is reimplemented in Go (or the Rust `Service` is embedded) — today it is
bound to the Rust in-memory `Service` state + diesel persistence, which is the
actual work. Because the SC has been hollowed to registry + generation-increment
+ PS heartbeat + a vestigial scheduler over 7 YB tables, that reimplementation is
a much smaller target than it first appeared; the genuinely irreducible piece is
the generation authority (attach-hook).

## Design decisions

- **[2026-07-02] No per-restart generation bump.** When `attach-hook` moves into
  amp's deploy code, the generation is minted **once per deploy** and injected
  into the pod's `GENERATION` env. A pod *self-restart* reuses that generation —
  the pageserver's attach CAS treats `stored == ours` as an idempotent re-attach
  (`cas_bump_generation_on_attach`, see Appendix). We are **not** preserving the
  current pod behaviour of bumping the generation on every process restart. This
  is safe under the **single-writer-per-branch** topology: one pageserver per
  branch, a restart replaces the same writer, and any attach on a *different* node
  routes through amp's deploy, which mints a fresh generation. Revisit only if the
  topology ever allows more than one pageserver per branch. *(Resolves the
  "generation-bump-on-restart" design question — decided: skip.)*

- **[2026-07-02] `node_register`'s DNS check is the storage controller's concern,
  not the pod's.** The pod only *declares* its address; the SC's handler
  (`service.rs:3571`) runs `lookup_host` on the declared HTTP addr **from its own
  network view** and 503s if it doesn't resolve, so it can later heartbeat the
  node. It checks the HTTP host only (the PG host may be client-only resolvable)
  and does *not* require the node to be online (availability starts `Offline`).
  Consequence for moving the call into amp: the resolver is still the co-located
  SC, so the only new requirement is **ordering** — amp must create the pod's
  network identity (Docker container / k8s Service, what makes `SC_REACHABLE_HOST`
  resolvable) *before* calling `node_register`. Sequence: create container/Service
  → `node_register` → `attach-hook` → inject `GENERATION` → finish deploy. *(This
  is the "node_register DNS ordering" design question — understood, not a blocker.)*

## Appendix: attach-hook & generation fencing

The attach-hook (`POST /debug/v1/attach-hook`, body `{branch_id, node_id}`,
returns `{gen}`) is the keystone of Set B. Despite the `/debug/v1/` path and the
"test hook" comment in the SC source, it is **load-bearing** here: upstream
Neon's pageserver called the controller back via `controller_upcall_client`
(deleted in this fork), so the launcher/pod drives the attach-hook explicitly
("Generation handoff is launcher-owned", `yb-pageserver.md:61`).

What it does, server side (`service.rs:777` → `persistence.rs:704`):

1. **Auto-creates the branch** row if missing (placement `Attached(0)`, gen 0) —
   which is why amp needs no separate "create branch" call; first attach
   materializes the registry row.
2. **Bumps the generation atomically**: `UPDATE branches SET generation =
   generation + 1, generation_pageserver = <node_id> RETURNING generation`. The
   SC is the single authority issuing a monotonic per-branch counter; it returns
   the new value, which the pod bakes into `pageserver.toml` before starting the
   pageserver.

Why it exists — **generation fencing**. Every endpoint runs its own pageserver,
and they all write to the *same shared YB* (`pageserver.pages`,
`pageserver.timeline_state`). On restart / re-attach / partition, two pageservers
can transiently both think they own a branch; concurrent writes would corrupt it.
The generation is the fencing token (`yb-pageserver.md:634` "Generation fencing"):

- On attach the pageserver CASes its gen into `timeline_state.current_generation`
  (`cas_bump_generation_on_attach`, `WHERE current_generation < $new`; forward-only).
- Every durable flush is gated `WHERE current_generation = our_gen`
  (`advance_consistent_lsn`). A stale pageserver matches 0 rows → `StaleGeneration`
  → tears the branch down. So only the most-recently-attached pageserver can
  advance durable state; older incarnations are locked out at the storage layer.

Migration consequence (see "Can Set B go in-process"): attach-hook's *return
value must reach the pageserver before it writes*, so moving it into amp means
calling it and injecting `GENERATION` into the pod spec **before** deploy. Moving
it is safe **only** because of the single-writer-per-branch topology: a pod
self-restart reusing its generation is an idempotent re-attach (CAS sees
`stored == ours`); the fence only does real work when a *different* node attaches,
and that always routes through amp's deploy. That single-writer assumption is the
thing to keep verifying.

## Open follow-ups

- ~~Trace each Set-B endpoint into its `http.rs` handler.~~ **Done** — see "Set B
  handler trace" and "Can Set B go in-process".
- ~~Confirm/handle the three dead `/v1/...` client methods.~~ **Moot** — removed
  upstream in the `#168`/`#171` cleanups.
- ~~**supervisor.py drift:**~~ Resolved in this PR (removed dead safekeeper calls and sk_set).
- ~~**Stale prose in storage-controller.md:**~~ Resolved in this PR (updated intro and heartbeat sections).
- ~~**Re-verify task/deps.go doc references:**~~ Resolved in this PR (updated references to controller-project-create.md).
