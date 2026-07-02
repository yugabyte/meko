# Storage-controller integration — work items

Living checklist for integrating the `storage_controller` (Rust, "SC") into the
amp controller (Go, `controller/`). Companion to the API inventory in
[`sc-integration.md`](sc-integration.md).

This is a planning scratchpad for the effort, not the canonical tracker: per
[`../../docs/issue-tracking.md`](../../docs/issue-tracking.md), scheduled work
graduates to a GitHub issue (area label + milestone). Link the issue next to the
item when it's filed.

Status key: `[ ]` todo · `[~]` in progress · `[x]` done · `[-]` dropped.

## Resume point (as of 2026-07-02, `origin/main` `b1aa32c9c`)

Where we are — analysis/scoping phase, **no code changed yet**.

The model changed a lot from the first pass: `[#168]` collapsed
`(tenant_id, timeline_id)` → flat **`branch_id`**; `[#171]` deleted the SC
safekeeper registry; `[#16]` + upcall removals gutted the pageserver control
path; `[#113]` moved amp's mutating APIs onto the task framework. The docs are
refreshed to this reality.

Established (all in [`sc-integration.md`](sc-integration.md)):
- **SC's current role:** a registry + generation authority over 7 branch-keyed
  YB tables, run by amp as a managed subprocess. Hollowed down to
  generation-increment + node registry + PS heartbeat + recipe/endpoint/branch
  records + a vestigial scheduler.
- **Set A** (amp → SC): now just `Ready` + the pending-init recipe registry
  (`RecordPendingTimelineInit`/`DeletePendingTimelineInit`, plus an unwired
  `ListPendingTimelineInits`). The 3 old dead `/v1/...` methods are **gone**
  (removed upstream in `#168`/`#171`).
- **Set B** (endpoint pod → SC): now **3** — `node_register`, `attach-hook`
  (`{branch_id, node_id}`), `branch register` (`start_lsn`; `sk_set` dropped).
  Also called by `yb_light_local` + `test_runner`; never by the daemon crates.
