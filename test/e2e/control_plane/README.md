# Control-plane e2e suite (controller-on-Docker)

The `e2e_control_plane` crate: a deliberately thin driver that exercises
the **controller** control plane on Docker. `run-controller.sh` starts the `controller`
container (with its Docker endpoint provisioner enabled) against the
host's YugabyteDB, then this binary calls controller's REST API
(`user → storage pool → timeline → endpoint`) and runs `psql` against the
endpoint controller returns.

**controller owns all provisioning** — create-timeline records the tenant + its
main timeline, and create-endpoint deploys the compute container
synchronously via controller's `deploy.Deployer` (the Docker driver;
amp-controller.md §5.6), returning its address. The harness contains
no orchestration; `main.rs` is the harness side.

It runs the SC-free self-bootstrap path: controller deploys the endpoint with
`--deploy-bootstrap`, so it provisions the storage schema and initdb's the
recorded tenant/timeline on first start (there is no create-database flow
yet). Storage-controller-coordinated deploy is future work.

## Running

```sh
test/e2e/control_plane/run-controller.sh           # create timeline + endpoint, psql the endpoint
test/e2e/control_plane/run-controller.sh -n 3      # repeat the suite 3x
```

Prerequisites: Docker; a YugabyteDB on the host at `:5433`
(yugabyte/yugabyte); the `yb-light/neon-endpoint:dev` image present (build
it once with
`docker build -t yb-light/neon-endpoint:dev -f deploy/endpoint/Dockerfile .`).
`run-controller.sh` builds `yb-light/controller:dev` itself and tears everything down on
exit.

## Layout

```
test/e2e/control_plane/
  run-controller.sh   starts controller in Docker, then builds + runs the suite
  Cargo.toml   the `e2e_control_plane` crate
  main.rs      arg parse + controller REST client + scenarios + run loop
```

Shared Postgres assertion helpers (the `db` module) live in
[`../libs/e2e_common`](../libs/e2e_common), used by both this suite and the
storage suite.
