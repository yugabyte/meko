# meko-dev operational scripts

Scripts for the **meko-dev** amp deployment (kube context `meko-dev-eks`, namespace
`amp-dev`). Both are parameterized via env vars; the defaults target meko-dev
against the `amp2-db` YugabyteDB universe (in namespace `yb-dev-amp2-db`).

## `amp-redeploy.sh` — clean redeploy of the control plane

Points the `amp-dev` control plane at a given image `TAG` and an external,
**passwordless** YB universe, on a **fresh `yb_storage` schema**, while preserving
the tailnet URLs (`amp-api`/`amp-ui`/`amp-mcp`) and the OpenTelemetry collector CRs.

```sh
CONFIRM=yes TAG=tot-<shortsha> ./amp-redeploy.sh     # full redeploy
./amp-redeploy.sh                                    # dry preview (no mutations)
```

Steps: re-mint the ECR pull secret → `CREATE DATABASE yb_storage` on the universe
(the controller does **not** auto-create it) → uninstall the old per-endpoint
`yb-ep-*` releases → `helm upgrade` the control plane (with `--force-conflicts`,
since `controller`/`ui` images are often `kubectl set image`'d, creating a
server-side-apply field-manager conflict) → wait for rollout → delete the old
in-namespace `yugabyted` backend → verify the tailnet hostnames + OTel CRs
survived. Destructive steps run only with `CONFIRM=yes`.

Key env vars: `TAG` (required), `NS`, `KCTX`, `ECR`, `CHART`, `YB_HOSTPORTS`,
`YB_NS`, `YB_DB`, `AWS_REGION`.

## `amp-integration-test.sh` — end-to-end provisioning test

Drives the controller REST API to provision a fresh `read_write` endpoint, asserts
it comes up as **PostgreSQL 18** and does a real **write/read** round-trip, then
tears the endpoint + project down. Repeatable and non-destructive to the stack;
exits non-zero on failure (CI-friendly).

```sh
./amp-integration-test.sh
```

Requires a reachable `amp-controller` (it port-forwards the API and the endpoint's
compute), `python3`, and a `psql` client (auto-detected; override with `PGBIN`).

> The endpoint compute defaults to user `cloud_admin` / db `postgres`; the test
> connects over a `kubectl port-forward` so it does not depend on ELB/tailnet
> reachability from the runner.