- **Direction (user's idea):** move Set B calls out of `supervisor.py` into amp's
  **deploy code** — `node_register`/`attach-hook` need no pod-runtime data
  (restoring control-plane ownership); post-integration they become in-process
  calls. `branch register` is the holdout (`start_lsn` = pageserver
  `last_record_lsn`, produced inside the pod).
- **attach-hook** = the monotonic generation fencing token; single-writer-per-
  branch topology is what makes moving it safe.

Next concrete steps (pick up here):
1. Do the **cleanup** items below (`supervisor.py` drift, stale
   `storage-controller.md` prose) — low-risk, decoupled from the integration.
2. Resolve the two design questions before the deploy-code move:
   **generation-bump-on-restart** semantics, and **`node_register` DNS ordering**
   per deploy driver.
3. Scope the two-step migration: move `node_register`/`attach-hook` into
   `internal/deploy` over the loopback client → collapse to in-process at
   integration.

## Cleanup — DONE 2026-07-02 (uncommitted)

- [x] **Fixed the `supervisor.py` drift from `#171`.** Removed the dead
      `/control/v1/safekeeper/{id}` + `/scheduling_policy` calls and the whole
      best-effort try/except block from `sc_register_and_attach`; dropped `sk_set`
      from the branch-register body; removed the now-orphaned
      `SAFEKEEPER_{HTTP,PG}_PUB_PORT` vars; refreshed the docstring + the
      branch-register comment. `python3 -m ast` parse clean.
- [x] **Fixed stale prose in `architecture/storage-controller.md`.** Intro no
      longer claims SK tracking; "What it does" drops the safekeeper-assignment
      bullet and scopes heartbeats to pageservers; the "Diverges from upstream"
      safekeeper section rewritten to "surface removed entirely (#171)". Left the
      `--timelines-onto-safekeepers` / `--timeline-safekeeper-count` flags in the
      Run section — verified they **still exist** in `main.rs`/`service.rs`.
- [x] **Fixed `task/deps.go` doc references** — `controller-timeline-create.md`
      → `controller-project-create.md` (the real file). `go build` + `go vet`
      clean.

Note: `go vet`/diagnostics surface a **pre-existing** unused `seedBranchFork` in
`controller/internal/task/fakes_test.go` (no callers in the rebased tree) — not
introduced by this cleanup; left alone.

## Investigation (scopes later work)

- [x] **Trace each Set-B endpoint into its `http.rs` handler.** Done — see
      [`sc-integration.md` § "Set B handler trace"](sc-integration.md). Handlers
      are thin; logic is in `Service`/`persistence`. `branch register`
      (`timeline_register`) is ~pure-DB; `attach_hook`/`node_register` are coupled
      to in-memory `Service` state + side effects (generation increment, DNS).
- [x] **Design questions for the deploy-code move — both resolved 2026-07-02**
      (see [`sc-integration.md` § "Design decisions"](sc-integration.md)):
  - **generation-bump-on-restart → DECIDED: skip.** amp mints the generation
    once per deploy and injects it; a pod self-restart reuses it (idempotent
    re-attach). Not preserving the per-restart bump. Safe under
    single-writer-per-branch; revisit only if >1 pageserver per branch is ever
    allowed.
  - **`node_register` DNS ordering → UNDERSTOOD, not a blocker.** The DNS check is
    the *storage controller's* (`service.rs:3571`), from its own network view —
    the pod only declares its address. Moving the call into amp only requires
    ordering: create the pod's container/Service (so `SC_REACHABLE_HOST` resolves)
    → `node_register` → `attach-hook` → inject `GENERATION` → finish deploy.

## Pre-warm data-plane constraints (investigated 2026-07-02)

Findings recorded in [`sc-integration-migration.md` § "Data-plane constraints for
pre-warm"](sc-integration-migration.md). The data plane is a static, spawn-time,
one-writer-per-branch binding; the pageserver generation is the only teardown-safe
fence. Follow-ups surfaced (all future / pre-warm, not blockers for step 1):

- [x] **Safekeeper reassignment safety (correctness) — DECIDED 2026-07-02: add a
      generation-fenced SK write path.** Confirmed no YB-side fence today
      (`yb_wal_storage.rs:1182` WAL INSERT has no `ON CONFLICT`/predicate;
      `control_file.rs:243` timeline_state UPDATE keyed on branch_id only; `term`
      is stored but not enforced). Decision: reuse **amp's branch attach
      generation** for both writers — add `attach_generation INT` to
      `safekeeper.timeline_state`, CAS-bump on attach (mirror
      `cas_bump_generation_on_attach`), and **gate the `wals` INSERT itself** (WAL
      rows collide on the PK, unlike the pageserver's idempotent `(key,lsn)` rows,
      so fencing only the flush pointer is insufficient) plus the flush-state
      UPDATE. Clean-stop ordering is NOT sufficient (can't cover crash/partition).
      **Hard prerequisite for Phase 5; Phases 1–4 don't need it** (1:1 model =
      single-SK-by-construction). See
      [`sc-integration-migration.md`](sc-integration-migration.md).
- [ ] **Make teardown symmetric.** Endpoint delete does no storage-layer cleanup
      today. Add a detach/unconfigure step (clean-stop per-branch processes,
      especially the SK; optionally use the existing-but-unused attach-hook detach
      mode). Do **not** reset the generation.
- [ ] **Supervisor "boot unbound, then assign."** For the pool: start a pod with
      compute `Empty` and PS/SK not yet bound; at assign, spawn PS/SK with the
      branch + inject the generation + first-`/configure` the compute. Live rebind
      of an already-running compute is not supported (storage binding immutable).
- [ ] **Read-only endpoints (future).** Plumb `role` → `EndpointSpec` → Helm/
      supervisor → Postgres Standby mode + replication; ensure the RO pageserver
      does not write (generation fence is per-branch, not per-role).

## Integration (pending design)

- [~] **Two-step migration of `node_register` + `attach-hook` into amp** —
      scoped in [`sc-integration-migration.md`](sc-integration-migration.md).
      Recommended path: Option C (attach-hook pre-create for the generation,
      node_register post-create). **Blocker to resolve first:** is `node_register`
      load-bearing in amp or bookkeeping-only? (decides whether an SC tweak is
      even needed). `branch register` stays pod-driven.
- [ ] **Internalize Set A** as in-process Go calls: the pending-init recipe
      registry + `/ready` gate stop being HTTP round trips once the SC logic lives
      in-process.
- [ ] **Preserve Set B** as the pod-facing HTTP surface the integrated controller
      serves to remote endpoint pods (`node_register`, `attach-hook`,
      `branch register`, + `/ready` / `/status` probes) — unless ownership is
      inverted into deploy code (see above).
- [ ] **Merge SC persistence** into amp's universe: the 7 `storage_controller.*`
      tables (`branches`, `timelines`, `pending_timeline_inits`, `endpoints`,
      `nodes`, `metadata_health`, `timeline_imports`) vs. amp's YBDB schema.
- [ ] **Keep the `control_plane` dev harness (`yb_light_local`) + `test_runner`
      working** — both exercise the same Set-B surface as independent consumers.
