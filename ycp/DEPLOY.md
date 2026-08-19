# Deploying the YCP data plane

The image built from this directory — `meko-agent` — and how it is
built, configured, tested and operated. For the component overview see
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
useful outside a cluster — use the e2e below instead.

## Container images

Built and pushed to the platform ECR registry on every merge to `main` by
[`.github/workflows/ycp-images.yml`](../.github/workflows/ycp-images.yml), tagged
with both the commit SHA (pin this in a deployment) and `latest`. PRs build them
but never push.

| Dockerfile | Binary | Image |
|------------|--------|-------|
| `Dockerfile.agent` | `cmd/agent` (BYOC pull agent) | `161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/meko-agent` |

It is `gcr.io/distroless/static:nonroot` at runtime — no shell, no package
manager, so `kubectl exec` into them will not give you a prompt. Use logs and
port-forward.

The control plane's image (`meko/meko-api-server`) is **not** built by CI; it is
still pushed by hand. The runbook's Prerequisites section says how to check that a
given tag carries the control-plane flags.

To build locally (`podman build` works the same, and is what the e2e uses when
docker is absent):

```bash
docker build -f Dockerfile.agent -t meko-agent:dev .
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
proving the status token was persisted on both sides. It then commands a second
database, asserts the agent brings it up, and withdraws it to assert the GC.

It deploys the control plane under a `/ycp` base path, matching how production
fronts it at `https://devcloud.yugabyte.com/ycp/`, and asserts the mount: the
console answers at `/ycp/` and `/ycp`, the API under `/ycp/api/v1`, the domain
root 404s, and `/api/v1/health` still answers there (which is also what the pod's
readiness probe uses). The agent's `YCP_URL` carries the subpath. The prefix lives
only in `$BASE` in `run.sh` and is substituted into the manifests' `__BASE_PATH__`
placeholder at apply time, so `YCP_E2E_BASE_PATH= ./test/e2e/run.sh` tests the
root mount with nothing else to edit.

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
[`meko-pr-build.yml`](../.github/workflows/meko-pr-build.yml). The e2e fires on `ycp/**` and
on the `api_server` files that make up the control plane (its flags, router,
environment service/repository/handlers, console and migrations), since it builds
that image too.

## Telling an attached agent what to run

**You don't.** Registering an environment is the whole operator step:

```bash
curl -X POST "$YCP/api/v1/environments" \
  -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H 'content-type: application/json' \
  -d '{"name":"acme-prod","placement":"cloud","region":"us-east-2"}'
```

The control plane resolves the Meko release itself and stores it as that environment's
stack; the enroll response hands it to the agent, which deploys the lot. The caller
supplies a name. It does not need to know which six image tags go together — that is
the knowledge a control plane exists to hold, and it is pinned in one place,
[`meko/api_server/internal/service/dataplane_release.go`](../meko/api_server/internal/service/dataplane_release.go).
**To ship a new data-plane release, bump the tags there.** Environments registered
afterwards get the new set; existing ones keep what they were registered with.

What comes up, in the agent's own namespace:

| Component | Image | Port |
|---|---|---|
| `yugabytedb` | `yugabyte-db/ybdb-meko-release` — the MAGE-enabled build | 5433 (YSQL) |
| `api-server` | `meko/meko-api-server` | 25433 |
| `mcp-server` | `meko/meko-mcp-server` | 8000 |
| `rag-worker` | `meko/rag-agent` | — |
| `inference-gateway` | `meko/inference-gateway` | 8088 |
| `ui` | `meko/meko-ui` | 5000 |

plus `mage` and `pg_dist_rag` (and `vector`, via `CASCADE`) in the `meko` database.

Fleet-wide knobs, on the control plane rather than per environment:

| Control-plane flag / env | Chart value | Effect |
|---|---|---|
| `--dataplane-registry` / `MEKO_DATAPLANE_REGISTRY` | `apiServer.dataplane.registry` | Where the pinned images are pulled from. Defaults to the platform ECR; set it to mirror the release somewhere else without touching six tags. |
| `--dataplane-image` (repeatable) | — | Override one component: `-dataplane-image mcp_server=<ref>`. Names: `database`, `api_server`, `mcp_server`, `rag_worker`, `inference_gateway`, `ui`. A reference that names a host is taken as written, so an override can point at a different registry entirely. A misspelt component fails startup rather than silently deploying the pinned image. |
| `--dataplane-mcp-image` / `MEKO_DATAPLANE_MCP_IMAGE` | `apiServer.dataplane.mcpImage` | The `mcp_server` override under its older name. Kept because the chart and the local scripts set it. |
| `--dataplane-image-pull-secret` / `MEKO_DATAPLANE_IMAGE_PULL_SECRET` | `apiServer.dataplane.imagePullSecret` | `image_pull_secret` for the seeded stack. **Every image in the release is private**, so without this the whole stack sits in `ImagePullBackOff` — needed on every GKE cluster, since GKE has no AWS IAM. The control plane warns at boot if it is unset. |
| `--dataplane-secret-ref` / `MEKO_DATAPLANE_SECRET_REF` | `apiServer.dataplane.secretRef` | `secret_ref` for the seeded stack — the Secret holding `OPENAI_API_KEY`, the AWS keys and the Cognito settings. |
| `--no-bootstrap-defaults` / `MEKO_NO_BOOTSTRAP_DEFAULTS` | `apiServer.dataplane.noBootstrapDefaults` | Seed nothing. Every environment then runs only what desired state declares. |
| `--console-embed-admin-secret` / `MEKO_CONSOLE_EMBED_ADMIN_SECRET` | — | Write the admin secret into the console page so it never asks for one. **Off by default, and leave it off where the console is reachable by anyone you would not hand the fleet-admin credential to** — it grants no new capability against that control plane, since the console can already do everything the credential allows, but it does turn "can load the console" into "has the secret". Unset, the console asks in-page and keeps it in `sessionStorage`. |

Both Secrets named above live in the **agent's** namespace, in whatever cluster that
is. The control plane only ever learns their names — desired state carries no secret
material, so nothing here is a credential. That is also why `api_server`'s Cognito
flags gained environment fallbacks (`COGNITO_REGION`, `COGNITO_POOL_ID`,
`COGNITO_CLIENT_ID`, `COGNITO_CLIENT_SECRET`, `COGNITO_DOMAIN`): a flag value is
visible in the pod spec to anyone who can read the namespace, so the data-plane
api-server takes its credentials from `envFrom` and only its database wiring as flags.
The one flag that carries a secret, `-db-password`, is written `$(DB_PASSWORD)` and
expanded by Kubernetes from a `secretKeyRef`.

`POST /api/v1/environments` also takes `"desired": [...]` to declare an environment's
resources explicitly instead — `[]` registers one that runs nothing until commanded.

To change what an environment runs afterwards, replace its desired state. That is one
call, and the API is the only way to make it — the console reads desired state but
does not edit it. Desired state wins over the stack the agent enrolled with whenever the
agent can read it, so this is also how the stack is replaced or withdrawn (`[]` runs
nothing); the enrolled copy is the fallback for when the control plane is unreachable:

```bash
curl -X POST "$YCP/api/v1/environments/$ENV/desired" \
  -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H 'content-type: application/json' \
  -d '{"resources":[
        {"kind":"database","name":"meko","spec":{}},
        {"kind":"mcp-server","name":"mcp","spec":{
           "image":"161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/meko-mcp-server:<tag>",
           "image_pull_secret":"meko-ecr",
           "secret_ref":"meko-data-plane"}},
      ]}'

curl -H "X-Meko-Admin-Secret: $ADMIN_KEY" "$YCP/api/v1/environments/$ENV/desired"   # read it back
```

The set is **replaced**, not merged: anything left out is garbage-collected on the
next tick.

A withdrawn database's **volume survives**, though. The GC deletes the StatefulSet, and
Kubernetes does not delete PersistentVolumeClaims created from `volumeClaimTemplates`
unless a retention policy says so — which the agent deliberately does not set, because
silently destroying a customer's data on a desired-state edit is not a default anything
should have. Withdrawing a database therefore leaves `data-<name>-0` behind, holding its
storage until someone deletes it:

```bash
kubectl -n $ANS get pvc -l meko.dev/stack=true    # what the stack still holds
kubectl -n $ANS delete pvc data-<name>-0          # deliberate, and irreversible
```

Spec fields, all optional unless noted. Secret fields are **names**, never values.

| Kind | Field | Default | Notes |
|------|-------|---------|-------|
| `meko-stack` | `images.database` | **required** | The MAGE-enabled YugabyteDB build. No default: `mage` and `pg_dist_rag` are not in the public `yugabytedb` image, so a fallback would start cleanly and then fail every `CREATE EXTENSION`. |
| | `images.api_server` / `.mcp_server` / `.rag_worker` / `.inference_gateway` / `.ui` | _(none)_ | Private images, no tag worth guessing. A component with no image is reported as failed and the rest of the stack still comes up. |
| | `image_pull_secret` / `secret_ref` | _(none)_ | Secret **names** in the agent's namespace, applied to every component. `secret_ref` is mounted with `envFrom` and `optional: true`, so a Secret the operator has not created yet does not block the rollout. |
| | `database` | `{engine: yugabytedb}` | A full `database` spec (below). The engine defaults to `yugabytedb` here rather than `postgres`, and `master_flags`/`tserver_flags` default to `ysql_yb_enable_mage=true` on both. Declaring either flag list replaces that default rather than merging with it — a flag you cannot unset is worse than one you have to set. |
| | `database_name` | `meko` | Created by the agent, and where the extensions go. |
| | `extensions` | `["mage","pg_dist_rag"]` | An explicit `[]` installs none, for an external database whose owner manages its own. Names must be plain identifiers. |
| | `disable` | _(none)_ | Components to leave out, by name (`ui`, `inference-gateway`, …). The rest still comes up. |
| | `expose` | _(none)_ | Puts a LoadBalancer in front of a component instead of its ClusterIP: `{"ui":"internal"}` or `{"ui":"external"}`. **`internal` is VPC-only** (what the control plane itself uses, and not reachable from a laptop without a VPN — port-forwarding stays the usual way in); **`external` is a public address**, so it is only right for a component that authenticates its own callers. Meko's api-server does not when Cognito is unset, which is the default. Unmentioned components keep their ClusterIP; an unrecognised value is refused rather than guessed. |
| | `public_base_url` | _(none)_ | The address a browser reaches this data plane on. Becomes the gateway's `INFERENCE_CORS_ORIGINS` — its default list is the SaaS origins, so without this a UI served from the data plane calls a gateway that refuses the request, and a chat silently never answers. |
| | `langfuse_base_url` / `aws_region` / `s3_bucket` / `log_level` | unset / `us-east-1` / unset / `INFO` | |
| `database` | `mode` | `managed` | `external` points the stack at a database the agent does not run (the customer's YugabyteDB). Then `host` is required and no component is reported for it. |
| | `engine` | `postgres` | Or `yugabytedb`. Sets the reported component name and the port/user/database defaults, and for `yugabytedb` the entrypoint (`bin/yugabyted start`, which the image does not run on its own), a **headless** Service, and `PGPASSWORD` alongside `YSQL_PASSWORD`. |
| | `master_flags` / `tserver_flags` | _(none)_ | YugabyteDB gflags as an object, rendered into `--master_flags` / `--tserver_flags`. Sorted before rendering, so the pod template does not change — and the database does not restart — from one tick to the next. |
| | `image` | `pgvector/pgvector:pg16` | pgvector because mem0 stores embeddings; a stock postgres starts fine and fails on the first memory write. `yugabytedb/yugabyte:…` for that engine. |
| | `port` / `user` / `database` | per engine | `5432`/`postgres`/`postgres`, or `5433`/`yugabyte`/`yugabyte`. |
| | `secret_ref` / `password_key` | `meko-database` / `password` | The agent generates the password once if the Secret is absent. |
| | `storage_size` / `storage_class` | `8Gi` / cluster default | **Cannot be changed after creation** — `volumeClaimTemplates` are immutable. |
| | `command` / `args` | image default | Escape hatch for an engine the agent has no special knowledge of. |
| `mcp-server` | `image` | **required** | Private image, no default worth guessing. |
| | `image_pull_secret` | _(none)_ | Needed wherever the namespace has no ECR credentials. |
| | `secret_ref` | _(none)_ | Mounted whole with `envFrom` — `OPENAI_API_KEY`, AWS keys. Optional at the pod level, so a missing Secret does not block the rollout. |
| | `database_name` | `meko` | Created by the agent inside the declared database. |
| | `replicas` / `port` | `1` / `8000` | |
| | `meko_base_url` / `langfuse_base_url` / `aws_region` / `log_level` | unset / unset / `us-east-1` / `INFO` | |
| | `create_tables` | managed → `true` | `MEM0_CREATE_TABLES`. On for a database the agent created (nothing else provisioned a schema), off for an external one. |
| `rag-worker` | `image` | **required** | As above; plus `s3_bucket`, `aws_region`. |

`langfuse` has no reconciler: v3 needs ClickHouse, Redis and object storage
alongside it, which is a chart's job. A declared `langfuse` resource is logged and
skipped, and `meko-stack` does not include it.

**The inference gateway will not start without `OPENAI_API_KEY`** in the Secret named by
`secret_ref`. It builds its LangChain client while the app is being constructed, so a
missing key is not a degraded gateway — the process exits with
`openai.OpenAIError: Missing credentials` before it serves anything, and the component
reports 0/1 forever. The MCP server is laxer about the same key: it starts and fails on
the first embedding instead.

**The api-server waits for its database rather than crash-looping at it.** It gets an
init container that polls the YSQL port, because `api_server` pings five times over
fifteen seconds and then exits, while a `yugabyted` takes about ninety seconds to come
up — measured on kind, the pod without the gate was still in a five-minute
`CrashLoopBackOff` eleven minutes in, long after the database was serving. A pod sitting
at `Init:0/1` also says what it is waiting for, which `CrashLoopBackOff` does not.

Two things the stack cannot configure, worth knowing before pointing a browser at it:

- **The UI's Cognito settings are baked into its bundle at build time**
  (`VITE_COGNITO_*` in `meko_ui/Dockerfile`), so a data plane authenticates against
  whatever pool its image was built for. Only the runtime allow-list in
  `meko_ui/server/index.ts` — feature flags, the admin view, RudderStack — is settable
  from desired state.
- **No Langfuse means no observability endpoints.** `meko-stack` does not deploy
  Langfuse (see above), and `api_server`'s observability routes need a direct connection
  to Langfuse's Postgres — without `--langfuse-db-*` they return 503. Everything else
  the api-server serves is unaffected.

The api-server itself needs no Kubernetes access: it runs with the namespace's default
ServiceAccount and never builds an in-cluster client, so it does not need RBAC of its
own and cannot fail to start for want of it.

Datapacks are **not** modelled here. An environment is the unit YCP manages; what
runs inside Meko — datapacks, users, tenants — is Meko's own concern, created
through its api-server once the stack is up.

## Which environments a fleet call sees

The admin secret is a fleet-wide credential, so a caller presenting only it — the
console, the e2e, your curl — reads and writes every environment. That is the
default; `--require-account-header` (below) makes it a 400 instead.

`X-Meko-Account-ID` narrows a call to one account. YBM sends it on every request it
proxies, stamped from the account it has already authenticated:

```bash
curl -H "X-Meko-Admin-Secret: $ADMIN_KEY" -H "X-Meko-Account-ID: $ACCOUNT" \
  "$YCP/api/v1/environments"                      # only $ACCOUNT's environments
```

With the header set, registration stamps the new environment with that account and
ignores any `account_id` in the body, and every other `/environments` route — read,
patch, deregister, desired state — is restricted to environments carrying it. One
that belongs to another account answers 404, the same as one that does not exist.

Environment ids come from the name, and the name is unique per account rather than
across the fleet — `(account_id, id)` is what the database enforces. So two accounts
may each register `prod` and each gets its own environment; neither can address or
overwrite the other's. Re-registering your own name updates it, as it always did.

The id an operator and an agent use is unchanged by that. Rows are keyed internally
by a uuid, which is what the desired-resources table and the enrollment handshake
join on, but nothing in the API takes it: `{id}` in the path, `?envId=` from the
agent, and `--set agent.envName=` are all still the name-derived slug.

The one call that can now be ambiguous is an **unscoped** read of an id two accounts
both hold: it answers 409 rather than picking one. Send an account to resolve it.
That cannot happen for environments registered before the per-account index, whose
ids were unique fleet-wide — and it cannot happen at all with the header required.

Environments registered before accounts were stamped carry no account, and are
visible only to an unscoped caller.

To make the header mandatory, so no call can reach the whole fleet:

| Control-plane flag / env | Chart value | Effect |
|---|---|---|
| `--require-account-header` / `MEKO_REQUIRE_ACCOUNT_HEADER` | `apiServer.requireAccountHeader` | Answer 400 to any fleet call with no `X-Meko-Account-ID`, instead of serving it fleet-wide. **Off by default**, because it takes the unscoped path away: the console, the e2e and operator curl all authenticate with the admin secret alone. For a control plane reached only through YBM. |

The agent API is unaffected either way — it authenticates with the enrollment and
status tokens, not the admin secret.

## Operating an attached agent

```bash
kubectl -n $ANS logs deploy/meko-agent            # the pull loop, one line per tick
kubectl -n $ANS get secret meko-agent-state       # its persisted identity
kubectl -n $ANS get all,cm -l meko.dev/stack=true # what it provisioned from desired state
```

A healthy first start:

```
meko-agent starting: env=meko-dev ycp=http://…
cluster facts: provider=aws region=us-east-2 k8s=v1.33.13-eks-… nodes=17
enrolled: environment_id=meko-dev (status token acquired)
status: state=READY components=[meko-agent 1/1] desired=0 rev=0 -> 204
```

A healthy **re**start says `resuming as env=meko-dev with the persisted status
token; not enrolling` and never mentions enrolling. If it does re-enroll, its
`meko-agent-state` Secret is missing or unreadable, and it will keep working only
until the enrollment token's 30-minute TTL expires.

The same Secret holds the stack, so a restarted agent also says `enrolled with a Meko
stack to run; deploying it on every tick` without asking the control plane anything. If
it instead says `no stack came with enrollment`, this environment has none — a control
plane running `--no-bootstrap-defaults`, one registered with an explicit `desired`, or
one older than this feature. The environment then runs only what desired state declares.

Failure modes worth knowing:

| Symptom | Cause |
|---|---|
| `cannot read agent state` then exit | the ServiceAccount lacks `get` on secrets in its namespace. Deliberately fatal: enrolling instead would spend a bootstrap credential to replace one that may already be there. |
| `enrolled but could not persist agent state` | it lacks `create`/`update` on secrets. Also fatal, while the enrollment token is still reusable. |
| `enroll -> 401`, repeating | the enrollment token is expired or wrong. Mint another and update the Secret. |
| `desired-state unavailable … holding what we last read and changing nothing else` | the control plane is unreachable or erroring. The agent keeps reconciling toward the last desired state it read — that is the point of persisting it — and skips only garbage collection, since it cannot know what an operator withdrew from a response it never got. A data plane survives a control plane being deleted outright: one ran for two and a half days that way, six components healthy throughout. |
| every pod `ImagePullBackOff` | no `image_pull_secret`. The whole release is private; the control plane warns about this at boot. |
| the database is `1/1` but `create extension mage` fails | the wrong database image. `mage` and `pg_dist_rag` are in `yugabyte-db/ybdb-meko-release`, not the public `yugabytedb` image. |
| `api-server` sits at `Init:0/1` | it is waiting for the YSQL port. Normal for the first ~90s; persisting past that means the database is not coming up. |
| a component is `0/1` with `InvalidImageName` | an image reference the kubelet cannot parse — nearly always a placeholder left in a copied example (`…`, `<registry>`, `<tag>`). The agent refuses these now and says so on the component row, so seeing it means the workload predates that check. |
| a fixed `images.database` does not take effect | **a StatefulSet whose pod can never become Ready blocks its own replacement.** Rolling updates wait for the current pod, so a pod stuck in `InvalidImageName` or `ImagePullBackOff` is never replaced however many times the spec is corrected. Delete it: `kubectl -n $ANS delete pod <db>-0`. Deployments do not have this problem — only the database is a StatefulSet. |
| `api-server` crash-loops on a migration naming `dist_rag` | it started before the extensions existed. Self-correcting — the agent creates them each tick and kubelet restarts the pod — and the reason is on the `api-server` component row, which carries the extension error. |
| `inference-gateway` 0/1 with `Missing credentials` | no `OPENAI_API_KEY` in the Secret named by `secret_ref`. It builds its LLM client at startup, so this is fatal rather than degraded. |
| the database never reports ready, `no password supplied` in the probe | the container is missing `PGPASSWORD`. `yugabyted` turns `ysql_enable_auth` on as soon as `YSQL_PASSWORD` is set, so the readiness probe's own `ysqlsh` needs the password too. The agent sets both; a hand-written `command`/`args` override that drops the env will not. |
| the database is up but everything gets `password authentication failed for user "yugabyte"` | its **first** start failed part-way through initialisation, and `yugabyted` applies `YSQL_PASSWORD` only on first install — so auth is on with the default password while every consumer uses the generated one, and no number of restarts will fix it. Check `kubectl logs <db-pod> --previous` for what killed the first start (a full node is the one seen in practice: `insufficient disk space`, which `yugabyted` hits while setting the password). Recovery is to discard the half-initialised volume and let it initialise again, which **destroys any data in it**: `kubectl -n $ANS delete pvc data-<db>-0 --wait=false && kubectl -n $ANS delete pod <db>-0`. |
| `apply … -> 403` on a stack resource | the ServiceAccount lacks `patch` (server-side apply) or the resource itself. The stack reconcilers need `patch` (server-side apply) plus `statefulsets` and `configmaps` — see `test/e2e/agent.yaml` for the full set. |
| `data-plane db not reachable yet … retrying each tick` | normal for the first ~30s after a `database` is declared. Persisting past that means the database is not coming up: check the StatefulSet. If the pod is `1/1` and connects still time out, see the DNS note below. |
| `reconcile mcp-server/… needs spec.image` | the private Meko images have no default. Declare `spec.image`, and `spec.image_pull_secret` for a namespace with no pull credentials. |
| `this database's StatefulSet … cannot be changed in place` | `storage_size` (or another `volumeClaimTemplates` field) differs from what the StatefulSet was created with, which Kubernetes forbids changing. The database keeps serving; either declare the original value or delete the StatefulSet deliberately. |

`meko-agent-state` holds a live `ycp_sts_` token that the control plane keeps
honouring until the environment is deregistered. Deleting the agent's release does
not remove it — delete the namespace, and `DELETE /api/v1/environments/{id}` to
stop the token being accepted.

### In-cluster addresses carry a trailing dot

Every Service address the agent hands out — `YUGABYTEDB_URL`, the DSNs it composes —
is written `svc.ns.svc.cluster.local**.**`, absolute.

Kubernetes puts `ndots:5` in every pod's resolv.conf, and `meko.ycp-agent.svc.cluster.local`
has four dots, so a resolver treats it as *relative*: it tries the whole search list
first (`…svc.cluster.local.ycp-agent.svc.cluster.local`, and so on) before asking for
the name as written. Go's resolver does this, and the bogus lookups alone were enough
to exhaust a 10-second connect budget. The symptom is a database that is plainly
healthy — `1/1`, reachable by ClusterIP, instant for the same name written with a
trailing dot — while every connection the agent makes times out, so components report
0/1 for a database sitting next to them. If you write one of these addresses by hand
(`spec.host` on an external database, `agent.ycpUrl`), the short form (`meko`) and the
absolute form both work; the four-dot relative form is the one to avoid.

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
| `AGENT_VERSION` | `v1.4.2` | Version string reported in status. Not derived from the build. |

