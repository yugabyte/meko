# Two-step migration — move `node_register` + `attach-hook` into amp

Scoping for moving the endpoint pod's SC registration calls out of
`deploy/endpoint/supervisor.py` and into amp's deploy code, so they become
in-process calls after integration. Companion to
[`sc-integration.md`](sc-integration.md) (inventory) and
[`sc-integration-worklist.md`](sc-integration-worklist.md) (checklist).

**Scope:** `node_register` + `attach-hook` (+ generation handoff) only.
`branch register` stays pod-driven — its `start_lsn` is the pageserver's
`last_record_lsn`, produced *inside* the pod after materialization
(`supervisor.py:670`); amp can't supply it at deploy time.

## The two steps

1. **Relocate (SC still a subprocess).** amp's deploy code makes `attach-hook` +
   `node_register` via the existing loopback `storagecontroller.Client`; the pod
   stops self-registering; amp injects the resulting `GENERATION` into the pod.
   No wire/behaviour change visible to the SC — same HTTP calls, different caller.
2. **Collapse (at integration).** Those two loopback client calls become direct
   in-process `Service` calls (or Go reimplementations). The pod is unaffected —
   it already just reads a `GENERATION` env. This step is cheap *because* step 1
   already localized the calls to amp.

## Strategic direction: create-then-configure (supersedes the Option C recommendation)

The longer-term goal is a **pre-warmed pool**: endpoints created but *unassigned*,
then bound to a branch on demand for fast turnaround. That reframes this
migration, because **the generation is an *assign-time* value, not a create-time
one** — a warm, unassigned pod has no branch, so there is nothing to attach and no
generation to mint until assignment. The generation's job at assignment is to
*fence the pageserver that previously served that branch*, which is exactly what
makes dynamic reassignment safe.

Consequences:

- **Option C (bake `GENERATION` into env at create) is a dead-end for pre-warm** —
  it only works when create == assign. Keep it as the *simplest interim* shape,
  but don't over-invest: it is thrown away once create and assign split.
- **The target is Option A as a push/configure API**: amp selects a warm pod and
  pushes an assignment (`POST /configure {…}`) to it at runtime; the generation is
  one field of that payload. Framed this way, A is not an "awkward startup
  barrier" but the configure primitive the pool needs anyway — and the first
  version (create-then-immediately-configure) is throwaway-free.
- **Caveat — the pageserver is spawn-time configured** (single-branch process, gen
  baked at spawn; no runtime `location_config`/attach endpoint — verified). So
  "configure a warm pod" today means *respawn its storage processes* with the new
  branch+gen; the container/image/base stays warm (still a big win vs a cold
  container), but it is not a zero-cost in-process reattach. True instant
  reassignment would need a new runtime-attach API on the pageserver — separate,
  larger work.

Generation-delivery mechanisms for the runtime (Option A) path:

| | Mechanism | Driver-agnostic | Notes |
|---|---|---|---|
| **A1 pull-from-amp** | pod GETs its `(branch, gen)` from an amp endpoint | ✅ | smallest pod change; pod→amp control call. Good stepping stone. |
| A2 pull-from-SC | pod reads `branches.generation` (amp does the write) | ✅ | keeps pod→SC; racy |
| A3 push-to-pod ⭐ | amp POSTs `/configure` to a pod listener | ✅ | inverts direction; **the natural pool/configure shape** |
| A4 file/volume | amp writes a file the pod reads | ❌ Docker-only | breaks k8s |

## The configure contract

Split the endpoint's fields into **create-time** (pod/cluster-static) and
**configure-time** (per-assignment). Configure carries only the second set.

Create-time (set once per warm pod; not in configure):

- `node_id` — the pod's stable pageserver/safekeeper identity. **Change from
  today:** currently derived from `BRANCH_ID` (`supervisor.py:112`), which only
  works because create == assign; in a pool it must be **per-pod and stable** so
  one pod serves different branches over its life.
- YB connection block, listen ports, compute port/password, image, the configure
  channel itself.

Configure request (per-assignment):

