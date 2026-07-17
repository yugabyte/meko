# Control-plane integration suite (controller-on-Docker)

The `integration_control_plane` crate: a deliberately thin driver that exercises
the **controller** control plane on Docker. `run-controller.sh` starts the `controller`
container (with its Docker endpoint provisioner enabled) against the
host's YugabyteDB, then this binary calls controller's REST API
(`user → storage pool → project → branch → endpoint`) and runs Postgres against the
endpoint addresses controller returns.

**controller owns all provisioning** — create-project records the tenant + its
default branch, create-branch forks an existing branch (recording its fork
ancestry), and create-endpoint deploys the compute container synchronously via
controller's `deploy.Deployer` (the Docker driver; amp-controller.md §5.6),
returning its address. The first endpoint deployed on a branch materializes it.
The harness contains no orchestration; `main.rs` is the harness side.

It runs the SC-free self-bootstrap path: controller deploys the endpoint with
`--deploy-bootstrap`, so it provisions the storage schema and initdb's the
recorded tenant/timeline on first start (there is no create-database flow
yet). Storage-controller-coordinated deploy is future work.

## What it tests — `branch_lineage`

The suite creates a user and a storage pool once, then runs the `branch_lineage`
scenario, which is a full branching workout against the controller's API:

1. **create-project** → a tenant and its default branch `main`; deploy a
   read_write endpoint on `main` and assert the compute serves the exact
   tenant/timeline the controller bound it to (`yb_amp.tenant_id` /
   `yb_amp.timeline_id`).
2. **seed + branch** — seed `main`, then fork the tree:

   ```text
     main ──┬── b1 ── b2     (b2 forks the branch b1)
            └── b3           (b3 forks the root main, a second time)
   ```

   `b1` forks `main`; `b2` forks the branch `b1` (branch-of-branch); `b3` forks
   `main` again (a second fork off the root). Each fork is pinned to the parent's
   current flush LSN — read from the parent's live endpoint
   (`pg_current_wal_flush_lsn()`) and passed as `fork_point_lsn`. (The SC-free
   deploy path doesn't publish the safekeeper port the controller's *default*
   fork-point resolution reads, so an explicit `fork_point_lsn` is required here;
   it's also more deterministic.)
3. **validate copy-on-write isolation** — every branch reads its ancestors'
   pre-fork rows (count *and* a whole-table checksum), never its ancestors'
   post-fork writes, and its own writes never leak to a sibling or an ancestor.
4. **control-plane metadata** — fork ancestry (`is_default`, `parent_branch_id`,
   `fork_lsn`) on each create, and `listBranches` reporting all four branches.

The endpoints are then retired (`deleteEndpoint`). `-n` repeats the scenario
against a fresh project each time.

## Running

```sh
test/integration/control_plane/run-controller.sh           # one branch_lineage run
test/integration/control_plane/run-controller.sh -n 3      # repeat the scenario 3x (fresh project each)
```

Prerequisites: Docker; a YugabyteDB on the host at `:5433`
(yugabyte/yugabyte); the `yb-light/neon-endpoint:dev` image present (build
it once with
`docker build -t yb-light/neon-endpoint:dev -f deploy/endpoint/Dockerfile .`).
`run-controller.sh` builds `yb-light/controller:dev` itself and tears everything down on
exit. `branch_lineage` brings up four endpoint containers (main + b1 + b2 + b3),
so it needs enough headroom for four single-container computes at once.

`run-controller.sh` bind-mounts the repo's current `deploy/endpoint/supervisor.py`
into every endpoint (`AMP_DEPLOY_SUPERVISOR_PATH`), so the endpoint image's
*supervisor* never goes stale between rebuilds — but its **binaries**
(pageserver/safekeeper/compute) are still whatever was baked in. If a test fails
because an endpoint's pageserver/safekeeper lacks a feature the supervisor or
controller now drives, rebuild the image:
`docker build -t yb-light/neon-endpoint:dev -f deploy/endpoint/Dockerfile .`.

## Layout

```
test/integration/control_plane/
  run-controller.sh   starts controller in Docker, then builds + runs the suite
  Cargo.toml   the `integration_control_plane` crate
  main.rs      arg parse + controller REST client + branch_lineage + run loop
```

Shared Postgres assertion helpers (the `db` module) live in
[`../libs/integration_common`](../libs/integration_common), used by both this suite and the
compute suite.
