# Deploying the YCP data plane

The two images built from this directory — `meko-agent` and `dpmcp` — and how they
are built, configured, tested and operated. For the component overview see
[`ARCHITECTURE.md`](./ARCHITECTURE.md).

**Deploying the control plane is not covered here.** It is `api_server` with
`--control-plane-mode`, installed from the `meko` Helm chart; the step-by-step is
`charts/meko/YCP-RUNBOOK.md` in
[meko-devops](https://github.com/yugabyte/meko-devops). §4 of that runbook is also
how an agent gets attached to it in a real cluster. What follows is for building
and debugging the agent itself.

## Prerequisites

- Go 1.25+ to build.
- For the build-and-deploy test: `kind`, `kubectl`, `curl`, `python3`, and a
  container engine — docker or podman, auto-detected (`YCP_E2E_ENGINE` overrides).

## Run locally

The agent needs an in-cluster Kubernetes API to read facts from, so it is not
useful outside a cluster — use the e2e below instead. `dpmcp` does run standalone:

```bash
go run ./cmd/dpmcp -addr :3000          # POST /mcp (JSON-RPC), GET /healthz
```

## Container images

Built and pushed to the platform ECR registry on every merge to `main` by
[`.github/workflows/ycp-images.yml`](../.github/workflows/ycp-images.yml), tagged
with both the commit SHA (pin this in a deployment) and `latest`. PRs build them
but never push.

| Dockerfile | Binary | Image |
|------------|--------|-------|
| `Dockerfile.agent` | `cmd/agent` (BYOC pull agent) | `161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/meko-agent` |
| `Dockerfile.dpmcp` | `cmd/dpmcp` (per-datapack MCP) | `161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/dpmcp` |

Both are `gcr.io/distroless/static:nonroot` at runtime — no shell, no package
manager, so `kubectl exec` into them will not give you a prompt. Use logs and
port-forward.

The control plane's image (`meko/meko-api-server`) is **not** built by CI; it is
still pushed by hand. The runbook's Prerequisites section says how to check that a
given tag carries the control-plane flags.

To build locally (`podman build` works the same, and is what the e2e uses when
docker is absent):

```bash
docker build -f Dockerfile.agent -t meko-agent:dev .
docker build -f Dockerfile.dpmcp -t dpmcp:dev      .
```

## Tests

Unit tests need nothing running:

```bash
go test ./...
```

The build-and-deploy test is the end-to-end one. It builds the api-server and
agent images from source, loads them into a throwaway `kind` cluster, and drives
the whole bring-up against the **real control plane** (`api_server
--control-plane-mode` on a plain Postgres): every workload rolls out including the
migrate init container, `/api/v1/health` reports both databases up, the fleet API
rejects an unauthenticated call, registering an environment mints an enrollment
token, the agent enrolls with it and reaches `READY` with real cluster facts, and
— the assertion that matters most — the agent **restarts without re-enrolling**,
proving the status token was persisted on both sides.

It deploys the control plane under a `/ycp` base path, matching how production
fronts it at `https://devcloud.yugabyte.com/ycp/`, and asserts the mount: the
console answers at `/ycp/` and `/ycp`, the API under `/ycp/api/v1`, the domain
root 404s, and `/api/v1/health` still answers there (which is also what the pod's
readiness probe uses). The agent's `YCP_URL` carries the subpath. The prefix lives
only in `$BASE` in `run.sh` and is substituted into the manifests' `__BASE_PATH__`
placeholder at apply time, so `YCP_E2E_BASE_PATH= ./test/e2e/run.sh` tests the
root mount with nothing else to edit.

`dpmcp`'s image is built too, though nothing deploys it — this run provisions no
datapacks. Building it is what stops a broken `Dockerfile.dpmcp` from merging
green and then failing `ycp-images.yml` on `main`, where the only symptom is a
per-datapack MCP pod stuck in `ImagePullBackOff`.

```bash
./test/e2e/run.sh                  # create, test, tear down
YCP_E2E_KEEP=1 ./test/e2e/run.sh   # leave the cluster up to poke at
```

It creates and deletes its own cluster (`kind-ycp-e2e`) and pins every `kubectl`
call to that context, refusing to run if the context does not resolve to the kind
node it just created — so it cannot touch whatever cluster your kubeconfig
happens to point at. On failure it dumps pod state, container logs and events.

Knobs: `YCP_E2E_CLUSTER`, `YCP_E2E_TAG`, `YCP_E2E_PORT`, `YCP_E2E_TIMEOUT`,
`YCP_E2E_KEEP`, `YCP_E2E_BASE_PATH`.

Both run in CI — see the `meko_ycp` and `meko_ycp_e2e` jobs in
[`pr-build.yml`](../.github/workflows/pr-build.yml). The e2e fires on `ycp/**` and
on the `api_server` files that make up the control plane (its flags, router,
environment service/repository/handlers, console and migrations), since it builds
that image too.

## Operating an attached agent

```bash
kubectl -n $ANS logs deploy/meko-agent            # the pull loop, one line per tick
kubectl -n $ANS get secret meko-agent-state       # its persisted identity
```

A healthy first start:

```
meko-agent starting: env=meko-dev ycp=http://…
cluster facts: provider=aws region=us-east-2 k8s=v1.33.13-eks-… nodes=17
enrolled: environment_id=meko-dev (status token acquired)
status: state=READY datapacks=0 desired=0 rev=0 -> 204
```

A healthy **re**start says `resuming as env=meko-dev with the persisted status
token; not enrolling` and never mentions enrolling. If it does re-enroll, its
`meko-agent-state` Secret is missing or unreadable, and it will keep working only
until the enrollment token's 30-minute TTL expires.

Failure modes worth knowing:

| Symptom | Cause |
|---|---|
| `cannot read agent state` then exit | the ServiceAccount lacks `get` on secrets in its namespace. Deliberately fatal: enrolling instead would spend a bootstrap credential to replace one that may already be there. |
| `enrolled but could not persist agent state` | it lacks `create`/`update` on secrets. Also fatal, while the enrollment token is still reusable. |
| `enroll -> 401`, repeating | the enrollment token is expired or wrong. Mint another and update the Secret. |
| `desired-state unavailable … leaving the data plane untouched this tick` | the control plane is unreachable or erroring. The agent does nothing rather than reconciling against an empty desired state. |

`meko-agent-state` holds a live `mko_sts_` token that the control plane keeps
honouring until the environment is deregistered. Deleting the agent's release does
not remove it — delete the namespace, and `DELETE /api/v1/environments/{id}` to
stop the token being accepted.

## Configuration reference

**`meko-agent`** — all environment variables; the chart's `agent.*` values set them.

| Env | Default | Purpose |
|-----|---------|---------|
| `YCP_URL` | `http://ycp:8080` | Control-plane base URL. Include the subpath when the control plane runs with `--base-path` (e.g. `https://devcloud.yugabyte.com/ycp`); a trailing slash is trimmed. |
| `ENV_NAME` | `itest-mcs` | Environment to enroll as; must match the registered id (the slug of the name it was registered under). The default is demo residue — always set it. |
| `ENROLLMENT_TOKEN` | _(unset)_ | One-time bootstrap credential, 30-min TTL. Read from a Secret, never a command line. |
| `ENV_PLACEMENT` | `cloud` | Reported placement. |
| `ENV_PROVIDER` | `gke` | Reported provider; not detected. |
| `ENV_REGION` | `us-west1` | Reported region; not detected. |
| `MANAGED_NS_PREFIX` | `ycp-dp-` | Namespace prefix the agent creates **and garbage-collects**. Anything matching it is considered agent-owned. |
| `DP_DB_URL` | _(empty)_ | Data-plane Postgres DSN. Empty = datapacks reconcile to a namespace only, no per-datapack database. |
| `DPMCP_IMAGE` | `postgrest/postgrest:v12.2.3` | Image for provisioned per-datapack servers. The default is not our `dpmcp` image — set it. |
| `AGENT_VERSION` | `v1.4.2` | Version string reported in status. Not derived from the build. |

**`dpmcp`**

| Flag / env | Default | Purpose |
|------------|---------|---------|
| `-addr` | `:3000` | Listen address. |
| `-db` / `DP_DB_URL` | _(empty)_ | Datapack Postgres DSN; empty = in-memory store. |