| Arg | Today's field | Notes |
|---|---|---|
| `branch_id` | `BRANCH_ID` | branch this pod now serves (storage hex32 id) |
| `generation` | `GENERATION` | fence token amp minted via `attach-hook(branch_id, node_id)` — the reason configure is a runtime call |
| `role` | `CreateEndpointArgs.Role` | `read_write`\|`read_only`; **not yet passed to the pod** (`EndpointSpec` has no role) — configure would start delivering it |
| `pg_version` | `ENDPOINT_PG_VERSION` | really a **pool selector** (fixed PG binary per pod); passed for match/validation |
| shape: `bootstrap` \| (`ancestor_branch_id`,`ancestor_lsn`) \| attach-existing | `BOOTSTRAP` / `ANCESTOR_*` | mutually exclusive |
| `branch_name` *(opt)* | — | if the SC branch-name mapping is set at configure |

Configure response:

- **`start_lsn`** — the materialized branch's start LSN (pageserver
  `last_record_lsn`). This **folds in the `branch register` holdout**: instead of
  the pod calling the SC, configure *returns* `start_lsn` and amp records it
  in-process. The lone pod-runtime output.
- ready/health + the compute connection address.

## Who mints what — amp owns the ids, the data plane only enforces

Neither id needs data-plane participation to be *created*; both can be minted in
amp and passed in.

- **`branch_id` — amp already mints it.** A `uuid.New()` in the controller
  (`task/tasks.go:75`, `core/pg.go:148`), rendered to 32-hex via `hex32`; the
  storage layer carries it verbatim (the SC's `branches` row is even lazily
  auto-created from the supplied id by `attach-hook`, `service.rs:815`). The data
  plane never creates it — pass-in is the existing design.
- **`generation` — the SC mints it as a durable per-branch DB counter**
  (`increment_generation`, `persistence.rs:704`: `UPDATE branches SET generation =
  generation + 1 … RETURNING`), seeded to 0 at branch-row creation. The **data
  plane only receives and enforces** it: the pageserver CASes it forward-only into
  `timeline_state.current_generation` and gates every write on
  `current_generation = our_gen`; its CAS doesn't care *who* produced the number,
  only that it's strictly greater than what's stored. So **amp can own the
  counter** — moving `branches.generation` into amp's universe *is* integrating
  the generation authority. The only invariant: *each attach of a branch gets a
  strictly greater integer than every prior attach — durably, atomically, across
  restarts and concurrent configures.*
- **Nuance — monotonicity, not contiguity.** Generation *gaps* are harmless
  (nothing needs +1 exactly). So the repo's "never `SET col = col + 1`"
  idempotency rule doesn't bite here: a retry that double-increments just burns a
  number; it never reuses or regresses one. Use `RETURNING` and hand the pod the
  value actually observed; an orphaned gen from an ambiguous-commit retry is fine.
  The only fatal outcome is issuing a value ≤ one already handed out.
