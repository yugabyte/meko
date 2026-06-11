---
status: done
severity: medium
area: yb_light_local
opened: 2026-06-10
---

# Co-locate pageserver + safekeeper under the endpoint folder

## Landed

PS+SK now live under `endpoints/<name>/{pageserver,safekeeper}`; the
old top-level `pageserver_<id>/` and `safekeepers/sk<id>/` dirs are gone.
Provisioning moved to `endpoint create`: `tenant create` and `timeline
branch` only record a `PendingTimelineInit` (keyed by timeline id,
carrying tenant + branch name + Bootstrap/Branch mode); the first
`endpoint create` for a timeline runs that pending init (allocates the
pair, starts the processes, runs initdb/import or the branch's
safekeeper-create), then registers the branch-name mapping with the
storage controller (deferred because the controller rejects a branch
name for a timeline it doesn't yet know).

Key implementation points beyond the original plan:

- `LocalEnv` keeps the init-TOML confs as `pageserver_templates` /
  `safekeeper_templates`; `pageservers` (discovered from
  `endpoints/*/pageserver/`) and `safekeepers` (persisted) start empty
  on a fresh repo. `allocate_{pageserver,safekeeper}_conf` is the single
  allocation path — the first node reuses the lowest-id template's
  id/ports, later ones get the next id + per-id port delta.
- `PageServerConf` gained a non-serialized `data_dir` (the discovered /
  allocated `endpoints/<name>/pageserver`); `repo_path()` uses it.
  `SafekeeperConf` gained `endpoint_id`; its datadir derives from it.
- `StorageController::from_env` and the SC-start safekeeper-count / auth
  derivation fall back to the templates (no node is provisioned at
  `start`); the main safekeeper is registered with the controller during
  bootstrap provisioning, before `tenant_timeline_create`.
- `endpoint stop --destroy` deprovisions the co-located pair (stops the
  PS/SK, drops the SK from config) and tolerates a never-started
  endpoint (no pgdata → skip pg_ctl).

Tests: `control_plane` unit tests (discovery scan, path helpers,
allocation, pending-init round-trip) + `test/e2e/fast/layout.sh`.
Verified with `cargo test -p control_plane` and `test/e2e/run.sh all`.

## Goal

`yb_light_local` lays out the local cluster as top-level
`pageserver_<id>/` and `safekeepers/sk<id>/` dirs beside `endpoints/`.
Since [task 22](22_single-timeline-pageserver.md) the topology is one
PS+SK pair per timeline, and each endpoint is the consumer of exactly
one such pair — the id-suffixed top-level dirs obscure which PS/SK
belongs to which endpoint. Target layout:

```
.yb_light/
  endpoints/
    main/
      pgdata/, compute_ctl.pid, compute.log    # as today
      pageserver/    # pageserver.toml, identity.toml, .pid, .log
      safekeeper/    # safekeeper.pid, safekeeper-<id>.log
    ep_b1/
      pgdata/ …
      pageserver/
      safekeeper/
```

Decisions: (1) layout is `endpoints/<name>/{pageserver,safekeeper}` —
no id suffixes in paths; ids live on in `identity.toml` / config.
(2) PS/SK **provisioning moves to `endpoint create`** — both for the
main timeline (today provisioned at `init`/`tenant create`) and for
branches (today provisioned at `timeline branch`).

## Lifecycle changes

| Step | Today | After |
|---|---|---|
| `init` | creates `pageserver_1/`, `safekeepers/sk1/` (`local_env.rs:977,987`) | writes config only; no PS/SK dirs |
| `start` | starts SC, endpoint_storage, all SKs, PS-with-identity (`yb_light_local.rs:2121`) | same, but over whatever PS/SK have been provisioned so far (none on a fresh repo) |
| `tenant create` | bakes identity into default PS, starts it, PS bootstraps timeline (`yb_light_local.rs:1228-1298`) | allocates tenant/timeline ids, registers `main` branch mapping, records pending-init (Bootstrap, pg_version) in config; keeps the `successfully created` println (e2e awk depends on it) |
| `timeline branch` | allocates + starts new PS+SK pair (`yb_light_local.rs:1340-1465`) | captures `start_lsn` from ancestor SK (as today) + pg_version, registers branch mapping, records pending-init (Branch{ancestor, lsn}); no processes |
| `endpoint create <name>` | only `cplane.new_endpoint` (`yb_light_local.rs:1593`) | provisions the timeline's PS+SK under `endpoints/<name>/` if none serves that timeline yet, runs the pending init, then `new_endpoint` |

`endpoint create` provisioning, by pending-init mode:

- **Bootstrap** (main): PS first — `set_timeline_identity(Bootstrap)`,
  `register_and_attach`, `start` (PS runs initdb + import), then
  `storage_controller.tenant_timeline_create` (creates SK timeline) —
  i.e. the body of today's `TenantCmd::Create` moves here.
- **Branch**: SK first — create dir, `initialize`, `start`,
  `create_timeline` at the persisted `start_lsn`; then PS —
  `initialize_from_template(ancestor_ps)`,
  `set_timeline_identity(Branch{ancestor, lsn})`, `start` — i.e. the
  provisioning half of today's `TimelineCmd::Branch` moves here.
- **Already provisioned** (second endpoint on a branch, e.g. a static
  replica): skip provisioning, just `new_endpoint`. The PS/SK folder
  stays under the *first* endpoint created for that timeline.

## Path + discovery changes (`control_plane/src/`)

- `local_env.rs`
  - `pageserver_data_dir(id)` (`:488`) and `safekeeper_data_dir(name)`
    (`:493`) become endpoint-keyed:
    `endpoints/<endpoint>/pageserver`, `endpoints/<endpoint>/safekeeper`.
  - **Discovery scan** (`:696-788`): instead of globbing top-level
    `pageserver_*` and parsing the id from the dirname, scan
    `endpoints/*/pageserver/` dirs; the id comes from `identity.toml`
    alone (already read; drop the dirname cross-check). `PageServerConf`
    gains a non-serialized `data_dir: PathBuf` captured at scan time;
    `PageServerNode::repo_path()` (`pageserver.rs:192`) uses it.
  - `SafekeeperConf` (persisted in `.yb_light/config`) gains an
    `endpoint_id: String` field; `SafekeeperNode::datadir_path*`
    (`safekeeper.rs:89-93`) derive from it. Main SK also gets its
    `timeline_id` baked at provisioning (the `timeline_id: None` legacy
    fallback in `safekeepers_for_timeline`, `local_env.rs:420-436`,
    stays for one transition but is no longer exercised).
  - `LocalEnv::init` (`:864-987`): stop creating PS/SK dirs; keep the
    conf entries from the init TOML as **templates** (ports,
    `wal_yb_config`, auth) that `endpoint create` clones via the
    existing `allocate_branch_pageserver_conf` /
    `allocate_branch_safekeeper_conf` (`yb_light_local.rs:1067-1127`) —
    those become the single allocation path for *all* PS/SK including
    the first. Port math (lowest-id template + per-id delta) unchanged.
  - New persisted config section for pending timeline init:
    `{ timeline_id → Bootstrap{pg_version} | Branch{ancestor_timeline_id, start_lsn, pg_version} }`,
    cleared once provisioned.
- `yb_light_local.rs`: move provisioning code per the table;
  `handle_start_all_impl` / status check / `try_stop_all` keep iterating
  `env.pageservers` / `env.safekeepers` and need no structural change
  (empty sets on a fresh repo already pass the status check).
- **Endpoint deletion** (`endpoint.rs:1209-1210` removes the endpoint
  dir wholesale): must first stop the co-located PS/SK and only then
  remove the dir — and refuse (or cascade-deprovision) when the endpoint
  owns a still-referenced PS/SK. Minimal v1: stop PS/SK processes, drop
  the SK conf from config, remove the dir.

No migration shim for old layouts: local repos are throwaway
(`clean.sh` / re-`init`).

## Consumers to update (path strings)

- `test/e2e/run.sh:94-101` — `PS_PIDFILE`, `SK_PIDFILE`, `LOG` →
  `endpoints/main/pageserver/…`, `endpoints/main/safekeeper/…`.
- e2e `.sh` tests with hardcoded paths: `fast/restart-correctness.sh`,
  `fast/restart-wal-replay-yb.sh`, `fast/going-away.sh`,
  `slow/restart-wal-replay.sh`, `fast/branch-endpoint.sh` (branch PS
  paths → `endpoints/ep_b1/pageserver`; `pageserver stop --id 2` CLI
  calls are id-based and unchanged). `branch-endpoint.sh` also needs
  its flow updated: the PS/SK now appear at `endpoint create ep_b1`,
  not at `timeline branch b1`.
- `benchmark/scripts/*.sh` (5 files), `test_runner/bin/…deep_l0_stack.py`,
  `test_runner/performance/…getpage…py` — same path substitutions.
- Docs: `architecture/high-level.md`, `architecture/safekeeper.md`
  layout diagrams; check `runbooks/*.md` for `pageserver_1`/`sk1`
  references.

## Tests (ship with the implementation)

- **Unit (`control_plane`)**:
  - discovery-scan: materialise a temp repo with
    `endpoints/{a,b}/pageserver/{pageserver.toml,identity.toml}` and
    assert `LocalEnv::load_config` reconstructs both `PageServerConf`s
    (id from `identity.toml`, `data_dir` pointing at the endpoint
    subdir); negative cases (endpoint dir without `pageserver/` is
    skipped, malformed toml errors).
  - path helpers: `pageserver_data_dir` / `safekeeper_data_dir` return
    `endpoints/<name>/{pageserver,safekeeper}`.
  - allocation: `allocate_branch_{pageserver,safekeeper}_conf` from a
    template-only env (fresh repo, nothing provisioned) and from an env
    with existing pairs — ids and port deltas monotonic.
  - pending-init round-trip: persist `Bootstrap{pg_version}` and
    `Branch{ancestor, start_lsn}` entries through
    `persist_config`/`load_config`; cleared after provision.
- **e2e**: new `test/e2e/fast/layout.sh` pinning the on-disk contract:
  after `tenant create` no PS/SK dirs exist; after
  `endpoint create main` exactly
  `endpoints/main/{pageserver,safekeeper}` exist with live pidfiles and
  no top-level `pageserver_*` / `safekeepers/` dirs; after
  `timeline branch b1` still nothing new; after `endpoint create ep_b1`
  the pair appears under `endpoints/ep_b1/`. Existing
  `branch-endpoint.sh` + restart tests (with updated paths) cover
  process behaviour on the new layout.

## Verification

1. `cargo build` (debug) + `cargo test -p control_plane`.
2. Fresh-cluster smoke by hand: `init` → `start` (no PS/SK dirs yet) →
   `tenant create` → `endpoint create main` (PS+SK appear under
   `endpoints/main/`, processes up) → `endpoint start main` → psql
   round-trip. Then `timeline branch b1` → `endpoint create ep_b1` →
   branch PS/SK under `endpoints/ep_b1/` → branch reads work.
3. `test/e2e/run.sh all` — full suite green, including
   `branch-endpoint.sh` (updated flow), the restart tests (updated
   pidfile paths), and the `.sql` golden diffs (unchanged).
4. `stop`/`start` of the whole cluster after provisioning — discovery
   re-finds PS from `endpoints/*/pageserver`, SK from config.

## Risks / notes

- `tenant create` no longer leaves a running PS; anything that assumed
  "tenant exists ⇒ PS up" (scripts, muscle memory) now needs
  `endpoint create` first. e2e's `setup_cluster` already does
  tenant create → endpoint create → endpoint start, so only ordering
  *inside* those handlers changes.
- A timeline's PS/SK folder is owned by the first endpoint created for
  it; deleting that endpoint deprovisions the pair (v1 semantics above).
- `timeline branch` output today prints provisioning lines; anything
  that greps them (none found in e2e, but re-check) moves to
  `endpoint create` output.