- **`start_lsn` — the lone data-plane output.** For a **bootstrap** branch it is
  produced inside the pod by `initdb` (the pageserver's `last_record_lsn`), so amp
  cannot mint it; it comes back in the configure response. (For a **fork**,
  `ancestor_lsn` is an input amp already resolves.)

Summary: amp mints everything on the configure **request** side (`branch_id`,
`generation`, role, shape); the data plane contributes only `start_lsn` on the
**response** side.

### Decision: generation counter ownership (greenfield)

Assuming a **greenfield deployment** (no legacy customers, no upgrade path), amp
owns the generation counter directly:

- **Storage:** a new, minimal table in amp's own control schema, keyed 1:1 by
  `branch_id`, holding just the counter: `(branch_id, generation int4)`.
  Branch-scoped so it **outlives endpoints** — the generation must keep advancing
  across endpoint recreation / failover / reassignment, so it cannot live on the
  churny `endpoint` row. Owned by amp's golang-migrate migrations; no diesel, no
  `storage_controller.branches` involvement.
- **Type:** `int4` (treated as `u32` on the Rust side), matching the generation
  baked into `pageserver.toml` and the data-plane `timeline_state.current_generation`.
- **Increment:** single-row `UPDATE … SET generation = generation + 1 … RETURNING
  generation`, serialized per branch by the row lock. Gaps are harmless (fencing
  needs monotonicity, not contiguity), so `+1` is safe under the connection-drop
  replay contract — always use the observed `RETURNING` value.
- **Task-step idempotency:** mint → persist the value on amp's row → hand to the
  pod, and **reuse the recorded value on step replay** (a re-mint is safe but would
  drift the pod's baked generation from what amp recorded).
- **Greenfield effect:** amp is the sole issuer from each branch's first attach, so
  there is no cross-system high-water seeding; the only residual requirement is
  that the counter is durable across amp restarts (it is a DB row → satisfied).
  The two-step relay→mint staging is therefore *optional* — an engineering-risk
  choice, not a data-safety requirement.
- **Single-issuer invariant:** amp is the sole issuer for amp-managed branches; the
  SC's `attach-hook` / `branches.generation` counter is not in amp's path (it
  remains only for `yb_light_local` / tests on their disjoint dev/test branches).

### Phase 1 decisions (2026-07-02)

- **NODE_ID allocation → per-pod, via a small allocator/sequence.** amp assigns
  the pageserver/safekeeper node id (the identity the generation is recorded
  against), **decoupled from branch and endpoint**, so a pooled pod that outlives
  any one branch keeps a stable id and pre-warm needs no rework. This replaces the
  pod's current `branch_id`-derived `NODE_ID` (`supervisor.py`), which would
  collide/reuse on reassignment. Type `i64` (the SC `NodeId`), passed to the pod
  as the `NODE_ID` env.
- **Issued generation persisted on the `endpoint` row.** The `branch_generation`
  counter holds the *next* value; the value actually handed to a given pod is
  recorded on that endpoint's row, so a create-endpoint task-step replay **reuses**
  it (reads it back) rather than re-minting. Deterministic; a re-mint would be safe
  (gaps harmless) but would drift the pod's baked generation from what amp
  recorded.

## Current flow (what changes)

- Pod: `supervisor.py:sc_register_and_attach()` does `node_register` →
  `attach-hook` (returns gen) → bakes gen into `pageserver.toml` → starts the
  pageserver. Gated on `STORAGE_CONTROLLER_URL` being set. It also derives
  `NODE_ID` from `BRANCH_ID`.
- amp deploy (`docker.go` / `kubernetes.go`): sets `STORAGE_CONTROLLER_URL`,
  `SC_REACHABLE_HOST` (= container name / Service DNS), the `*_PUB_PORT`s.
- `step_create_endpoint.go`: `deployer.CreateEndpoint(spec)` is **one synchronous
  shot** — `docker run -d` / `helm upgrade --install --wait` brings up es+sk+ps+
  compute together and blocks until compute is reachable.
- amp already owns a `storagecontroller.Client` (loopback) used for `Ready` +
  pending-init.

## The core constraint

The generation must be in `pageserver.toml` before the pageserver boots, so amp
must know it before the pod's pageserver starts. But:

- `node_register` requires the pod's HTTP address to **resolve from the SC's
  view** (`service.rs:3571` `lookup_host`, else 503). That resolves only once the
  pod's network identity exists (Docker container created; k8s Service created).
- `attach-hook` is **DNS-free** and produces the generation, but `set_attached` →
  `update_node_ref_counts` (`scheduler.rs:188`) hits `debug_assert!(false)` +
  error-log + early-return when the node isn't in the scheduler. So attach-hook
  before node_register is **fine in release** (the generation increment already
  committed to the DB) but **panics in debug/testing builds**.

So "create network identity" must be separable from "start the pageserver", OR
attach-hook must run before the pod exists. That's the design choice below.

## Ordering options

| Option | How | Docker | k8s | Cost |
|---|---|---|---|---|
| **A — pod barrier** | Create the pod as today; `supervisor.py` blocks before starting the pageserver until amp hands it a generation (pod GETs it from amp, or reads a written value). amp does node_register→attach-hook after the pod's identity resolves. | ✅ | ✅ | Pod keeps an amp dependency (waits for gen); most moving parts. |
| **B — identity first** | Create only the network identity first (k8s: the Service; Docker: a placeholder/paused container), node_register→attach-hook, then start the pod with `GENERATION` baked in. | ⚠️ no separate Service object; needs two-phase container | ✅ clean, but the single `helm --install` must be split (Service release, then workload) | Driver-divergent. |
| **C — attach-hook first** ⭐ | amp calls `attach-hook` **before** creating the pod (DNS-free) → gen; create the pod with `GENERATION` injected; call `node_register` **after** the pod exists (liveness/bookkeeping, idempotent). | ✅ | ✅ | Needs one small SC tweak: don't `debug_assert!`/error when attach-hook races an unregistered node (legitimate in our single-writer model). Or confirm node_register is bookkeeping-only (below) and the tweak is moot. |

**Recommendation: Option C — but only as the interim (create == assign) shape.**
It's driver-agnostic, needs no pod barrier, and matches the decisions already made
([`sc-integration.md` § "Design decisions"](sc-integration.md)): per-deploy
generation, and `node_register` is liveness/bookkeeping. The one SC change
(downgrade the `debug_assert!`/error path to a debug log for an
attach-of-unregistered-node) is in-bounds since we own and are integrating the SC.

> **Superseded for the long term** — see "Strategic direction: create-then-configure"
> above. Once endpoints are created-then-assigned from a warm pool, the generation
> is an assign-time value and C's bake-at-create no longer applies; the target is
> Option A as a push/configure API. Treat C as the fastest path *only if* create
> and assign stay coupled in the near term.

## Key open question (settle before implementing)

**Is `node_register` load-bearing in amp, or bookkeeping-only?** Post-`#16` there
is no reconciler acting on node availability; the PS heartbeater exists "for
liveness." attach-hook works without the node row (release); amp reaches the
endpoint's SK directly via `node.Client`, not via the SC's node describe. If
node_register only feeds heartbeat metrics + `/control/v1/node` describe (no
control action), then Option C is trivial: attach-hook pre-create for the gen,
node_register best-effort post-create, no ordering headache and no SC tweak
needed. **Confirm what, if anything, consumes the PS heartbeater's result.**

## Concrete changes — Step 1 (Option C)

1. **`storagecontroller.Client`**: add `AttachHook(ctx, branchID, nodeID) (gen, error)`
   (`POST /debug/v1/attach-hook`) and `NodeRegister(ctx, nodeID, httpAddr, httpPort, pgAddr, pgPort)`
   (`POST /control/v1/node`).
2. **amp deploy / create-endpoint step**: compute/assign `NODE_ID` (mirror the
   current branch-id-derived value, or assign explicitly — keep it stable across
   pod restarts). Sequence: `AttachHook` → get gen → `CreateEndpoint` with
   `GENERATION` env set and `NODE_ID` env set → `NodeRegister` (post-create,
   best-effort). `NODE_ID` moves from pod-derived to amp-supplied (the pod already
   reads a `NODE_ID` env).
3. **amp deploy env**: set `GENERATION=<gen>` + `NODE_ID=<n>`; stop triggering the
   pod's self-registration. Decouple `branch register` (which stays pod-driven,
   needs `start_lsn`) from the removed node/attach path — e.g. a distinct flag
   (`SC_BRANCH_REGISTER_URL`) so the pod still records the branch but no longer
   does node/attach.
4. **`supervisor.py`**: delete `sc_register_and_attach` (node_register +
   attach-hook); keep reading `GENERATION`/`NODE_ID` env (already does); keep the
   `branch register` call under its new flag.
5. **(Option C only) SC tweak**: soften `update_node_ref_counts` /
   `set_attached` so an attach-of-unregistered-node is a debug log, not a
   `debug_assert!` — unless the open question shows node_register can just run
   pre-create too.

## Step 2 (at integration)

Replace the two `storagecontroller.Client` loopback calls with in-process
`Service.attach_hook` / `Service.node_register` (or Go equivalents against amp's
YB universe). Pod contract unchanged (still just `GENERATION`/`NODE_ID` env).

## Risks / notes

- **Dev harness + tests unaffected in Step 1.** `yb_light_local` and
  `test_runner` call these SC routes themselves; we're changing amp's deploy +
  the amp endpoint pod, not the SC routes, so both keep working.
- **Failure handling.** attach-hook then failed pod-create: the generation was
  bumped but unused — harmless (next attempt bumps again; single-writer). Both
  calls are idempotent.
- **Restart semantics** match the "no per-restart bump" decision: `GENERATION`
  env is fixed from the last deploy; a pod self-restart reuses it (idempotent
  re-attach).
- **Testing.** Extend `internal/storagecontroller/integration_test.go` (fake SC)
  for the new client methods; run the amp-on-Docker integration (`#105`) and, if touched,
  `test/integration/compute`.

## Suggested sequencing

1. Answer the open question (is node_register bookkeeping-only?). 30-min code read.
2. Add the two `storagecontroller.Client` methods + fake-SC test coverage.
3. Wire Option C into the create-endpoint step; add `NODE_ID`/`GENERATION`
   injection; decouple `branch register` behind its own flag.
4. Strip `sc_register_and_attach` from `supervisor.py`.
5. (If needed) the SC debug_assert softening.
6. integration on Docker; then k8s.
7. Later, Step 2: collapse to in-process.

## Data-plane constraints for pre-warm (investigated 2026-07-02)

Four investigations into whether the data plane can support create-then-configure
and reassignment. Bottom line: **the data plane is a static, spawn-time,
one-writer-per-branch binding, and the pageserver generation is the *only*
teardown-safe fence.**

- **Safekeeper has no write fence.** WAL writes to YB (`yb_wal_storage.rs:1182`
  INSERT, `control_file.rs:243` UPDATE) carry no generation/term predicate;
  `mconf.generation`/`term` only validate *incoming* walproposer WAL
  (`safekeeper.rs:1062`), not writes out to YB. Safety rests entirely on the
  **single-safekeeper-per-branch** constraint (`require_single_safekeeper`, boot +
  walproposer greeting; `architecture/safekeeper.md:361`). A stale/lingering SK is
  **not** fenced the way a stale pageserver is.
- **compute_ctl reconfigures settings, not storage.** `POST /configure`
  reconfigures a *running* compute (settings via `pg_ctl reload`; roles/dbs/
  extensions via SQL) but **never re-points Postgres at a different branch/PS/SK**
  (`reconfigure()` ignores the storage fields; connstr is fixed at start,
  `compute.rs:2024`). A fresh `Empty → first /configure → Running` assign works; a
  live rebind of a running compute does not.
- **Read-only endpoints are metadata-only.** The `endpoint_role` enum + the
  `one_writer_per_branch` partial unique index exist and are enforced
  (`controller/migrations/0001_init.up.sql:111`), but role is **not plumbed to the
  pod** (`EndpointSpec` has no `Role`; `supervisor.py:377` hardcodes
  `mode:"Primary"`). Generation is per-branch, not per-role. RO is future work
  (Standby mode + replication; the RO pageserver must not write, since the fence is
  per-branch).
- **Teardown is asymmetric with attach.** Endpoint delete = remove pod + delete
  amp's endpoint row (`step_delete_endpoint.go:46`); **no storage-layer cleanup** —
  no detach, no generation/placement reset. The attach-hook **detach mode**
  (`node_id=None` → `persistence.detach`: clears `generation_pageserver`, sets
  `Detached`, **keeps generation**) exists but is **called by nothing**. The SC is
  left believing the branch is attached to a now-dead pageserver. No
  pod-reuse/return-to-pool today (1:1 endpoint↔branch).

Cross-cutting implications:

- **Why the generation model is robust:** it fences on *attach*, is monotonic, and
  is never reset, so a re-attach always gets a higher generation and fences
  whatever came before — *without* requiring teardown to have run cleanly. That's
  why the pageserver tolerates hard shutdown.
- **The safekeeper is the reassignment weak link — RESOLVED (decision):** no fence
  today (`yb_wal_storage.rs:1182` WAL INSERT is plain, no `ON CONFLICT`;
  `control_file.rs:243` timeline_state UPDATE keyed on branch_id only; `term`
  stored but unenforced). **Decision: add a generation-fenced SK write path,
  reusing amp's branch attach generation** — one generation per attach handed to
  both the pageserver and the safekeeper; add `attach_generation INT` to
  `safekeeper.timeline_state`, CAS-bump on attach, and **gate the `wals` INSERT
  itself** (WAL rows physically collide on the PK, unlike the pageserver's
  idempotent `(key,lsn)` page rows — so fencing only the flush-state pointer is
  insufficient) plus the flush-state UPDATE. A new attach then fences the old
  pod's PS *and* SK on their next write, with no dependency on confirming the old
  pod is dead (a primary whose WAL can't be durably written makes no progress, so
  the WAL write is the chokepoint). Clean-stop ordering is *not* sufficient
  (crash/partition). **Hard prerequisite for Phase 5; Phases 1–4 don't need it.**
- **Fresh assign viable, live rebind not:** pre-warm's near-term shape is create a
  pod *unbound* (compute `Empty`, PS/SK not yet started), then at assign spawn
  PS/SK with the branch + mint the generation + first-`/configure` the compute.
  Reuse-after-release means cycling the per-branch processes — supervisor work to
  support "boot unbound, then assign."
- **Teardown must become symmetric for a pool:** a real detach/unconfigure step
  (clean-stop the per-branch processes, especially the SK; optionally record detach
  via the existing detach mode). Generation stays un-reset.
